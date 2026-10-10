package vpc

import (
	"fmt"
	"strings"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// VpcLookupArgs defines the inputs for importing an existing VPC into Anvil.
// Anvil does not manage, modify, or own the imported VPC — it is read-only.
// Downstream components behave identically whether the VPC was created or imported.
type VpcLookupArgs struct {
	// VpcId is the ID of the existing VPC to import.
	VpcId string `pulumi:"vpcId"`

	// PrivateSubnetIds are the IDs of the private subnets.
	// If omitted, Anvil will auto-discover them by querying all subnets in the
	// VPC and filtering by route table — private subnets have no IGW route.
	PrivateSubnetIds []string `pulumi:"privateSubnetIds,optional"`

	// PublicSubnetIds are the IDs of the public subnets.
	// If omitted, Anvil will auto-discover them by querying all subnets in the
	// VPC and filtering by route table — public subnets have a route to an IGW.
	PublicSubnetIds []string `pulumi:"publicSubnetIds,optional"`
}

// VpcFromId imports an existing VPC into Anvil without managing or modifying it.
// Returns an identical output shape to NewVpc — vpcId, privateSubnetIds,
// publicSubnetIds, availabilityZones, and defaultSecurityGroupId.
//
// Flow logs, NAT, and bastion are not available on an imported VPC.
// Those resources already exist or don't — Anvil is not managing them.
//
// If subnet IDs are not provided, Anvil discovers them by inspecting route
// tables for each subnet in the VPC. Public subnets have a route to an
// Internet Gateway (igw-*), private subnets do not. If auto-discovery
// produces unexpected results, provide subnet IDs explicitly.
func VpcFromId(ctx *pulumi.Context, name string, args VpcLookupArgs, opts ...pulumi.ResourceOption) (*Vpc, error) {
	v := &Vpc{name: name}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, v, opts...); err != nil {
		return nil, err
	}

	// 1. Look up the VPC metadata.
	vpcLookup, err := ec2.LookupVpc(ctx, &ec2.LookupVpcArgs{
		Id: &args.VpcId,
	}, pulumi.Parent(v))
	if err != nil {
		return nil, fmt.Errorf("failed to look up VPC %q: %w", args.VpcId, err)
	}

	// 2. Resolve subnets — explicit or auto-discovered.
	var privateSubnetIds []string
	var publicSubnetIds []string

	if len(args.PrivateSubnetIds) > 0 && len(args.PublicSubnetIds) > 0 {
		// User provided both — use them directly.
		privateSubnetIds = args.PrivateSubnetIds
		publicSubnetIds = args.PublicSubnetIds
	} else {
		// Auto-discover by querying all subnets in the VPC then classifying
		// each by whether its route table has a route to an Internet Gateway.
		discovered, err := discoverSubnets(ctx, args.VpcId, v)
		if err != nil {
			return nil, err
		}

		if len(args.PrivateSubnetIds) > 0 {
			privateSubnetIds = args.PrivateSubnetIds
		} else {
			privateSubnetIds = discovered.private
		}

		if len(args.PublicSubnetIds) > 0 {
			publicSubnetIds = args.PublicSubnetIds
		} else {
			publicSubnetIds = discovered.public
		}
	}

	if len(privateSubnetIds) == 0 {
		return nil, fmt.Errorf(
			"no private subnets found in VPC %q — provide privateSubnetIds explicitly if subnets exist but have non-standard route table configuration",
			args.VpcId,
		)
	}

	if len(publicSubnetIds) == 0 {
		return nil, fmt.Errorf(
			"no public subnets found in VPC %q — provide publicSubnetIds explicitly if subnets exist but have non-standard route table configuration",
			args.VpcId,
		)
	}

	// 3. Resolve AZ names from the discovered private subnets.
	// We look up each subnet to get its AZ, deduplicate, and preserve order.
	azSet := make(map[string]struct{})
	var resolvedAZs []string

	for _, subnetId := range privateSubnetIds {
		sid := subnetId
		subnet, err := ec2.LookupSubnet(ctx, &ec2.LookupSubnetArgs{
			Id: &sid,
		}, pulumi.Parent(v))
		if err != nil {
			return nil, fmt.Errorf("failed to look up subnet %q: %w", sid, err)
		}
		if _, seen := azSet[subnet.AvailabilityZone]; !seen {
			azSet[subnet.AvailabilityZone] = struct{}{}
			resolvedAZs = append(resolvedAZs, subnet.AvailabilityZone)
		}
	}

	// 4. Look up the default security group for this VPC.
	defaultSGLookup, err := ec2.LookupSecurityGroup(ctx, &ec2.LookupSecurityGroupArgs{
		VpcId: &args.VpcId,
		Filters: []ec2.GetSecurityGroupFilter{
			{
				Name:   "group-name",
				Values: []string{"default"},
			},
		},
	}, pulumi.Parent(v))
	if err != nil {
		return nil, fmt.Errorf("failed to look up default security group for VPC %q: %w", args.VpcId, err)
	}

	// 5. Validate DNS settings — warn if not enabled.
	// DNS hostnames and resolution are required for RDS, ECS service discovery,
	// and PrivateLink. We can't enforce this on an imported VPC but we surface it.
	_ = cfg // cfg reserved for future availability/environment hints
	if !vpcLookup.EnableDnsHostnames {
		ctx.Log.Warn(fmt.Sprintf(
			"VPC %q has DNS hostnames disabled. RDS, ECS service discovery, and PrivateLink require DNS hostnames to be enabled.",
			args.VpcId,
		), &pulumi.LogArgs{Resource: v})
	}
	if !vpcLookup.EnableDnsSupport {
		ctx.Log.Warn(fmt.Sprintf(
			"VPC %q has DNS support disabled. RDS, ECS service discovery, and PrivateLink require DNS support to be enabled.",
			args.VpcId,
		), &pulumi.LogArgs{Resource: v})
	}

	// 6. Wire outputs — identical shape to NewVpc.
	privateInputs := make([]pulumi.StringInput, len(privateSubnetIds))
	for i, id := range privateSubnetIds {
		privateInputs[i] = pulumi.String(id)
	}

	publicInputs := make([]pulumi.StringInput, len(publicSubnetIds))
	for i, id := range publicSubnetIds {
		publicInputs[i] = pulumi.String(id)
	}

	azInputs := make([]pulumi.StringInput, len(resolvedAZs))
	for i, az := range resolvedAZs {
		azInputs[i] = pulumi.String(az)
	}

	v.VpcId = pulumi.String(args.VpcId).ToStringOutput()
	v.PrivateSubnetIds = stringInputsToArrayOutput(privateInputs)
	v.PublicSubnetIds = stringInputsToArrayOutput(publicInputs)
	v.AvailabilityZones = stringInputsToArrayOutput(azInputs)
	v.DefaultSecurityGroupId = pulumi.String(defaultSGLookup.Id).ToStringOutput()

	// BastionInstanceId and BastionSecurityGroupId are left as zero-value
	// StringOutputs — not applicable for imported VPCs.

	ctx.RegisterResourceOutputs(v, pulumi.Map{
		"vpcId":                  pulumi.String(args.VpcId),
		"privateSubnetIds":       stringInputsToArray(privateInputs),
		"publicSubnetIds":        stringInputsToArray(publicInputs),
		"availabilityZones":      pulumi.ToStringArray(resolvedAZs),
		"defaultSecurityGroupId": pulumi.String(defaultSGLookup.Id),
	})

	return v, nil
}

// discoveredSubnets holds the result of auto-discovery.
type discoveredSubnets struct {
	public  []string
	private []string
}

// discoverSubnets queries all subnets in the VPC and classifies each as
// public or private by inspecting its route table.
//
// Classification logic:
//   - Public  = route table has a route with a gateway ID starting with "igw-"
//   - Private = route table has no such route
//
// This works on any VPC regardless of tagging conventions.
func discoverSubnets(ctx *pulumi.Context, vpcId string, parent pulumi.Resource) (*discoveredSubnets, error) {
	// Get all subnets in the VPC.
	subnetsResult, err := ec2.GetSubnets(ctx, &ec2.GetSubnetsArgs{
		Filters: []ec2.GetSubnetsFilter{
			{
				Name:   "vpc-id",
				Values: []string{vpcId},
			},
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return nil, fmt.Errorf("failed to list subnets in VPC %q: %w", vpcId, err)
	}

	if len(subnetsResult.Ids) == 0 {
		return nil, fmt.Errorf("no subnets found in VPC %q", vpcId)
	}

	result := &discoveredSubnets{}

	for _, subnetId := range subnetsResult.Ids {
		sid := subnetId

		// Look up the route table explicitly associated with this subnet.
		// Falls back to the main route table if no explicit association exists —
		// this mirrors how AWS itself resolves routing for a subnet.
		rt, err := ec2.LookupRouteTable(ctx, &ec2.LookupRouteTableArgs{
			SubnetId: &sid,
		}, pulumi.Parent(parent))
		if err != nil {
			// If explicit association lookup fails, try the main route table.
			rt, err = ec2.LookupRouteTable(ctx, &ec2.LookupRouteTableArgs{
				VpcId: &vpcId,
				Filters: []ec2.GetRouteTableFilter{
					{
						Name:   "association.main",
						Values: []string{"true"},
					},
				},
			}, pulumi.Parent(parent))
			if err != nil {
				return nil, fmt.Errorf("failed to look up route table for subnet %q: %w", sid, err)
			}
		}

		if hasIGWRoute(rt) {
			result.public = append(result.public, sid)
		} else {
			result.private = append(result.private, sid)
		}
	}

	return result, nil
}

// hasIGWRoute returns true if any route in the route table points to an
// Internet Gateway (gateway ID starts with "igw-").
func hasIGWRoute(rt *ec2.LookupRouteTableResult) bool {
	for _, route := range rt.Routes {
		if strings.HasPrefix(route.GatewayId, "igw-") {
			return route.CidrBlock == "0.0.0.0/0"
		}
	}
	return false
}
