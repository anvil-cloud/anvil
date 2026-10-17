package cmd

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/anvil-cloud/anvil/cmd/anvil/cmd/scaffold"
	"github.com/anvil-cloud/anvil/codegen"
	"github.com/spf13/cobra"
)

var (
	generateLang  string
	generatePath  string
	generateStage string
)

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate code from your infrastructure: Rust Lambda packages and typed database clients",
	Long: `Makes your code match what your Pulumi program declares. Runs a Pulumi
preview (no real resources created) to discover functions and schemas, then:

  • Creates the Cargo package for every Rust Lambda whose handler doesn't
    exist yet, with a blank handler, and a Cargo workspace at the project
    root if there isn't one. Existing code is never edited or deleted.
    (anvil deploy does this too, before building.)

  • Generates fully-typed database interfaces for every DSQL schema: Row,
    Insert, and Update types plus CRUD query helpers, written to gen/ (or
    the --path you provide).

Each invocation with --lang/--path is remembered per stage, so a later bare
"anvil generate" — and every anvil deploy, which generates before building —
reproduce every configured language and output path. With Rust Lambdas, the
Rust client is always generated too, as a crate in gen/db that new Lambdas
depend on.

Supported languages: typescript, python, go, rust`,
	RunE: runGenerate,
}

func init() {
	generateCmd.Flags().StringVar(&generateLang, "lang", "", "Target language: typescript, python, go, rust (auto-detected from entry point if omitted)")
	generateCmd.Flags().StringVar(&generatePath, "path", "", "Output directory, relative to the project root (default: gen/)")
	generateCmd.Flags().StringVar(&generateStage, "stage", "", "Stage name for schema discovery")
	rootCmd.AddCommand(generateCmd)
}

var generators = map[string]codegen.Generator{
	"typescript": &codegen.TypeScriptGenerator{},
	"nodejs":     &codegen.TypeScriptGenerator{},
	"python":     &codegen.PythonGenerator{},
	"go":         &codegen.GoGenerator{},
	"rust":       &codegen.RustGenerator{},
}

// manifestEntries converts the build manifest's schema entries into codegen input.
func manifestEntries(manifest *buildManifest) []codegen.ManifestSchemaEntry {
	entries := make([]codegen.ManifestSchemaEntry, len(manifest.Schemas))
	for i, s := range manifest.Schemas {
		entries[i] = codegen.ManifestSchemaEntry{
			Component:   s.Component,
			SchemasJSON: s.Schemas,
		}
	}
	return entries
}

// generateTargetsFor resolves which language/path pairs to generate. An explicit
// --lang/--path invocation yields a single target; otherwise the stage's saved
// config is used, falling back to the auto-detected language at the default path.
func generateTargetsFor(projectRoot, stage string, explicit bool) ([]generateTarget, error) {
	if explicit {
		lang := generateLang
		if lang == "" {
			rt, err := detectRuntime(projectRoot)
			if err != nil {
				return nil, fmt.Errorf("could not auto-detect language (use --lang): %w", err)
			}
			lang = rt.Name
		}
		path := generatePath
		if path == "" {
			path = defaultGenPath
		}
		return []generateTarget{{Lang: normalizeLang(lang), Path: path}}, nil
	}

	saved, err := loadGenerateTargets(projectRoot, stage)
	if err != nil {
		return nil, err
	}
	if len(saved) > 0 {
		return saved, nil
	}

	rt, err := detectRuntime(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("could not auto-detect language (use --lang): %w", err)
	}
	return []generateTarget{{Lang: normalizeLang(rt.Name), Path: defaultGenPath}}, nil
}

// generateProject makes generated code match the program, and runs before
// any build so builds always see current code: typed database clients for
// every target (plus the gen/db crate whenever there are Rust Lambdas, which
// depend on it), then packages for any Rust Lambda that doesn't exist yet.
// verb labels each output line (e.g. "Generated" or "Updated").
func generateProject(projectRoot string, targets []generateTarget, functions []FunctionSpec, manifest *buildManifest, verb string) error {
	var dbCrate string
	if manifest != nil && len(manifest.Schemas) > 0 {
		if hasRustFunctions(functions) {
			targets = withRustTarget(targets)
		}
		var err error
		dbCrate, err = runGenerateTargets(projectRoot, targets, manifestEntries(manifest), verb)
		if err != nil {
			return err
		}
	}
	return scaffoldFunctions(projectRoot, functions, dbCrate)
}

// withRustTarget adds the Rust client at the default path unless a Rust
// target is already configured. It is implied by having Rust Lambdas, so it
// is never saved to the stage's generate config.
func withRustTarget(targets []generateTarget) []generateTarget {
	for _, t := range targets {
		if normalizeLang(t.Lang) == "rust" {
			return targets
		}
	}
	return append(append([]generateTarget{}, targets...), generateTarget{Lang: "rust", Path: defaultGenPath})
}

// runGenerateTargets generates code for each target from the manifest
// schemas. The Rust client is a crate in <path>/db, joined to the Cargo
// workspace (created at the project root if needed); its directory is
// returned so new Rust Lambdas can depend on it.
func runGenerateTargets(projectRoot string, targets []generateTarget, entries []codegen.ManifestSchemaEntry, verb string) (dbCrate string, err error) {
	for _, t := range targets {
		lang := normalizeLang(t.Lang)
		gen, ok := generators[lang]
		if !ok {
			return "", fmt.Errorf("unsupported language %q — supported: typescript, python, go, rust", t.Lang)
		}

		outDir := t.Path
		if !filepath.IsAbs(outDir) {
			outDir = filepath.Join(projectRoot, t.Path)
		}

		if lang == "rust" {
			outDir = filepath.Join(outDir, codegen.RustCrateName)
			created, err := scaffold.RustCrate(projectRoot, outDir)
			for _, c := range created {
				fmt.Printf("  created %s\n", c)
			}
			if err != nil {
				return "", err
			}
			if dbCrate == "" {
				dbCrate = outDir
			}
		}

		if err := codegen.RunFromManifest(entries, gen, outDir); err != nil {
			return "", err
		}
		fmt.Printf("  %s %s types in %s\n", verb, gen.Name(), outDir)
	}
	return dbCrate, nil
}

func runGenerate(cmd *cobra.Command, args []string) error {
	projectRoot, err := findProjectRoot()
	if err != nil {
		return err
	}

	stage := resolveStage(generateStage)
	explicit := generateLang != "" || generatePath != ""

	targets, err := generateTargetsFor(projectRoot, stage, explicit)
	if err != nil {
		return err
	}

	// Validate languages up front so we fail before running a preview.
	for _, t := range targets {
		if _, ok := generators[normalizeLang(t.Lang)]; !ok {
			return fmt.Errorf("unsupported language %q — supported: typescript, python, go, rust", t.Lang)
		}
	}

	ctx := context.Background()

	fmt.Println("  Discovering functions and schemas...")

	functions, err := discoverFunctions(ctx, stage)
	if err != nil {
		return fmt.Errorf("discovery failed: %w", err)
	}

	manifest, err := readFullManifest()
	if err != nil {
		return err
	}
	if (manifest == nil || len(manifest.Schemas) == 0) && !hasRustFunctions(functions) {
		return fmt.Errorf("nothing to generate — no Rust Lambdas or DSQL schemas found in your program")
	}

	if err := generateProject(projectRoot, targets, functions, manifest, "Generated"); err != nil {
		return err
	}

	// Remember an explicit invocation so future bare runs and every deploy
	// reproduce the same language/path. Non-fatal on failure.
	if explicit {
		saved, _ := loadGenerateTargets(projectRoot, stage)
		for _, t := range targets {
			saved = upsertGenerateTarget(saved, t)
		}
		if err := saveGenerateTargets(projectRoot, stage, saved); err != nil {
			fmt.Printf("  ⚠️  Could not save generate config: %s\n", err)
		}
	}

	return nil
}
