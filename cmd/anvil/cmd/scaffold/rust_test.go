package scaffold

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// stubCargo replaces cargo with a fake that records calls; `cargo init`
// creates what the real one would (Cargo.toml + hello-world main.rs, unless
// main.rs exists).
func stubCargo(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string

	origCargo := runCargo
	t.Cleanup(func() { runCargo = origCargo })

	runCargo = func(dir string, args ...string) error {
		calls = append(calls, args)
		if args[0] == "init" {
			pkg := args[len(args)-1]
			if err := os.WriteFile(filepath.Join(pkg, "Cargo.toml"), []byte("[package]\nname = \"x\"\n"), 0o644); err != nil {
				return err
			}
			main := filepath.Join(pkg, "src", "main.rs")
			if _, err := os.Stat(main); os.IsNotExist(err) {
				if err := os.MkdirAll(filepath.Dir(main), 0o755); err != nil {
					return err
				}
				return os.WriteFile(main, []byte("fn main() { println!(\"Hello, world!\"); }\n"), 0o644)
			}
		}
		return nil
	}
	return &calls
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
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

func TestRustCreatesWorkspaceAndPackages(t *testing.T) {
	root := t.TempDir()
	calls := stubCargo(t)
	write(t, filepath.Join(root, ".gitignore"), ".anvil/\nnode_modules/") // no trailing newline

	created, err := Rust(root, []RustFunction{
		{Name: "api", Handler: "functions/api"},
		{Name: "worker", Handler: "functions/worker"},
	}, RustOptions{})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"Cargo.toml (workspace)", "rust-toolchain.toml", ".gitignore (+ /target)", "functions/api/", "functions/worker/"}
	if !reflect.DeepEqual(created, want) {
		t.Errorf("created = %q, want %q", created, want)
	}

	manifest := read(t, filepath.Join(root, "Cargo.toml"))
	for _, s := range []string{`resolver = "3"`, `edition = "2024"`, `rust-version = "1.99.1"`, `lambda_runtime = { version = "1", features = ["tracing"] }`} {
		if !strings.Contains(manifest, s) {
			t.Errorf("workspace Cargo.toml missing %q", s)
		}
	}
	if strings.Contains(manifest, "[profile.release]") {
		t.Error("workspace Cargo.toml must not set a release profile (cargo lambda's defaults apply)")
	}
	if tc := read(t, filepath.Join(root, "rust-toolchain.toml")); !strings.Contains(tc, `channel = "1.99.1"`) || !strings.Contains(tc, "aarch64-unknown-linux-gnu") ||
		!strings.Contains(tc, `components = ["rust-analyzer", "rustfmt", "clippy"]`) {
		t.Errorf("rust-toolchain.toml = %q", tc)
	}
	if gi := read(t, filepath.Join(root, ".gitignore")); gi != ".anvil/\nnode_modules/\n/target\n" {
		t.Errorf(".gitignore = %q", gi)
	}
	if main := read(t, filepath.Join(root, "functions/api/src/main.rs")); main != handlerTemplate {
		t.Errorf("main.rs is not the handler template:\n%s", main)
	}

	// Our own workspace defines the features, so cargo add needs none.
	for _, c := range *calls {
		if c[0] == "add" && strings.Contains(strings.Join(c, " "), "--features") {
			t.Errorf("unexpected --features in %q", c)
		}
	}
}

func TestRustSkipsExistingCode(t *testing.T) {
	root := t.TempDir()
	calls := stubCargo(t)
	write(t, filepath.Join(root, "Cargo.toml"), "[workspace]\nmembers = [\"functions/api\"]\n")
	write(t, filepath.Join(root, "functions/api/Cargo.toml"), "[package]\nname = \"api\"\n")
	write(t, filepath.Join(root, "functions/api/src/main.rs"), "// user code\n")

	created, err := Rust(root, []RustFunction{{Name: "api", Handler: "functions/api"}}, RustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 || len(*calls) != 0 {
		t.Errorf("existing package was touched: created %q, cargo calls %q", created, *calls)
	}
	if got := read(t, filepath.Join(root, "functions/api/src/main.rs")); got != "// user code\n" {
		t.Errorf("main.rs changed: %q", got)
	}
}

func TestRustJoinsExistingWorkspace(t *testing.T) {
	root := t.TempDir()
	calls := stubCargo(t)
	userManifest := "# user's workspace\n[workspace]\nmembers = []\n"
	write(t, filepath.Join(root, "Cargo.toml"), userManifest)

	created, err := Rust(root, []RustFunction{{Name: "api", Handler: "functions/api"}}, RustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(created, []string{"functions/api/"}) {
		t.Errorf("created = %q", created)
	}
	if got := read(t, filepath.Join(root, "Cargo.toml")); got != userManifest {
		t.Errorf("user's workspace manifest was rewritten by Anvil: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "rust-toolchain.toml")); !os.IsNotExist(err) {
		t.Error("rust-toolchain.toml must not be created when joining a user's workspace")
	}

	// A workspace Anvil didn't create may lack the features: pass them.
	var add []string
	for _, c := range *calls {
		if c[0] == "add" {
			add = c
		}
	}
	if !strings.Contains(strings.Join(add, " "), "--features lambda_runtime/tracing,tokio/macros") {
		t.Errorf("cargo add = %q, want the template's features", add)
	}
}

func TestRustKeepsAnExistingMainRS(t *testing.T) {
	root := t.TempDir()
	stubCargo(t)
	write(t, filepath.Join(root, "functions/api/src/main.rs"), "// started by hand\n")

	if _, err := Rust(root, []RustFunction{{Name: "api", Handler: "functions/api"}}, RustOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(root, "functions/api/src/main.rs")); got != "// started by hand\n" {
		t.Errorf("main.rs was overwritten: %q", got)
	}
}

func TestRustStopsOnRootPackage(t *testing.T) {
	root := t.TempDir()
	calls := stubCargo(t)
	write(t, filepath.Join(root, "Cargo.toml"), "[package]\nname = \"app\"\n")

	_, err := Rust(root, []RustFunction{{Name: "api", Handler: "functions/api"}}, RustOptions{})
	if err == nil || !strings.Contains(err.Error(), "is a package, not a workspace") {
		t.Fatalf("error = %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("cargo ran despite stopping: %q", *calls)
	}
	if got := read(t, filepath.Join(root, "Cargo.toml")); got != "[package]\nname = \"app\"\n" {
		t.Errorf("root package was modified: %q", got)
	}
}

func TestRustBinFile(t *testing.T) {
	t.Run("created inside an existing package", func(t *testing.T) {
		root := t.TempDir()
		calls := stubCargo(t)
		write(t, filepath.Join(root, "functions/tools/Cargo.toml"), "[package]\nname = \"tools\"\n")

		created, err := Rust(root, []RustFunction{{Name: "sync", Handler: "functions/tools/src/bin/sync.rs"}}, RustOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(created, []string{"functions/tools/src/bin/sync.rs"}) {
			t.Errorf("created = %q", created)
		}
		if got := read(t, filepath.Join(root, "functions/tools/src/bin/sync.rs")); got != handlerTemplate {
			t.Errorf("bin file is not the template: %q", got)
		}
		if len(*calls) != 0 {
			t.Errorf("no cargo calls needed for a src/bin file, got %q", *calls)
		}
	})

	t.Run("no package to put it in", func(t *testing.T) {
		root := t.TempDir()
		stubCargo(t)

		_, err := Rust(root, []RustFunction{{Name: "sync", Handler: "functions/tools/src/bin/sync.rs"}}, RustOptions{})
		if err == nil || !strings.Contains(err.Error(), `point handler at a package directory (e.g. "functions/sync")`) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestIgnoreTargetLeavesExistingRule(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".gitignore"), "target/\n")
	added, err := ignoreTarget(root)
	if err != nil || added {
		t.Fatalf("added = %v, err = %v", added, err)
	}
}

func TestRustAddsDBDependencyToNewPackagesOnly(t *testing.T) {
	root := t.TempDir()
	calls := stubCargo(t)
	write(t, filepath.Join(root, "Cargo.toml"), "[workspace]\nmembers = [\"functions/old\"]\n")
	write(t, filepath.Join(root, "functions/old/Cargo.toml"), "[package]\nname = \"old\"\n")
	db := filepath.Join(root, "gen", "db")

	_, err := Rust(root, []RustFunction{
		{Name: "old", Handler: "functions/old"},
		{Name: "api", Handler: "functions/api"},
	}, RustOptions{DBCrate: db})
	if err != nil {
		t.Fatal(err)
	}

	var dbAdds []string
	for _, c := range *calls {
		joined := strings.Join(c, " ")
		if c[0] == "add" && strings.Contains(joined, "--path "+db) {
			dbAdds = append(dbAdds, joined)
		}
	}
	if len(dbAdds) != 1 || !strings.Contains(dbAdds[0], filepath.Join(root, "functions/api/Cargo.toml")) {
		t.Errorf("db dependency adds = %q, want exactly one, for the new package", dbAdds)
	}
}

func TestRustCrate(t *testing.T) {
	t.Run("creates the workspace and joins it", func(t *testing.T) {
		root := t.TempDir()
		calls := stubCargo(t)
		db := filepath.Join(root, "gen", "db")

		created, err := RustCrate(root, db)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"Cargo.toml (workspace)", "rust-toolchain.toml", ".gitignore (+ /target)", "gen/db/"}
		if !reflect.DeepEqual(created, want) {
			t.Errorf("created = %q, want %q", created, want)
		}
		if len(*calls) != 1 || strings.Join((*calls)[0], " ") != "init --lib --vcs none "+db {
			t.Errorf("cargo calls = %q, want one cargo init --lib", *calls)
		}
	})

	t.Run("existing crate is left alone", func(t *testing.T) {
		root := t.TempDir()
		calls := stubCargo(t)
		write(t, filepath.Join(root, "gen/db/Cargo.toml"), "[package]\nname = \"db\"\n")

		created, err := RustCrate(root, filepath.Join(root, "gen", "db"))
		if err != nil || len(created) != 0 || len(*calls) != 0 {
			t.Errorf("created %q, calls %q, err %v; want nothing", created, *calls, err)
		}
	})
}

func TestPackageName(t *testing.T) {
	tests := map[string]string{
		"testRust":   "test-rust",
		"api":        "api",
		"APIHandler": "api-handler",
		"orderV2":    "order-v2",
		"orders_api": "orders-api",
		"Worker":     "worker",
		"test":       "test-fn",
		"std":        "std-fn",
		"fn":         "fn-fn",
		"db":         "db-fn",
		"2fa":        "fn-2fa",
		"my.lambda!": "my-lambda",
		"__":         "lambda",
	}
	for in, want := range tests {
		if got := packageName(in); got != want {
			t.Errorf("packageName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRustNamesPackageAfterTheLambda(t *testing.T) {
	root := t.TempDir()
	calls := stubCargo(t)

	if _, err := Rust(root, []RustFunction{{Name: "testRust", Handler: "backend/functions/test"}}, RustOptions{}); err != nil {
		t.Fatal(err)
	}

	var init []string
	for _, c := range *calls {
		if c[0] == "init" {
			init = c
		}
	}
	if !strings.Contains(strings.Join(init, " "), "--name test-rust") {
		t.Errorf("cargo init = %q, want --name test-rust", init)
	}
}
