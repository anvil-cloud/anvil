package cmd

import (
	"strings"
	"testing"
)

func createdState() orgState {
	return orgState{
		OUs:       map[string]bool{"Prod": true, "Suspended": true},
		Accounts:  map[string]string{"payments-prod": "111111111111", "legacy": "669591255121"},
		Created:   map[string]bool{"payments-prod": true, "legacy": false},
		AccountOu: map[string]string{"payments-prod": "Prod", "legacy": "Prod"},
	}
}

func TestPlanAccountChanges(t *testing.T) {
	plan := &orgPlan{Accounts: []orgPlanAccount{
		{Name: "payments-prod", Ou: "Prod", Kind: "create"},
		{Name: "analytics-prod", Ou: "Prod", Kind: "create"},
		{Name: "legacy", Ou: "Prod", Kind: "import", AccountID: "669591255121"},
	}}
	c := planAccountChanges(plan, createdState())
	if len(c.Create) != 1 || c.Create[0].Name != "analytics-prod" || len(c.Suspend) != 0 {
		t.Errorf("changes = %+v", c)
	}

	// Removing a created account suspends it; removing an import doesn't.
	c = planAccountChanges(&orgPlan{}, createdState())
	if len(c.Suspend) != 1 || c.Suspend[0] != "payments-prod" {
		t.Errorf("changes = %+v", c)
	}

	// A rename is neither a create nor a suspend.
	c = planAccountChanges(&orgPlan{Accounts: []orgPlanAccount{
		{Name: "billing-prod", Ou: "Prod", Kind: "create", RenamedFrom: "payments-prod"},
	}}, createdState())
	if len(c.Create)+len(c.Suspend) != 0 {
		t.Errorf("changes = %+v", c)
	}
}

func TestCheckOrgRenames_CreatedAccountForgotRenamedFrom(t *testing.T) {
	r := checkOrgRenames(&orgPlan{Accounts: []orgPlanAccount{
		{Name: "billing-prod", Ou: "Prod", Kind: "create"},
		{Name: "legacy", Ou: "Prod", Kind: "import", AccountID: "669591255121"},
	}}, createdState())
	if !strings.Contains(joined(r.Errors), "add renamedFrom: 'payments-prod' to 'billing-prod'") {
		t.Errorf("errors = %v", r.Errors)
	}

	// In a different OU it's a new account plus a removal, not a rename.
	r = checkOrgRenames(&orgPlan{Accounts: []orgPlanAccount{{Name: "billing-dev", Ou: "NonProd", Kind: "create"}}}, createdState())
	if len(r.Errors) != 0 {
		t.Errorf("errors = %v", r.Errors)
	}

	// renamedFrom across created and imported is refused.
	r = checkOrgRenames(&orgPlan{Accounts: []orgPlanAccount{{Name: "x", Ou: "Prod", Kind: "create", RenamedFrom: "legacy"}}}, createdState())
	if !strings.Contains(joined(r.Errors), "one is created by Org and the other imported") {
		t.Errorf("errors = %v", r.Errors)
	}
}

func TestQuotaVerdict(t *testing.T) {
	if err := quotaVerdict(orgAccountCount{InOrg: 8, Suspended: 1, Quota: 10}, 4); err == nil ||
		!strings.Contains(err.Error(), "needs 12 accounts but the org's quota is 10") {
		t.Errorf("over quota: %v", err)
	}
	if err := quotaVerdict(orgAccountCount{InOrg: 8, Quota: 10}, 1); err != nil {
		t.Errorf("9 of 10 should warn, not fail: %v", err)
	}
	if err := quotaVerdict(orgAccountCount{InOrg: 7, Invitations: 2, Quota: 10}, 2); err == nil {
		t.Error("invitations count toward the quota")
	}
}

func TestConfirmAccountChanges_NoTerminalNeedsFlags(t *testing.T) {
	t.Cleanup(func() { orgYesCreateAccounts, orgYesSuspendAccounts = false, false })
	c := orgAccountChanges{Create: []orgPlanAccount{{Name: "a"}}, Suspend: []string{"b"}}
	err := confirmAccountChanges(c)
	if err == nil || !strings.Contains(err.Error(), "--yes-create-accounts and --yes-suspend-accounts") {
		t.Errorf("got %v", err)
	}
	orgYesCreateAccounts, orgYesSuspendAccounts = true, true
	if err := confirmAccountChanges(c); err != nil {
		t.Errorf("flags should approve: %v", err)
	}
	if err := confirmAccountChanges(orgAccountChanges{}); err != nil {
		t.Errorf("nothing to approve: %v", err)
	}
}
