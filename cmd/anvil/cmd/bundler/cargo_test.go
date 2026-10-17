package bundler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeWorkspace lays out a workspace on disk and stubs cargo metadata to
// describe it, so resolution is tested without running cargo:
//
//	root/
//	  api/        one bin (api)
//	  tools/      three bins: src/main.rs (tools), src/bin/sync.rs (sync),
//	              src/bin/report.rs renamed to [[bin]] "weekly-report"
//	  shared/     lib only
func fakeWorkspace(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	files := []string{
		"Cargo.toml",
		"api/Cargo.toml", "api/src/main.rs",
		"tools/Cargo.toml", "tools/src/main.rs", "tools/src/bin/sync.rs", "tools/src/bin/report.rs",
		"shared/Cargo.toml", "shared/src/lib.rs",
		"notes.txt",
	}
	for _, f := range files {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	pkg := func(name string, targets ...CargoTarget) CargoPackage {
		return CargoPackage{Name: name, ManifestPath: filepath.Join(root, name, "Cargo.toml"), Targets: targets}
	}
	bin := func(name, src string) CargoTarget {
		return CargoTarget{Name: name, Kind: []string{"bin"}, SrcPath: filepath.Join(root, src)}
	}
	ws := CargoWorkspace{
		Root:      root,
		TargetDir: filepath.Join(root, "target"),
		Packages: []CargoPackage{
			pkg("api", bin("api", "api/src/main.rs")),
			pkg("tools",
				bin("tools", "tools/src/main.rs"),
				bin("sync", "tools/src/bin/sync.rs"),
				bin("weekly-report", "tools/src/bin/report.rs")),
			pkg("shared", CargoTarget{Name: "shared", Kind: []string{"lib"}, SrcPath: filepath.Join(root, "shared/src/lib.rs")}),
		},
	}
	raw, err := json.Marshal(ws)
	if err != nil {
		t.Fatal(err)
	}

	original := cargoMetadata
	cargoMetadata = func(string) ([]byte, error) { return raw, nil }
	t.Cleanup(func() { cargoMetadata = original })
	return root
}

func TestResolveRustBin(t *testing.T) {
	root := fakeWorkspace(t)

	tests := []struct {
		name        string
		handler     string
		wantPackage string
		wantBin     string
		wantErr     string
	}{
		{"package with one binary", "api", "api", "api", ""},
		{"main.rs of a multi-bin package", "tools/src/main.rs", "tools", "tools", ""},
		{"src/bin file", "tools/src/bin/sync.rs", "tools", "sync", ""},
		{"renamed [[bin]] resolves by source file", "tools/src/bin/report.rs", "tools", "weekly-report", ""},
		{"package with several binaries", "tools", "", "", "has 3 binaries (tools, sync, weekly-report)"},
		{"library-only package", "shared", "", "", "has no binary target"},
		{"directory without Cargo.toml", "empty", "", "", "no Cargo.toml in this directory"},
		{"missing path", "nope", "", "", "does not exist"},
		{"not a .rs file", "notes.txt", "", "", "neither a Cargo package directory nor a .rs file"},
		{"lib source is not a binary", "shared/src/lib.rs", "", "", "is not the source of any binary"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveRustBin(filepath.Join(root, tt.handler))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Package != tt.wantPackage || got.Name != tt.wantBin {
				t.Errorf("got %s/%s, want %s/%s", got.Package, got.Name, tt.wantPackage, tt.wantBin)
			}
		})
	}
}

func TestResolveRustBinSuggestsAFile(t *testing.T) {
	root := fakeWorkspace(t)
	t.Chdir(root)

	_, err := ResolveRustBin("tools")
	if err == nil || !strings.Contains(err.Error(), `e.g. handler: "tools/src/main.rs"`) {
		t.Fatalf("error = %v, want a pasteable handler suggestion", err)
	}
}

func TestGroupRustBins(t *testing.T) {
	wsA := &CargoWorkspace{Root: "/a"}
	wsB := &CargoWorkspace{Root: "/b"}
	bin := func(ws *CargoWorkspace, pkg, name string) *RustBin {
		return &RustBin{Workspace: ws, Package: pkg, Name: name}
	}

	t.Run("groups by workspace and architecture", func(t *testing.T) {
		groups, err := GroupRustBins(
			[]*RustBin{
				bin(wsA, "worker", "worker"),
				bin(wsA, "api", "api"),
				bin(wsB, "jobs", "jobs"),
				bin(wsA, "api", "api"), // second function using the same binary
				bin(wsA, "legacy", "legacy"),
			},
			[]string{"arm64", "arm64", "arm64", "arm64", "x86_64"},
		)
		if err != nil {
			t.Fatal(err)
		}

		got := make([]string, len(groups))
		for i, g := range groups {
			got[i] = g.Workspace.Root + " " + g.Architecture + " " + strings.Join(g.Bins, ",")
		}
		want := []string{"/a arm64 api,worker", "/a x86_64 legacy", "/b arm64 jobs"}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("groups = %q, want %q", got, want)
		}
	})

	t.Run("same binary name in two packages", func(t *testing.T) {
		_, err := GroupRustBins(
			[]*RustBin{bin(wsA, "api", "server"), bin(wsA, "admin", "server")},
			[]string{"arm64", "arm64"},
		)
		if err == nil || !strings.Contains(err.Error(), `both have a binary named "server"`) {
			t.Fatalf("error = %v, want a duplicate-name error", err)
		}
	})
}
