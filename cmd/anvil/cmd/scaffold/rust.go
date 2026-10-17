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
	"strings"
)

// RustFunction is one Rust Lambda from the build manifest.
type RustFunction struct {
	Name    string
	Handler string // package directory or .rs file, relative to the project root
}

// pinnedRustVersion is the toolchain new workspaces pin in
// rust-toolchain.toml (and their rust-version). Fixed rather than copied from
// the host's rustc, so every new project starts on the same Rust; rustup
// installs it on first use. Bump it deliberately with Anvil releases. It
// must stay >= 1.85 (edition 2024).
const pinnedRustVersion = "1.99.1"

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
)

// RustOptions tunes scaffolding.
type RustOptions struct {
	// DBCrate is the generated DSQL client crate's directory, when the
	// project has DSQL schemas. New packages get a path dependency on it;
	// existing packages are never edited to add one.
	DBCrate string
}

// Rust scaffolds every Rust function whose handler doesn't exist yet and
// returns one line per file or package it created, for the CLI to print.
// Functions that already exist are skipped silently.
func Rust(projectRoot string, fns []RustFunction, opts RustOptions) ([]string, error) {
	s := &rustScaffolder{root: projectRoot, opts: opts}
	for _, fn := range fns {
		if err := s.function(fn); err != nil {
			return s.created, fmt.Errorf("lambda %q: %w", fn.Name, err)
		}
	}
	return s.created, nil
}

type rustScaffolder struct {
	root    string
	opts    RustOptions
	created []string
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
	if err := runCargo(s.root, "init", "--bin", "--vcs", "none", "--name", packageName(fn.Name), path); err != nil {
		return err
	}

	if err := s.addDependencies(path, definesTemplateDeps(workspace)); err != nil {
		return err
	}
	if s.opts.DBCrate != "" {
		if err := runCargo(s.root, "add", "--manifest-path", filepath.Join(path, "Cargo.toml"), "--path", s.opts.DBCrate); err != nil {
			return err
		}
	}

	if !hadMain {
		if err := os.WriteFile(mainRS, []byte(handlerTemplate), 0o644); err != nil {
			return err
		}
	}

	s.note(fn.Handler + "/")
	return nil
}

// RustCrate makes dir a library package in the project's Cargo workspace
// (creating the workspace at the project root if there is none), for a
// generated crate like gen/db. It returns what it created; nothing if dir
// already has a Cargo.toml. The caller then writes the crate's files.
func RustCrate(projectRoot, dir string) ([]string, error) {
	if exists(filepath.Join(dir, "Cargo.toml")) {
		return nil, nil
	}

	s := &rustScaffolder{root: projectRoot}
	if _, err := s.ensureWorkspace(dir); err != nil {
		return s.created, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return s.created, err
	}
	// cargo init adds it to the workspace's members; the generator then
	// replaces the Cargo.toml and lib.rs it writes.
	if err := runCargo(projectRoot, "init", "--lib", "--vcs", "none", dir); err != nil {
		return s.created, err
	}

	rel, err := filepath.Rel(projectRoot, dir)
	if err != nil {
		rel = dir
	}
	s.note(filepath.ToSlash(rel) + "/")
	return s.created, nil
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

	if err := os.WriteFile(rootManifest, []byte(workspaceManifest(pinnedRustVersion)), 0o644); err != nil {
		return "", err
	}
	s.note("Cargo.toml (workspace)")

	toolchain := filepath.Join(s.root, "rust-toolchain.toml")
	if !exists(toolchain) {
		if err := os.WriteFile(toolchain, []byte(toolchainFile(pinnedRustVersion)), 0o644); err != nil {
			return "", err
		}
		s.note("rust-toolchain.toml")
	}

	if added, err := ignoreTarget(s.root); err != nil {
		return "", err
	} else if added {
		s.note(".gitignore (+ /target)")
	}

	return rootManifest, nil
}

// addDependencies adds the template's dependencies. cargo add inherits them
// from [workspace.dependencies] when the workspace defines them; features are
// passed only when it doesn't already enable the ones the template uses.
func (s *rustScaffolder) addDependencies(pkgDir string, workspaceHasThem bool) error {
	args := []string{"add", "--manifest-path", filepath.Join(pkgDir, "Cargo.toml"), "lambda_runtime", "tokio", "serde_json"}
	if !workspaceHasThem {
		args = append(args, "--features", "lambda_runtime/tracing,tokio/macros")
	}
	return runCargo(s.root, args...)
}

var (
	workspaceLambdaRuntime = regexp.MustCompile(`(?m)^\s*lambda_runtime\s*=.*"tracing"`)
	workspaceTokio         = regexp.MustCompile(`(?m)^\s*tokio\s*=.*"macros"`)
)

// definesTemplateDeps reports whether a workspace manifest already defines
// lambda_runtime and tokio with the features the handler template needs, as
// the one Anvil creates does. A line-based check is enough for that shape;
// anything else just gets the features passed explicitly, which is harmless.
func definesTemplateDeps(manifest string) bool {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return false
	}
	return workspaceLambdaRuntime.Match(data) && workspaceTokio.Match(data)
}

func (s *rustScaffolder) note(rel string) {
	s.created = append(s.created, rel)
}

// ── Package names ──────────────────────────────────────────

// reservedPackageNames are names Cargo rejects for a package (they clash with
// Rust's built-in crates or keywords), plus "db", the generated DSQL client.
var reservedPackageNames = map[string]bool{
	"test": true, "core": true, "std": true, "alloc": true, "proc-macro": true, "proc_macro": true,
	"db": true,
	"as": true, "async": true, "await": true, "break": true, "const": true, "continue": true,
	"crate": true, "dyn": true, "else": true, "enum": true, "extern": true, "false": true,
	"fn": true, "for": true, "gen": true, "if": true, "impl": true, "in": true, "let": true,
	"loop": true, "match": true, "mod": true, "move": true, "mut": true, "pub": true,
	"ref": true, "return": true, "self": true, "static": true, "struct": true, "super": true,
	"trait": true, "true": true, "try": true, "type": true, "unsafe": true, "use": true,
	"where": true, "while": true, "yield": true, "abstract": true, "become": true, "box": true,
	"do": true, "final": true, "macro": true, "override": true, "priv": true, "typeof": true,
	"unsized": true, "virtual": true,
}

var nonPackageChars = regexp.MustCompile(`[^a-z0-9]+`)

// packageName is the Cargo package name for a Lambda: its Anvil name in
// kebab-case ("testRust" → "test-rust"), so the handler's directory can be
// called anything — even a name Cargo reserves, like "test". A Lambda whose
// own name is reserved gets a "-fn" suffix.
func packageName(lambdaName string) string {
	isUpper := func(r rune) bool { return r >= 'A' && r <= 'Z' }
	isLowerOrDigit := func(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') }

	// Split camelCase at word starts, keeping runs of capitals together:
	// "APIHandler" → "api-handler", "orderV2" → "order-v2".
	runes := []rune(lambdaName)
	var b strings.Builder
	for i, r := range runes {
		if isUpper(r) && i > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
			if isLowerOrDigit(prev) || (isUpper(prev) && nextLower) {
				b.WriteByte('-')
			}
		}
		if isUpper(r) {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	name := strings.Trim(nonPackageChars.ReplaceAllString(b.String(), "-"), "-")

	switch {
	case name == "":
		name = "lambda"
	case name[0] >= '0' && name[0] <= '9':
		name = "fn-" + name
	case reservedPackageNames[name]:
		name += "-fn"
	}
	return name
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
