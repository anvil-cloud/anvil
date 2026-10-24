package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
)

var (
	initName   string
	initLang   string
	initCloud  string
	initRegion string
	initStage  string
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a new Anvil project",
	Long:  `Scaffolds a new Anvil project with anvil.yaml, an entry point file, and dependency files for your chosen language.`,
	RunE:  runInit,
}

func init() {
	rootCmd.AddCommand(versionCmd)
	initInitCmd()
}

func initInitCmd() {
	initCmd.Flags().StringVar(&initName, "name", "", "Project name (defaults to current directory name)")
	initCmd.Flags().StringVar(&initLang, "lang", "", "Language: ts, python, or go")
	initCmd.Flags().StringVar(&initCloud, "cloud", "", "Target clouds: aws, gcp, or aws,gcp")
	initCmd.Flags().StringVar(&initRegion, "region", "", "Default region for the primary cloud")
	initCmd.Flags().StringVar(&initStage, "stage", "", "Stage to deploy to (defaults to your OS username)")
	rootCmd.AddCommand(initCmd)
}

func runInit(cmd *cobra.Command, args []string) error {
	reader := bufio.NewReader(os.Stdin)

	// ── Check for existing project ──
	if _, err := os.Stat("anvil.yaml"); err == nil {
		if !isTTY() {
			return fmt.Errorf("anvil.yaml already exists. Use --name to specify a different directory.")
		}

		prompt := promptui.Prompt{
			Label:     "anvil.yaml already exists. Overwrite",
			IsConfirm: true,
		}
		_, err := prompt.Run()
		if err != nil {
			fmt.Println("  Cancelled.")
			return nil
		}
		fmt.Println()
	}

	printBanner()

	// ── Project name ──
	projectName := initName
	if projectName == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
		defaultName := filepath.Base(cwd)

		if isTTY() {
			fmt.Printf("  Project name (%s): ", defaultName)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)
			if input != "" {
				projectName = input
			} else {
				projectName = defaultName
			}
		} else {
			projectName = defaultName
		}
	}
	projectName = strings.ReplaceAll(projectName, " ", "-")

	// ── Language ──
	lang := initLang
	if lang == "" {
		if !isTTY() {
			return fmt.Errorf("--lang is required in non-interactive mode. Options: ts, python, go")
		}

		prompt := promptui.Select{
			Label: "Language",
			Items: []string{"TypeScript", "Python", "Go"},
			Templates: &promptui.SelectTemplates{
				Label:    "  {{ . }}",
				Active:   "  {{ \"▸\" | cyan }} {{ . | bold }}",
				Inactive: "    {{ . }}",
				Selected: "  {{ \"✔\" | green }} {{ . }}",
			},
			Size: 3,
		}

		idx, _, err := prompt.Run()
		if err != nil {
			return fmt.Errorf("language selection cancelled")
		}

		switch idx {
		case 0:
			lang = "ts"
		case 1:
			lang = "python"
		case 2:
			lang = "go"
		}
	}

	// Normalise language input
	switch strings.ToLower(lang) {
	case "ts", "typescript":
		lang = "ts"
	case "python", "py":
		lang = "python"
	case "go", "golang":
		lang = "go"
	default:
		return fmt.Errorf("Unknown language %q. Options: ts, python, go.", lang)
	}

	// ── Clouds ──
	clouds := []string{}
	if initCloud != "" {
		for _, c := range strings.Split(initCloud, ",") {
			c = strings.TrimSpace(strings.ToLower(c))
			if c != "aws" && c != "gcp" {
				return fmt.Errorf("Unknown cloud %q. Options: aws, gcp.", c)
			}
			clouds = append(clouds, c)
		}
	} else {
		if !isTTY() {
			clouds = []string{"aws"}
		} else {
			prompt := promptui.Select{
				Label: "Target clouds",
				Items: []string{"AWS", "GCP", "AWS + GCP"},
				Templates: &promptui.SelectTemplates{
					Label:    "  {{ . }}",
					Active:   "  {{ \"▸\" | cyan }} {{ . | bold }}",
					Inactive: "    {{ . }}",
					Selected: "  {{ \"✔\" | green }} {{ . }}",
				},
				Size: 3,
			}

			idx, _, err := prompt.Run()
			if err != nil {
				return fmt.Errorf("cloud selection cancelled")
			}

			switch idx {
			case 0:
				clouds = []string{"aws"}
			case 1:
				clouds = []string{"gcp"}
			case 2:
				clouds = []string{"aws", "gcp"}
			}
		}
	}

	// ── Region ──
	region := initRegion
	if region == "" && isTTY() {
		defaultReg := cloudDefaultRegion(clouds[0])
		fmt.Printf("\n  Default region (%s): ", defaultReg)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input != "" {
			region = input
		} else {
			region = defaultReg
		}
	}
	if region == "" {
		region = cloudDefaultRegion(clouds[0])
	}

	// ── Stage ──
	stage, err := resolveInitStage(initStage, reader)
	if err != nil {
		return err
	}

	// ── Generate files ──
	fmt.Println()

	// anvil.yaml. The stage becomes the active one, so `anvil deploy` uses it
	// without falling back to the OS username. It's bootstrapped on first deploy.
	err = writeAnvilConfig(anvilConfig{
		Project: projectName,
		Active:  stage,
		Stages:  make(map[string]*stageConfig),
	})
	if err != nil {
		return fmt.Errorf("failed to write anvil.yaml: %w", err)
	}
	printCheck("Created anvil.yaml")

	// Entry point + deps
	switch lang {
	case "ts":
		err = scaffoldTypeScript(projectName, clouds, region)
	case "python":
		err = scaffoldPython(projectName, clouds, region)
	case "go":
		err = scaffoldGo(projectName, clouds, region)
	}
	if err != nil {
		return err
	}

	// ── Install dependencies ──
	switch lang {
	case "ts":
		fmt.Println()
		fmt.Printf("  Installing dependencies...\n\n")
		installCmd := exec.Command("npm", "install")
		installCmd.Stdout = os.Stdout
		installCmd.Stderr = os.Stderr
		if err := installCmd.Run(); err != nil {
			fmt.Printf("  %s npm install failed. Run it manually.\n", yellow("⚠"))
		}
	case "python":
		fmt.Println()
		fmt.Printf("  Installing dependencies...\n\n")
		installCmd := exec.Command("pip", "install", "-r", "requirements.txt")
		installCmd.Stdout = os.Stdout
		installCmd.Stderr = os.Stderr
		if err := installCmd.Run(); err != nil {
			fmt.Printf("  %s pip install failed. Run it manually.\n", yellow("⚠"))
		}
	case "go":
		fmt.Println()
		fmt.Printf("  Installing dependencies...\n\n")
		installCmd := exec.Command("go", "mod", "tidy")
		installCmd.Stdout = os.Stdout
		installCmd.Stderr = os.Stderr
		if err := installCmd.Run(); err != nil {
			fmt.Printf("  %s go mod tidy failed. Run it manually.\n", yellow("⚠"))
		}
	}

	// .gitignore
	if _, err := os.Stat(".gitignore"); os.IsNotExist(err) {
		err = os.WriteFile(".gitignore", []byte(gitignoreContent(lang)), 0644)
		if err == nil {
			printCheck("Created .gitignore")
		}
	}

	fmt.Println()
	next := fmt.Sprintf("  Run `anvil deploy` to deploy stage %q.", stage)
	if isTTY() {
		fmt.Println(dim(next))
	} else {
		fmt.Println(next)
	}

	return nil
}

// defaultInitStage is the stage `anvil init` suggests: the OS username, made
// valid as a stage name, or "dev" when there isn't a usable one.
func defaultInitStage() string {
	if u, err := user.Current(); err == nil {
		if s := sanitizeBucketPart(u.Username); validStageName.MatchString(s) {
			return s
		}
	}
	return "dev"
}

// resolveInitStage returns the --stage flag if set, otherwise asks for the
// stage name, suggesting defaultInitStage. Without a terminal it uses the
// suggestion.
func resolveInitStage(flag string, reader *bufio.Reader) (string, error) {
	if flag != "" {
		if !validStageName.MatchString(flag) {
			return "", fmt.Errorf("Invalid stage name %q.\n  Use letters, numbers and hyphens, starting with a letter or number.", flag)
		}
		return flag, nil
	}

	suggested := defaultInitStage()
	if !isTTY() {
		return suggested, nil
	}

	fmt.Println()
	fmt.Println(dim("  A stage is one separate copy of this app: its own resources and state."))
	fmt.Println(dim("  Use your name for a personal stage, or a name like \"staging\" or \"prod\"."))
	for {
		fmt.Printf("  Stage name (%s): ", suggested)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "" {
			return suggested, nil
		}
		if validStageName.MatchString(input) {
			return input, nil
		}
		fmt.Printf("  %s %q isn't a valid stage name. Use letters, numbers and hyphens, starting with a letter or number.\n", yellow("⚠"), input)
	}
}

// ── Scaffolders ──

func scaffoldTypeScript(project string, clouds []string, region string) error {
	err := os.WriteFile("anvil.config.ts", []byte(tsEntryPoint(clouds, region)), 0644)
	if err != nil {
		return fmt.Errorf("failed to write anvil.config.ts: %w", err)
	}
	printCheck("Created anvil.config.ts")

	err = os.WriteFile("package.json", []byte(tsPackageJSON(project, clouds)), 0644)
	if err != nil {
		return fmt.Errorf("failed to write package.json: %w", err)
	}
	printCheck("Created package.json")

	err = os.WriteFile("tsconfig.json", []byte(tsConfig()), 0644)
	if err != nil {
		return fmt.Errorf("failed to write tsconfig.json: %w", err)
	}
	printCheck("Created tsconfig.json")

	return nil
}

func scaffoldPython(project string, clouds []string, region string) error {
	err := os.WriteFile("anvil.config.py", []byte(pyEntryPoint(clouds, region)), 0644)
	if err != nil {
		return fmt.Errorf("failed to write anvil.config.py: %w", err)
	}
	printCheck("Created anvil.config.py")

	err = os.WriteFile("requirements.txt", []byte(pyRequirements(clouds)), 0644)
	if err != nil {
		return fmt.Errorf("failed to write requirements.txt: %w", err)
	}
	printCheck("Created requirements.txt")

	return nil
}

func scaffoldGo(project string, clouds []string, region string) error {
	err := os.WriteFile("anvil.config.go", []byte(goEntryPoint(clouds, region)), 0644)
	if err != nil {
		return fmt.Errorf("failed to write anvil.config.go: %w", err)
	}
	printCheck("Created anvil.config.go")

	modulePath := fmt.Sprintf("github.com/%s", project)
	err = os.WriteFile("go.mod", []byte(goMod(modulePath, goSDKVersion(version, mainModuleVersion(), latestGoSDKVersion))), 0644)
	if err != nil {
		return fmt.Errorf("failed to write go.mod: %w", err)
	}
	printCheck("Created go.mod")

	return nil
}

// ── Templates ──

func tsEntryPoint(clouds []string, region string) string {
	hasAWS := containsCloud(clouds, "aws")
	hasGCP := containsCloud(clouds, "gcp")

	imports := "import { App } from '@anvil-cloud/sdk';\n"
	if hasAWS || hasGCP {
		imports += "import * as anvil from '@anvil-cloud/sdk';\n"
	}

	providers := ""
	if hasAWS {
		providers += fmt.Sprintf("    aws: { region: '%s' },\n", region)
	}
	if hasGCP {
		gcpRegion := region
		if hasAWS {
			gcpRegion = "australia-southeast1"
		}
		providers += fmt.Sprintf("    gcp: { region: '%s' },\n", gcpRegion)
	}

	return fmt.Sprintf(`%s
export default new App({
  defaults: {
    tags: {},
  },
  providers: {
%s  },
  run(ctx) {
    // Your infrastructure goes here

    ctx.export('stage', ctx.stage);
  },
});
`, imports, providers)
}

func tsPackageJSON(project string, clouds []string) string {
	deps := `    "@anvil-cloud/sdk": "latest",
    "@pulumi/pulumi": "^3.0.0"`

	if containsCloud(clouds, "aws") {
		deps += `,
    "@pulumi/aws": "^7.21.0"`
	}
	if containsCloud(clouds, "gcp") {
		deps += `,
    "@pulumi/gcp": "^9.0.0"`
	}

	return fmt.Sprintf(`{
  "name": "%s",
  "version": "0.0.1",
  "main": "anvil.config.ts",
  "dependencies": {
%s
  },
  "devDependencies": {
    "typescript": "^5.0.0",
    "@types/node": "^20.0.0"
  }
}
`, project, deps)
}

func tsConfig() string {
	return `{
  "compilerOptions": {
    "target": "ES2020",
    "module": "nodenext",
    "lib": ["ES2020"],
    "strict": true,
    "esModuleInterop": true,
    "skipLibCheck": true,
    "outDir": "./bin",
    "rootDir": "."
  },
  "include": ["./*.ts"],
  "exclude": ["node_modules"]
}
`
}

func pyEntryPoint(clouds []string, region string) string {
	hasAWS := containsCloud(clouds, "aws")
	hasGCP := containsCloud(clouds, "gcp")

	imports := "import anvil_cloud as anvil\n"
	_ = hasGCP // suppress unused warning when only AWS

	providers := ""
	if hasAWS {
		providers += fmt.Sprintf(`    aws_providers={
        "aws": anvil.AwsProviderConfig(region="%s"),
    },
`, region)
	}
	if hasGCP {
		gcpRegion := region
		if hasAWS {
			gcpRegion = "australia-southeast1"
		}
		providers += fmt.Sprintf(`    gcp_providers={
        "gcp": anvil.GcpProviderConfig(region="%s"),
    },
`, gcpRegion)
	}

	return fmt.Sprintf(`%s

def main(ctx: anvil.Context) -> None:
    # Your infrastructure goes here

    ctx.export("stage", ctx.stage)


anvil.run(anvil.AppConfig(
    defaults=anvil.DefaultsConfig(
        tags={},
    ),
%s    run=main,
))
`, imports, providers)
}

func pyRequirements(clouds []string) string {
	deps := "anvil-cloud\npulumi>=3.0.0,<4.0.0\n"
	if containsCloud(clouds, "aws") {
		deps += "pulumi-aws>=7.21.0\n"
	}
	if containsCloud(clouds, "gcp") {
		deps += "pulumi-gcp>=9.0.0\n"
	}
	return deps
}

func goEntryPoint(clouds []string, region string) string {
	hasAWS := containsCloud(clouds, "aws")
	hasGCP := containsCloud(clouds, "gcp")
	_ = hasGCP // suppress unused warning when only AWS

	imports := `"github.com/anvil-cloud/anvil/sdk/go/anvil"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"`

	providers := ""
	if hasAWS {
		providers += fmt.Sprintf(`		AwsProviders: map[string]*anvil.AwsProviderConfig{
			"aws": {Region: "%s"},
		},
`, region)
	}
	if hasGCP {
		gcpRegion := region
		if hasAWS {
			gcpRegion = "australia-southeast1"
		}
		providers += fmt.Sprintf(`		GcpProviders: map[string]*anvil.GcpProviderConfig{
			"gcp": {Region: "%s"},
		},
`, gcpRegion)
	}

	return fmt.Sprintf(`package main

import (
	%s
)

func main() {
	anvil.Run(anvil.AppConfig{
		Defaults: &anvil.DefaultsConfig{
			Tags: map[string]string{},
		},
%s		Run: func(ctx *anvil.Context) error {
			// Your infrastructure goes here

			ctx.Export("stage", pulumi.String(ctx.Stage))
			return nil
		},
	})
}
`, imports, providers)
}

// anvilGoSDKModule is the module path of the Anvil Go SDK. It lives in the
// sdk/go/anvil subdirectory of the repo, so each release is published as a
// `sdk/go/anvil/vX.Y.Z` tag (see targetPublishGo in build/deploy.go and the
// publish-go job in .github/workflows/release.yml).
const anvilGoSDKModule = "github.com/anvil-cloud/anvil/sdk/go/anvil"

// releaseVersionRe matches a plain release version (optionally v-prefixed):
// no pre-release, build metadata, pseudo-version or "+dirty" suffix — those
// never have a matching sdk/go/anvil tag.
var releaseVersionRe = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// goSDKVersion picks the Anvil Go SDK version a new Go project should require.
//
// The CLI and the Go SDK are released together from the same commit with the
// same version (vX.Y.Z and sdk/go/anvil/vX.Y.Z), so a release CLI requires its
// own version. In order:
//
//  1. cliVersion — the ldflags-injected `version` (release builds).
//  2. buildInfoVersion — the main module version Go stamps into binaries built
//     with `go install github.com/anvil-cloud/anvil/cmd/anvil@vX.Y.Z`.
//  3. latest() — for dev builds, the newest SDK version the Go module proxy
//     can resolve right now (`go list -m <sdk>@latest`). A dev build has no
//     version of its own, and the version in provider/base-schema.json may be
//     an unreleased bump with no tag yet, so "latest published" is the only
//     choice guaranteed to resolve.
//
// It returns "" when none of these yields a version; goMod then leaves the
// requirement out and `go mod tidy` resolves the SDK to its latest version.
func goSDKVersion(cliVersion, buildInfoVersion string, latest func() (string, error)) string {
	for _, v := range []string{cliVersion, buildInfoVersion} {
		if releaseVersionRe.MatchString(v) {
			return "v" + strings.TrimPrefix(v, "v")
		}
	}
	if latest != nil {
		if v, err := latest(); err == nil {
			v = strings.TrimSpace(v)
			if releaseVersionRe.MatchString(v) {
				return "v" + strings.TrimPrefix(v, "v")
			}
		}
	}
	return ""
}

// mainModuleVersion returns the version Go stamped into this binary's build
// info ("(devel)" or a pseudo-version for local builds, which goSDKVersion
// ignores).
func mainModuleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return ""
}

// latestGoSDKVersion asks the go command for the newest published Anvil Go
// SDK version. GOWORK=off keeps a surrounding go.work from interfering.
func latestGoSDKVersion() (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Version}}", anvilGoSDKModule+"@latest")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// goMod renders the scaffolded go.mod. Only the Anvil SDK is pinned:
// github.com/pulumi/pulumi/sdk/v3 is deliberately not listed, so `go mod
// tidy` adds it at exactly the version the chosen Anvil SDK requires (it is
// already in the module graph via the SDK's go.mod) instead of a literal here
// that drifts out of step with the SDK.
func goMod(modulePath, sdkVersion string) string {
	var require string
	if sdkVersion != "" {
		require = fmt.Sprintf("\nrequire %s %s\n", anvilGoSDKModule, sdkVersion)
	}
	return fmt.Sprintf("module %s\n\ngo 1.22\n%s", modulePath, require)
}

// ── Helpers ──

func containsCloud(clouds []string, target string) bool {
	for _, c := range clouds {
		if c == target {
			return true
		}
	}
	return false
}

func cloudDefaultRegion(cloud string) string {
	switch cloud {
	case "gcp":
		return "australia-southeast1"
	default:
		return "ap-southeast-2"
	}
}

func gitignoreContent(lang string) string {
	base := `.anvil/
node_modules/
`
	switch lang {
	case "ts":
		return base + `bin/
*.js
*.d.ts
`
	case "python":
		return base + `__pycache__/
*.pyc
.venv/
`
	case "go":
		return base
	default:
		return base
	}
}
