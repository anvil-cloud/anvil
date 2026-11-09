// sdk/go/anvil/vpc_defaults.go
// Hand-written. Copied in from sdk/overlays/go during gen-sdk.
//
// App-wide defaults for the security groups Anvil creates.

package anvil

import (
	anvilaws "github.com/anvil-cloud/anvil/sdk/go/anvil/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// VpcDefaultsConfig holds app-wide defaults for VPC networking (DefaultsConfig.Vpc).
type VpcDefaultsConfig struct {
	// AllowAllOutbound puts back the rule AWS adds to a new security group
	// (all traffic to 0.0.0.0/0) on every security group Anvil creates:
	// Lambda, VPC endpoint and DSQL endpoint. A resource's own
	// AllowAllOutbound wins over this. `anvil preview` warns for every group
	// it applies to.
	AllowAllOutbound bool
}

// vpcDefaultsTransformation fills in AllowAllOutbound on every Anvil component
// that owns a security group, unless it sets the value itself.
func vpcDefaultsTransformation(args *pulumi.ResourceTransformationArgs) *pulumi.ResourceTransformationResult {
	on := pulumi.Bool(true)
	switch p := args.Props.(type) {
	case *anvilaws.VpcEndpointArgs:
		if p.AllowAllOutbound != nil {
			return nil
		}
		c := *p
		c.AllowAllOutbound = on
		return &pulumi.ResourceTransformationResult{Props: &c, Opts: args.Opts}
	case *anvilaws.LambdaArgs:
		var vpc *anvilaws.LambdaVpcArgsArgs
		switch v := p.Vpc.(type) {
		case anvilaws.LambdaVpcArgsArgs:
			vpc = &v
		case *anvilaws.LambdaVpcArgsArgs:
			if v != nil {
				c := *v
				vpc = &c
			}
		}
		if vpc == nil || vpc.AllowAllOutbound != nil {
			return nil
		}
		vpc.AllowAllOutbound = on
		c := *p
		c.Vpc = *vpc
		return &pulumi.ResourceTransformationResult{Props: &c, Opts: args.Opts}
	case *anvilaws.DSQLArgs:
		var vpc *anvilaws.DSQLVpcArgsArgs
		switch v := p.Vpc.(type) {
		case anvilaws.DSQLVpcArgsArgs:
			vpc = &v
		case *anvilaws.DSQLVpcArgsArgs:
			if v != nil {
				c := *v
				vpc = &c
			}
		}
		if vpc == nil || vpc.AllowAllOutbound != nil {
			return nil
		}
		vpc.AllowAllOutbound = on
		c := *p
		c.Vpc = *vpc
		return &pulumi.ResourceTransformationResult{Props: &c, Opts: args.Opts}
	}
	return nil
}

// registerVpcDefaults applies DefaultsConfig.Vpc to the rest of the program.
func registerVpcDefaults(ctx *pulumi.Context, d *DefaultsConfig) error {
	if d == nil || d.Vpc == nil || !d.Vpc.AllowAllOutbound {
		return nil
	}
	return ctx.RegisterStackTransformation(vpcDefaultsTransformation)
}
