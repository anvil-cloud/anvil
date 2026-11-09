package vpcsg

import (
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	awsvpc "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/vpc"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// VpcPlacementArgs defines the VPC placement for a compute resource.
// Used by Lambda, ECS, and EC2 — all compute resources that can be
// placed inside a VPC for access to private resources.
type VpcPlacementArgs struct {
	// VpcId is the ID of the VPC to place the compute resource in.
	VpcId string `pulumi:"vpcId"`

	// PrivateSubnetIds are the IDs of the private subnets to attach
	// the compute resource to. Always private — compute resources
	// must never be placed in public subnets.
	PrivateSubnetIds []string `pulumi:"privateSubnetIds"`
}

// CreateSecurityGroup creates a dedicated security group for a compute resource
// with zero inbound and zero outbound rules.
//
// AWS creates a default allow-all egress rule on every new security group.
// Pulumi removes this automatically when no inline egress rules are specified,
// which is the behaviour we rely on here. Nothing is reachable or can reach
// anything until explicitly granted. SG-to-SG rules for VPC endpoints are
// created by the SDK grants runtime (one ingress + one egress rule per grant).
//
// Named: ${stage}-${name}-sg-${stageId}
func CreateSecurityGroup(
	ctx *pulumi.Context,
	name, stage, stageId string,
	vpcId pulumi.StringInput,
	parent pulumi.Resource,
) (*ec2.SecurityGroup, error) {
	sgName := fmt.Sprintf("%s-%s-sg-%s", stage, name, stageId)

	sg, err := ec2.NewSecurityGroup(ctx, name+"-sg", &ec2.SecurityGroupArgs{
		VpcId:       vpcId,
		Description: pulumi.String(fmt.Sprintf("Anvil managed security group for %s - zero inbound, zero outbound by default", name)),
		// No ingress or egress rules — nothing is reachable until explicitly granted.
		// Pulumi removes the AWS default allow-all egress rule when no inline
		// egress rules are specified.
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(sgName),
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return nil, fmt.Errorf("failed to create security group for %s: %w", name, err)
	}

	return sg, nil
}

// AddInternetEgressRule creates a SecurityGroupEgressRule allowing outbound
// internet traffic from the compute resource's security group to 0.0.0.0/0.
//
// Used when hasNat is true. SG-to-SG rules for VPC endpoints are created by
// the SDK grants runtime, not here.
//
// fromPort and toPort are inclusive. Pass 0 and 65535 for all ports.
func AddInternetEgressRule(
	ctx *pulumi.Context,
	name string,
	securityGroupId pulumi.StringInput,
	fromPort int,
	toPort int,
	parent pulumi.Resource,
) error {
	_, err := awsvpc.NewSecurityGroupEgressRule(ctx, name+"-internet-egress", &awsvpc.SecurityGroupEgressRuleArgs{
		SecurityGroupId: securityGroupId,
		CidrIpv4:        pulumi.String("0.0.0.0/0"),
		FromPort:        pulumi.Int(fromPort),
		ToPort:          pulumi.Int(toPort),
		IpProtocol:      pulumi.String("tcp"),
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create internet egress rule %s: %w", name, err)
	}
	return nil
}

// AddCIDREgressRule creates a SecurityGroupEgressRule allowing outbound traffic
// from the compute resource's security group to a specific CIDR range and port.
//
// Used by vpc.cidrs — one rule per { range, port } pair.
func AddCIDREgressRule(
	ctx *pulumi.Context,
	name string,
	securityGroupId pulumi.StringInput,
	cidr string,
	fromPort int,
	toPort int,
	parent pulumi.Resource,
) error {
	_, err := awsvpc.NewSecurityGroupEgressRule(ctx, name+"-cidr-egress", &awsvpc.SecurityGroupEgressRuleArgs{
		SecurityGroupId: securityGroupId,
		CidrIpv4:        pulumi.String(cidr),
		FromPort:        pulumi.Int(fromPort),
		ToPort:          pulumi.Int(toPort),
		IpProtocol:      pulumi.String("tcp"),
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create CIDR egress rule %s: %w", name, err)
	}
	return nil
}
