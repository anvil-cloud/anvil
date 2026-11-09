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

	// Service is the AWS service to route privately, e.g. "sqs" or
	// "secretsmanager". The full com.amazonaws.{region}.{service} name is
	// constructed at deploy time.
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

	// AllowAllOutbound adds the rule AWS puts on a new security group (all
	// traffic to 0.0.0.0/0) to the endpoint's SG. An endpoint only replies, so
	// it never needs this; it exists so App.defaults.vpc.allowAllOutbound can
	// apply to every Anvil security group.
	AllowAllOutbound bool `pulumi:"allowAllOutbound,optional"`
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
	// endpoint. It starts with zero rules. Each grant that passes this endpoint
	// adds one ingress rule from the granted compute's own SG on 443, so only
	// granted compute can reach the endpoint at the network layer.
	SecurityGroupId pulumi.StringOutput `pulumi:"securityGroupId"`

	// Service is the AWS service this endpoint routes, e.g. "sqs". Grants read
	// it to check that the endpoint matches the service being granted.
	Service pulumi.StringOutput `pulumi:"service"`
}

func (v *VpcEndpoint) Annotate(a infer.Annotator) {
	a.SetToken("aws", "VpcEndpoint")
	a.Describe(&v, "An Anvil-managed AWS Interface VPC Endpoint. Creates one ENI per private subnet with private DNS enabled. The endpoint security group starts with zero rules; passing this endpoint to a grant (e.g. queue.grantSendMessage(fn, { endpoint })) adds a one-way ingress rule from that compute's own security group on port 443. Access is enforced at three layers: network (one-way SG rules per grant), IAM role policy (scoped per grant, limited to this endpoint via aws:SourceVpce), and endpoint policy (ceiling on allowed actions, limited to principals in this account).")
}

// endpointPolicyStatement is a single statement in the endpoint policy JSON.
//
// Principal is always "*" and the Condition limits callers to principals in
// this account. Calls from compute code are signed by the execution role
// session, not by a service principal such as lambda.amazonaws.com, so a
// service-principal policy would match nothing and deny every call.
type endpointPolicyStatement struct {
	Effect    string                       `json:"Effect"`
	Principal string                       `json:"Principal"`
	Action    []string                     `json:"Action"`
	Resource  string                       `json:"Resource"`
	Condition map[string]map[string]string `json:"Condition"`
}

// accountCondition limits a statement to principals in the given account.
func accountCondition(accountID string) map[string]map[string]string {
	return map[string]map[string]string{
		"StringEquals": {"aws:PrincipalAccount": accountID},
	}
}

// endpointPolicyDocument is the full endpoint policy JSON structure.
type endpointPolicyDocument struct {
	Version   string                    `json:"Version"`
	Statement []endpointPolicyStatement `json:"Statement"`
}

// buildDefaultEndpointPolicy constructs the default allow-all endpoint policy
// limited to principals in this account. Called when no overridePermissions are set.
func buildDefaultEndpointPolicy(accountID string) (string, error) {
	stmt := endpointPolicyStatement{
		Effect:    "Allow",
		Principal: "*",
		Action:    []string{"*"},
		Resource:  "*",
		Condition: accountCondition(accountID),
	}

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
// Each statement is limited to principals in this account.
// Resource defaults to "*" when the permission entry omits it.
func buildOverrideEndpointPolicy(perms []VpcEndpointPermission, accountID string) (pulumi.StringOutput, error) {
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
				Effect:    key.Effect,
				Principal: "*",
				Action:    stmtMap[key],
				Resource:  key.Resource,
				Condition: accountCondition(accountID),
			}
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
//  1. Network:       The endpoint SG starts with zero rules. Each grant that
//     passes this endpoint adds ingress 443 from the granted
//     compute's own SG, plus egress 443 on that compute's SG to
//     this SG. Compute never carries this SG, so the rules only
//     match the endpoint's ENIs and members never trust each other.
//  2. IAM:           The grant's IAM statement is limited to this endpoint
//     with aws:SourceVpce, so stolen credentials fail outside it.
//  3. Endpoint policy: Ceiling on what actions principals in this account can
//     perform through this endpoint. Defaults to allow all (*).
//     Use overridePermissions for explicit Allow/Deny control.
func NewVpcEndpoint(ctx *pulumi.Context, name string, args VpcEndpointArgs, opts ...pulumi.ResourceOption) (*VpcEndpoint, error) {
	v := &VpcEndpoint{}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, v, opts...); err != nil {
		return nil, err
	}

	// 1. Dedicated security group for this endpoint.
	//    Zero inbound and zero outbound rules. Ingress is added per grant,
	//    one rule per granted compute SG (see the grants runtime in the SDKs).
	sg, err := vpcsg.CreateSecurityGroup(ctx, name, stage, stageId, args.VpcId, v)
	if err != nil {
		return nil, err
	}
	if args.AllowAllOutbound {
		if err := vpcsg.AllowAllOutbound(ctx, name, sg.ID().ToStringOutput(), v); err != nil {
			return nil, err
		}
	}

	// 2. Resolve the account ID for the endpoint policy condition.
	caller, err := aws.GetCallerIdentity(ctx, &aws.GetCallerIdentityArgs{}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve AWS account ID: %w", err)
	}

	// 3. Build the endpoint policy.
	//
	// Default: allow all actions (*) for principals in this account.
	// Override: explicit Allow/Deny statements from overridePermissions.
	//
	// AWS managed services (SNS, EventBridge, S3) call services through the
	// AWS internal network and bypass the VPC endpoint entirely, so the
	// policy never needs to admit them.
	var endpointPolicy pulumi.StringInput
	if len(args.OverridePermissions) == 0 {
		policy, err := buildDefaultEndpointPolicy(caller.AccountId)
		if err != nil {
			return nil, err
		}
		endpointPolicy = pulumi.String(policy)
	} else {
		policyOutput, err := buildOverrideEndpointPolicy(args.OverridePermissions, caller.AccountId)
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
	//   - SecurityGroupIds: dedicated SG, ingress added per grant
	//   - Policy: endpoint policy limited to principals in this account
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
	v.Service = serviceOutput

	ctx.RegisterResourceOutputs(v, pulumi.Map{
		"endpointId":      endpoint.ID(),
		"dnsName":         dnsName,
		"securityGroupId": sg.ID(),
		"service":         serviceOutput,
	})

	return v, nil
}
