package bucket

// NOTE: This file shows the grant methods that will be added to the Bucket component.
// These methods go in provider/aws/bucket/grants.go — a separate file alongside bucket.go.
// This keeps the grant logic isolated from the core bucket resource creation.

import (
	"fmt"

	aws "github.com/anvil-cloud/anvil/provider/aws"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// GrantRead grants s3:GetObject and s3:ListBucket permissions on this bucket
// to the target compute resource's execution role.
//
// Paths are optional — if omitted, grants access to the entire bucket.
//
// Usage:
//
//	bucket.GrantRead(fn, nil)                           // full bucket
//	bucket.GrantRead(fn, []string{"uploads/*"})         // scoped to prefix
//	bucket.GrantRead(fn, []string{"a/*", "b/*"})        // multiple prefixes
//	bucket.GrantRead(fn, nil, provider.GrantOptions{    // with justification
//	    Justification: "Serves uploaded images",
//	})
func (b *Bucket) GrantRead(ctx *pulumi.Context, target provider.GrantTarget, paths []string, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-read", b.name, target.Name())
	arns := aws.BuildResourceARNs(b.Arn, paths)

	return aws.CreateGrant(ctx, b, name, target, []string{
		"s3:GetObject",
		"s3:ListBucket",
	}, arns, o)
}

// GrantWrite grants s3:PutObject permissions on this bucket
// to the target compute resource's execution role.
func (b *Bucket) GrantWrite(ctx *pulumi.Context, target provider.GrantTarget, paths []string, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-write", b.name, target.Name())
	arns := aws.BuildResourceARNs(b.Arn, paths)

	return aws.CreateGrant(ctx, b, name, target, []string{
		"s3:PutObject",
	}, arns, o)
}

// GrantReadWrite grants s3:GetObject, s3:ListBucket, and s3:PutObject permissions
// on this bucket to the target compute resource's execution role.
func (b *Bucket) GrantReadWrite(ctx *pulumi.Context, target provider.GrantTarget, paths []string, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-readwrite", b.name, target.Name())
	arns := aws.BuildResourceARNs(b.Arn, paths)

	return aws.CreateGrant(ctx, b, name, target, []string{
		"s3:GetObject",
		"s3:ListBucket",
		"s3:PutObject",
	}, arns, o)
}

// GrantDelete grants s3:DeleteObject permissions on this bucket
// to the target compute resource's execution role.
func (b *Bucket) GrantDelete(ctx *pulumi.Context, target provider.GrantTarget, paths []string, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)
	name := fmt.Sprintf("%s-%s-delete", b.name, target.Name())
	arns := aws.BuildResourceARNs(b.Arn, paths)

	return aws.CreateGrant(ctx, b, name, target, []string{
		"s3:DeleteObject",
	}, arns, o)
}

// GrantFullAccess grants s3:GetObject, s3:ListBucket, s3:PutObject, and s3:DeleteObject
// permissions on this bucket to the target compute resource's execution role.
//
// This is an escape hatch — prefer scoped grants (GrantRead, GrantWrite, etc.).
// A warning is surfaced during `anvil preview` if no justification is provided.
func (b *Bucket) GrantFullAccess(ctx *pulumi.Context, target provider.GrantTarget, opts ...provider.GrantOptions) error {
	o := provider.MergeGrantOptions(opts)

	// Warn if no justification on full access
	if o.Justification == "" {
		ctx.Log.Warn(fmt.Sprintf(
			"⚠ %s → full access granted with no justification. "+
				"Consider scoping with GrantRead, GrantWrite, or GrantDelete, "+
				"or add a justification: GrantFullAccess(target, GrantOptions{Justification: \"...\"})",
			b.name,
		), nil)
	} else {
		ctx.Log.Info(fmt.Sprintf(
			"ℹ %s → full access granted. Justification: %q",
			b.name, o.Justification,
		), nil)
	}

	name := fmt.Sprintf("%s-%s-fullaccess", b.name, target.Name())
	arns := aws.BuildResourceARNs(b.Arn, nil) // full bucket, no path scoping

	return aws.CreateGrant(ctx, b, name, target, []string{
		"s3:GetObject",
		"s3:ListBucket",
		"s3:PutObject",
		"s3:DeleteObject",
	}, arns, o)
}

// targetName is no longer needed — use target.Name() directly
