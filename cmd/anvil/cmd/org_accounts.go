package cmd

// Account changes for anvil org preview/deploy: accounts Org will create or
// suspend, the hard stop before creating any, and the account quota.
// Design: docs/design/org/05-accounts.md (Confirming account creation,
// Account quota, Removing an account).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
)

var (
	orgYesCreateAccounts  bool
	orgYesSuspendAccounts bool
)

// orgAccountChanges are the permanent account changes in this run.
type orgAccountChanges struct {
	Create  []orgPlanAccount // created by this deploy
	Suspend []string         // created accounts leaving config, moved to Suspended
}

// planAccountChanges compares the plan with state.
func planAccountChanges(plan *orgPlan, st orgState) orgAccountChanges {
	var c orgAccountChanges
	for _, a := range plan.Accounts {
		if a.Kind != "create" {
			continue
		}
		if _, ok := st.Accounts[a.Name]; ok {
			continue
		}
		if _, ok := st.Accounts[a.RenamedFrom]; ok && a.RenamedFrom != "" {
			continue
		}
		c.Create = append(c.Create, a)
	}
	for name, created := range st.Created {
		if created && !orgAccountWritten(plan, name) {
			c.Suspend = append(c.Suspend, name)
		}
	}
	sort.Strings(c.Suspend)
	return c
}

// printAccountChanges lists the permanent changes.
func printAccountChanges(c orgAccountChanges) {
	if len(c.Create) == 0 && len(c.Suspend) == 0 {
		return
	}
	width := 0
	for _, a := range c.Create {
		width = max(width, len(a.Name))
	}
	for _, n := range c.Suspend {
		width = max(width, len(n))
	}
	for _, a := range c.Create {
		fmt.Printf("  %s %-*s  %s\n", green("+"), width, a.Name, a.Ou)
	}
	for _, n := range c.Suspend {
		fmt.Printf("  %s %-*s  moved to Suspended (never closed; close it in the console if you mean to)\n", yellow("~"), width, n)
	}
	fmt.Println()
	switch n := len(c.Create); {
	case n == 1:
		fmt.Printf("  1 new account is permanent: its email can never be reused,\n  and closing it takes 90 days.\n\n")
	case n > 1:
		fmt.Printf("  %d new accounts are permanent: their emails can never be reused,\n  and closing an account takes 90 days.\n\n", n)
	}
}

// confirmAccountChanges is the hard stop before deploy creates or suspends
// accounts: type "yes" in a terminal; without one, the matching
// --yes-create-accounts / --yes-suspend-accounts flag. --yes elsewhere never
// covers it.
func confirmAccountChanges(c orgAccountChanges) error {
	needCreate := len(c.Create) > 0 && !orgYesCreateAccounts
	needSuspend := len(c.Suspend) > 0 && !orgYesSuspendAccounts
	if !needCreate && !needSuspend {
		return nil
	}
	if !isTTY() {
		var flags []string
		if needCreate {
			flags = append(flags, "--yes-create-accounts")
		}
		if needSuspend {
			flags = append(flags, "--yes-suspend-accounts")
		}
		return fmt.Errorf("This deploy changes accounts permanently (listed above), and there's no terminal to confirm.\n"+
			"  Pass %s to approve it.", strings.Join(flags, " and "))
	}
	var what []string
	if needCreate {
		what = append(what, pick(len(c.Create) == 1, "create it", "create them"))
	}
	if needSuspend {
		what = append(what, pick(len(c.Suspend) == 1, "suspend the removed account", "suspend the removed accounts"))
	}
	fmt.Printf("  Type \"yes\" to %s: ", strings.Join(what, " and "))
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		return fmt.Errorf("cancelled — nothing was changed")
	}
	fmt.Println()
	return nil
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// orgAccountCount is what counts against the org's account quota.
type orgAccountCount struct {
	InOrg       int // every account the org lists: management, active, suspended, pending closure
	Suspended   int
	Invitations int // open invitations count too
	Quota       int
}

// checkAccountQuota fails when this deploy would go over the account quota,
// and warns when it ends within one of it. Tell only: Anvil never requests an
// increase. Skipped with a warning if the quota can't be read.
func checkAccountQuota(ctx context.Context, cfg aws.Config, newAccounts int) error {
	if newAccounts == 0 {
		return nil
	}
	count, err := readAccountCount(ctx, cfg)
	if err != nil {
		printWarn(fmt.Sprintf("Couldn't check the account quota (%v); creating anyway. AWS refuses accounts over the quota.", err))
		return nil
	}
	return quotaVerdict(count, newAccounts)
}

func quotaVerdict(c orgAccountCount, newAccounts int) error {
	total := c.InOrg + c.Invitations + newAccounts
	fmt.Printf("  Accounts   in org %d (incl. management, %d suspended, %d invitations) + %d new = %d\n",
		c.InOrg, c.Suspended, c.Invitations, newAccounts, total)
	fmt.Printf("             quota %d\n", c.Quota)
	if total > c.Quota {
		return fmt.Errorf("This deploy needs %d accounts but the org's quota is %d.\n"+
			"  Request an increase to at least %d in the management account, then deploy:\n"+
			"  Service Quotas → AWS Organizations → Default maximum number of accounts (us-east-1)", total, c.Quota, total)
	}
	if total >= c.Quota-1 {
		printWarn(fmt.Sprintf("Accounts %d of %d after this deploy. Request a quota increase before adding more.", total, c.Quota))
	}
	fmt.Println()
	return nil
}

func readAccountCount(ctx context.Context, cfg aws.Config) (orgAccountCount, error) {
	var c orgAccountCount
	org := organizations.NewFromConfig(cfg)
	pages := organizations.NewListAccountsPaginator(org, &organizations.ListAccountsInput{})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return c, fmt.Errorf("listing accounts: %w", err)
		}
		for _, a := range page.Accounts {
			c.InOrg++
			if a.Status != orgtypes.AccountStatusActive {
				c.Suspended++
			}
		}
	}
	hs := organizations.NewListHandshakesForOrganizationPaginator(org, &organizations.ListHandshakesForOrganizationInput{
		Filter: &orgtypes.HandshakeFilter{ActionType: orgtypes.ActionTypeInviteAccountToOrganization},
	})
	for hs.HasMorePages() {
		page, err := hs.NextPage(ctx)
		if err != nil {
			return c, fmt.Errorf("listing invitations: %w", err)
		}
		for _, h := range page.Handshakes {
			if h.State == orgtypes.HandshakeStateOpen || h.State == orgtypes.HandshakeStateRequested {
				c.Invitations++
			}
		}
	}
	quota, err := readAccountQuota(ctx, cfg)
	if err != nil {
		return c, err
	}
	c.Quota = quota
	return c, nil
}

// readAccountQuota reads Organizations' account quota (a global quota, read
// in us-east-1): the applied value if the org has one, else AWS's default.
// Found by name, not code, so a renamed code doesn't break it.
func readAccountQuota(ctx context.Context, cfg aws.Config) (int, error) {
	east := cfg.Copy()
	east.Region = "us-east-1"
	sq := servicequotas.NewFromConfig(east)
	match := func(name *string) bool {
		return strings.Contains(strings.ToLower(aws.ToString(name)), "number of accounts")
	}

	applied := servicequotas.NewListServiceQuotasPaginator(sq, &servicequotas.ListServiceQuotasInput{ServiceCode: aws.String("organizations")})
	for applied.HasMorePages() {
		page, err := applied.NextPage(ctx)
		if err != nil {
			break // fall back to the default
		}
		for _, q := range page.Quotas {
			if match(q.QuotaName) && q.Value != nil {
				return int(*q.Value), nil
			}
		}
	}
	defaults := servicequotas.NewListAWSDefaultServiceQuotasPaginator(sq, &servicequotas.ListAWSDefaultServiceQuotasInput{ServiceCode: aws.String("organizations")})
	for defaults.HasMorePages() {
		page, err := defaults.NextPage(ctx)
		if err != nil {
			return 0, fmt.Errorf("reading the account quota: %w", err)
		}
		for _, q := range page.Quotas {
			if match(q.QuotaName) && q.Value != nil {
				return int(*q.Value), nil
			}
		}
	}
	return 0, errors.New("Service Quotas has no account quota for AWS Organizations")
}
