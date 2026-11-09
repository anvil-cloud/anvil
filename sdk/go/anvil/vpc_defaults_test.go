package anvil

import (
	"testing"

	anvilaws "github.com/anvil-cloud/anvil/sdk/go/anvil/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// The default fills in allowAllOutbound on Lambda (vpc), VpcEndpoint and DSQL
// (vpc), leaves explicit values alone, and skips a Lambda with no vpc.
func TestVpcDefaultsTransformation(t *testing.T) {
	mocks := &grantMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		if err := registerVpcDefaults(ctx, &DefaultsConfig{Vpc: &VpcDefaultsConfig{AllowAllOutbound: true}}); err != nil {
			return err
		}
		net := anvilaws.LambdaVpcArgsArgs{VpcId: pulumi.String("vpc-1"), PrivateSubnetIds: pulumi.StringArray{pulumi.String("subnet-1")}}
		rt := anvilaws.LambdaRuntime("nodejs24.x")
		if _, err := anvilaws.NewLambda(ctx, "defaulted", &anvilaws.LambdaArgs{Runtime: rt, Vpc: net}); err != nil {
			return err
		}
		explicit := net
		explicit.AllowAllOutbound = pulumi.Bool(false)
		if _, err := anvilaws.NewLambda(ctx, "explicit", &anvilaws.LambdaArgs{Runtime: rt, Vpc: explicit}); err != nil {
			return err
		}
		if _, err := anvilaws.NewLambda(ctx, "novpc", &anvilaws.LambdaArgs{Runtime: rt}); err != nil {
			return err
		}
		if _, err := anvilaws.NewVpcEndpoint(ctx, "ep", &anvilaws.VpcEndpointArgs{
			VpcId: pulumi.String("vpc-1"), PrivateSubnetIds: pulumi.StringArray{pulumi.String("subnet-1")}, Service: pulumi.String("sqs"),
		}); err != nil {
			return err
		}
		_, err := anvilaws.NewDSQL(ctx, "db", &anvilaws.DSQLArgs{
			Vpc: anvilaws.DSQLVpcArgsArgs{VpcId: pulumi.String("vpc-1"), PrivateSubnetIds: pulumi.StringArray{pulumi.String("subnet-1")}},
		})
		return err
	}, pulumi.WithMocks("project", "stack", mocks))
	if err != nil {
		t.Fatal(err)
	}

	inputs := map[string]resource.PropertyMap{}
	for _, r := range mocks.resources {
		inputs[r.Name] = r.Inputs
	}
	vpcFlag := func(name string) resource.PropertyValue {
		vpc, ok := inputs[name]["vpc"]
		if !ok || !vpc.IsObject() {
			return resource.NewNullProperty()
		}
		return vpc.ObjectValue()["allowAllOutbound"]
	}
	if v := vpcFlag("defaulted"); !v.IsBool() || !v.BoolValue() {
		t.Errorf("defaulted Lambda: allowAllOutbound = %v, want true", v)
	}
	if v := vpcFlag("explicit"); !v.IsBool() || v.BoolValue() {
		t.Errorf("explicit Lambda: allowAllOutbound = %v, want false (explicit wins)", v)
	}
	if _, ok := inputs["novpc"]["vpc"]; ok {
		t.Errorf("Lambda without vpc got a vpc block")
	}
	if v := inputs["ep"]["allowAllOutbound"]; !v.IsBool() || !v.BoolValue() {
		t.Errorf("VpcEndpoint: allowAllOutbound = %v, want true", v)
	}
	if v := vpcFlag("db"); !v.IsBool() || !v.BoolValue() {
		t.Errorf("DSQL: allowAllOutbound = %v, want true", v)
	}
}

// No default, no transformation.
func TestVpcDefaultsOffRegistersNothing(t *testing.T) {
	mocks := &grantMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		if err := registerVpcDefaults(ctx, &DefaultsConfig{}); err != nil {
			return err
		}
		_, err := anvilaws.NewVpcEndpoint(ctx, "ep2", &anvilaws.VpcEndpointArgs{
			VpcId: pulumi.String("vpc-1"), PrivateSubnetIds: pulumi.StringArray{pulumi.String("subnet-1")}, Service: pulumi.String("sqs"),
		})
		return err
	}, pulumi.WithMocks("project", "stack", mocks))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mocks.resources[0].Inputs["allowAllOutbound"]; ok {
		t.Error("allowAllOutbound set without a default")
	}
}
