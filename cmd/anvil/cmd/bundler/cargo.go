package bundler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CargoWorkspace is the subset of `cargo metadata` output Anvil needs.
type CargoWorkspace struct {
	Root      string         `json:"workspace_root"`
	TargetDir string         `json:"target_directory"`
	Packages  []CargoPackage `json:"packages"`
}

// CargoPackage is one workspace member.
type CargoPackage struct {
	Name         string        `json:"name"`
	ManifestPath string        `json:"manifest_path"`
	Targets      []CargoTarget `json:"targets"`
}

// CargoTarget is one build target of a package (lib, bin, test, ...).
type CargoTarget struct {
	Name    string   `json:"name"`
	Kind    []string `json:"kind"`
	SrcPath string   `json:"src_path"`
}

func (t CargoTarget) isBin() bool {
	for _, k := range t.Kind {
		if k == "bin" {
			return true
		}
	}
	return false
}

// RustBin is a resolved binary: the workspace it builds in and its name.
type RustBin struct {
	Workspace *CargoWorkspace
	Package   string
	Name      string
}

// cargoMetadata runs `cargo metadata` for the package at manifestPath. It is
// a variable so tests can replace it.
var cargoMetadata = func(manifestPath string) ([]byte, error) {
	cmd := exec.Command("cargo", "metadata", "--format-version", "1", "--no-deps", "--manifest-path", manifestPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if _, lookErr := exec.LookPath("cargo"); lookErr != nil {
			return nil, fmt.Errorf("Rust Lambdas need the Rust toolchain (cargo not found): https://rustup.rs")
		}
		return nil, fmt.Errorf("cargo metadata failed: %s", strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ResolveRustBin maps a Rust handler to the binary it names. The handler is
// a package directory (which must have exactly one binary) or a .rs file
// (the binary whose source is that file, including renamed [[bin]] targets).
// Workspaces are found by Cargo itself, so the handler can be any member.
func ResolveRustBin(handler string) (*RustBin, error) {
	abs, err := filepath.Abs(handler)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("%s does not exist", handler)
	}

	isFile := !info.IsDir()
	if isFile && filepath.Ext(abs) != ".rs" {
		return nil, fmt.Errorf("%s is neither a Cargo package directory nor a .rs file", handler)
	}

	manifest, err := findPackageManifest(abs, isFile)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", handler, err)
	}

	raw, err := cargoMetadata(manifest)
	if err != nil {
		return nil, err
	}

	var ws CargoWorkspace
	if err := json.Unmarshal(raw, &ws); err != nil {
		return nil, fmt.Errorf("could not read cargo metadata: %w", err)
	}

	if isFile {
		return binForFile(&ws, handler, abs)
	}
	return binForPackage(&ws, handler, manifest)
}

// findPackageManifest returns the Cargo.toml that owns path: the directory's
// own manifest, or for a .rs file the nearest one above it.
func findPackageManifest(path string, isFile bool) (string, error) {
	if !isFile {
		manifest := filepath.Join(path, "Cargo.toml")
		if _, err := os.Stat(manifest); err != nil {
			return "", fmt.Errorf("no Cargo.toml in this directory")
		}
		return manifest, nil
	}

	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		manifest := filepath.Join(dir, "Cargo.toml")
		if _, err := os.Stat(manifest); err == nil {
			return manifest, nil
		}
		if parent := filepath.Dir(dir); parent == dir {
			return "", fmt.Errorf("no Cargo.toml found above this file")
		}
	}
}

func binForPackage(ws *CargoWorkspace, handler, manifest string) (*RustBin, error) {
	for _, pkg := range ws.Packages {
		if !samePath(pkg.ManifestPath, manifest) {
			continue
		}

		var bins []CargoTarget
		for _, t := range pkg.Targets {
			if t.isBin() {
				bins = append(bins, t)
			}
		}

		switch len(bins) {
		case 0:
			return nil, fmt.Errorf("%s has no binary target; a Rust Lambda needs a src/main.rs", handler)
		case 1:
			return &RustBin{Workspace: ws, Package: pkg.Name, Name: bins[0].Name}, nil
		default:
			names := make([]string, len(bins))
			for i, b := range bins {
				names[i] = b.Name
			}
			return nil, fmt.Errorf("%s has %d binaries (%s).\n  Point handler at one of them, e.g. handler: %q",
				handler, len(bins), strings.Join(names, ", "), suggestHandler(bins[0].SrcPath))
		}
	}
	return nil, fmt.Errorf("%s is not a member of the Cargo workspace at %s", handler, ws.Root)
}

func binForFile(ws *CargoWorkspace, handler, abs string) (*RustBin, error) {
	for _, pkg := range ws.Packages {
		for _, t := range pkg.Targets {
			if t.isBin() && samePath(t.SrcPath, abs) {
				return &RustBin{Workspace: ws, Package: pkg.Name, Name: t.Name}, nil
			}
		}
	}
	return nil, fmt.Errorf("%s is not the source of any binary in the Cargo workspace at %s", handler, ws.Root)
}

// suggestHandler writes a binary's source path relative to the working
// directory (the project root), so the suggestion can be pasted as-is.
func suggestHandler(srcPath string) string {
	if cwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(cwd, srcPath); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return srcPath
}

// SameWorkspace reports whether a and b are the same Cargo workspace.
func SameWorkspace(a, b *CargoWorkspace) bool {
	return samePath(a.Root, b.Root)
}

// samePath compares two paths after resolving symlinks (macOS temp dirs and
// /var vs /private/var would otherwise differ).
func samePath(a, b string) bool {
	return canonical(a) == canonical(b)
}

func canonical(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// RustBuildGroup is one cargo invocation: every binary in one workspace for
// one architecture. Cargo locks the target directory, so separate builds per
// function would only queue; one grouped run also compiles shared
// dependencies once.
type RustBuildGroup struct {
	Workspace    *CargoWorkspace
	Architecture string
	Bins         []string
}

// GroupRustBins groups resolved binaries by (workspace root, architecture).
// Each bin appears once per group even if several functions use it. Groups
// and their bins are sorted so builds run in a stable order.
//
// Cargo selects binaries by name across the workspace, so two packages with
// a binary of the same name can't be built apart: that is an error.
func GroupRustBins(bins []*RustBin, architectures []string) ([]RustBuildGroup, error) {
	type key struct{ root, arch string }
	index := map[key]int{}
	owner := map[key]map[string]string{} // bin name -> package
	var groups []RustBuildGroup

	for i, b := range bins {
		k := key{canonical(b.Workspace.Root), architectures[i]}
		gi, ok := index[k]
		if !ok {
			gi = len(groups)
			index[k] = gi
			owner[k] = map[string]string{}
			groups = append(groups, RustBuildGroup{Workspace: b.Workspace, Architecture: architectures[i]})
		}

		if pkg, seen := owner[k][b.Name]; seen {
			if pkg != b.Package {
				return nil, fmt.Errorf("packages %s and %s both have a binary named %q; rename one ([[bin]] name) so Cargo can build them apart", pkg, b.Package, b.Name)
			}
			continue
		}
		owner[k][b.Name] = b.Package
		groups[gi].Bins = append(groups[gi].Bins, b.Name)
	}

	for i := range groups {
		sort.Strings(groups[i].Bins)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Workspace.Root != groups[j].Workspace.Root {
			return groups[i].Workspace.Root < groups[j].Workspace.Root
		}
		return groups[i].Architecture < groups[j].Architecture
	})
	return groups, nil
}
