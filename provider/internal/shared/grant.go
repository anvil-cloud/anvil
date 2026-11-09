package provider

import "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

// GrantTarget is implemented by any compute resource that can receive
// IAM permissions from infra resources via grant methods (e.g. grantRead, grantWrite).
//
// Any Anvil compute component (Lambda, SvelteKitSite, future EC2/ECS/etc.)
// must implement this interface to be a valid target for resource grants.
type GrantTarget interface {
	// Name returns the logical resource name (the string passed to the constructor).
	// Used to build deterministic IAM policy resource names (e.g. "uploads-api-read").
	Name() string

	// RoleARN returns the ARN of the IAM execution role attached to this
	// compute resource. Infra resources use this to create scoped IAM
	// policies granting specific permissions.
	RoleARN() pulumi.StringOutput
}

// GrantOptions provides optional metadata for grant methods.
// All fields are optional — pass an empty struct or omit entirely.
type GrantOptions struct {
	// Justification documents why this grant is needed.
	// Stored as a tag on the generated IAM policy resource for audit purposes.
	// Surfaced as a warning during `anvil preview` if missing on grantFullAccess calls.
	Justification string
}

// MergeGrantOptions extracts a single GrantOptions from a variadic slice.
// Returns an empty GrantOptions if none provided.
func MergeGrantOptions(opts []GrantOptions) GrantOptions {
	if len(opts) > 0 {
		return opts[0]
	}
	return GrantOptions{}
}
