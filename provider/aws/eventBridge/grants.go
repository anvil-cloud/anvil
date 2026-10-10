package eventBridge

import (
	"fmt"

	aws "github.com/anvil-cloud/anvil/provider/aws"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// GrantPutEvents grants events:PutEvents on this bus to the target's execution role.
//
// Use this for producers — Lambda functions or services that publish events onto the bus.
//
// Usage:
//
//	bus.GrantPutEvents(ctx, fn)
func (e *EventBus) GrantPutEvents(ctx *pulumi.Context, target provider.GrantTarget, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-putevents", e.name, target.Name())
	return aws.CreateGrant(ctx, e, name, target, []string{
		"events:PutEvents",
	}, []pulumi.StringInput{e.Arn}, o)
}
