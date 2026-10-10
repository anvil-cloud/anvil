package vpc

import (
	"fmt"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// NatArgs configures outbound internet access for private subnets.
type NatArgs struct {
	// NatType is the type of NAT to provision.
	// "gateway" — one AWS managed NAT Gateway per AZ (~$32/month per AZ + $0.045/GB).
	// "fck-nat" — single fck-nat EC2 instance shared across all AZs (~$4-6/month).
	NatType string `pulumi:"natType"`

	// InstanceType is the EC2 instance type for the fck-nat instance.
	// Only applies when NatType is "fck-nat". Default: "t4g.small".
	InstanceType string `pulumi:"instanceType,optional"`
}

func resolveNatInstanceType(nat *NatArgs) string {
	if nat.InstanceType != "" {
		return nat.InstanceType
	}
	return "t4g.small"
}

// createNatGateways provisions one AWS managed NAT Gateway per AZ.
func createNatGateways(
	ctx *pulumi.Context,
	name, stage, stageId string,
	resolvedAZs []string,
	publicSubnets []*ec2.Subnet,
	privateRouteTables []*ec2.RouteTable,
	parent pulumi.Resource,
) error {
	for i := range resolvedAZs {
		eipName := provider.PhysicalName(stage, fmt.Sprintf("%s-nat-%d", name, i+1), "eip", stageId)
		eip, err := ec2.NewEip(ctx, fmt.Sprintf("%s-nat-eip-%d", name, i+1), &ec2.EipArgs{
			Domain: pulumi.String("vpc"),
			Tags: pulumi.StringMap{
				"Name":      pulumi.String(eipName),
				"ManagedBy": pulumi.String("anvil"),
			},
		}, pulumi.Parent(parent))
		if err != nil {
			return fmt.Errorf("failed to create EIP for NAT Gateway %d: %w", i+1, err)
		}

		natName := provider.PhysicalName(stage, fmt.Sprintf("%s-%d", name, i+1), "nat-gw", stageId)
		natGW, err := ec2.NewNatGateway(ctx, fmt.Sprintf("%s-nat-gw-%d", name, i+1), &ec2.NatGatewayArgs{
			SubnetId:     publicSubnets[i].ID(),
			AllocationId: eip.ID(),
			Tags: pulumi.StringMap{
				"Name":      pulumi.String(natName),
				"ManagedBy": pulumi.String("anvil"),
			},
		}, pulumi.Parent(parent))
		if err != nil {
			return fmt.Errorf("failed to create NAT Gateway %d: %w", i+1, err)
		}

		if _, err = ec2.NewRoute(ctx, fmt.Sprintf("%s-private-nat-route-%d", name, i+1), &ec2.RouteArgs{
			RouteTableId:         privateRouteTables[i].ID(),
			DestinationCidrBlock: pulumi.String("0.0.0.0/0"),
			NatGatewayId:         natGW.ID(),
		}, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to create NAT route for AZ %d: %w", i+1, err)
		}
	}
	return nil
}

// createNatInstance provisions a single fck-nat EC2 instance.
func createNatInstance(
	ctx *pulumi.Context,
	name, stage, stageId string,
	instanceType string,
	privateCIDRs []string,
	publicSubnet *ec2.Subnet,
	privateRouteTables []*ec2.RouteTable,
	parent pulumi.Resource,
) error {
	natAMI, err := ec2.LookupAmi(ctx, &ec2.LookupAmiArgs{
		MostRecent: pulumi.BoolRef(true),
		Owners:     []string{"568608671756"},
		Filters: []ec2.GetAmiFilter{
			{
				Name:   "name",
				Values: []string{"fck-nat-al2023-*-arm64-ebs"},
			},
			{
				Name:   "architecture",
				Values: []string{"arm64"},
			},
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to resolve fck-nat AMI: %w", err)
	}

	ingressRules := make(ec2.SecurityGroupIngressArray, len(privateCIDRs))
	for i, cidr := range privateCIDRs {
		ingressRules[i] = &ec2.SecurityGroupIngressArgs{
			Description: pulumi.String(fmt.Sprintf("NAT forwarding from private subnet %d", i+1)),
			FromPort:    pulumi.Int(0),
			ToPort:      pulumi.Int(0),
			Protocol:    pulumi.String("-1"),
			CidrBlocks:  pulumi.StringArray{pulumi.String(cidr)},
		}
	}

	natSGName := provider.PhysicalName(stage, name, "nat-sg", stageId)
	natSG, err := ec2.NewSecurityGroup(ctx, name+"-nat-sg", &ec2.SecurityGroupArgs{
		VpcId:       publicSubnet.VpcId,
		Description: pulumi.String("Security group for Anvil fck-nat instance - inbound from private subnets only"),
		Ingress:     ingressRules,
		Egress: ec2.SecurityGroupEgressArray{
			&ec2.SecurityGroupEgressArgs{
				Description: pulumi.String("Allow all outbound for NAT forwarding"),
				FromPort:    pulumi.Int(0),
				ToPort:      pulumi.Int(0),
				Protocol:    pulumi.String("-1"),
				CidrBlocks:  pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
		},
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(natSGName),
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create NAT instance security group: %w", err)
	}

	natInstanceName := provider.PhysicalName(stage, name, "nat-instance", stageId)
	natInstance, err := ec2.NewInstance(ctx, name+"-nat-instance", &ec2.InstanceArgs{
		Ami:                      pulumi.String(natAMI.Id),
		InstanceType:             pulumi.String(instanceType),
		SubnetId:                 publicSubnet.ID(),
		VpcSecurityGroupIds:      pulumi.StringArray{natSG.ID()},
		SourceDestCheck:          pulumi.Bool(false),
		AssociatePublicIpAddress: pulumi.Bool(true),
		MetadataOptions: &ec2.InstanceMetadataOptionsArgs{
			HttpEndpoint:            pulumi.String("enabled"),
			HttpTokens:              pulumi.String("required"),
			HttpPutResponseHopLimit: pulumi.Int(1),
		},
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(natInstanceName),
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create NAT instance: %w", err)
	}

	for i, rt := range privateRouteTables {
		if _, err = ec2.NewRoute(ctx, fmt.Sprintf("%s-private-nat-route-%d", name, i+1), &ec2.RouteArgs{
			RouteTableId:         rt.ID(),
			DestinationCidrBlock: pulumi.String("0.0.0.0/0"),
			NetworkInterfaceId:   natInstance.PrimaryNetworkInterfaceId,
		}, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to create NAT instance route for private subnet %d: %w", i+1, err)
		}
	}

	return nil
}
