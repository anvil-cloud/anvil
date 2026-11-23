// Package orgaccount holds what Org's account resources share: AWS clients,
// placing and tagging accounts, and anvil-org-automation inside them.
// Used by provider/aws/orgImportedAccount and provider/aws/orgAccount.
package orgaccount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	// AutomationRole is Anvil's own role in every managed member account.
	AutomationRole = "anvil-org-automation"
	// AccessRole is the break-glass role AWS creates in accounts made inside
	// an organization without a RoleName. Org uses it once per imported
	// account, to bootstrap.
	AccessRole = "OrganizationAccountAccessRole"

	TagManaged   = "anvil:managed"
	TagOrg       = "anvil:org"
	TagLifecycle = "anvil:lifecycle"

	adminPolicy = "arn:aws:iam::aws:policy/AdministratorAccess"
)

// LoadAWS loads the deployer's credentials (the management account).
func LoadAWS(ctx context.Context) (aws.Config, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return aws.Config{}, fmt.Errorf("loading AWS credentials: %w", err)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return cfg, nil
}

// Move puts the account in ouID, wherever it is now.
func Move(ctx context.Context, org *organizations.Client, id, ouID string) error {
	parents, err := org.ListParents(ctx, &organizations.ListParentsInput{ChildId: aws.String(id)})
	if err != nil {
		return fmt.Errorf("reading where account %s is: %w", id, err)
	}
	if len(parents.Parents) != 1 {
		return fmt.Errorf("account %s has %d parents, expected 1", id, len(parents.Parents))
	}
	current := aws.ToString(parents.Parents[0].Id)
	if current == ouID {
		return nil
	}
	_, err = org.MoveAccount(ctx, &organizations.MoveAccountInput{
		AccountId:           aws.String(id),
		SourceParentId:      aws.String(current),
		DestinationParentId: aws.String(ouID),
	})
	if err != nil {
		return fmt.Errorf("moving account %s to %s: %w", id, ouID, err)
	}
	return nil
}

// ManagedTags are Anvil's tags plus the user's, for an account in orgName.
func ManagedTags(orgName string, tags map[string]string) map[string]string {
	want := map[string]string{TagManaged: "true", TagOrg: orgName}
	for k, v := range tags {
		want[k] = v
	}
	return want
}

// OrgTags turns a map into sorted Organizations tags.
func OrgTags(m map[string]string) []orgtypes.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tags := make([]orgtypes.Tag, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, orgtypes.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return tags
}

// Tag sets Anvil's and the user's tags on the account, and removes user tags
// dropped since the last deploy (oldTags).
func Tag(ctx context.Context, org *organizations.Client, id, orgName string, tags, oldTags map[string]string) error {
	want := ManagedTags(orgName, tags)
	if _, err := org.TagResource(ctx, &organizations.TagResourceInput{
		ResourceId: aws.String(id),
		Tags:       OrgTags(want),
	}); err != nil {
		return fmt.Errorf("tagging account %s: %w", id, err)
	}
	var removed []string
	for k := range oldTags {
		if _, ok := want[k]; !ok {
			removed = append(removed, k)
		}
	}
	if len(removed) > 0 {
		sort.Strings(removed)
		if _, err := org.UntagResource(ctx, &organizations.UntagResourceInput{
			ResourceId: aws.String(id),
			TagKeys:    removed,
		}); err != nil {
			return fmt.Errorf("removing old tags from account %s: %w", id, err)
		}
	}
	return nil
}

// Untag removes tags from the account; an account that's gone is fine.
func Untag(ctx context.Context, org *organizations.Client, id string, keys []string) error {
	_, err := org.UntagResource(ctx, &organizations.UntagResourceInput{
		ResourceId: aws.String(id),
		TagKeys:    keys,
	})
	var gone *orgtypes.TargetNotFoundException
	if err != nil && !errors.As(err, &gone) {
		return err
	}
	return nil
}

// Assume returns config for role in account id, failing now rather than on
// the first call.
func Assume(ctx context.Context, base aws.Config, id, role, session string) (aws.Config, error) {
	arn := fmt.Sprintf("arn:aws:iam::%s:role/%s", id, role)
	provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(base), arn, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = session
	})
	if _, err := provider.Retrieve(ctx); err != nil {
		return aws.Config{}, err
	}
	cfg := base.Copy()
	cfg.Credentials = aws.NewCredentialsCache(provider)
	return cfg, nil
}

// AssumeWhenReady assumes role in a new account, retrying while AWS finishes
// setting the account up (the role exists before it's assumable).
func AssumeWhenReady(ctx context.Context, base aws.Config, id, role, session string, timeout time.Duration) (aws.Config, error) {
	deadline := time.Now().Add(timeout)
	wait := 5 * time.Second
	for {
		cfg, err := Assume(ctx, base, id, role, session)
		if err == nil {
			// One cheap call proves the credentials work in the account.
			if _, err = iam.NewFromConfig(cfg).GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)}); err == nil {
				return cfg, nil
			}
		}
		if time.Now().After(deadline) {
			return aws.Config{}, fmt.Errorf("account %s wasn't ready after %s: %w", id, timeout, err)
		}
		select {
		case <-ctx.Done():
			return aws.Config{}, ctx.Err()
		case <-time.After(wait):
		}
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}

// TrustPolicy lets exactly the given principals assume the role. IAM stores
// role principals as unique IDs, so a deleted and re-created role with the
// same name doesn't inherit the trust.
func TrustPolicy(principals []string) (string, error) {
	sorted := append([]string(nil), principals...)
	sort.Strings(sorted)
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]any{"AWS": sorted},
			"Action":    "sts:AssumeRole",
		}},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// EnsureAutomationRole creates anvil-org-automation in the account (member
// is config already inside it), or narrows its trust to exactly deployers,
// and makes sure it's admin. Returns its ARN.
func EnsureAutomationRole(ctx context.Context, member aws.Config, id string, deployers []string) (string, error) {
	trust, err := TrustPolicy(deployers)
	if err != nil {
		return "", err
	}
	client := iam.NewFromConfig(member)
	got, err := client.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(AutomationRole)})
	var missing *iamtypes.NoSuchEntityException
	switch {
	case errors.As(err, &missing):
		out, err := client.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(AutomationRole),
			AssumeRolePolicyDocument: aws.String(trust),
			Description:              aws.String("Anvil Org automation. Managed by anvil org; only Org's deployers can assume it."),
			Tags:                     []iamtypes.Tag{{Key: aws.String(TagManaged), Value: aws.String("true")}},
		})
		if err != nil {
			return "", fmt.Errorf("account %s: creating %s: %w", id, AutomationRole, err)
		}
		got = &iam.GetRoleOutput{Role: out.Role}
	case err != nil:
		return "", fmt.Errorf("account %s: reading %s: %w", id, AutomationRole, err)
	default:
		if _, err := client.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
			RoleName:       aws.String(AutomationRole),
			PolicyDocument: aws.String(trust),
		}); err != nil {
			return "", fmt.Errorf("account %s: updating %s trust: %w", id, AutomationRole, err)
		}
	}
	if _, err := client.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		RoleName:  aws.String(AutomationRole),
		PolicyArn: aws.String(adminPolicy),
	}); err != nil {
		return "", fmt.Errorf("account %s: attaching AdministratorAccess to %s: %w", id, AutomationRole, err)
	}
	return aws.ToString(got.Role.Arn), nil
}
