package aws

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/anvil-cloud/anvil/cmd/anvil/cmd/bundler"
)

// cargoLambdaImage is the Docker image used when cargo-lambda isn't
// installed. Pinned by digest so an outside image can't change what gets
// compiled into users' Lambdas; bump it deliberately with Anvil releases.
const cargoLambdaImage = "ghcr.io/cargo-lambda/cargo-lambda:1.9.2@sha256:aa011e79594474d5252cd1e251c0ce21c19c6cc094fbd557857855fd6bd018e0"

// cargoLambdaInstallURL is linked from errors. cargo-lambda is no longer on
// crates.io, so `cargo install cargo-lambda` doesn't work.
const cargoLambdaInstallURL = "https://www.cargo-lambda.info/guide/installation.html"

// RustFunction is one Rust Lambda from the build manifest.
type RustFunction struct {
	Name         string
	Handler      string // package directory or .rs file
	Architecture string // "arm64" or "x86_64"
}

// Hooks for tests: finding executables and running commands.
var (
	lookPath   = exec.LookPath
	runCommand = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
)

// BuildRustLambdas builds every Rust function and zips each binary as
// bootstrap at .anvil/build/{name}.zip. Functions are grouped so each
// (workspace, architecture) pair is one cargo lambda run.
//
// Returns the successful builds and, separately, an error per failed
// function. A failed group fails every function in it.
func BuildRustLambdas(fns []RustFunction) (map[string]*BuildResult, map[string]error) {
	results := map[string]*BuildResult{}
	failed := map[string]error{}
	if len(fns) == 0 {
		return results, failed
	}

	// ── Resolve handlers to binaries ───────────────────
	var resolved []RustFunction
	var bins []*bundler.RustBin
	var archs []string
	for _, fn := range fns {
		if _, err := targetTriple(fn.Architecture); err != nil {
			failed[fn.Name] = err
			continue
		}
		bin, err := bundler.ResolveRustBin(fn.Handler)
		if err != nil {
			failed[fn.Name] = err
			continue
		}
		resolved = append(resolved, fn)
		bins = append(bins, bin)
		archs = append(archs, fn.Architecture)
	}
	if len(resolved) == 0 {
		return results, failed
	}

	failAll := func(err error) {
		for _, fn := range resolved {
			failed[fn.Name] = err
		}
	}

	groups, err := bundler.GroupRustBins(bins, archs)
	if err != nil {
		failAll(err)
		return results, failed
	}

	useDocker, err := selectRustBuilder()
	if err != nil {
		failAll(err)
		return results, failed
	}

	// ── One cargo lambda run per group ─────────────────
	for _, g := range groups {
		label := strings.Join(g.Bins, ", ")
		how := "cargo lambda"
		if useDocker {
			how = "cargo lambda in Docker"
		}
		fmt.Printf("  🔨 Building %s (%s, %s)...\n", label, how, g.Architecture)

		lambdaDir, buildErr := runRustBuild(g, useDocker)

		// ── Zip each function's binary ─────────────────
		for i, fn := range resolved {
			if !bundler.SameWorkspace(bins[i].Workspace, g.Workspace) || archs[i] != g.Architecture {
				continue
			}
			if buildErr != nil {
				failed[fn.Name] = buildErr
				continue
			}

			binary, err := os.ReadFile(filepath.Join(lambdaDir, bins[i].Name, "bootstrap"))
			if err != nil {
				failed[fn.Name] = fmt.Errorf("cargo lambda produced no bootstrap for %s: %w", bins[i].Name, err)
				continue
			}

			zipPath := bundler.ZipFilePath(fn.Name)
			if err := bundler.ZipFile(zipPath, "bootstrap", binary, 0o755); err != nil {
				failed[fn.Name] = fmt.Errorf("failed to zip %s: %w", fn.Name, err)
				continue
			}

			results[fn.Name] = &BuildResult{FunctionName: fn.Name, ZipPath: zipPath}
			fmt.Printf("  ✅ %s built successfully\n", fn.Name)
		}
	}

	return results, failed
}

// targetTriple maps Anvil's architecture to the Rust target Lambda runs.
func targetTriple(architecture string) (string, error) {
	switch architecture {
	case "arm64":
		return "aarch64-unknown-linux-gnu", nil
	case "x86_64":
		return "x86_64-unknown-linux-gnu", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q for a Rust Lambda (use arm64 or x86_64)", architecture)
	}
}

// minCargoLambdaVersion is the oldest local cargo-lambda that can build
// against the Rust that new workspaces pin. Older releases bundle a
// cargo-zigbuild that passes Rust 1.99's -Wl,--fix-cortex-a53-843419 to Zig
// for aarch64-linux (which rejects it), and fail to link aws-lc-sys, which
// the AWS SDK uses. The pinned Docker image (cargoLambdaImage) meets it.
var minCargoLambdaVersion = [3]int{1, 9, 2}

var cargoLambdaVersionRe = regexp.MustCompile(`cargo-lambda (\d+)\.(\d+)\.(\d+)`)

// selectRustBuilder picks local cargo-lambda when it's new enough, then
// Docker. A local Rust toolchain is required either way (cargo metadata
// already ran by now).
func selectRustBuilder() (useDocker bool, err error) {
	if _, err := lookPath("cargo-lambda"); err != nil {
		if dockerRunning() {
			return true, nil
		}
		return false, errors.New("Rust Lambdas need cargo-lambda or Docker to build.\n" +
			"  Install cargo-lambda: " + cargoLambdaInstallURL + "\n" +
			"  (or start Docker to build in a container)")
	}

	version, ok := localCargoLambdaVersion()
	if !ok {
		// Don't block a build on our parsing of its output.
		fmt.Println("  ⚠️  Couldn't read the cargo-lambda version; building with it anyway")
		return false, nil
	}
	if !versionLess(version, minCargoLambdaVersion) {
		return false, nil
	}

	old := fmt.Sprintf("cargo-lambda %s is too old to build Rust Lambdas (%s or newer is needed)",
		formatVersion(version), formatVersion(minCargoLambdaVersion))
	if dockerRunning() {
		fmt.Printf("  ⚠️  %s; building in Docker instead\n", old)
		return true, nil
	}
	return false, errors.New(old + ".\n" +
		"  Upgrade: " + cargoLambdaInstallURL + " (Homebrew: brew install cargo-lambda/tap/cargo-lambda)\n" +
		"  (or start Docker to build in a container)")
}

// localCargoLambdaVersion parses `cargo lambda --version`, e.g.
// "cargo-lambda 1.9.2 (67129fc 2026-08-21Z)".
func localCargoLambdaVersion() ([3]int, bool) {
	out, err := runCommand("cargo", "lambda", "--version")
	if err != nil {
		return [3]int{}, false
	}
	m := cargoLambdaVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func formatVersion(v [3]int) string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

func dockerRunning() bool {
	if _, err := lookPath("docker"); err != nil {
		return false
	}
	_, err := runCommand("docker", "info", "--format", "{{.ServerVersion}}")
	return err == nil
}

// runRustBuild runs one group's build and returns the host directory holding
// <bin>/bootstrap for each binary.
func runRustBuild(g bundler.RustBuildGroup, useDocker bool) (string, error) {
	triple, _ := targetTriple(g.Architecture) // validated before grouping

	var name string
	var args []string
	var lambdaDir string
	if useDocker {
		name, args, lambdaDir = dockerBuildCommand(g, triple)
	} else {
		name, args, lambdaDir = localBuildCommand(g, triple)
	}

	out, err := runCommand(name, args...)
	if err != nil {
		return "", fmt.Errorf("cargo lambda build failed:\n%s", tail(string(out), 40))
	}
	return lambdaDir, nil
}

// localBuildCommand builds with the host's cargo-lambda into the workspace's
// own target directory. Binaries go to target/lambda/<arch>/<bin>, so they
// never collide across architectures or with the user's own
// `cargo lambda build` output (target/lambda/<bin>).
func localBuildCommand(g bundler.RustBuildGroup, triple string) (string, []string, string) {
	lambdaDir := filepath.Join(g.Workspace.TargetDir, "lambda", g.Architecture)
	args := []string{
		"lambda", "build", "--release",
		"--target", triple,
		"--manifest-path", filepath.Join(g.Workspace.Root, "Cargo.toml"),
		"--lambda-dir", lambdaDir,
	}
	for _, b := range g.Bins {
		args = append(args, "--bin", b)
	}
	return "cargo", args, lambdaDir
}

// dockerBuildCommand builds in the pinned cargo-lambda image with the
// workspace mounted at /workspace. It uses its own target dir
// (target/docker): the container's Linux host artifacts and the host's would
// otherwise keep invalidating each other's cache. The host's crate registry
// and git caches are mounted so crates aren't re-downloaded every build.
// Path dependencies outside the workspace root aren't visible in the
// container.
func dockerBuildCommand(g bundler.RustBuildGroup, triple string) (string, []string, string) {
	const mount = "/workspace"
	hostLambdaDir := filepath.Join(g.Workspace.Root, "target", "docker", "lambda", g.Architecture)
	containerLambdaDir := mount + "/target/docker/lambda/" + g.Architecture

	cargoHome := os.Getenv("CARGO_HOME")
	if cargoHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cargoHome = filepath.Join(home, ".cargo")
		}
	}

	args := []string{"run", "--rm"}
	if runtime.GOOS != "windows" {
		// Files in target/ owned by the user, not root (matters on Linux).
		args = append(args, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-e", "HOME=/tmp")
	}
	args = append(args, "-v", g.Workspace.Root+":"+mount, "-w", mount)
	if cargoHome != "" {
		for _, sub := range []string{"registry", "git"} {
			hostDir := filepath.Join(cargoHome, sub)
			// Create it first, or Docker creates it owned by root.
			_ = os.MkdirAll(hostDir, 0o755)
			args = append(args, "-v", hostDir+":/usr/local/cargo/"+sub)
		}
	}
	args = append(args,
		"-e", "CARGO_TARGET_DIR="+mount+"/target/docker",
		"--entrypoint", "cargo",
		cargoLambdaImage,
		"lambda", "build", "--release",
		"--target", triple,
		"--lambda-dir", containerLambdaDir,
	)
	for _, b := range g.Bins {
		args = append(args, "--bin", b)
	}
	return "docker", args, hostLambdaDir
}

// tail returns the last n lines of s; cargo's full output is long, and the
// error is at the end.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"  ..."}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}
