package cmd

// anvil org init: asks which OU layout to start from, explains what each OU
// is for, and scaffolds an Org project (TypeScript for now): the entry point
// with every OU written out, package.json, tsconfig.json, anvil.org.yaml and
// .gitignore. Nothing is created in AWS until anvil org deploy.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
)

var (
	orgInitName          string
	orgInitRegion        string
	orgInitSDKPath       string
	orgInitTier          string
	orgInitSandbox       bool
	orgInitPolicyStaging bool
)

var orgInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a new Org project in the current directory",
	Long: `Creates an Org project (TypeScript): anvil.config.ts with new anvil.Org({...}),
anvil.org.yaml, package.json, tsconfig.json and .gitignore.

Init asks which layout to start from and writes every OU out in
anvil.config.ts, each with a comment saying what it's for. The file is the
whole truth: edit it freely afterwards. Nothing is created in AWS until
anvil org deploy.

Tiers (--tier):
  minimum  Security, Workloads (Prod, NonProd), Suspended
  network  minimum + Infrastructure
  sra      network + Deployments
Plus Sandbox (on by default, --sandbox=false) and Policy Staging (--policy-staging).

Create the organization in the AWS console first (all features enabled);
Org adopts it and never creates or deletes it.`,
	RunE: runOrgInit,
}

func init() {
	orgInitCmd.Flags().StringVar(&orgInitName, "name", "", "Project name (defaults to the current directory name)")
	orgInitCmd.Flags().StringVar(&orgInitRegion, "region", "", "Home region (default: set on the first preview or deploy)")
	orgInitCmd.Flags().StringVar(&orgInitSDKPath, "sdk-path", "", "Use a local @anvil-cloud/sdk checkout instead of the published package")
	orgInitCmd.Flags().StringVar(&orgInitTier, "tier", "", "Starting layout: minimum, network or sra (asked if not set)")
	orgInitCmd.Flags().BoolVar(&orgInitSandbox, "sandbox", true, "Include the Sandbox OU (asked if not set)")
	orgInitCmd.Flags().BoolVar(&orgInitPolicyStaging, "policy-staging", false, "Include the Policy Staging OU (asked if not set)")
	orgCmd.AddCommand(orgInitCmd)
}

func runOrgInit(cmd *cobra.Command, args []string) error {
	if _, err := os.Stat("anvil.yaml"); err == nil {
		return fmt.Errorf("This directory is an App project (anvil.yaml). Create the Org project in its own directory.")
	}
	if _, err := os.Stat(orgConfigFile); err == nil {
		return fmt.Errorf("%s already exists: this is already an Org project.", orgConfigFile)
	}
	for _, f := range []string{"anvil.config.ts", "anvil.config.py", "anvil.config.go"} {
		if _, err := os.Stat(f); err == nil {
			return fmt.Errorf("%s already exists. Run anvil org init in an empty directory.", f)
		}
	}

	printBanner()

	reader := bufio.NewReader(os.Stdin)
	project, err := orgInitProjectName(reader)
	if err != nil {
		return err
	}
	choice, err := orgInitLayout(cmd, reader)
	if err != nil {
		return err
	}
	ous, err := orgLayoutOus(choice)
	if err != nil {
		return err
	}

	sdkDep := `"latest"`
	if orgInitSDKPath != "" {
		abs, err := filepath.Abs(orgInitSDKPath)
		if err != nil {
			return fmt.Errorf("invalid --sdk-path: %w", err)
		}
		if _, err := os.Stat(filepath.Join(abs, "package.json")); err != nil {
			return fmt.Errorf("--sdk-path %s has no package.json; point it at sdk/nodejs in your Anvil checkout.", abs)
		}
		sdkDep = fmt.Sprintf(`"file:%s"`, abs)
	}

	files := []struct{ name, content string }{
		{"anvil.config.ts", orgEntryPoint(ous)},
		{"package.json", orgPackageJSON(project, sdkDep)},
		{"tsconfig.json", tsConfig()},
	}
	for _, f := range files {
		if err := os.WriteFile(f.name, []byte(f.content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", f.name, err)
		}
		printCheck("Created " + f.name)
	}

	cfg := &orgFileConfig{Project: project, Region: orgInitRegion}
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}
	if err := writeOrgConfig(root, cfg); err != nil {
		return fmt.Errorf("failed to write %s: %w", orgConfigFile, err)
	}
	printCheck("Created " + orgConfigFile)

	if _, err := os.Stat(".gitignore"); os.IsNotExist(err) {
		if err := os.WriteFile(".gitignore", []byte(gitignoreContent("ts")), 0644); err != nil {
			return fmt.Errorf("failed to write .gitignore: %w", err)
		}
		printCheck("Created .gitignore")
	}

	fmt.Println()
	fmt.Println("  anvil.config.ts lists these OUs. Org creates them on deploy:")
	fmt.Println()
	for _, line := range orgLayoutTree(ous) {
		fmt.Println("    " + line)
	}
	fmt.Println()
	fmt.Println("  Next, with management-account credentials:")
	fmt.Println("    npm install")
	if orgInitRegion == "" {
		fmt.Println("    anvil org preview --region <home region>")
	} else {
		fmt.Println("    anvil org preview")
	}
	fmt.Println()
	hint := "  The first preview creates the Org state bucket in the management account."
	if isTTY() {
		hint = dim(hint)
	}
	fmt.Println(hint)
	return nil
}

func orgInitProjectName(reader *bufio.Reader) (string, error) {
	name := orgInitName
	if name == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get working directory: %w", err)
		}
		name = filepath.Base(cwd)
		if isTTY() {
			fmt.Printf("  Project name (%s): ", name)
			input, _ := reader.ReadString('\n')
			if input = strings.TrimSpace(input); input != "" {
				name = input
			}
		}
	}
	return strings.ReplaceAll(name, " ", "-"), nil
}

func orgPackageJSON(project, sdkDep string) string {
	return fmt.Sprintf(`{
  "name": "%s",
  "version": "0.0.1",
  "main": "anvil.config.ts",
  "dependencies": {
    "@anvil-cloud/sdk": %s,
    "@pulumi/pulumi": "^3.0.0",
    "@pulumi/aws": "^7.21.0"
  },
  "devDependencies": {
    "typescript": "^5.0.0",
    "@types/node": "^20.0.0"
  }
}
`, strings.ToLower(project), sdkDep)
}

// orgInitLayout asks for the tier, Sandbox and Policy Staging, explaining
// each, unless flags set them. Without a terminal, unset choices take their
// defaults (minimum, Sandbox on, Policy Staging off).
func orgInitLayout(cmd *cobra.Command, reader *bufio.Reader) (orgLayoutChoice, error) {
	c := orgLayoutChoice{Tier: orgInitTier, Sandbox: orgInitSandbox, PolicyStaging: orgInitPolicyStaging}
	ask := isTTY()

	if c.Tier == "" {
		c.Tier = "minimum"
		if ask {
			fmt.Println()
			fmt.Println("  Every tier writes these OUs into anvil.config.ts:")
			for _, ou := range orgOuCatalog {
				if slices.Contains(orgCoreOus, ou.Path) {
					fmt.Printf("    %-18s %s\n", ou.Path, dim(ou.Why))
				}
			}
			fmt.Println()
			prompt := promptui.Select{
				Label: "Tier",
				Items: orgTiers,
				Templates: &promptui.SelectTemplates{
					Label:    "  {{ . }}",
					Active:   "  {{ \"▸\" | cyan }} {{ .Name | bold }}  {{ .Summary | faint }}",
					Inactive: "    {{ .Name }}  {{ .Summary | faint }}",
					Selected: "  {{ \"✔\" | green }} Tier {{ .Name }}",
				},
				Size: len(orgTiers),
			}
			idx, _, err := prompt.Run()
			if err != nil {
				return c, fmt.Errorf("tier selection cancelled")
			}
			c.Tier = orgTiers[idx].Name
		}
	}
	if _, err := findOrgTier(c.Tier); err != nil {
		return c, err
	}

	if ask && !flagChanged(cmd, "sandbox") {
		c.Sandbox = askYesNo(reader, "Sandbox OU", whyOu("Sandbox"), true)
	}
	if ask && !flagChanged(cmd, "policy-staging") {
		c.PolicyStaging = askYesNo(reader, "Policy Staging OU", whyOu("PolicyStaging"), false)
	}
	return c, nil
}

func whyOu(path string) string {
	for _, ou := range orgOuCatalog {
		if ou.Path == path {
			return ou.Why
		}
	}
	return ""
}

func askYesNo(reader *bufio.Reader, label, why string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Printf("\n  %s\n  %s? (%s): ", dim(why+"."), label, hint)
	input, _ := reader.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

func flagChanged(cmd *cobra.Command, name string) bool {
	return cmd != nil && cmd.Flags().Changed(name)
}
