// sdk/go/anvil/endpoint_grants.go
// Hand-written. Copied in from sdk/overlays/go during gen-sdk.
//
// Grant targets and VPC endpoint network wiring for Go programs. Go resources
// don't expose their logical name synchronously, so the adapters here take the
// name you constructed the resource with.

package anvil

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"

	anvilaws "github.com/anvil-cloud/anvil/sdk/go/anvil/aws"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	awsvpc "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/vpc"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// NetworkGrantTarget is a GrantTarget with a security group on its network
// interfaces. Grants that pass an Endpoint need one.
type NetworkGrantTarget interface {
	GrantTarget
	// GrantSecurityGroupId resolves to "" when the target is not in a VPC.
	GrantSecurityGroupId() pulumi.StringOutput
}

type lambdaTarget struct {
	name string
	fn   *anvilaws.Lambda
}

func (t *lambdaTarget) GrantName() string                 { return t.name }
func (t *lambdaTarget) GrantRoleArn() pulumi.StringOutput { return t.fn.RoleArn }
func (t *lambdaTarget) GrantSecurityGroupId() pulumi.StringOutput {
	return t.fn.SecurityGroupId.ApplyT(func(id *string) string {
		if id == nil {
			return ""
		}
		return *id
	}).(pulumi.StringOutput)
}

// LambdaTarget makes an Anvil Lambda a grant target. Pass the same name you
// constructed it with; it names the IAM policy and SG rules.
//
//	fn, _ := anvilaws.NewLambda(ctx, "processor", &anvilaws.LambdaArgs{...})
//	target := anvil.LambdaTarget("processor", fn)
func LambdaTarget(name string, fn *anvilaws.Lambda) NetworkGrantTarget {
	return &lambdaTarget{name: name, fn: fn}
}

// GrantEndpoint is a VPC endpoint a grant routes through. Build it with Endpoint.
type GrantEndpoint struct {
	// Name is the endpoint's logical name; it names the SG rules.
	Name            string
	EndpointId      pulumi.StringOutput
	SecurityGroupId pulumi.StringOutput
	Service         pulumi.StringOutput
	// Opts are applied to the SG rules, e.g. ctx.Provider("aws"). SG rules are
	// regional, so pass the provider the endpoint was created with.
	Opts []pulumi.ResourceOption
}

// Endpoint wraps an Anvil VpcEndpoint for GrantOptions.Endpoint. Pass the same
// name you constructed it with, and the provider option you created it with:
//
//	anvil.Endpoint("sqs", sqsEndpoint, ctx.Provider("aws"))
func Endpoint(name string, ep *anvilaws.VpcEndpoint, opts ...pulumi.ResourceOption) *GrantEndpoint {
	return &GrantEndpoint{
		Name:            name,
		EndpointId:      ep.EndpointId,
		SecurityGroupId: ep.SecurityGroupId,
		Service:         ep.Service,
		Opts:            opts,
	}
}

// IAM action prefixes whose VPC endpoint service has a different name.
var endpointServiceAliases = map[string]string{
	"kinesis": "kinesis-streams",
}

// endpointServiceForActions returns the VPC endpoint service the actions
// belong to, e.g. ["sqs:SendMessage"] → "sqs". All actions must share one.
func endpointServiceForActions(actions []string) (string, error) {
	var prefix string
	for _, a := range actions {
		p, _, ok := strings.Cut(a, ":")
		if !ok {
			return "", fmt.Errorf("action %q has no service prefix", a)
		}
		if prefix != "" && p != prefix {
			return "", fmt.Errorf("an endpoint grant must cover one service, got %q and %q", prefix, p)
		}
		prefix = p
	}
	if prefix == "" {
		return "", fmt.Errorf("an endpoint grant needs at least one action")
	}
	if alias, ok := endpointServiceAliases[prefix]; ok {
		return alias, nil
	}
	return prefix, nil
}

var (
	wiredEndpointPairsMu sync.Mutex
	wiredEndpointPairs   = map[string]bool{}
)

// ConnectToEndpoint opens one-way network access from a target to a VPC
// endpoint: ingress on the endpoint SG from the target SG, and egress on the
// target SG to the endpoint SG, both on port. Created once per (endpoint,
// target, port). The target never carries the endpoint SG, so targets never
// trust each other.
//
// The rules have no parent: they read the endpoint's and the target's outputs,
// and a child of a component that reads that component's outputs waits on
// itself. Pass the endpoint's provider in opts (SG rules are regional).
func ConnectToEndpoint(
	ctx *pulumi.Context,
	endpointName string,
	endpointSecurityGroupId pulumi.StringInput,
	target GrantTarget,
	port int,
	opts ...pulumi.ResourceOption,
) error {
	nt, ok := target.(NetworkGrantTarget)
	if !ok {
		return fmt.Errorf(
			"%s cannot use a VPC endpoint because it has no security group; wrap a VPC Lambda with anvil.LambdaTarget",
			target.GrantName(),
		)
	}

	key := fmt.Sprintf("%s|%s|%d", endpointName, target.GrantName(), port)
	wiredEndpointPairsMu.Lock()
	done := wiredEndpointPairs[key]
	wiredEndpointPairs[key] = true
	wiredEndpointPairsMu.Unlock()
	if done {
		return nil
	}

	targetName := target.GrantName()
	targetSgID := nt.GrantSecurityGroupId().ApplyT(func(id string) (string, error) {
		if id == "" {
			return "", fmt.Errorf(
				"%s is not in a VPC, so it cannot be granted through endpoint %s; set Vpc on it or drop the endpoint",
				targetName, endpointName,
			)
		}
		return id, nil
	}).(pulumi.StringOutput)
	tags := pulumi.StringMap{"ManagedBy": pulumi.String("anvil")}

	if _, err := awsvpc.NewSecurityGroupIngressRule(ctx,
		fmt.Sprintf("%s-from-%s-%d", endpointName, targetName, port),
		&awsvpc.SecurityGroupIngressRuleArgs{
			SecurityGroupId:           endpointSecurityGroupId,
			ReferencedSecurityGroupId: targetSgID,
			IpProtocol:                pulumi.String("tcp"),
			FromPort:                  pulumi.Int(port),
			ToPort:                    pulumi.Int(port),
			Tags:                      tags,
		}, opts...); err != nil {
		return fmt.Errorf("ingress %s → %s: %w", targetName, endpointName, err)
	}
	if _, err := awsvpc.NewSecurityGroupEgressRule(ctx,
		fmt.Sprintf("%s-to-%s-%d", targetName, endpointName, port),
		&awsvpc.SecurityGroupEgressRuleArgs{
			SecurityGroupId:           targetSgID,
			ReferencedSecurityGroupId: endpointSecurityGroupId,
			IpProtocol:                pulumi.String("tcp"),
			FromPort:                  pulumi.Int(port),
			ToPort:                    pulumi.Int(port),
			Tags:                      tags,
		}, opts...); err != nil {
		return fmt.Errorf("egress %s → %s: %w", targetName, endpointName, err)
	}
	return nil
}

// ConnectToDSQL opens network access on 5432 from a target to a DSQL cluster's
// VPC endpoint. Call it alongside the DSQLConnect grant when the cluster was
// created with vpc and without hasNat. name is the cluster's logical name;
// opts should carry the provider the cluster was created with.
func ConnectToDSQL(ctx *pulumi.Context, name string, cluster *anvilaws.DSQL, target GrantTarget, opts ...pulumi.ResourceOption) error {
	endpointSgID := cluster.VpcEndpointSecurityGroupIds.ApplyT(func(ids map[string]string) (string, error) {
		if len(ids) != 1 {
			return "", fmt.Errorf("%s should have exactly one VPC endpoint, found %d; was it created with vpc and without hasNat?", name, len(ids))
		}
		for _, id := range ids {
			return id, nil
		}
		return "", nil
	}).(pulumi.StringOutput)
	return ConnectToEndpoint(ctx, name+"-vpce", endpointSgID, target, 5432, opts...)
}

// EndpointAccessOptions are the optional IAM half of GrantEndpointAccess.
// Set both or neither.
type EndpointAccessOptions struct {
	// Actions to allow through the endpoint, e.g. "secretsmanager:GetSecretValue".
	// Every action must belong to the endpoint's service.
	Actions []string
	// Resources the actions apply to. Use "*" only for actions that take no resource.
	Resources []pulumi.StringInput
}

// endpointActionPrefixes maps VPC endpoint services to their IAM action
// prefixes where the two differ. Any other service uses its own name.
var endpointActionPrefixes = map[string][]string{
	"kinesis-streams": {"kinesis"},
	"ecr.api":         {"ecr"},
	"ecr.dkr":         {"ecr"},
	"monitoring":      {"cloudwatch"},
}

// actionsOutsideService returns the actions that don't belong to service.
func actionsOutsideService(service string, actions []string) []string {
	allowed, ok := endpointActionPrefixes[service]
	if !ok {
		allowed = []string{service}
	}
	var outside []string
	for _, a := range actions {
		prefix, _, _ := strings.Cut(a, ":")
		match := false
		for _, p := range allowed {
			if p == prefix {
				match = true
			}
		}
		if !match {
			outside = append(outside, a)
		}
	}
	return outside
}

// shortHash is 32-bit FNV-1a in hex, the same as the TS and Python runtimes.
func shortHash(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

var (
	endpointAccessPoliciesMu sync.Mutex
	endpointAccessPolicies   = map[string]bool{}
)

// GrantEndpointAccess grants a VPC compute resource access to an AWS service
// through a VPC endpoint. Use it for services Anvil has no component for
// (Secrets Manager, KMS, SSM, STS, ECR, CloudWatch Logs, ...).
//
// It always opens the network: ingress 443 on the endpoint's SG from the
// target's own SG, and egress 443 on the target's SG to the endpoint's SG
// (once per pair, shared with CreateGrant's Endpoint option).
//
// With opts.Actions and opts.Resources it also adds an IAM statement on the
// target's role, limited to the endpoint with aws:SourceVpce. The SG rules
// and the policy use endpoint.Opts (pass the provider to anvil.Endpoint).
//
//	anvil.GrantEndpointAccess(pctx, anvil.Endpoint("secrets", ep, ctx.Provider("aws")), target,
//	    &anvil.EndpointAccessOptions{
//	        Actions:   []string{"secretsmanager:GetSecretValue"},
//	        Resources: []pulumi.StringInput{dbSecretArn},
//	    })
func GrantEndpointAccess(ctx *pulumi.Context, endpoint *GrantEndpoint, target GrantTarget, opts *EndpointAccessOptions) error {
	var actions []string
	var resources []pulumi.StringInput
	if opts != nil {
		actions, resources = opts.Actions, opts.Resources
	}
	if (len(actions) == 0) != (len(resources) == 0) {
		return fmt.Errorf("GrantEndpointAccess(%s, %s) needs both Actions and Resources, or neither", target.GrantName(), endpoint.Name)
	}

	if err := ConnectToEndpoint(ctx, endpoint.Name, endpoint.SecurityGroupId, target, 443, endpoint.Opts...); err != nil {
		return err
	}
	if len(actions) == 0 {
		return nil
	}

	sorted := append([]string(nil), actions...)
	sort.Strings(sorted)
	name := fmt.Sprintf("%s-%s-access-%s", endpoint.Name, target.GrantName(), shortHash(strings.Join(sorted, ",")))
	endpointAccessPoliciesMu.Lock()
	dup := endpointAccessPolicies[name]
	endpointAccessPolicies[name] = true
	endpointAccessPoliciesMu.Unlock()
	if dup {
		return fmt.Errorf("%s is already granted these actions through %s; put all the resources for the same actions in one GrantEndpointAccess call", target.GrantName(), endpoint.Name)
	}

	inputs := toOutputs(resources)
	n := len(inputs)
	inputs = append(inputs, endpoint.EndpointId, endpoint.Service)
	policy := pulumi.All(inputs...).ApplyT(func(args []interface{}) (string, error) {
		arns := make([]string, n)
		for i := 0; i < n; i++ {
			arns[i] = args[i].(string)
		}
		endpointID, service := args[n].(string), args[n+1].(string)
		if outside := actionsOutsideService(service, actions); len(outside) > 0 {
			return "", fmt.Errorf("endpoint %s routes %q, so it cannot carry %s", endpoint.Name, service, strings.Join(outside, ", "))
		}
		b, err := json.Marshal(map[string]interface{}{
			"Version": "2012-10-17",
			"Statement": []map[string]interface{}{{
				"Effect":    "Allow",
				"Action":    actions,
				"Resource":  arns,
				"Condition": map[string]interface{}{"StringEquals": map[string]string{"aws:SourceVpce": endpointID}},
			}},
		})
		return string(b), err
	}).(pulumi.StringOutput)

	roleName := target.GrantRoleArn().ApplyT(func(arn string) string {
		if i := strings.LastIndex(arn, "/"); i >= 0 {
			return arn[i+1:]
		}
		return arn
	}).(pulumi.StringOutput)

	// No parent: it reads the endpoint's and the target's outputs.
	_, err := iam.NewRolePolicy(ctx, name, &iam.RolePolicyArgs{Role: roleName, Policy: policy}, endpoint.Opts...)
	return err
}
