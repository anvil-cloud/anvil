package lambda

import (
	"fmt"

	aws "github.com/anvil-cloud/anvil/provider/aws"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// GrantInvoke grants lambda:InvokeFunction permission on this Lambda
// to the target compute resource's execution role.
func (l *Lambda) GrantInvoke(ctx *pulumi.Context, target provider.GrantTarget, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-invoke", l.name, target.Name())
	arns := aws.BuildResourceARNs(l.Arn, nil)

	return aws.CreateGrant(ctx, l, name, target, []string{
		"lambda:InvokeFunction",
	}, arns, o)
}
