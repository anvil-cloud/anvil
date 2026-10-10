package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
)

var (
	initName   string
	initLang   string
	initCloud  string
	initRegion string
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

	// ── Generate files ──
	fmt.Println()

	// anvil.yaml
	err := writeAnvilConfig(anvilConfig{
		Project: projectName,
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
	if isTTY() {
		fmt.Println(dim("  Run `anvil deploy` to get started."))
	} else {
		fmt.Println("  Run `anvil deploy` to get started.")
	}

	return nil
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
	err = os.WriteFile("go.mod", []byte(goMod(modulePath, clouds)), 0644)
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
    "module": "commonjs",
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

func goMod(modulePath string, clouds []string) string {
	return fmt.Sprintf(`module %s

go 1.22

require (
	github.com/anvil-cloud/anvil/sdk/go/anvil v0.0.4
	github.com/pulumi/pulumi/sdk/v3 v3.145.0
)
`, modulePath)
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
