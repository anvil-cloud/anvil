// Package orgImportedAccount is the Org resource that brings an existing
// member account under Anvil: it moves the account into its OU, tags it, and
// (unless opted out) bootstraps anvil-org-automation inside it through
// OrganizationAccountAccessRole.
//
// It is a custom resource, not a component: Pulumi's own Account resource
// removes the account from the organization when deleted. This one never
// moves, removes or closes an account on delete; it only releases it (removes
// Anvil's tags). Design: docs/design/org/05-accounts.md.
package orgImportedAccount

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/anvil-cloud/anvil/provider/internal/orgaccount"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/pulumi/pulumi-go-provider/infer"
)

const (
	// AutomationRole is Anvil's own role in every managed member account.
	AutomationRole = orgaccount.AutomationRole
	accessRole     = orgaccount.AccessRole
	tagManaged     = orgaccount.TagManaged
	tagOrg         = orgaccount.TagOrg
)

// OrgImportedAccount is the custom resource.
type OrgImportedAccount struct{}

// Resource registers OrgImportedAccount with the provider (picked up by
// scripts/registry).
func Resource() infer.InferredResource { return infer.Resource(&OrgImportedAccount{}) }

func (r *OrgImportedAccount) Annotate(a infer.Annotator) {
	a.SetToken("aws", "OrgImportedAccount")
	a.Describe(r, "An existing member account managed by an Anvil Org. Used by anvil.Org; "+
		"deleting it only releases the account (removes Anvil's tags), never moves, removes or closes it.")
}

// Args are the resource inputs.
type Args struct {
	AccountID    string            `pulumi:"accountId" provider:"replaceOnChanges"`
	OuID         string            `pulumi:"ouId"`
	OrgName      string            `pulumi:"orgName"`
	Tags         map[string]string `pulumi:"tags,optional"`
	Bootstrap    *bool             `pulumi:"bootstrap,optional"`
	DeployerArns []string          `pulumi:"deployerArns,optional"`
}

func (a *Args) Annotate(an infer.Annotator) {
	an.Describe(&a.AccountID, "The existing member account's 12-digit ID.")
	an.Describe(&a.OuID, "The OU to move the account into.")
	an.Describe(&a.OrgName, "The Org project name, tagged on the account as anvil:org.")
	an.Describe(&a.Tags, "Extra tags on the account.")
	an.Describe(&a.Bootstrap, "Create anvil-org-automation in the account through OrganizationAccountAccessRole. Default true.")
	an.Describe(&a.DeployerArns, "Exact IAM ARNs anvil-org-automation trusts (Org's deployers). Required when bootstrapping.")
}

// State is the resource output.
type State struct {
	Args
	AutomationRoleArn string `pulumi:"automationRoleArn"`
	Bootstrapped      bool   `pulumi:"bootstrapped"`
}

func (s *State) Annotate(an infer.Annotator) {
	an.Describe(&s.AutomationRoleArn, "ARN of anvil-org-automation in the account, or empty when not bootstrapped.")
	an.Describe(&s.Bootstrapped, "Whether anvil-org-automation was bootstrapped.")
}

func (a Args) bootstrap() bool { return a.Bootstrap == nil || *a.Bootstrap }

var accountIDPattern = regexp.MustCompile(`^\d{12}$`)

// Create imports the account: checks it, moves it, tags it, bootstraps.
func (r *OrgImportedAccount) Create(ctx context.Context, req infer.CreateRequest[Args]) (infer.CreateResponse[State], error) {
	state, err := r.reconcile(ctx, req.Inputs, nil, req.DryRun)
	return infer.CreateResponse[State]{ID: req.Inputs.AccountID, Output: state}, err
}

// Update reconciles placement, tags and the automation role with new inputs.
func (r *OrgImportedAccount) Update(ctx context.Context, req infer.UpdateRequest[Args, State]) (infer.UpdateResponse[State], error) {
	state, err := r.reconcile(ctx, req.Inputs, &req.State, req.DryRun)
	return infer.UpdateResponse[State]{Output: state}, err
}

// Delete releases the account: removes Anvil's tags. It never moves, removes
// or closes the account, and leaves anvil-org-automation in place.
func (r *OrgImportedAccount) Delete(ctx context.Context, req infer.DeleteRequest[State]) (infer.DeleteResponse, error) {
	cfg, err := orgaccount.LoadAWS(ctx)
	if err != nil {
		return infer.DeleteResponse{}, err
	}
	keys := []string{tagManaged, tagOrg}
	for k := range req.State.Tags {
		keys = append(keys, k)
	}
	if err := orgaccount.Untag(ctx, organizations.NewFromConfig(cfg), req.State.AccountID, keys); err != nil {
		return infer.DeleteResponse{}, fmt.Errorf("releasing account %s: removing Anvil's tags: %w", req.State.AccountID, err)
	}
	return infer.DeleteResponse{}, nil
}

func (r *OrgImportedAccount) reconcile(ctx context.Context, in Args, old *State, dryRun bool) (State, error) {
	state := State{Args: in}
	if !accountIDPattern.MatchString(in.AccountID) {
		return state, fmt.Errorf("accountId %q must be 12 digits", in.AccountID)
	}
	if in.bootstrap() && len(in.DeployerArns) == 0 {
		return state, fmt.Errorf("account %s: bootstrapping needs at least one deployer ARN to trust", in.AccountID)
	}
	if in.bootstrap() {
		state.AutomationRoleArn = fmt.Sprintf("arn:aws:iam::%s:role/%s", in.AccountID, AutomationRole)
		state.Bootstrapped = true
	}

	cfg, err := orgaccount.LoadAWS(ctx)
	if err != nil {
		return state, err
	}
	org := organizations.NewFromConfig(cfg)

	// Read-only checks run on preview too, so a wrong ID fails before deploy.
	if err := checkAccount(ctx, org, in.AccountID); err != nil {
		return state, err
	}
	if dryRun {
		return state, nil
	}

	if err := orgaccount.Move(ctx, org, in.AccountID, in.OuID); err != nil {
		return state, err
	}
	var oldTags map[string]string
	if old != nil {
		oldTags = old.Tags
	}
	if err := orgaccount.Tag(ctx, org, in.AccountID, in.OrgName, in.Tags, oldTags); err != nil {
		return state, err
	}
	if in.bootstrap() {
		arn, err := ensureAutomationRole(ctx, cfg, in.AccountID, in.DeployerArns)
		if err != nil {
			return state, err
		}
		state.AutomationRoleArn = arn
	} else if old != nil && old.Bootstrapped {
		// Turning bootstrap off leaves the existing role; Org just stops using it.
		state.AutomationRoleArn = old.AutomationRoleArn
		state.Bootstrapped = true
	}
	return state, nil
}

// checkAccount verifies the account is an active member of this organization
// and not the management account.
func checkAccount(ctx context.Context, org *organizations.Client, id string) error {
	desc, err := org.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	if err != nil {
		return fmt.Errorf("reading the organization: %w", err)
	}
	if aws.ToString(desc.Organization.MasterAccountId) == id {
		return fmt.Errorf("account %s is the management account; Org never manages it as a member", id)
	}
	out, err := org.DescribeAccount(ctx, &organizations.DescribeAccountInput{AccountId: aws.String(id)})
	if err != nil {
		var notFound *orgtypes.AccountNotFoundException
		if errors.As(err, &notFound) {
			return fmt.Errorf("account %s isn't in this organization (%s). Check the ID, or invite the account first",
				id, aws.ToString(desc.Organization.Id))
		}
		return fmt.Errorf("reading account %s: %w", id, err)
	}
	if out.Account.State != orgtypes.AccountStateActive {
		return fmt.Errorf("account %s is %s, not active; only active accounts can be imported", id, out.Account.State)
	}
	return nil
}

// ensureAutomationRole gets into the account and sets up anvil-org-automation.
// It goes in through anvil-org-automation itself when that already works, and
// only otherwise through OrganizationAccountAccessRole (the one-time
// bootstrap, with a session name the break-glass alert labels as expected).
func ensureAutomationRole(ctx context.Context, cfg aws.Config, id string, deployers []string) (string, error) {
	member, err := orgaccount.Assume(ctx, cfg, id, AutomationRole, "anvil-org-deploy")
	if err != nil {
		member, err = orgaccount.Assume(ctx, cfg, id, accessRole, "anvil-bootstrap-"+id)
		if err != nil {
			return "", fmt.Errorf("account %s: can't get in to create %s.\n"+
				"  Org bootstraps through %s, which AWS creates only in accounts made inside the org.\n"+
				"  Create %s in the account (trusting the management account), or set\n"+
				"  import: { bootstrap: { disable: true } } to import it without bootstrapping.\n  (%v)",
				id, AutomationRole, accessRole, accessRole, err)
		}
	}
	return orgaccount.EnsureAutomationRole(ctx, member, id, deployers)
}
