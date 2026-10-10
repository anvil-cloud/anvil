package vpcendpoint

import (
	"encoding/json"
	"fmt"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/anvil-cloud/anvil/provider/internal/vpcsg"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// computePrincipals are the AWS service principals that can access endpoints
// through the VPC. These cover all Anvil-managed compute resource types.
// AWS managed services (SNS, EventBridge, S3) call SQS through the AWS
// internal network and never go through the VPC endpoint — they are not
// listed here.
var computePrincipals = []string{
	"lambda.amazonaws.com",
	"ecs-tasks.amazonaws.com",
	"ec2.amazonaws.com",
}

// VpcEndpointPermission defines a single permission statement in the endpoint policy.
// When overridePermissions is set, these replace the default allow-all policy.
// Effect must be "Allow" or "Deny". Action is an IAM action string, e.g. "sqs:SendMessage".
// Resource defaults to "*" if omitted.
type VpcEndpointPermission struct {
	// Effect is the IAM effect — "Allow" or "Deny".
	Effect string `pulumi:"effect"`

	// Action is the IAM action string, e.g. "sqs:SendMessage", "secretsmanager:GetSecretValue".
	Action string `pulumi:"action"`

	// Resource is the ARN to scope this permission to.
	// Defaults to "*" if omitted — applies to all resources of this service.
	// Accepts Output<string> — pass resource ARNs directly, e.g. queue.arn.
	Resource pulumi.StringInput `pulumi:"resource,optional"`
}

// VpcEndpointArgs defines the inputs for an Anvil-managed Interface VPC Endpoint.
//
// Accepts both Anvil-managed VPC fields and imported VPC fields — pass vpcId
// and privateSubnetIds directly, consistent with VpcPlacementArgs.
//
// Cost: ~$7-9/month per endpoint per AZ. A 3-AZ deployment costs ~$21-27/month
// per endpoint service. Create endpoints deliberately — only for services with
// meaningful traffic volume or strict private routing requirements.
type VpcEndpointArgs struct {
	// VpcId is the ID of the VPC to create the endpoint in.
	VpcId pulumi.StringInput `pulumi:"vpcId"`

	// PrivateSubnetIds are the IDs of the private subnets to attach the endpoint
	// to. AWS places one ENI per subnet. Pass all private subnet IDs — typically
	// one per AZ.
	PrivateSubnetIds pulumi.StringArrayInput `pulumi:"privateSubnetIds"`

	// Service is the AWS service to route privately.
	// Validated against the AwsVpcEndpointService enum in the schema.
	// The full com.amazonaws.{region}.{service} name is constructed at deploy time.
	Service pulumi.StringInput `pulumi:"service"`

	// OverridePermissions replaces the default allow-all endpoint policy with
	// explicit Allow and Deny statements scoped to the compute principals.
	//
	// When omitted, the endpoint policy allows all actions (*) for all Anvil
	// compute principals (Lambda, ECS, EC2).
	//
	// When set, only the specified actions are permitted — the user is
	// responsible for declaring every action their compute resources need.
	// Supports both Allow and Deny effects, enabling fine-grained control
	// such as allowing sqs:SendMessage while explicitly denying sqs:DeleteQueue.
	//
	// Resource defaults to "*" if omitted on a permission entry.
	// Accepts Output<string> for resource ARNs — pass queue.arn, secret.arn etc directly.
	//
	// Example:
	//   overridePermissions: [
	//     { effect: "Allow", action: "sqs:SendMessage" },
	//     { effect: "Allow", action: "sqs:ReceiveMessage", resource: queue.arn },
	//     { effect: "Deny",  action: "sqs:DeleteQueue" },
	//   ]
	OverridePermissions []VpcEndpointPermission `pulumi:"overridePermissions,optional"`
}

// VpcEndpoint is the Anvil-managed Interface VPC Endpoint component resource.
type VpcEndpoint struct {
	pulumi.ResourceState

	// EndpointId is the AWS VPC endpoint ID, e.g. vpce-0abc1234567890abc.
	EndpointId pulumi.StringOutput `pulumi:"endpointId"`

	// DnsName is the first DNS name assigned to the endpoint.
	// With private DNS enabled, normal consumers use the standard AWS SDK
	// hostname — this is exposed for debugging and multi-VPC architectures only.
	DnsName pulumi.StringOutput `pulumi:"dnsName"`

	// SecurityGroupId is the ID of the dedicated security group attached to this
	// endpoint. Access requires explicit opt-in — a compute resource must have
	// this SG attached (via GrantEndpointAccess) to reach the endpoint at the
	// network layer. Resources without it are blocked regardless of IAM.
	SecurityGroupId pulumi.StringOutput `pulumi:"securityGroupId"`

	name string
}

func (v *VpcEndpoint) Annotate(a infer.Annotator) {
	a.SetToken("aws", "VpcEndpoint")
	a.Describe(&v, "An Anvil-managed AWS Interface VPC Endpoint. Creates one ENI per private subnet with private DNS enabled. The endpoint security group uses a self-referencing ingress rule on port 443 — only compute resources that have been explicitly granted access (via GrantEndpointAccess) can reach the endpoint at the network layer. Access is enforced at three layers: network (self-referencing SG), IAM role policy (scoped per compute resource), and endpoint policy (blanket ceiling on allowed actions for all compute principals).")
}

// Name returns the logical name of this endpoint, used for rule naming in grants.
func (v *VpcEndpoint) Name() string {
	return v.name
}

// endpointPolicyStatement is a single statement in the endpoint policy JSON.
type endpointPolicyStatement struct {
	Effect    string `json:"Effect"`
	Principal struct {
		Service []string `json:"Service"`
	} `json:"Principal"`
	Action   []string `json:"Action"`
	Resource string   `json:"Resource"`
}

// endpointPolicyDocument is the full endpoint policy JSON structure.
type endpointPolicyDocument struct {
	Version   string                    `json:"Version"`
	Statement []endpointPolicyStatement `json:"Statement"`
}

// buildDefaultEndpointPolicy constructs the default allow-all endpoint policy
// scoped to Anvil compute principals. Called when no overridePermissions are set.
func buildDefaultEndpointPolicy() (string, error) {
	stmt := endpointPolicyStatement{
		Effect:   "Allow",
		Action:   []string{"*"},
		Resource: "*",
	}
	stmt.Principal.Service = computePrincipals

	doc := endpointPolicyDocument{
		Version:   "2012-10-17",
		Statement: []endpointPolicyStatement{stmt},
	}

	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("failed to marshal default endpoint policy: %w", err)
	}
	return string(b), nil
}

// buildOverrideEndpointPolicy constructs the endpoint policy from the user-supplied
// overridePermissions. Groups permissions by effect into separate statements.
// Each statement shares the same compute principals.
// Resource defaults to "*" when the permission entry omits it.
func buildOverrideEndpointPolicy(perms []VpcEndpointPermission) (pulumi.StringOutput, error) {
	// Validate all effects upfront before touching any Outputs.
	for i, perm := range perms {
		if perm.Effect != "Allow" && perm.Effect != "Deny" {
			return pulumi.StringOutput{}, fmt.Errorf(
				"overridePermissions[%d]: effect must be \"Allow\" or \"Deny\", got %q",
				i, perm.Effect,
			)
		}
		if perm.Action == "" {
			return pulumi.StringOutput{}, fmt.Errorf(
				"overridePermissions[%d]: action is required",
				i,
			)
		}
	}

	// Collect all resource Outputs so we can resolve them with pulumi.All.
	resourceOutputs := make([]interface{}, len(perms))
	for i, perm := range perms {
		if perm.Resource != nil {
			resourceOutputs[i] = perm.Resource.ToStringOutput()
		} else {
			resourceOutputs[i] = pulumi.String("*").ToStringOutput()
		}
	}

	policyOutput := pulumi.All(resourceOutputs...).ApplyT(func(resolved []interface{}) (string, error) {
		// Group actions by effect+resource into statements.
		// Key: "Effect|Resource" → actions
		type stmtKey struct {
			Effect   string
			Resource string
		}
		stmtMap := make(map[stmtKey][]string)
		// Preserve insertion order for deterministic output.
		var keyOrder []stmtKey

		for i, perm := range perms {
			resource := resolved[i].(string)
			key := stmtKey{Effect: perm.Effect, Resource: resource}
			if _, exists := stmtMap[key]; !exists {
				keyOrder = append(keyOrder, key)
			}
			stmtMap[key] = append(stmtMap[key], perm.Action)
		}

		statements := make([]endpointPolicyStatement, 0, len(keyOrder))
		for _, key := range keyOrder {
			stmt := endpointPolicyStatement{
				Effect:   key.Effect,
				Action:   stmtMap[key],
				Resource: key.Resource,
			}
			stmt.Principal.Service = computePrincipals
			statements = append(statements, stmt)
		}

		doc := endpointPolicyDocument{
			Version:   "2012-10-17",
			Statement: statements,
		}

		b, err := json.Marshal(doc)
		if err != nil {
			return "", fmt.Errorf("failed to marshal override endpoint policy: %w", err)
		}
		return string(b), nil
	}).(pulumi.StringOutput)

	return policyOutput, nil
}

// NewVpcEndpoint creates a new Anvil-managed Interface VPC Endpoint component.
//
// Access model (three layers):
//
//  1. Network:       Endpoint SG self-referencing ingress on port 443.
//     Only compute resources with the endpoint SG attached can
//     reach the endpoint ENIs — being in the VPC is not sufficient.
//  2. IAM:           Compute resource execution roles must have explicit grants
//     for the service (e.g. sqs:SendMessage on specific queues).
//  3. Endpoint policy: Blanket ceiling on what actions compute principals can
//     perform through this endpoint. Defaults to allow all (*).
//     Use overridePermissions for explicit Allow/Deny control.
func NewVpcEndpoint(ctx *pulumi.Context, name string, args VpcEndpointArgs, opts ...pulumi.ResourceOption) (*VpcEndpoint, error) {
	v := &VpcEndpoint{name: name}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, v, opts...); err != nil {
		return nil, err
	}

	// 1. Dedicated security group for this endpoint.
	//    Zero inbound and zero outbound rules by default — nothing can reach
	//    the endpoint until the self-referencing ingress rule is added below.
	sg, err := vpcsg.CreateSecurityGroup(ctx, name, stage, stageId, args.VpcId, v)
	if err != nil {
		return nil, err
	}

	// 2. Self-referencing ingress rule on port 443.
	//
	// The endpoint SG references itself as the ingress source. A compute resource
	// can only reach the endpoint if it has this SG attached — being in the same
	// VPC is not sufficient. This is the network-layer opt-in gate.
	//
	// GrantEndpointAccess wires the other side: an egress rule on the compute
	// resource's SG pointing at this endpoint SG, plus attaches the endpoint SG
	// to the compute resource's ENI. Both sides must be present for traffic to flow:
	//   - Endpoint SG: ingress ← endpoint SG (this rule, wired once, never grows)
	//   - Compute SG:  egress  → endpoint SG (wired per GrantEndpointAccess call)
	//   - Compute ENI: endpoint SG attached  (wired per GrantEndpointAccess call)
	if err := vpcsg.AddIngressRule(
		ctx,
		fmt.Sprintf("%s-self-ingress", name),
		sg.ID().ToStringOutput(),
		sg.ID().ToStringOutput(),
		443, 443,
		"tcp",
		v,
	); err != nil {
		return nil, fmt.Errorf("failed to wire self-referencing ingress on endpoint SG: %w", err)
	}

	// 3. Build the endpoint policy.
	//
	// Default: allow all actions (*) for all Anvil compute principals.
	// Override: explicit Allow/Deny statements from overridePermissions.
	//
	// The policy is scoped to Lambda, ECS, and EC2 service principals only.
	// AWS managed services (SNS, EventBridge, S3) call services through the
	// AWS internal network — they bypass the VPC endpoint entirely and are
	// not listed as principals here.
	var endpointPolicy pulumi.StringInput
	if len(args.OverridePermissions) == 0 {
		policy, err := buildDefaultEndpointPolicy()
		if err != nil {
			return nil, err
		}
		endpointPolicy = pulumi.String(policy)
	} else {
		policyOutput, err := buildOverrideEndpointPolicy(args.OverridePermissions)
		if err != nil {
			return nil, err
		}
		endpointPolicy = policyOutput
	}

	// 4. Resolve the AWS region synchronously — safe to call outside Apply.
	region, err := aws.GetRegion(ctx, &aws.GetRegionArgs{}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve AWS region: %w", err)
	}
	awsRegion := region.Region

	endpointName := provider.PhysicalName(stage, name, "vpce", stageId)

	// 5. Lift inputs to Outputs so they can be passed directly to ec2.NewVpcEndpoint.
	vpcIdOutput := args.VpcId.ToStringOutput()
	serviceOutput := args.Service.ToStringOutput()
	subnetIdsOutput := args.PrivateSubnetIds.ToStringArrayOutput()

	// Build the full service name as an Output by applying over the service string.
	serviceNameOutput := serviceOutput.ApplyT(func(svc string) string {
		return fmt.Sprintf("com.amazonaws.%s.%s", awsRegion, svc)
	}).(pulumi.StringOutput)

	// 6. Interface VPC Endpoint.
	//   - VpcEndpointType: "Interface" — creates ENIs in each subnet
	//   - PrivateDnsEnabled: true — overrides public service hostnames inside the VPC
	//     so consumers use standard AWS SDK endpoints (e.g. sqs.ap-southeast-2.amazonaws.com)
	//     and never reference vpce-xxx hostnames directly
	//   - SubnetIds: one per private subnet, AWS places one ENI per AZ automatically
	//   - SecurityGroupIds: dedicated SG with self-referencing ingress on 443
	//   - Policy: endpoint policy scoped to Anvil compute principals
	endpoint, err := ec2.NewVpcEndpoint(ctx, name+"-vpce", &ec2.VpcEndpointArgs{
		VpcId:             vpcIdOutput,
		ServiceName:       serviceNameOutput,
		VpcEndpointType:   pulumi.String("Interface"),
		PrivateDnsEnabled: pulumi.Bool(true),
		SubnetIds:         subnetIdsOutput,
		SecurityGroupIds:  pulumi.StringArray{sg.ID()},
		Policy:            endpointPolicy,
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(endpointName),
			"ManagedBy": pulumi.String("anvil"),
			"Service":   serviceOutput,
		},
	}, pulumi.Parent(v))
	if err != nil {
		return nil, fmt.Errorf("failed to create VPC endpoint: %w", err)
	}

	// 7. Extract the first DNS entry for the dnsName output.
	//    With private DNS enabled, consumers never need this — it's exposed for
	//    debugging and multi-VPC architectures where the private DNS override
	//    does not propagate to peered VPCs.
	dnsName := endpoint.DnsEntries.ApplyT(func(entries []ec2.VpcEndpointDnsEntry) string {
		if len(entries) == 0 {
			return ""
		}
		if entries[0].DnsName == nil {
			return ""
		}
		return *entries[0].DnsName
	}).(pulumi.StringOutput)

	// 8. Wire outputs.
	v.EndpointId = endpoint.ID().ToStringOutput()
	v.DnsName = dnsName
	v.SecurityGroupId = sg.ID().ToStringOutput()

	ctx.RegisterResourceOutputs(v, pulumi.Map{
		"endpointId":      endpoint.ID(),
		"dnsName":         dnsName,
		"securityGroupId": sg.ID(),
	})

	return v, nil
}
