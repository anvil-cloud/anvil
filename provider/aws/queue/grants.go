package queue

import (
	"fmt"

	aws "github.com/anvil-cloud/anvil/provider/aws"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// GrantSendMessage grants sqs:SendMessage on this queue to the target's execution role.
//
// Use this for producers — Lambda functions or services that push messages onto the queue.
//
// Usage:
//
//	queue.GrantSendMessage(ctx, fn)
func (q *Queue) GrantSendMessage(ctx *pulumi.Context, target provider.GrantTarget, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-send", q.name, target.Name())
	return aws.CreateGrant(ctx, q, name, target, []string{
		"sqs:SendMessage",
	}, []pulumi.StringInput{q.Arn}, o)
}

// GrantConsumeMessages grants the permissions needed to consume messages from this queue:
// sqs:ReceiveMessage, sqs:DeleteMessage, sqs:GetQueueAttributes.
//
// Use this for consumers — Lambda functions triggered by or polling the queue.
//
// Usage:
//
//	queue.GrantConsumeMessages(ctx, fn)
func (q *Queue) GrantConsumeMessages(ctx *pulumi.Context, target provider.GrantTarget, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-consume", q.name, target.Name())
	return aws.CreateGrant(ctx, q, name, target, []string{
		"sqs:ReceiveMessage",
		"sqs:DeleteMessage",
		"sqs:GetQueueAttributes",
	}, []pulumi.StringInput{q.Arn}, o)
}

// GrantFullAccess grants all queue operations to the target's execution role.
//
// This is an escape hatch — prefer GrantSendMessage or GrantConsumeMessages.
// A warning is surfaced during `anvil preview` if no justification is provided.
//
// Usage:
//
//	queue.GrantFullAccess(ctx, fn)
//	queue.GrantFullAccess(ctx, fn, provider.GrantOptions{Justification: "order processor"})
func (q *Queue) GrantFullAccess(ctx *pulumi.Context, target provider.GrantTarget, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)

	if o.Justification == "" {
		ctx.Log.Warn(fmt.Sprintf(
			"⚠ %s → full access granted with no justification. "+
				"Consider scoping with GrantSendMessage or GrantConsumeMessages, "+
				"or add a justification: GrantFullAccess(ctx, target, GrantOptions{Justification: \"...\"})",
			q.name,
		), nil)
	} else {
		ctx.Log.Info(fmt.Sprintf(
			"ℹ %s → full access granted. Justification: %q",
			q.name, o.Justification,
		), nil)
	}

	name := fmt.Sprintf("%s-%s-fullaccess", q.name, target.Name())
	return aws.CreateGrant(ctx, q, name, target, []string{
		"sqs:SendMessage",
		"sqs:ReceiveMessage",
		"sqs:DeleteMessage",
		"sqs:GetQueueAttributes",
		"sqs:ChangeMessageVisibility",
		"sqs:PurgeQueue",
	}, []pulumi.StringInput{q.Arn}, o)
}
