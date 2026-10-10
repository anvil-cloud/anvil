package cmd

import (
	"context"
	"fmt"
	"path/filepath"

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
	Short: "Generate typed database client code from your schema",
	Long: `Discovers DSQL schemas from your Pulumi program and generates fully-typed
database interfaces for your language. Produces Row, Insert, and Update types
plus CRUD query helpers for every table in every schema.

Runs a Pulumi preview (no real resources created) to extract schema definitions,
then generates code to gen/ (or the --path you provide).

Each invocation with --lang/--path is remembered per stage, so a later bare
"anvil generate" — and the automatic post-deploy refresh — reproduce every
configured language and output path.

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

// runGenerateTargets generates code for each target from the manifest schemas.
// verb labels each output line (e.g. "Generated" or "Updated").
func runGenerateTargets(projectRoot string, targets []generateTarget, entries []codegen.ManifestSchemaEntry, verb string) error {
	for _, t := range targets {
		gen, ok := generators[normalizeLang(t.Lang)]
		if !ok {
			return fmt.Errorf("unsupported language %q — supported: typescript, python, go, rust", t.Lang)
		}

		outDir := t.Path
		if !filepath.IsAbs(outDir) {
			outDir = filepath.Join(projectRoot, t.Path)
		}

		if err := codegen.RunFromManifest(entries, gen, outDir); err != nil {
			return err
		}
		fmt.Printf("  %s %s types in %s\n", verb, gen.Name(), outDir)
	}
	return nil
}

// refreshGeneratedTypes regenerates typed database client code from the build
// manifest after a deploy, using the stage's saved generate config (falling back
// to the auto-detected language at gen/). Errors are non-fatal.
func refreshGeneratedTypes(manifest *buildManifest, stage string) {
	projectRoot, err := findProjectRoot()
	if err != nil {
		return
	}

	targets, err := generateTargetsFor(projectRoot, stage, false)
	if err != nil {
		return
	}

	if err := runGenerateTargets(projectRoot, targets, manifestEntries(manifest), "Updated"); err != nil {
		fmt.Printf("  ⚠️  Schema codegen refresh failed: %s\n", err)
	}
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

	fmt.Println("  Discovering schemas...")

	if _, err := discoverFunctions(ctx, stage); err != nil {
		return fmt.Errorf("schema discovery failed: %w", err)
	}

	manifest, err := readFullManifest()
	if err != nil {
		return err
	}
	if manifest == nil || len(manifest.Schemas) == 0 {
		return fmt.Errorf("no database schemas found — make sure your Pulumi program defines DSQL schemas")
	}

	if err := runGenerateTargets(projectRoot, targets, manifestEntries(manifest), "Generated"); err != nil {
		return err
	}

	// Remember an explicit invocation so future bare runs and the post-deploy
	// refresh reproduce the same language/path. Non-fatal on failure.
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
