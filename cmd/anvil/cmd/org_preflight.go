package cmd

// Org preflight: read-only checks that the caller is the management account
// of an all-features organization Org can manage.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// orgDefaultRegion is the home region when neither --region nor the AWS config
// sets one.
const orgDefaultRegion = "ap-southeast-2"

// orgInfo is what preflight learned about the organization.
type orgInfo struct {
	ID                  string
	RootID              string
	ManagementAccountID string
	// DeployerArn is the exact IAM ARN of whoever is deploying: the role
	// behind an assumed-role (SSO) session, or an IAM user. anvil-org-automation
	// trusts it.
	DeployerArn string
}

// orgAWSConfig loads AWS config for the home region: anvil.org.yaml, then
// --region, then the AWS config's region, then orgDefaultRegion.
func orgAWSConfig(ctx context.Context, recorded string) (aws.Config, string, error) {
	region := recorded
	if region == "" {
		region = orgRegion
	}
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, "", fmt.Errorf("AWS credentials not found or expired.\n  Sign in to the management account (e.g. aws sso login) and try again.")
	}
	if cfg.Region == "" {
		cfg.Region = orgDefaultRegion
	}
	if recorded != "" && orgRegion != "" && orgRegion != recorded {
		printWarn(fmt.Sprintf("--region %s ignored: %s already records home region %s", orgRegion, orgConfigFile, recorded))
	}
	return cfg, cfg.Region, nil
}

// orgPreflight checks the caller is the management account of an
// all-features organization not managed by Control Tower. Read-only.
func orgPreflight(ctx context.Context, cfg aws.Config) (*orgInfo, error) {
	ident, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("AWS credentials not found or expired.\n  Sign in to the management account (e.g. aws sso login) and try again.")
	}
	caller := aws.ToString(ident.Account)
	deployer, err := deployerArn(ctx, cfg, aws.ToString(ident.Arn))
	if err != nil {
		return nil, err
	}

	client := organizations.NewFromConfig(cfg)
	out, err := client.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	if err != nil {
		var notInUse *orgtypes.AWSOrganizationsNotInUseException
		if errors.As(err, &notInUse) {
			return nil, fmt.Errorf("Account %s isn't in an organization.\n"+
				"  Create one in the console (AWS Organizations → Create an organization, all features), then try again.", caller)
		}
		return nil, fmt.Errorf("failed to read the organization: %s", mapError(err.Error()))
	}
	o := out.Organization
	management := aws.ToString(o.MasterAccountId)
	if caller != management {
		return nil, fmt.Errorf("Org must deploy from the management account (%s).\n"+
			"  These credentials are for %s. Sign in to the management account and try again.", management, caller)
	}
	if o.FeatureSet != orgtypes.OrganizationFeatureSetAll {
		return nil, fmt.Errorf("Organization %s has only consolidated billing enabled.\n"+
			"  Enable all features (AWS Organizations → Settings), then try again:\n"+
			"  https://docs.aws.amazon.com/organizations/latest/userguide/orgs_manage_org_support-all-features.html",
			aws.ToString(o.Id))
	}

	managedByControlTower, err := orgUsesControlTower(ctx, client)
	if err != nil {
		return nil, err
	}
	if managedByControlTower {
		return nil, fmt.Errorf("This org is managed by Control Tower. Org doesn't support Control Tower yet;\n" +
			"  Anvil Apps work in its accounts as normal.")
	}

	roots, err := client.ListRoots(ctx, &organizations.ListRootsInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to read the organization root: %s", mapError(err.Error()))
	}
	if len(roots.Roots) != 1 {
		return nil, fmt.Errorf("expected one organization root, found %d", len(roots.Roots))
	}

	return &orgInfo{
		ID:                  aws.ToString(o.Id),
		RootID:              aws.ToString(roots.Roots[0].Id),
		ManagementAccountID: management,
		DeployerArn:         deployer,
	}, nil
}

// orgUsesControlTower reports whether Control Tower has trusted access, which
// it enables when it sets up a landing zone.
func orgUsesControlTower(ctx context.Context, client *organizations.Client) (bool, error) {
	pages := organizations.NewListAWSServiceAccessForOrganizationPaginator(client, &organizations.ListAWSServiceAccessForOrganizationInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("failed to read the organization's trusted services: %s", mapError(err.Error()))
		}
		for _, svc := range page.EnabledServicePrincipals {
			if aws.ToString(svc.ServicePrincipal) == "controltower.amazonaws.com" {
				return true, nil
			}
		}
	}
	return false, nil
}

// deployerArn turns the caller's STS ARN into the exact IAM ARN a trust policy
// can name: the role (with its path, e.g. aws-reserved/sso.amazonaws.com/…)
// behind an assumed-role session, or the IAM user itself.
func deployerArn(ctx context.Context, cfg aws.Config, callerArn string) (string, error) {
	switch {
	case strings.Contains(callerArn, ":assumed-role/"):
		roleName := roleNameFromAssumedRole(callerArn)
		out, err := iam.NewFromConfig(cfg).GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
		if err != nil {
			return "", fmt.Errorf("failed to read your role %s: %s", roleName, mapError(err.Error()))
		}
		return aws.ToString(out.Role.Arn), nil
	case strings.Contains(callerArn, ":user/"):
		return callerArn, nil
	case strings.HasSuffix(callerArn, ":root"):
		return "", fmt.Errorf("Org can't deploy as the root user (root can't assume roles).\n" +
			"  Sign in as an admin through IAM Identity Center (or an IAM role) and try again.")
	default:
		return "", fmt.Errorf("unrecognised caller %s; sign in as an IAM Identity Center user, IAM role or IAM user", callerArn)
	}
}

// roleNameFromAssumedRole returns the role name in
// arn:aws:sts::<acct>:assumed-role/<role>/<session>.
func roleNameFromAssumedRole(arn string) string {
	_, rest, _ := strings.Cut(arn, ":assumed-role/")
	name, _, _ := strings.Cut(rest, "/")
	return name
}
