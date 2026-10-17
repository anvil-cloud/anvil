package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	awsbundler "github.com/anvil-cloud/anvil/cmd/anvil/cmd/bundler/aws"
)

// TestGenerateProjectWithDBReal runs the generate step on an empty project
// with a DSQL schema and two Rust Lambdas, using the real cargo, then builds
// the Lambdas. That checks gen/db joins the workspace, new Lambdas depend on
// it, and the generated client compiles against the real tokio-postgres. It
// downloads crates, so it only runs when ANVIL_RUST_E2E=1.
func TestGenerateProjectWithDBReal(t *testing.T) {
	if os.Getenv("ANVIL_RUST_E2E") != "1" {
		t.Skip("set ANVIL_RUST_E2E=1 to generate and build with real cargo")
	}

	repoTestdata, err := filepath.Abs(filepath.Join("..", "..", "..", "codegen", "testdata", "anvil.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(repoTestdata)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		DSQL map[string]struct {
			Schemas json.RawMessage `json:"schemas"`
		} `json:"dsql"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	manifest := &buildManifest{}
	for name, comp := range file.DSQL {
		manifest.Schemas = append(manifest.Schemas, SchemaSpec{Component: name, Type: "anvil:aws:DSQL", Schemas: comp.Schemas})
	}

	root := t.TempDir()
	t.Chdir(root)
	functions := []FunctionSpec{
		{Name: "api", Type: "anvil:aws:Lambda", Runtime: "rust", Handler: "functions/api", Entry: "functions/api", Architecture: "arm64"},
		{Name: "worker", Type: "anvil:aws:Lambda", Runtime: "rust", Handler: "functions/worker", Entry: "functions/worker", Architecture: "arm64"},
	}

	// No saved targets, no detectable language: the Rust client still comes
	// from having Rust Lambdas.
	if err := generateProject(root, nil, functions, manifest, "Generated"); err != nil {
		t.Fatal(err)
	}

	workspace := read(t, filepath.Join(root, "Cargo.toml"))
	for _, m := range []string{`"gen/db"`, `"functions/api"`, `"functions/worker"`} {
		if !strings.Contains(workspace, m) {
			t.Errorf("workspace members missing %s:\n%s", m, workspace)
		}
	}
	api := read(t, filepath.Join(root, "functions/api/Cargo.toml"))
	if !strings.Contains(api, `path = "../../gen/db"`) {
		t.Errorf("functions/api doesn't depend on gen/db:\n%s", api)
	}
	// gen/db created the workspace, so its dependencies carry the features:
	// the Lambda inherits them without repeating them.
	if !strings.Contains(api, "lambda_runtime.workspace = true") {
		t.Errorf("functions/api should inherit lambda_runtime plainly:\n%s", api)
	}
	if lib := read(t, filepath.Join(root, "gen/db/src/lib.rs")); !strings.Contains(lib, "pub mod myapp;") {
		t.Errorf("gen/db/src/lib.rs:\n%s", lib)
	}

	_, failed := awsbundler.BuildRustLambdas([]awsbundler.RustFunction{
		{Name: "api", Handler: "functions/api", Architecture: "arm64"},
		{Name: "worker", Handler: "functions/worker", Architecture: "arm64"},
	})
	for name, err := range failed {
		t.Fatalf("%s failed to build: %v", name, err)
	}

	// Regenerating rewrites gen/db only; the Lambdas are left alone.
	before := read(t, filepath.Join(root, "functions/api/Cargo.toml"))
	if err := generateProject(root, nil, functions, manifest, "Updated"); err != nil {
		t.Fatal(err)
	}
	if after := read(t, filepath.Join(root, "functions/api/Cargo.toml")); after != before {
		t.Error("regenerating changed a Lambda's Cargo.toml")
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
