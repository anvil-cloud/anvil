package cmd

// The OU layouts anvil org init offers. Init writes the chosen layout out in
// full in anvil.config.ts, one line per OU with a comment saying what it's
// for; the SDK implies nothing, so what's in the file is exactly what Org
// manages.

import (
	"fmt"
	"regexp"
	"strings"
)

// orgOu is one OU init can write, with the reason it exists.
type orgOu struct {
	Path string
	Why  string
}

// orgTier is a starting layout. Tiers only exist in init: the config never
// names one.
type orgTier struct {
	Name    string
	Summary string
	Adds    []string // OU paths beyond the core ones
}

// orgOuCatalog lists every OU init can write, in the order they're written
// (parents before children).
var orgOuCatalog = []orgOu{
	{"Security", "Org's core accounts: log archive, audit and emergency access"},
	{"Infrastructure", "Shared networking, directory, tooling and backup accounts"},
	{"Deployments", "CI/CD accounts that deploy into your workloads"},
	{"Workloads", "Parent of Prod and NonProd; guardrails here apply to both"},
	{"Workloads/Prod", "Production workload accounts; the strictest guardrails"},
	{"Workloads/NonProd", "Dev, test and staging workload accounts"},
	{"Sandbox", "Personal accounts for developers to experiment in, away from production"},
	{"PolicyStaging", "A throwaway account for testing guardrail changes before they reach real OUs"},
	{"Suspended", "Accounts waiting to be closed; guardrails will deny everything here"},
}

// orgExampleAccountOu gets a commented-out account import example.
const orgExampleAccountOu = "Workloads/Prod"

// orgCoreOus are written by every tier.
var orgCoreOus = []string{"Security", "Workloads", "Workloads/Prod", "Workloads/NonProd", "Suspended"}

var orgTiers = []orgTier{
	{"minimum", "Security, Workloads (Prod, NonProd) and Suspended. Right for most orgs", nil},
	{"network", "minimum + Infrastructure, for shared networking (Transit Gateway, egress, DNS)", []string{"Infrastructure"}},
	{"sra", "network + Deployments, AWS's full Security Reference Architecture layout", []string{"Infrastructure", "Deployments"}},
}

// orgLayoutChoice is what the user picked in init.
type orgLayoutChoice struct {
	Tier          string
	Sandbox       bool
	PolicyStaging bool
}

func findOrgTier(name string) (orgTier, error) {
	for _, t := range orgTiers {
		if t.Name == strings.ToLower(name) {
			return t, nil
		}
	}
	names := make([]string, len(orgTiers))
	for i, t := range orgTiers {
		names[i] = t.Name
	}
	return orgTier{}, fmt.Errorf("Unknown tier %q. Options: %s.", name, strings.Join(names, ", "))
}

// orgLayoutOus returns the OUs for a choice, in catalog order.
func orgLayoutOus(c orgLayoutChoice) ([]orgOu, error) {
	tier, err := findOrgTier(c.Tier)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, p := range orgCoreOus {
		want[p] = true
	}
	for _, p := range tier.Adds {
		want[p] = true
	}
	want["Sandbox"] = c.Sandbox
	want["PolicyStaging"] = c.PolicyStaging

	var out []orgOu
	for _, ou := range orgOuCatalog {
		if want[ou.Path] {
			out = append(out, ou)
		}
	}
	return out, nil
}

var jsIdentifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func tsKey(path string) string {
	if jsIdentifier.MatchString(path) {
		return path
	}
	return "'" + path + "'"
}

// orgEntryPoint writes anvil.config.ts with every OU listed.
func orgEntryPoint(ous []orgOu) string {
	var b strings.Builder
	b.WriteString(`import * as anvil from '@anvil-cloud/sdk';

// Run with management-account credentials: anvil org preview, then anvil org deploy.
export default new anvil.Org({
  // Root email for each account Org creates; {account} becomes its name. Use a
  // shared mailbox, not one person's inbox. Needed once you create an account.
  // email: 'aws+{account}@example.com',

  // Every OU and account Org manages, nested the way they sit in AWS. Only
  // what's written here exists. Each account goes in its OU's accounts;
  // moving its block moves the account, removing it releases the account.
  ou: {
`)
	writeOus(&b, orgOuChildren(ous), "", "    ")
	b.WriteString(`  },

  defaults: {
    tags: {},
  },
});
`)
	return b.String()
}

// orgOuChildren groups OUs by parent path ("" for Root), keeping their order.
func orgOuChildren(ous []orgOu) map[string][]orgOu {
	children := map[string][]orgOu{}
	for _, ou := range ous {
		parent := ""
		if i := strings.LastIndex(ou.Path, "/"); i >= 0 {
			parent = ou.Path[:i]
		}
		children[parent] = append(children[parent], ou)
	}
	return children
}

// writeOus writes one level of the ou map, nesting each OU's children in its
// own ou, with a comment per OU saying what it's for.
func writeOus(b *strings.Builder, children map[string][]orgOu, parent, indent string) {
	for _, ou := range children[parent] {
		fmt.Fprintf(b, "%s// %s.\n", indent, ou.Why)
		key := tsKey(orgOuName(ou.Path))
		if ou.Path == orgExampleAccountOu {
			fmt.Fprintf(b, "%s%s: {\n%s  enable: true,\n", indent, key, indent)
			fmt.Fprintf(b, "%s  // Each account is created by Org (permanent; deploy asks you to type yes),\n", indent)
			fmt.Fprintf(b, "%s  // or brought in with import. Removing one never closes it.\n", indent)
			fmt.Fprintf(b, "%s  // accounts: {\n", indent)
			fmt.Fprintf(b, "%s  //   'my-app-prod': { enable: true },\n", indent)
			fmt.Fprintf(b, "%s  //   'legacy-app': { enable: true, import: { accountId: '123456789012' } },\n", indent)
			fmt.Fprintf(b, "%s  // },\n%s},\n", indent, indent)
			continue
		}
		if len(children[ou.Path]) == 0 {
			fmt.Fprintf(b, "%s%s: { enable: true },\n", indent, key)
			continue
		}
		fmt.Fprintf(b, "%s%s: {\n%s  enable: true,\n%s  ou: {\n", indent, key, indent, indent)
		writeOus(b, children, ou.Path, indent+"    ")
		fmt.Fprintf(b, "%s  },\n%s},\n", indent, indent)
	}
}

// orgLayoutTree draws the OUs under Root with what each is for.
func orgLayoutTree(ous []orgOu) []string {
	children := orgOuChildren(ous)
	width := 0
	var measure func(parent string, depth int)
	measure = func(parent string, depth int) {
		for _, ou := range children[parent] {
			if w := depth*4 + 4 + len(orgOuName(ou.Path)); w > width {
				width = w
			}
			measure(ou.Path, depth+1)
		}
	}
	measure("", 0)

	lines := []string{"Root"}
	var walk func(parent, prefix string)
	walk = func(parent, prefix string) {
		kids := children[parent]
		for i, ou := range kids {
			branch, next := "├── ", "│   "
			if i == len(kids)-1 {
				branch, next = "└── ", "    "
			}
			label := prefix + branch + orgOuName(ou.Path)
			lines = append(lines, fmt.Sprintf("%-*s  %s", width, label, ou.Why))
			walk(ou.Path, prefix+next)
		}
	}
	walk("", "")
	return lines
}

func orgOuName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
