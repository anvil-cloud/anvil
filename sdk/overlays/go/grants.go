// sdk/go/anvil/grants.go
// Hand-written. Backed up/restored during gen-sdk like app.go and block.go.
//
// Provides the runtime grant execution for all resource grant methods.

package anvil

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// GrantTarget is implemented by any Anvil compute resource that can receive
// IAM permissions from infra resources via grant methods.
type GrantTarget interface {
	GrantName() string
	GrantRoleArn() pulumi.StringOutput
}

// GrantOptions provides optional metadata for grant methods.
type GrantOptions struct {
	Justification string

	// Endpoint routes the granted calls through a VPC endpoint. Anvil then
	// adds ingress 443 on the endpoint's SG from the target's own SG and
	// egress 443 on the target's SG to the endpoint's SG (once per pair), and
	// limits the IAM statement to the endpoint with aws:SourceVpce. The
	// endpoint's service must match the actions' service, and the target must
	// be a NetworkGrantTarget (e.g. from LambdaTarget) in a VPC.
	// Build it with Endpoint(name, ep).
	Endpoint *GrantEndpoint
}

// CreateGrant creates a scoped IAM RolePolicy granting the specified actions
// on the specified resource ARNs to the target's execution role.
func CreateGrant(
	ctx *pulumi.Context,
	parent pulumi.Resource,
	name string,
	target GrantTarget,
	actions []string,
	resourceArns []pulumi.StringInput,
	opts *GrantOptions,
) error {
	var endpoint *GrantEndpoint
	var service string
	if opts != nil && opts.Endpoint != nil {
		endpoint = opts.Endpoint
		svc, err := endpointServiceForActions(actions)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		service = svc
		if err := ConnectToEndpoint(ctx, endpoint.Name, endpoint.SecurityGroupId, target, 443, endpoint.Opts...); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	inputs := toOutputs(resourceArns)
	nArns := len(inputs)
	if endpoint != nil {
		inputs = append(inputs, endpoint.EndpointId, endpoint.Service)
	}

	policyJSON := pulumi.All(inputs...).ApplyT(func(args []interface{}) (string, error) {
		resources := make([]string, nArns)
		for i := 0; i < nArns; i++ {
			resources[i] = args[i].(string)
		}
		statement := map[string]interface{}{
			"Effect":   "Allow",
			"Action":   actions,
			"Resource": resources,
		}
		if endpoint != nil {
			endpointID := args[nArns].(string)
			if got := args[nArns+1].(string); got != service {
				return "", fmt.Errorf("%s: actions are for %q, but endpoint %s routes %q", name, service, endpoint.Name, got)
			}
			// Only works through this endpoint: credentials used from anywhere
			// else (e.g. leaked from the function's environment) are denied.
			statement["Condition"] = map[string]interface{}{
				"StringEquals": map[string]string{"aws:SourceVpce": endpointID},
			}
		}
		doc := map[string]interface{}{
			"Version":   "2012-10-17",
			"Statement": []map[string]interface{}{statement},
		}
		b, err := json.Marshal(doc)
		if err != nil {
			return "", fmt.Errorf("failed to marshal IAM policy: %w", err)
		}
		return string(b), nil
	}).(pulumi.StringOutput)

	roleName := target.GrantRoleArn().ApplyT(func(arn string) string {
		idx := strings.LastIndex(arn, "/")
		if idx >= 0 {
			return arn[idx+1:]
		}
		return arn
	}).(pulumi.StringOutput)

	policyName := name
	if opts != nil && opts.Justification != "" {
		policyName = fmt.Sprintf("%s-%s", name, sanitize(opts.Justification))
	}

	_, err := iam.NewRolePolicy(ctx, policyName, &iam.RolePolicyArgs{
		Role:   roleName,
		Policy: policyJSON,
	}, pulumi.Parent(parent))

	return err
}

// sanitize cleans a string for use in resource names.
func sanitize(s string) string {
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

// BuildResourceArns constructs the list of ARNs for a grant.
func BuildResourceArns(baseArn pulumi.StringOutput, paths []string) []pulumi.StringInput {
	arns := []pulumi.StringInput{baseArn}

	if len(paths) == 0 {
		arns = append(arns, pulumi.Sprintf("%s/*", baseArn))
	} else {
		for _, p := range paths {
			arns = append(arns, pulumi.Sprintf("%s/%s", baseArn, p))
		}
	}

	return arns
}

func toOutputs(inputs []pulumi.StringInput) []interface{} {
	outputs := make([]interface{}, len(inputs))
	for i, input := range inputs {
		outputs[i] = input.ToStringOutput()
	}
	return outputs
}
