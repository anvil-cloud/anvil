package vpc

import (
	"fmt"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// VpcArgs defines the inputs for an Anvil-managed VPC.
//
// The following are always enforced at no cost:
//   - DNS hostnames and DNS resolution enabled (required for RDS, ECS service
//     discovery, and PrivateLink)
//   - Public and private subnets across the specified AZ count
//   - Internet Gateway attached to public subnet route tables
//   - Deterministic CIDR carving so Pulumi plans never show unexpected churn
//
// Gateway endpoints (S3, DynamoDB) are explicitly opt-in via the VpcEndpoint
// component, consistent with Anvil's explicit grant philosophy.
type VpcArgs struct {
	// Cidr is the IPv4 CIDR block for the VPC.
	// Default: "10.0.0.0/16"
	Cidr string `pulumi:"cidr,optional"`

	// AvailabilityZones controls how many AZs to deploy subnets into.
	// Valid values: 1, 2, 3. Default: 1.
	// "high" availability in App.defaults maps to 3, "low" maps to 1.
	AvailabilityZones int `pulumi:"availabilityZones,optional"`

	// Nat configures outbound internet access for private subnets.
	// Omit for a fully private VPC with no outbound internet access.
	Nat *NatArgs `pulumi:"nat,optional"`

	// Bastion enables an SSM bastion host in the first public subnet.
	// Use this to connect to private resources (RDS, ElastiCache) locally.
	// No SSH, no port 22, no key pairs — access via AWS SSM Session Manager only.
	Bastion *BastionArgs `pulumi:"bastion,optional"`

	// FlowLogs enables VPC Flow Log capture. Opt-in only.
	// CloudWatch for active debugging, S3 for long-term compliance retention.
	// Either or both destinations can be enabled simultaneously.
	FlowLogs *FlowLogsArgs `pulumi:"flowLogs,optional"`
}

// Vpc is the Anvil-managed VPC component resource.
type Vpc struct {
	pulumi.ResourceState

	VpcId                  pulumi.StringOutput      `pulumi:"vpcId"`
	PrivateSubnetIds       pulumi.StringArrayOutput `pulumi:"privateSubnetIds"`
	PublicSubnetIds        pulumi.StringArrayOutput `pulumi:"publicSubnetIds"`
	AvailabilityZones      pulumi.StringArrayOutput `pulumi:"availabilityZones"`
	DefaultSecurityGroupId pulumi.StringOutput      `pulumi:"defaultSecurityGroupId"`

	// BastionInstanceId is the EC2 instance ID of the bastion host.
	// Use this with: aws ssm start-session --target <BastionInstanceId>
	// Only populated when bastion is enabled.
	BastionInstanceId pulumi.StringOutput `pulumi:"bastionInstanceId"`

	// BastionSecurityGroupId is the security group ID of the bastion host.
	// Forward-looking: use this to grant the bastion access to private resources.
	// Example: db.grant(network.bastion, { access: "readWrite" })
	// Only populated when bastion is enabled.
	BastionSecurityGroupId pulumi.StringOutput `pulumi:"bastionSecurityGroupId"`

	name string
}

func (v *Vpc) Annotate(a infer.Annotator) {
	a.SetToken("aws", "Vpc")
	a.Describe(&v, "An Anvil-managed VPC. Provides public and private subnets across one to three Availability Zones with an Internet Gateway and correct route tables. Optional NAT Gateway or fck-nat EC2 instance for outbound internet access. Optional SSM bastion host for private network access. DNS hostnames and resolution are enabled by default, required for RDS, ECS service discovery, and PrivateLink.")
}

func resolveAZCount(args VpcArgs, cfg *c.Config) int {
	if args.AvailabilityZones > 0 {
		if args.AvailabilityZones > 3 {
			return 3
		}
		return args.AvailabilityZones
	}
	if cfg.Get("availability") == "high" {
		return 3
	}
	return 1
}

func resolveCIDR(args VpcArgs) string {
	if args.Cidr != "" {
		return args.Cidr
	}
	return "10.0.0.0/16"
}

// publicSubnetCIDR returns the CIDR for public subnet at index i.
// Offset 0: 10.0.0.0/24, 10.0.1.0/24, 10.0.2.0/24
func publicSubnetCIDR(vpcCIDR string, i int) string {
	return fmt.Sprintf("%s.%d.0/24", vpcBase(vpcCIDR), i)
}

// privateSubnetCIDR returns the CIDR for private subnet at index i.
// Offset 10: 10.0.10.0/24, 10.0.11.0/24, 10.0.12.0/24
func privateSubnetCIDR(vpcCIDR string, i int) string {
	return fmt.Sprintf("%s.%d.0/24", vpcBase(vpcCIDR), 10+i)
}

// privateSubnetCIDRs returns all private subnet CIDRs for the given AZ count.
func privateSubnetCIDRs(vpcCIDR string, azCount int) []string {
	cidrs := make([]string, azCount)
	for i := 0; i < azCount; i++ {
		cidrs[i] = privateSubnetCIDR(vpcCIDR, i)
	}
	return cidrs
}

// vpcBase extracts the first two octets: "10.0.0.0/16" -> "10.0"
func vpcBase(cidr string) string {
	dots := 0
	for i, ch := range cidr {
		if ch == '.' {
			dots++
			if dots == 2 {
				return cidr[:i]
			}
		}
	}
	return "10.0"
}

func stringInputsToArrayOutput(inputs []pulumi.StringInput) pulumi.StringArrayOutput {
	arr := make(pulumi.StringArray, len(inputs))
	copy(arr, inputs)
	return arr.ToStringArrayOutput()
}

func stringInputsToArray(inputs []pulumi.StringInput) pulumi.StringArray {
	arr := make(pulumi.StringArray, len(inputs))
	copy(arr, inputs)
	return arr
}

// NewVpc creates a new Anvil-managed VPC component.
func NewVpc(ctx *pulumi.Context, name string, args VpcArgs, opts ...pulumi.ResourceOption) (*Vpc, error) {
	v := &Vpc{name: name}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")

	azCount := resolveAZCount(args, cfg)
	vpcCIDR := resolveCIDR(args)

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, v, opts...); err != nil {
		return nil, err
	}

	// 1. Validate nat config early.
	if args.Nat != nil {
		switch args.Nat.NatType {
		case "gateway", "fck-nat":
		default:
			return nil, fmt.Errorf("invalid nat.natType %q: must be \"gateway\" or \"fck-nat\"", args.Nat.NatType)
		}
	}

	// 2. Resolve AZ names via aws.GetAvailabilityZones.
	azResult, err := aws.GetAvailabilityZones(ctx, &aws.GetAvailabilityZonesArgs{
		State: pulumi.StringRef("available"),
	}, pulumi.Parent(v))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve availability zones: %w", err)
	}

	resolvedAZs := make([]string, azCount)
	for i := 0; i < azCount; i++ {
		if i >= len(azResult.Names) {
			return nil, fmt.Errorf("region does not have %d availability zones (found %d)", azCount, len(azResult.Names))
		}
		resolvedAZs[i] = azResult.Names[i]
	}

	// 3. VPC.
	vpcName := provider.PhysicalName(stage, name, "vpc", stageId)

	vpcResource, err := ec2.NewVpc(ctx, name, &ec2.VpcArgs{
		CidrBlock:          pulumi.String(vpcCIDR),
		EnableDnsHostnames: pulumi.Bool(true),
		EnableDnsSupport:   pulumi.Bool(true),
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(vpcName),
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(v))
	if err != nil {
		return nil, fmt.Errorf("failed to create VPC: %w", err)
	}

	// 4. Internet Gateway.
	igwName := provider.PhysicalName(stage, name, "igw", stageId)

	igw, err := ec2.NewInternetGateway(ctx, name+"-igw", &ec2.InternetGatewayArgs{
		VpcId: vpcResource.ID(),
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(igwName),
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(v))
	if err != nil {
		return nil, fmt.Errorf("failed to create Internet Gateway: %w", err)
	}

	// 5. Public subnets.
	publicSubnetIds := make([]pulumi.StringInput, azCount)
	publicSubnets := make([]*ec2.Subnet, azCount)

	pubRTName := provider.PhysicalName(stage, name, "public-rt", stageId)
	publicRT, err := ec2.NewRouteTable(ctx, name+"-public-rt", &ec2.RouteTableArgs{
		VpcId: vpcResource.ID(),
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(pubRTName),
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(v))
	if err != nil {
		return nil, fmt.Errorf("failed to create public route table: %w", err)
	}

	if _, err = ec2.NewRoute(ctx, name+"-public-default-route", &ec2.RouteArgs{
		RouteTableId:         publicRT.ID(),
		DestinationCidrBlock: pulumi.String("0.0.0.0/0"),
		GatewayId:            igw.ID(),
	}, pulumi.Parent(v)); err != nil {
		return nil, fmt.Errorf("failed to create public default route: %w", err)
	}

	for i, az := range resolvedAZs {
		subnetName := provider.PhysicalName(stage, fmt.Sprintf("%s-public-%d", name, i+1), "subnet", stageId)

		subnet, err := ec2.NewSubnet(ctx, fmt.Sprintf("%s-public-subnet-%d", name, i+1), &ec2.SubnetArgs{
			VpcId:               vpcResource.ID(),
			CidrBlock:           pulumi.String(publicSubnetCIDR(vpcCIDR, i)),
			AvailabilityZone:    pulumi.String(az),
			MapPublicIpOnLaunch: pulumi.Bool(true),
			Tags: pulumi.StringMap{
				"Name":      pulumi.String(subnetName),
				"ManagedBy": pulumi.String("anvil"),
				"Tier":      pulumi.String("public"),
			},
		}, pulumi.Parent(v))
		if err != nil {
			return nil, fmt.Errorf("failed to create public subnet %d: %w", i+1, err)
		}

		if _, err = ec2.NewRouteTableAssociation(ctx, fmt.Sprintf("%s-public-rta-%d", name, i+1), &ec2.RouteTableAssociationArgs{
			SubnetId:     subnet.ID(),
			RouteTableId: publicRT.ID(),
		}, pulumi.Parent(v)); err != nil {
			return nil, fmt.Errorf("failed to associate public subnet %d with route table: %w", i+1, err)
		}

		publicSubnetIds[i] = subnet.ID()
		publicSubnets[i] = subnet
	}

	// 6. Private subnets.
	privateSubnetIds := make([]pulumi.StringInput, azCount)
	privateRouteTables := make([]*ec2.RouteTable, azCount)

	for i, az := range resolvedAZs {
		subnetName := provider.PhysicalName(stage, fmt.Sprintf("%s-private-%d", name, i+1), "subnet", stageId)
		rtName := provider.PhysicalName(stage, fmt.Sprintf("%s-private-%d", name, i+1), "rt", stageId)

		rt, err := ec2.NewRouteTable(ctx, fmt.Sprintf("%s-private-rt-%d", name, i+1), &ec2.RouteTableArgs{
			VpcId: vpcResource.ID(),
			Tags: pulumi.StringMap{
				"Name":      pulumi.String(rtName),
				"ManagedBy": pulumi.String("anvil"),
				"Tier":      pulumi.String("private"),
			},
		}, pulumi.Parent(v))
		if err != nil {
			return nil, fmt.Errorf("failed to create private route table %d: %w", i+1, err)
		}
		privateRouteTables[i] = rt

		subnet, err := ec2.NewSubnet(ctx, fmt.Sprintf("%s-private-subnet-%d", name, i+1), &ec2.SubnetArgs{
			VpcId:            vpcResource.ID(),
			CidrBlock:        pulumi.String(privateSubnetCIDR(vpcCIDR, i)),
			AvailabilityZone: pulumi.String(az),
			Tags: pulumi.StringMap{
				"Name":      pulumi.String(subnetName),
				"ManagedBy": pulumi.String("anvil"),
				"Tier":      pulumi.String("private"),
			},
		}, pulumi.Parent(v))
		if err != nil {
			return nil, fmt.Errorf("failed to create private subnet %d: %w", i+1, err)
		}

		if _, err = ec2.NewRouteTableAssociation(ctx, fmt.Sprintf("%s-private-rta-%d", name, i+1), &ec2.RouteTableAssociationArgs{
			SubnetId:     subnet.ID(),
			RouteTableId: rt.ID(),
		}, pulumi.Parent(v)); err != nil {
			return nil, fmt.Errorf("failed to associate private subnet %d with route table: %w", i+1, err)
		}

		privateSubnetIds[i] = subnet.ID()
	}

	// 7. NAT (optional).
	if args.Nat != nil {
		switch args.Nat.NatType {
		case "gateway":
			if err := createNatGateways(ctx, name, stage, stageId, resolvedAZs, publicSubnets, privateRouteTables, v); err != nil {
				return nil, err
			}
		case "fck-nat":
			instanceType := resolveNatInstanceType(args.Nat)
			privateCIDRs := privateSubnetCIDRs(vpcCIDR, azCount)
			if err := createNatInstance(ctx, name, stage, stageId, instanceType, privateCIDRs, publicSubnets[0], privateRouteTables, v); err != nil {
				return nil, err
			}
		}
	}

	// 8. Bastion host (optional).
	if args.Bastion != nil {
		instanceType := resolveBastionInstanceType(args.Bastion)
		bastionInstance, bastionSG, err := createBastion(ctx, name, stage, stageId, instanceType, args.Bastion.AllowedCidrs, vpcResource, publicSubnets[0], v)
		if err != nil {
			return nil, err
		}
		v.BastionInstanceId = bastionInstance.ID().ToStringOutput()
		v.BastionSecurityGroupId = bastionSG.ID().ToStringOutput()
	}

	// 9. Flow logs (optional).
	if args.FlowLogs != nil {
		if err := createFlowLogs(ctx, name, stage, stageId, args.FlowLogs, vpcResource, v); err != nil {
			return nil, err
		}
	}

	// 10. Lock down the VPC default security group.
	defaultSG, err := ec2.NewDefaultSecurityGroup(ctx, name+"-default-sg", &ec2.DefaultSecurityGroupArgs{
		VpcId: vpcResource.ID(),
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(fmt.Sprintf("%s-default-sg-do-not-use", vpcName)),
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(v))
	if err != nil {
		return nil, fmt.Errorf("failed to lock down default security group: %w", err)
	}

	// 11. Wire outputs.
	v.VpcId = vpcResource.ID().ToStringOutput()
	v.DefaultSecurityGroupId = defaultSG.ID().ToStringOutput()
	v.PrivateSubnetIds = stringInputsToArrayOutput(privateSubnetIds)
	v.PublicSubnetIds = stringInputsToArrayOutput(publicSubnetIds)

	azStringInputs := make([]pulumi.StringInput, azCount)
	for i, az := range resolvedAZs {
		azStringInputs[i] = pulumi.String(az)
	}
	v.AvailabilityZones = stringInputsToArrayOutput(azStringInputs)

	ctx.RegisterResourceOutputs(v, pulumi.Map{
		"vpcId":                  vpcResource.ID(),
		"privateSubnetIds":       stringInputsToArray(privateSubnetIds),
		"publicSubnetIds":        stringInputsToArray(publicSubnetIds),
		"availabilityZones":      pulumi.ToStringArray(resolvedAZs),
		"defaultSecurityGroupId": defaultSG.ID(),
		"bastionInstanceId":      v.BastionInstanceId,
		"bastionSecurityGroupId": v.BastionSecurityGroupId,
	})

	return v, nil
}
