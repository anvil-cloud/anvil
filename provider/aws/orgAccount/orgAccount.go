// Package orgAccount is the Org resource that creates a member account:
// CreateAccount (with Anvil's own role in place of
// OrganizationAccountAccessRole), wait for it, move it to its OU straight
// away, then narrow anvil-org-automation's trust to Org's deployers.
//
// Accounts are permanent, so nothing here ever closes or removes one. Deleting
// the resource moves the account to Suspended, tags it
// anvil:lifecycle=suspended and releases it; the user closes it in the
// console. Design: docs/design/org/05-accounts.md.
package orgAccount

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/anvil-cloud/anvil/provider/internal/orgaccount"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/account"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
)

const (
	// AWS allows 5 CreateAccount requests in flight per organization; one
	// deploy is one provider process, so a package semaphore enforces it.
	maxInFlight = 5
	// CreateAccount usually takes a few minutes.
	createTimeout = 20 * time.Minute
	// The role exists before it's assumable.
	readyTimeout = 10 * time.Minute
)

var (
	inFlight     = make(chan struct{}, maxInFlight)
	emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	pollEvery    = 10 * time.Second
)

// OrgAccount is the custom resource.
type OrgAccount struct{}

// Resource registers OrgAccount with the provider (picked up by scripts/registry).
func Resource() infer.InferredResource { return infer.Resource(&OrgAccount{}) }

func (r *OrgAccount) Annotate(a infer.Annotator) {
	a.SetToken("aws", "OrgAccount")
	a.Describe(r, "A member account created by an Anvil Org. Used by anvil.Org; "+
		"deleting it never closes the account: it's moved to Suspended, tagged anvil:lifecycle=suspended and released.")
}

// Args are the resource inputs.
type Args struct {
	Name          string            `pulumi:"name"`
	Email         string            `pulumi:"email"`
	OuID          string            `pulumi:"ouId"`
	SuspendedOuID string            `pulumi:"suspendedOuId"`
	OrgName       string            `pulumi:"orgName"`
	Tags          map[string]string `pulumi:"tags,optional"`
	DeployerArns  []string          `pulumi:"deployerArns"`
}

func (a *Args) Annotate(an infer.Annotator) {
	an.Describe(&a.Name, "The account name. Changing it renames the account (account:PutAccountName).")
	an.Describe(&a.Email, "Root email, used only at creation. AWS needs the root user to change it, so a later change here is ignored with a warning.")
	an.Describe(&a.OuID, "The OU the account lives in.")
	an.Describe(&a.SuspendedOuID, "The Suspended OU the account moves to when removed from config.")
	an.Describe(&a.OrgName, "The Org project name, tagged on the account as anvil:org.")
	an.Describe(&a.Tags, "Extra tags on the account.")
	an.Describe(&a.DeployerArns, "Exact IAM ARNs anvil-org-automation trusts (Org's deployers).")
}

// State is the resource output.
type State struct {
	Args
	AccountID         string `pulumi:"accountId"`
	RootEmail         string `pulumi:"rootEmail"`
	AutomationRoleArn string `pulumi:"automationRoleArn"`
}

func (s *State) Annotate(an infer.Annotator) {
	an.Describe(&s.AccountID, "The new account's ID.")
	an.Describe(&s.RootEmail, "The root email the account was created with.")
	an.Describe(&s.AutomationRoleArn, "ARN of anvil-org-automation in the account.")
}

func validate(in Args) error {
	if in.Name == "" || len(in.Name) > 50 || strings.TrimSpace(in.Name) != in.Name {
		return fmt.Errorf("account name %q must be 1-50 characters with no leading or trailing spaces", in.Name)
	}
	if len(in.Email) > 64 || !emailPattern.MatchString(in.Email) {
		return fmt.Errorf("account %s: email %q must be a valid address of at most 64 characters", in.Name, in.Email)
	}
	if in.OuID == "" || in.SuspendedOuID == "" {
		return fmt.Errorf("account %s: needs its OU and the Suspended OU", in.Name)
	}
	if len(in.DeployerArns) == 0 {
		return fmt.Errorf("account %s: needs at least one deployer ARN for anvil-org-automation to trust", in.Name)
	}
	return nil
}

// Create makes the account, moves it into its OU and sets up
// anvil-org-automation. Once the account exists, any later failure is
// returned as ResourceInitFailedError, so state keeps the account and the
// next deploy finishes setup instead of creating another.
func (r *OrgAccount) Create(ctx context.Context, req infer.CreateRequest[Args]) (infer.CreateResponse[State], error) {
	in := req.Inputs
	state := State{Args: in, RootEmail: in.Email}
	if err := validate(in); err != nil {
		return infer.CreateResponse[State]{Output: state}, err
	}
	if req.DryRun {
		return infer.CreateResponse[State]{Output: state}, nil
	}
	cfg, err := orgaccount.LoadAWS(ctx)
	if err != nil {
		return infer.CreateResponse[State]{Output: state}, err
	}
	org := organizations.NewFromConfig(cfg)

	id, err := createAccount(ctx, org, in)
	if err != nil {
		return infer.CreateResponse[State]{Output: state}, err
	}
	state.AccountID = id
	state.AutomationRoleArn = fmt.Sprintf("arn:aws:iam::%s:role/%s", id, orgaccount.AutomationRole)

	if err := setUp(ctx, cfg, org, id, in, nil, true); err != nil {
		return infer.CreateResponse[State]{ID: id, Output: state}, infer.ResourceInitFailedError{Reasons: []string{
			fmt.Sprintf("account %s (%s) was created but setup didn't finish; deploy again to finish it: %v", in.Name, id, err),
		}}
	}
	return infer.CreateResponse[State]{ID: id, Output: state}, nil
}

// Update renames, moves, retags and re-trusts. The email is never changed.
func (r *OrgAccount) Update(ctx context.Context, req infer.UpdateRequest[Args, State]) (infer.UpdateResponse[State], error) {
	in, old := req.Inputs, req.State
	state := State{Args: in, AccountID: old.AccountID, RootEmail: old.RootEmail, AutomationRoleArn: old.AutomationRoleArn}
	if state.RootEmail == "" {
		state.RootEmail = old.Email
	}
	if err := validate(in); err != nil {
		return infer.UpdateResponse[State]{Output: state}, err
	}
	if in.Email != state.RootEmail {
		p.GetLogger(ctx).Warningf("account %s's root email stays %s: it's set at creation, and changing it needs the root user in the console",
			in.Name, state.RootEmail)
	}
	if req.DryRun {
		return infer.UpdateResponse[State]{Output: state}, nil
	}
	cfg, err := orgaccount.LoadAWS(ctx)
	if err != nil {
		return infer.UpdateResponse[State]{Output: state}, err
	}
	org := organizations.NewFromConfig(cfg)
	in.Email = state.RootEmail
	state.Email = state.RootEmail
	err = setUp(ctx, cfg, org, old.AccountID, in, &old, false)
	return infer.UpdateResponse[State]{Output: state}, err
}

// Delete never closes the account: it moves it to Suspended, tags it
// anvil:lifecycle=suspended, removes Anvil's managed tag and the user's tags,
// and releases it. anvil-org-automation is left in place: until break glass
// exists it's the only way back in.
func (r *OrgAccount) Delete(ctx context.Context, req infer.DeleteRequest[State]) (infer.DeleteResponse, error) {
	st := req.State
	cfg, err := orgaccount.LoadAWS(ctx)
	if err != nil {
		return infer.DeleteResponse{}, err
	}
	org := organizations.NewFromConfig(cfg)
	desc, err := org.DescribeAccount(ctx, &organizations.DescribeAccountInput{AccountId: aws.String(st.AccountID)})
	var notFound *orgtypes.AccountNotFoundException
	if errors.As(err, &notFound) {
		return infer.DeleteResponse{}, nil // already gone from the org
	}
	if err != nil {
		return infer.DeleteResponse{}, fmt.Errorf("reading account %s: %w", st.AccountID, err)
	}
	if desc.Account.State != orgtypes.AccountStateActive {
		return infer.DeleteResponse{}, nil // closing or closed; nothing left to do
	}
	if err := orgaccount.Move(ctx, org, st.AccountID, st.SuspendedOuID); err != nil {
		return infer.DeleteResponse{}, fmt.Errorf("moving removed account %s to Suspended: %w", st.Name, err)
	}
	if _, err := org.TagResource(ctx, &organizations.TagResourceInput{
		ResourceId: aws.String(st.AccountID),
		Tags:       orgaccount.OrgTags(map[string]string{orgaccount.TagLifecycle: "suspended"}),
	}); err != nil {
		return infer.DeleteResponse{}, fmt.Errorf("tagging removed account %s as suspended: %w", st.Name, err)
	}
	keys := []string{orgaccount.TagManaged}
	for k := range st.Tags {
		keys = append(keys, k)
	}
	if err := orgaccount.Untag(ctx, org, st.AccountID, keys); err != nil {
		return infer.DeleteResponse{}, fmt.Errorf("releasing account %s: %w", st.Name, err)
	}
	return infer.DeleteResponse{}, nil
}

// setUp places, tags and re-trusts the account, and renames it when the
// name changed. fresh is true right after CreateAccount, when the role needs
// time to become assumable.
func setUp(ctx context.Context, cfg aws.Config, org *organizations.Client, id string, in Args, old *State, fresh bool) error {
	// Move first, so the OU's guardrails apply before anything else happens
	// (a new account starts at Root).
	if err := orgaccount.Move(ctx, org, id, in.OuID); err != nil {
		return err
	}
	var oldTags map[string]string
	if old != nil {
		oldTags = old.Tags
	} else {
		// An account written back after removal loses its suspended tag.
		oldTags = map[string]string{orgaccount.TagLifecycle: ""}
	}
	if err := orgaccount.Tag(ctx, org, id, in.OrgName, in.Tags, oldTags); err != nil {
		return err
	}

	var member aws.Config
	var err error
	if fresh {
		member, err = orgaccount.AssumeWhenReady(ctx, cfg, id, orgaccount.AutomationRole, "anvil-org-deploy", readyTimeout)
	} else {
		member, err = orgaccount.Assume(ctx, cfg, id, orgaccount.AutomationRole, "anvil-org-deploy")
	}
	if err != nil {
		return fmt.Errorf("account %s (%s): can't assume %s: %w", in.Name, id, orgaccount.AutomationRole, err)
	}
	if _, err := orgaccount.EnsureAutomationRole(ctx, member, id, in.DeployerArns); err != nil {
		return err
	}

	// Renames run inside the account, so the org doesn't need trusted access
	// for AWS Account Management.
	if old != nil && old.Name != in.Name {
		if _, err := account.NewFromConfig(member).PutAccountName(ctx, &account.PutAccountNameInput{
			AccountName: aws.String(in.Name),
		}); err != nil {
			return fmt.Errorf("renaming account %s to %s: %w", old.Name, in.Name, err)
		}
	}
	return nil
}

// createAccount calls CreateAccount and waits for it, with Anvil's role in
// place of OrganizationAccountAccessRole. An account in this org with the
// same email and Anvil's tags for this org (a create whose state was lost)
// is adopted instead of failing on EMAIL_ALREADY_EXISTS.
func createAccount(ctx context.Context, org *organizations.Client, in Args) (string, error) {
	if id, err := findExisting(ctx, org, in); err != nil || id != "" {
		return id, err
	}

	select {
	case inFlight <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-inFlight }()

	tags := maps.Clone(orgaccount.ManagedTags(in.OrgName, in.Tags))
	var requestID string
	for attempt := 0; ; attempt++ {
		out, err := org.CreateAccount(ctx, &organizations.CreateAccountInput{
			AccountName: aws.String(in.Name),
			Email:       aws.String(in.Email),
			RoleName:    aws.String(orgaccount.AutomationRole),
			Tags:        orgaccount.OrgTags(tags),
		})
		var busy *orgtypes.ConcurrentModificationException
		var tooMany *orgtypes.TooManyRequestsException
		if (errors.As(err, &busy) || errors.As(err, &tooMany)) && attempt < 10 {
			if err := sleep(ctx, pollEvery); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", fmt.Errorf("creating account %s: %w", in.Name, err)
		}
		requestID = aws.ToString(out.CreateAccountStatus.Id)
		break
	}

	deadline := time.Now().Add(createTimeout)
	for {
		out, err := org.DescribeCreateAccountStatus(ctx, &organizations.DescribeCreateAccountStatusInput{
			CreateAccountRequestId: aws.String(requestID),
		})
		if err != nil {
			return "", fmt.Errorf("checking on account %s (request %s): %w", in.Name, requestID, err)
		}
		st := out.CreateAccountStatus
		switch st.State {
		case orgtypes.CreateAccountStateSucceeded:
			return aws.ToString(st.AccountId), nil
		case orgtypes.CreateAccountStateFailed:
			return "", createFailure(in, st.FailureReason)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("account %s is still being created after %s (request %s). "+
				"Deploy again once it shows in the console; Org adopts it by email", in.Name, createTimeout, requestID)
		}
		if err := sleep(ctx, pollEvery); err != nil {
			return "", err
		}
	}
}

// findExisting returns an account in the org with in.Email, if it's one this
// Org created; errors if the email belongs to an account Org doesn't manage.
func findExisting(ctx context.Context, org *organizations.Client, in Args) (string, error) {
	pages := organizations.NewListAccountsPaginator(org, &organizations.ListAccountsInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("listing accounts: %w", err)
		}
		for _, a := range page.Accounts {
			if !strings.EqualFold(aws.ToString(a.Email), in.Email) {
				continue
			}
			id := aws.ToString(a.Id)
			tags, err := org.ListTagsForResource(ctx, &organizations.ListTagsForResourceInput{ResourceId: aws.String(id)})
			if err != nil {
				return "", fmt.Errorf("reading tags on account %s: %w", id, err)
			}
			for _, t := range tags.Tags {
				if aws.ToString(t.Key) != orgaccount.TagOrg || aws.ToString(t.Value) != in.OrgName {
					continue
				}
				if a.Status != orgtypes.AccountStatusActive {
					return "", fmt.Errorf("account %s: email %s belongs to account %s, which is %s. "+
						"A closed account's email can never be reused; give this account another email", in.Name, in.Email, id, a.Status)
				}
				// Ours: a create whose state was lost, or an account removed
				// earlier (in Suspended) being written back. Take it back.
				return id, nil
			}
			return "", fmt.Errorf("account %s: email %s already belongs to account %s (%s) in this org, which Org doesn't manage. "+
				"Import it with import: { accountId: '%s' }, or give this account another email", in.Name, in.Email, id, aws.ToString(a.Name), id)
		}
	}
	return "", nil
}

// createFailure turns a CreateAccount failure reason into what to do.
func createFailure(in Args, reason orgtypes.CreateAccountFailureReason) error {
	r := string(reason)
	switch {
	case reason == orgtypes.CreateAccountFailureReasonEmailAlreadyExists:
		return fmt.Errorf("account %s: %s is already used by another AWS account (anywhere in AWS, closed ones included). "+
			"Give it another email with email: '...' on the account, or change the pattern", in.Name, in.Email)
	case reason == orgtypes.CreateAccountFailureReasonInvalidEmail:
		return fmt.Errorf("account %s: AWS rejected the email %s. Use a valid address", in.Name, in.Email)
	case reason == orgtypes.CreateAccountFailureReasonAccountLimitExceeded:
		return fmt.Errorf("account %s: the org is at its account quota. Request an increase in the management account "+
			"(Service Quotas → AWS Organizations → Default maximum number of accounts, us-east-1), then deploy again", in.Name)
	case strings.Contains(r, "BUSINESS_VALIDATION"):
		return fmt.Errorf("account %s: AWS is validating the management account's business details (%s). "+
			"Check the management account's billing and support cases, then deploy again", in.Name, reason)
	case strings.Contains(r, "PAYMENT_INSTRUMENT"):
		return fmt.Errorf("account %s: the management account's payment method is missing or invalid. Add one in Billing, then deploy again", in.Name)
	default:
		return fmt.Errorf("account %s: AWS couldn't create it (%s). Deploy again; if it keeps failing, contact AWS Support", in.Name, reason)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
