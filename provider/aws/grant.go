package aws

import (
	"encoding/json"
	"fmt"
	"strings"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// iamPolicyDocument is the JSON structure for an IAM policy.
type iamPolicyDocument struct {
	Version   string               `json:"Version"`
	Statement []iamPolicyStatement `json:"Statement"`
}

// iamPolicyStatement is a single statement in an IAM policy.
type iamPolicyStatement struct {
	Effect   string   `json:"Effect"`
	Action   []string `json:"Action"`
	Resource []string `json:"Resource"`
}

// CreateGrant creates a scoped IAM RolePolicy that grants the specified actions
// on the specified resource ARNs to the target's execution role.
//
// This is the core engine that all resource-specific grant methods delegate to.
// For example, Bucket.GrantRead calls this with s3:GetObject + s3:ListBucket actions.
//
// Parameters:
//   - ctx: Pulumi context
//   - parent: the infra resource creating the grant (e.g. the Bucket component) — the
//     IAM policy is created as a child of this resource
//   - name: deterministic resource name for the IAM policy (e.g. "uploads-api-read")
//   - target: the compute resource receiving permissions (implements GrantTarget)
//   - actions: IAM actions to grant (e.g. ["s3:GetObject", "s3:ListBucket"])
//   - resourceARNs: ARNs to scope the policy to (e.g. [bucketARN, bucketARN/*])
//   - opts: optional GrantOptions (justification metadata)
func CreateGrant(
	ctx *pulumi.Context,
	parent pulumi.Resource,
	name string,
	target provider.GrantTarget,
	actions []string,
	resourceARNs []pulumi.StringInput,
	opts provider.GrantOptions,
) error {
	// Build the policy document from the resolved ARNs
	policyJSON := pulumi.All(toOutputs(resourceARNs)...).ApplyT(func(args []interface{}) (string, error) {
		resources := make([]string, len(args))
		for i, a := range args {
			resources[i] = a.(string)
		}

		doc := iamPolicyDocument{
			Version: "2012-10-17",
			Statement: []iamPolicyStatement{
				{
					Effect:   "Allow",
					Action:   actions,
					Resource: resources,
				},
			},
		}

		b, err := json.Marshal(doc)
		if err != nil {
			return "", fmt.Errorf("failed to marshal IAM policy: %w", err)
		}
		return string(b), nil
	}).(pulumi.StringOutput)

	// Extract the role name from the role ARN
	// IAM RolePolicy needs the role name, not the full ARN
	roleName := target.RoleARN().ApplyT(func(arn string) string {
		// ARN format: arn:aws:iam::123456789:role/role-name
		// Extract everything after the last "/"
		for i := len(arn) - 1; i >= 0; i-- {
			if arn[i] == '/' {
				return arn[i+1:]
			}
		}
		return arn
	}).(pulumi.StringOutput)

	// Encode justification in resource name for audit trail
	policyName := name
	if opts.Justification != "" {
		policyName = fmt.Sprintf("%s-%s", name, sanitizeName(opts.Justification))
	}

	_, err := iam.NewRolePolicy(ctx, policyName, &iam.RolePolicyArgs{
		Role:   roleName,
		Policy: policyJSON,
	}, pulumi.Parent(parent))

	return err
}

// sanitizeName cleans a string for use in Pulumi resource names.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, c)
		} else {
			result = append(result, '-')
		}
	}
	if len(result) > 40 {
		result = result[:40]
	}
	return string(result)
}

// BuildResourceARNs constructs the list of ARNs for a grant based on the
// resource's base ARN and optional path scoping.
//
// If no paths are provided, grants access to the entire resource (baseARN + baseARN/*).
// If paths are provided, grants access to baseARN (for list operations) + each scoped path.
//
// Examples:
//
//	BuildResourceARNs(bucketARN, nil)            → [bucketARN, bucketARN/*]
//	BuildResourceARNs(bucketARN, ["tmp/*"])       → [bucketARN, bucketARN/tmp/*]
//	BuildResourceARNs(bucketARN, ["a/*", "b/*"])  → [bucketARN, bucketARN/a/*, bucketARN/b/*]
func BuildResourceARNs(baseARN pulumi.StringOutput, paths []string) []pulumi.StringInput {
	// Always include the base ARN (needed for operations like s3:ListBucket)
	arns := []pulumi.StringInput{baseARN}

	if len(paths) == 0 {
		arns = append(arns, pulumi.Sprintf("%s/*", baseARN))
	} else {
		for _, path := range paths {
			arns = append(arns, pulumi.Sprintf("%s/%s", baseARN, path))
		}
	}

	return arns
}

// toOutputs converts a slice of pulumi.StringInput to a slice of pulumi.Output
// for use with pulumi.All().
func toOutputs(inputs []pulumi.StringInput) []interface{} {
	outputs := make([]interface{}, len(inputs))
	for i, input := range inputs {
		outputs[i] = input.ToStringOutput()
	}
	return outputs
}
