package function

import (
	"github.com/anvil-cloud/anvil/provider/internal/hatch"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/cloudfunctionsv2"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type FunctionArgs struct {
	Name       string                            `pulumi:"name"`
	Location   string                            `pulumi:"location"`
	Runtime    string                            `pulumi:"runtime"`
	EntryPoint string                            `pulumi:"entryPoint"`
	Hatch      map[string]map[string]interface{} `pulumi:"hatch,optional"`
}

type Function struct {
	pulumi.ResourceState

	FunctionName pulumi.StringOutput `pulumi:"functionName"`
	Url          pulumi.StringOutput `pulumi:"url"`
	State        pulumi.StringOutput `pulumi:"state"`
}

func (f *Function) Annotate(a infer.Annotator) {
	a.Describe(&f, "An Anvil-managed GCP Cloud Function (2nd gen) with sensible defaults.")
}

func NewFunction(ctx *pulumi.Context, name string, args FunctionArgs, opts ...pulumi.ResourceOption) (*Function, error) {
	f := &Function{}
	err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, f, opts...)
	if err != nil {
		return nil, err
	}

	// Cloud Function (2nd gen)
	functionProps := hatch.Merge(args.Hatch["function"], pulumi.Map{
		"name":     pulumi.String(args.Name),
		"location": pulumi.String(args.Location),
		"buildConfig": pulumi.Map{
			"runtime":    pulumi.String(args.Runtime),
			"entryPoint": pulumi.String(args.EntryPoint),
		},
		"labels": pulumi.StringMap{
			"managed-by": pulumi.String("anvil"),
		},
	})

	res := &cloudfunctionsv2.Function{}
	err = ctx.RegisterResource("gcp:cloudfunctionsv2/function:Function", name, functionProps, res, pulumi.Parent(f))
	if err != nil {
		return nil, err
	}

	f.FunctionName = res.Name
	f.Url = res.Url
	f.State = res.State

	ctx.RegisterResourceOutputs(f, pulumi.Map{
		"functionName": res.Name,
		"url":          res.Url,
		"state":        res.State,
	})

	return f, nil
}
