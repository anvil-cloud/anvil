package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func joined(ms []string) string { return strings.Join(ms, "\n") }

func TestParseOrgState(t *testing.T) {
	dep := json.RawMessage(`{"resources":[
	  {"urn":"urn:pulumi:org::p::anvil:org:OrganizationalUnit::Workloads/Prod","type":"anvil:org:OrganizationalUnit"},
	  {"urn":"urn:pulumi:org::p::anvil:org:OrganizationalUnit$aws:organizations/organizationalUnit:OrganizationalUnit::ou-Workloads/Prod","type":"aws:organizations/organizationalUnit:OrganizationalUnit"},
	  {"urn":"urn:pulumi:org::p::anvil:aws:OrgImportedAccount::curiosity-unlocked","type":"anvil:aws:OrgImportedAccount","outputs":{"accountId":"669591255121"}}
	]}`)
	st, err := parseOrgState(dep)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OUs["Workloads/Prod"] || len(st.OUs) != 1 {
		t.Errorf("OUs = %v", st.OUs)
	}
	if st.Accounts["curiosity-unlocked"] != "669591255121" {
		t.Errorf("Accounts = %v", st.Accounts)
	}
	if st, err := parseOrgState(nil); err != nil || len(st.OUs) != 0 {
		t.Errorf("empty state: %v %v", st, err)
	}
}

func TestCheckOrgRenames_Accounts(t *testing.T) {
	st := orgState{OUs: map[string]bool{}, Accounts: map[string]string{"curiosity-unlocked": "669591255121"}}

	// Forgot renamedFrom: same account ID under a new name.
	r := checkOrgRenames(&orgPlan{Accounts: []orgPlanAccount{{Name: "cu-prod", AccountID: "669591255121"}}}, st)
	if !strings.Contains(joined(r.Errors), "add renamedFrom: 'curiosity-unlocked' to 'cu-prod'") {
		t.Errorf("errors = %v", r.Errors)
	}

	// Proper rename.
	r = checkOrgRenames(&orgPlan{Accounts: []orgPlanAccount{{Name: "cu-prod", AccountID: "669591255121", RenamedFrom: "curiosity-unlocked"}}}, st)
	if len(r.Errors) != 0 || !strings.Contains(joined(r.Info), "Renaming account curiosity-unlocked → cu-prod") {
		t.Errorf("report = %+v", r)
	}

	// renamedFrom pointing at a different account.
	r = checkOrgRenames(&orgPlan{Accounts: []orgPlanAccount{{Name: "x", AccountID: "111111111111", RenamedFrom: "curiosity-unlocked"}}}, st)
	if !strings.Contains(joined(r.Errors), "only renames the same account") {
		t.Errorf("errors = %v", r.Errors)
	}

	// Already renamed: suggest removing renamedFrom.
	done := orgState{OUs: map[string]bool{}, Accounts: map[string]string{"cu-prod": "669591255121"}}
	r = checkOrgRenames(&orgPlan{Accounts: []orgPlanAccount{{Name: "cu-prod", AccountID: "669591255121", RenamedFrom: "curiosity-unlocked"}}}, done)
	if !strings.Contains(joined(r.Info), "already renamed; remove renamedFrom") {
		t.Errorf("info = %v", r.Info)
	}

	// Unchanged account: nothing to say.
	r = checkOrgRenames(&orgPlan{Accounts: []orgPlanAccount{{Name: "curiosity-unlocked", AccountID: "669591255121"}}}, st)
	if len(r.Info)+len(r.Warn)+len(r.Errors) != 0 {
		t.Errorf("report = %+v", r)
	}
}

func TestCheckOrgRenames_OUs(t *testing.T) {
	st := orgState{OUs: map[string]bool{"Workloads": true, "Workloads/Prod": true, "Data": true}, Accounts: map[string]string{}}

	// Renamed parent: one note for the parent, none for the child that follows it.
	r := checkOrgRenames(&orgPlan{OUs: []orgPlanOu{
		{Path: "Apps", PreviousPath: "Workloads"},
		{Path: "Apps/Prod", PreviousPath: "Workloads/Prod"},
		{Path: "Data"},
	}}, st)
	if len(r.Info) != 1 || !strings.Contains(r.Info[0], "Renaming OU Workloads → Apps") || len(r.Warn) != 0 {
		t.Errorf("report = %+v", r)
	}

	// Forgot renamedFrom on an OU: warn, don't fail.
	r = checkOrgRenames(&orgPlan{OUs: []orgPlanOu{{Path: "Workloads"}, {Path: "Workloads/Prod"}, {Path: "Analytics"}}}, st)
	if !strings.Contains(joined(r.Warn), "If this is a rename, add renamedFrom: 'Data' to Analytics") || len(r.Errors) != 0 {
		t.Errorf("report = %+v", r)
	}
}
