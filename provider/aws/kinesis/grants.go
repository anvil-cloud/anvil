package kinesis

import (
	"fmt"

	aws "github.com/anvil-cloud/anvil/provider/aws"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// readActions is what a consumer needs on the stream. Shared by GrantRead and
// the consumers wiring so they never drift apart.
var readActions = []string{
	"kinesis:GetRecords",
	"kinesis:GetShardIterator",
	"kinesis:DescribeStream",
	"kinesis:DescribeStreamSummary",
	"kinesis:ListShards",
}

// GrantRead grants the permissions needed to consume records from this stream:
// kinesis:GetRecords, kinesis:GetShardIterator, kinesis:DescribeStream,
// kinesis:DescribeStreamSummary, kinesis:ListShards.
//
// This is the set a Lambda event source mapping needs on its execution role.
//
// Usage:
//
//	stream.GrantRead(ctx, fn)
func (k *KinesisStream) GrantRead(ctx *pulumi.Context, target provider.GrantTarget, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-read", k.name, target.Name())
	return aws.CreateGrant(ctx, k, name, target, readActions, []pulumi.StringInput{k.Arn}, o)
}

// GrantWrite grants the permissions needed to put records onto this stream:
// kinesis:PutRecord, kinesis:PutRecords, kinesis:DescribeStreamSummary,
// kinesis:ListShards.
//
// Usage:
//
//	stream.GrantWrite(ctx, fn)
func (k *KinesisStream) GrantWrite(ctx *pulumi.Context, target provider.GrantTarget, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-write", k.name, target.Name())
	return aws.CreateGrant(ctx, k, name, target, []string{
		"kinesis:PutRecord",
		"kinesis:PutRecords",
		"kinesis:DescribeStreamSummary",
		"kinesis:ListShards",
	}, []pulumi.StringInput{k.Arn}, o)
}

// GrantFullAccess grants all data-plane stream operations to the target's
// execution role.
//
// This is an escape hatch — prefer GrantRead or GrantWrite.
// A warning is surfaced during `anvil preview` if no justification is provided.
//
// Usage:
//
//	stream.GrantFullAccess(ctx, fn)
//	stream.GrantFullAccess(ctx, fn, provider.GrantOptions{Justification: "replay tool"})
func (k *KinesisStream) GrantFullAccess(ctx *pulumi.Context, target provider.GrantTarget, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)

	if o.Justification == "" {
		ctx.Log.Warn(fmt.Sprintf(
			"⚠ %s → full access granted with no justification. "+
				"Consider scoping with GrantRead or GrantWrite, "+
				"or add a justification: GrantFullAccess(ctx, target, GrantOptions{Justification: \"...\"})",
			k.name,
		), nil)
	} else {
		ctx.Log.Info(fmt.Sprintf(
			"ℹ %s → full access granted. Justification: %q",
			k.name, o.Justification,
		), nil)
	}

	name := fmt.Sprintf("%s-%s-fullaccess", k.name, target.Name())
	return aws.CreateGrant(ctx, k, name, target, []string{
		"kinesis:PutRecord",
		"kinesis:PutRecords",
		"kinesis:GetRecords",
		"kinesis:GetShardIterator",
		"kinesis:DescribeStream",
		"kinesis:DescribeStreamSummary",
		"kinesis:ListShards",
	}, []pulumi.StringInput{k.Arn}, o)
}
