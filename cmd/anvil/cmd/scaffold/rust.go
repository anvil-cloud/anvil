// Package scaffold creates the code for functions declared in the Anvil
// config that don't exist yet. It only ever creates: existing user code is
// never edited or deleted (the one exception is Cargo adding a new package
// to the workspace's members).
package scaffold

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// RustFunction is one Rust Lambda from the build manifest.
type RustFunction struct {
	Name    string
	Handler string // package directory or .rs file, relative to the project root
}

// minRustVersion is the oldest toolchain the generated workspace supports:
// edition 2024 needs 1.85 (lambda_runtime itself needs 1.84).
var minRustVersion = [3]int{1, 85, 0}

// Hooks for tests.
var (
	runCargo = func(dir string, args ...string) error {
		cmd := exec.Command("cargo", args...)
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if _, lookErr := exec.LookPath("cargo"); lookErr != nil {
				return fmt.Errorf("Rust Lambdas need the Rust toolchain (cargo not found): https://rustup.rs")
			}
			return fmt.Errorf("cargo %s failed: %s", args[0], strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	rustcVersion = func() (string, error) {
		out, err := exec.Command("rustc", "--version").Output()
		if err != nil {
			return "", fmt.Errorf("Rust Lambdas need the Rust toolchain (rustc not found): https://rustup.rs")
		}
		return string(out), nil
	}
)

// Rust scaffolds every Rust function whose handler doesn't exist yet and
// returns one line per file or package it created, for the CLI to print.
// Functions that already exist are skipped silently.
func Rust(projectRoot string, fns []RustFunction) ([]string, error) {
	s := &rustScaffolder{root: projectRoot}
	for _, fn := range fns {
		if err := s.function(fn); err != nil {
			return s.created, fmt.Errorf("lambda %q: %w", fn.Name, err)
		}
	}
	return s.created, nil
}

type rustScaffolder struct {
	root    string
	created []string

	// createdManifest is the workspace Cargo.toml this run created, if any.
	// Its [workspace.dependencies] already carry the features new packages
	// need; a workspace Anvil didn't create may not.
	createdManifest string
}

func (s *rustScaffolder) function(fn RustFunction) error {
	path := filepath.Join(s.root, fn.Handler)

	if filepath.Ext(path) == ".rs" {
		return s.binFile(fn, path)
	}

	if exists(filepath.Join(path, "Cargo.toml")) {
		return nil // already a package: user code
	}

	workspace, err := s.ensureWorkspace(path)
	if err != nil {
		return err
	}

	// cargo init, not cargo new: it also works when the directory exists,
	// keeps any src/main.rs already there, and adds the package to the
	// enclosing workspace's members.
	mainRS := filepath.Join(path, "src", "main.rs")
	hadMain := exists(mainRS)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := runCargo(s.root, "init", "--bin", "--vcs", "none", path); err != nil {
		return err
	}

	if err := s.addDependencies(path, workspace == s.createdManifest); err != nil {
		return err
	}

	if !hadMain {
		if err := os.WriteFile(mainRS, []byte(handlerTemplate), 0o644); err != nil {
			return err
		}
	}

	s.note(fn.Handler + "/")
	return nil
}

// binFile handles a .rs handler: inside an existing package, Cargo finds
// src/bin/*.rs on its own, so only the file is created.
func (s *rustScaffolder) binFile(fn RustFunction, path string) error {
	if exists(path) {
		return nil
	}
	if _, err := packageAbove(path); err != nil {
		return fmt.Errorf("%s: no Cargo package above this file to put it in; "+
			"point handler at a package directory (e.g. %q) to have one created", fn.Handler, "functions/"+fn.Name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(handlerTemplate), 0o644); err != nil {
		return err
	}
	s.note(fn.Handler)
	return nil
}

// ensureWorkspace returns the workspace manifest path will join: the nearest
// one above it, or a new one at the project root (Cargo's convention: one
// workspace per repository).
func (s *rustScaffolder) ensureWorkspace(path string) (string, error) {
	if manifest, found := workspaceAbove(path); found {
		return manifest, nil
	}

	rootManifest := filepath.Join(s.root, "Cargo.toml")
	if exists(rootManifest) {
		// It's there but has no [workspace] (or workspaceAbove would have
		// found it): a user's package. Converting it isn't ours to do.
		return "", fmt.Errorf("Cargo.toml at the project root is a package, not a workspace. " +
			"Add a [workspace] section to it (with the Lambda packages in members), or move it into its own directory")
	}

	version, err := toolchainVersion()
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(rootManifest, []byte(workspaceManifest(version)), 0o644); err != nil {
		return "", err
	}
	s.note("Cargo.toml (workspace)")

	toolchain := filepath.Join(s.root, "rust-toolchain.toml")
	if !exists(toolchain) {
		if err := os.WriteFile(toolchain, []byte(toolchainFile(version)), 0o644); err != nil {
			return "", err
		}
		s.note("rust-toolchain.toml")
	}

	if added, err := ignoreTarget(s.root); err != nil {
		return "", err
	} else if added {
		s.note(".gitignore (+ /target)")
	}

	s.createdManifest = rootManifest
	return rootManifest, nil
}

func (s *rustScaffolder) addDependencies(pkgDir string, ownWorkspace bool) error {
	args := []string{"add", "--manifest-path", filepath.Join(pkgDir, "Cargo.toml"), "lambda_runtime", "tokio", "serde_json"}
	if !ownWorkspace {
		// A workspace Anvil didn't create may not define these with the
		// features the template uses.
		args = append(args, "--features", "lambda_runtime/tracing,tokio/macros")
	}
	return runCargo(s.root, args...)
}

func (s *rustScaffolder) note(rel string) {
	s.created = append(s.created, rel)
}

// ── Cargo.toml inspection ──────────────────────────────────

var (
	workspaceHeader = regexp.MustCompile(`(?m)^\s*\[workspace[\].]`)
	packageHeader   = regexp.MustCompile(`(?m)^\s*\[package\]`)
)

// workspaceAbove finds the nearest Cargo.toml with a [workspace] table at or
// above dir, the same search Cargo does.
func workspaceAbove(dir string) (string, bool) {
	for d := dir; ; d = filepath.Dir(d) {
		manifest := filepath.Join(d, "Cargo.toml")
		if data, err := os.ReadFile(manifest); err == nil && workspaceHeader.Match(data) {
			return manifest, true
		}
		if parent := filepath.Dir(d); parent == d {
			return "", false
		}
	}
}

// packageAbove finds the nearest Cargo.toml with a [package] table above a
// file.
func packageAbove(file string) (string, error) {
	for d := filepath.Dir(file); ; d = filepath.Dir(d) {
		manifest := filepath.Join(d, "Cargo.toml")
		if data, err := os.ReadFile(manifest); err == nil && packageHeader.Match(data) {
			return manifest, nil
		}
		if parent := filepath.Dir(d); parent == d {
			return "", fmt.Errorf("no package found")
		}
	}
}

// ── Toolchain ──────────────────────────────────────────────

var rustcVersionRe = regexp.MustCompile(`rustc (\d+)\.(\d+)\.(\d+)`)

// toolchainVersion is the host's rustc version, which the generated
// rust-toolchain.toml pins: it's already installed, and it's what the user
// is developing with. It must be new enough for edition 2024.
func toolchainVersion() (string, error) {
	out, err := rustcVersion()
	if err != nil {
		return "", err
	}
	m := rustcVersionRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("could not read the Rust version from %q", strings.TrimSpace(out))
	}

	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	if v[0] < minRustVersion[0] || (v[0] == minRustVersion[0] && v[1] < minRustVersion[1]) {
		return "", fmt.Errorf("Rust %d.%d.%d is too old for Rust Lambdas (edition 2024 needs %d.%d or newer): run `rustup update`",
			v[0], v[1], v[2], minRustVersion[0], minRustVersion[1])
	}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]), nil
}

// ── .gitignore ─────────────────────────────────────────────

// ignoreTarget adds /target to the project's .gitignore unless it's already
// ignored. Cargo.lock is deliberately not ignored: for deployed binaries it
// pins exactly what ships.
func ignoreTarget(root string) (bool, error) {
	path := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		switch strings.TrimSpace(line) {
		case "target", "target/", "/target", "/target/":
			return false, nil
		}
	}

	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	data = append(data, []byte("/target\n")...)
	return true, os.WriteFile(path, data, 0o644)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
