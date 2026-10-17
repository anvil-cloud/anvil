package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	awsbundler "github.com/anvil-cloud/anvil/cmd/anvil/cmd/bundler/aws"
)

// TestScaffoldThenBuildReal scaffolds two Lambdas into an empty project with
// the real cargo, builds them with the real cargo lambda, then scaffolds
// again to check nothing is touched. It downloads crates, so it only runs
// when ANVIL_RUST_E2E=1.
func TestScaffoldThenBuildReal(t *testing.T) {
	if os.Getenv("ANVIL_RUST_E2E") != "1" {
		t.Skip("set ANVIL_RUST_E2E=1 to scaffold and build with real cargo")
	}

	root := t.TempDir()
	t.Chdir(root)
	fns := []RustFunction{
		{Name: "api", Handler: "functions/api"},
		// A directory Cargo would reject as a package name: the package is
		// named after the Lambda instead.
		{Name: "worker", Handler: "functions/test"},
	}

	created, err := Rust(root, fns, RustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created: %q", created)

	manifest := read(t, filepath.Join(root, "Cargo.toml"))
	for _, m := range []string{`"functions/api"`, `"functions/test"`} {
		if !strings.Contains(manifest, m) {
			t.Errorf("cargo didn't add %s to members:\n%s", m, manifest)
		}
	}
	if pkg := read(t, filepath.Join(root, "functions/test/Cargo.toml")); !strings.Contains(pkg, `name = "worker"`) {
		t.Errorf("functions/test should be package worker:\n%s", pkg)
	}
	pkg := read(t, filepath.Join(root, "functions/api/Cargo.toml"))
	for _, s := range []string{"edition.workspace = true", "lambda_runtime.workspace = true", "tokio.workspace = true", "serde_json.workspace = true"} {
		if !strings.Contains(pkg, s) {
			t.Errorf("functions/api/Cargo.toml missing %q:\n%s", s, pkg)
		}
	}

	results, failed := awsbundler.BuildRustLambdas([]awsbundler.RustFunction{
		{Name: "api", Handler: "functions/api", Architecture: "arm64"},
		{Name: "worker", Handler: "functions/test", Architecture: "arm64"},
	})
	for name, err := range failed {
		t.Fatalf("%s failed to build: %v", name, err)
	}
	if len(results) != 2 {
		t.Fatalf("built %d functions, want 2", len(results))
	}

	// Second run: everything exists, so nothing is created or changed.
	before := read(t, filepath.Join(root, "Cargo.toml"))
	again, err := Rust(root, fns, RustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("second run created %q, want nothing", again)
	}
	if after := read(t, filepath.Join(root, "Cargo.toml")); after != before {
		t.Error("second run changed the workspace Cargo.toml")
	}
}
