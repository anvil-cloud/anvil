package cmd

// Rename checks for anvil org preview/deploy. Accounts and OUs are tracked by
// name, so renaming a key without renamedFrom would release an account and
// import it again (or delete an OU and create a new one). These checks compare
// what discovery planned with what's in state, before Pulumi runs, and say
// exactly what to add. Design: docs/design/org/05-accounts.md (Renames).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

// orgPlan is what the SDK's discovery run wrote (sdk/.../org/discovery.ts).
type orgPlan struct {
	OUs      []orgPlanOu      `json:"ous"`
	Accounts []orgPlanAccount `json:"accounts"`
}

type orgPlanOu struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previousPath,omitempty"`
}

type orgPlanAccount struct {
	Name        string `json:"name"`
	Ou          string `json:"ou"`
	Kind        string `json:"kind"` // "create" or "import"
	AccountID   string `json:"accountId,omitempty"`
	RenamedFrom string `json:"renamedFrom,omitempty"`
}

const (
	orgOuType              = "anvil:org:OrganizationalUnit"
	orgAwsOuType           = "aws:organizations/organizationalUnit:OrganizationalUnit"
	orgImportedAccountType = "anvil:aws:OrgImportedAccount"
	orgCreatedAccountType  = "anvil:aws:OrgAccount"
)

func readOrgPlan(settingsPath string) (*orgPlan, error) {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read discovery settings: %w", err)
	}
	var p orgPlan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("invalid discovery settings: %w", err)
	}
	return &p, nil
}

// orgState is what's in state today: OU paths, and accounts by name.
type orgState struct {
	OUs       map[string]bool
	Accounts  map[string]string // account ID by name
	Created   map[string]bool   // names of accounts Org created
	AccountOu map[string]string // OU path by account name
}

// parseOrgState reads OUs and imported accounts out of an exported deployment.
func parseOrgState(deployment json.RawMessage) (orgState, error) {
	st := orgState{OUs: map[string]bool{}, Accounts: map[string]string{}, Created: map[string]bool{}, AccountOu: map[string]string{}}
	if len(deployment) == 0 {
		return st, nil
	}
	var d struct {
		Resources []struct {
			URN     string                 `json:"urn"`
			Type    string                 `json:"type"`
			Inputs  map[string]interface{} `json:"inputs"`
			Outputs map[string]interface{} `json:"outputs"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(deployment, &d); err != nil {
		return st, fmt.Errorf("reading state: %w", err)
	}
	pathByOuID := map[string]string{}
	ouIDByAccount := map[string]string{}
	for _, r := range d.Resources {
		name := r.URN[strings.LastIndex(r.URN, "::")+2:]
		switch r.Type {
		case orgOuType:
			st.OUs[name] = true
		case orgAwsOuType:
			if id, _ := r.Outputs["id"].(string); id != "" {
				pathByOuID[id] = strings.TrimPrefix(name, "ou-")
			}
		case orgImportedAccountType, orgCreatedAccountType:
			id, _ := r.Outputs["accountId"].(string)
			if id == "" {
				id, _ = r.Inputs["accountId"].(string)
			}
			st.Accounts[name] = id
			st.Created[name] = r.Type == orgCreatedAccountType
			ouIDByAccount[name], _ = r.Inputs["ouId"].(string)
		}
	}
	for name, ouID := range ouIDByAccount {
		st.AccountOu[name] = pathByOuID[ouID]
	}
	return st, nil
}

// orgRenameReport is what checkOrgRenames found: notes to print, warnings,
// and errors that stop the run.
type orgRenameReport struct {
	Info   []string
	Warn   []string
	Errors []string
}

// checkOrgRenames compares the plan with state.
func checkOrgRenames(plan *orgPlan, st orgState) orgRenameReport {
	var r orgRenameReport

	// ── Accounts ──
	nameByID := map[string]string{}
	for name, id := range st.Accounts {
		nameByID[id] = name
	}
	for _, a := range plan.Accounts {
		if a.RenamedFrom != "" {
			oldID, oldInState := st.Accounts[a.RenamedFrom]
			_, newInState := st.Accounts[a.Name]
			switch {
			case oldInState && st.Created[a.RenamedFrom] != (a.Kind == "create"):
				r.Errors = append(r.Errors, fmt.Sprintf(
					"Account '%s' is renamedFrom '%s', but one is created by Org and the other imported. "+
						"renamedFrom only renames; keep '%s' as it was written (with or without import).",
					a.Name, a.RenamedFrom, a.Name))
			case oldInState && a.Kind == "create":
				r.Info = append(r.Info, fmt.Sprintf(
					"Renaming account %s → %s (%s): same account, renamed in AWS too. Remove renamedFrom after this deploy.",
					a.RenamedFrom, a.Name, oldID))
			case oldInState && oldID != a.AccountID:
				r.Errors = append(r.Errors, fmt.Sprintf(
					"Account '%s' is renamedFrom '%s', but '%s' is account %s and '%s' imports %s. "+
						"renamedFrom only renames the same account; drop it.",
					a.Name, a.RenamedFrom, a.RenamedFrom, oldID, a.Name, a.AccountID))
			case oldInState:
				r.Info = append(r.Info, fmt.Sprintf(
					"Renaming account %s → %s (%s): same account, nothing re-imported. Remove renamedFrom after this deploy.",
					a.RenamedFrom, a.Name, a.AccountID))
			case newInState:
				r.Info = append(r.Info, fmt.Sprintf(
					"Account '%s' is already renamed; remove renamedFrom: '%s'.", a.Name, a.RenamedFrom))
			default:
				r.Warn = append(r.Warn, fmt.Sprintf(
					"Account '%s': renamedFrom '%s' matches no account in state, so it's imported as new. "+
						"Check the old name, or drop renamedFrom.", a.Name, a.RenamedFrom))
			}
			continue
		}
		if _, ok := st.Accounts[a.Name]; ok {
			continue
		}
		if a.Kind == "create" {
			// A created account removed and another created in the same OU
			// is most likely a rename; creating is permanent, so stop.
			for old := range st.Created {
				if st.Created[old] && !orgAccountWritten(plan, old) && st.AccountOu[old] == a.Ou {
					r.Errors = append(r.Errors, fmt.Sprintf(
						"'%s' is removed and '%s' added in %s. If this is a rename, add renamedFrom: '%s' to '%s'.\n"+
							"    If you really mean to suspend %s and create a new account, deploy the removal first, then add %s.",
						old, a.Name, a.Ou, old, a.Name, old, a.Name))
				}
			}
			continue
		}
		if old, ok := nameByID[a.AccountID]; ok && old != a.Name {
			r.Errors = append(r.Errors, fmt.Sprintf(
				"Account %s is in state as '%s' but written as '%s'. Without renamedFrom it would be released and imported again.\n"+
					"    If this is a rename, add renamedFrom: '%s' to '%s'.",
				a.AccountID, old, a.Name, old, a.Name))
		}
	}

	// ── OUs ──
	planned := map[string]bool{}
	previous := map[string]bool{}
	var added []string
	for _, o := range plan.OUs {
		planned[o.Path] = true
		if o.PreviousPath == "" {
			if !st.OUs[o.Path] {
				added = append(added, o.Path)
			}
			continue
		}
		previous[o.PreviousPath] = true
		if orgOuName(o.PreviousPath) == orgOuName(o.Path) {
			continue // moved along with a renamed parent; the parent's note covers it
		}
		switch {
		case st.OUs[o.PreviousPath]:
			r.Info = append(r.Info, fmt.Sprintf(
				"Renaming OU %s → %s: same OU, accounts stay. Remove renamedFrom after this deploy.", o.PreviousPath, o.Path))
		case st.OUs[o.Path]:
			r.Info = append(r.Info, fmt.Sprintf(
				"OU '%s' is already renamed; remove renamedFrom: '%s'.", o.Path, orgOuName(o.PreviousPath)))
		default:
			r.Warn = append(r.Warn, fmt.Sprintf(
				"OU '%s': renamedFrom '%s' matches no OU in state, so it's created as new. Check the old name, or drop renamedFrom.",
				o.Path, orgOuName(o.PreviousPath)))
		}
	}
	var removed []string
	for p := range st.OUs {
		if !planned[p] && !previous[p] {
			removed = append(removed, p)
		}
	}
	sort.Strings(removed)
	for _, old := range removed {
		for _, nw := range added {
			if orgOuParent(old) == orgOuParent(nw) {
				r.Warn = append(r.Warn, fmt.Sprintf(
					"OU '%s' is removed and '%s' added %s. If this is a rename, add renamedFrom: '%s' to %s; "+
						"otherwise %s is deleted and %s created as a new OU.",
					old, nw, orgOuWhere(nw), orgOuName(old), orgOuName(nw), old, nw))
			}
		}
	}
	return r
}

// orgAccountWritten reports whether name is written, or is some account's renamedFrom.
func orgAccountWritten(plan *orgPlan, name string) bool {
	for _, a := range plan.Accounts {
		if a.Name == name || a.RenamedFrom == name {
			return true
		}
	}
	return false
}

func orgOuParent(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[:i]
	}
	return ""
}

func orgOuWhere(path string) string {
	if p := orgOuParent(path); p != "" {
		return "in " + p
	}
	return "under Root"
}

// reportOrgRenames checks the plan against the stack's state and prints what
// it found. Errors stop the run before anything changes.
func reportOrgRenames(ctx context.Context, s auto.Stack, plan *orgPlan) (orgState, error) {
	dep, err := s.Export(ctx)
	if err != nil {
		return orgState{}, fmt.Errorf("reading state to check renames: %w", err)
	}
	st, err := parseOrgState(dep.Deployment)
	if err != nil {
		return st, err
	}
	r := checkOrgRenames(plan, st)
	for _, m := range r.Info {
		fmt.Printf("  ℹ %s\n", m)
	}
	for _, m := range r.Warn {
		printWarn(m)
	}
	if len(r.Errors) > 0 {
		return st, fmt.Errorf("%s", strings.Join(r.Errors, "\n  "))
	}
	if len(r.Info)+len(r.Warn) > 0 {
		fmt.Println()
	}
	return st, nil
}
