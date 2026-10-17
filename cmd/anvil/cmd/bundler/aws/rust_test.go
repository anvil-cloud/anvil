package aws

import (
	"archive/zip"
	"bytes"
	"debug/elf"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/anvil-cloud/anvil/cmd/anvil/cmd/bundler"
)

func testGroup() bundler.RustBuildGroup {
	return bundler.RustBuildGroup{
		Workspace:    &bundler.CargoWorkspace{Root: "/proj", TargetDir: "/proj/target"},
		Architecture: "arm64",
		Bins:         []string{"api", "worker"},
	}
}

func TestLocalBuildCommand(t *testing.T) {
	name, args, lambdaDir := localBuildCommand(testGroup(), "aarch64-unknown-linux-gnu")

	if name != "cargo" {
		t.Errorf("name = %q", name)
	}
	want := "lambda build --release --target aarch64-unknown-linux-gnu --manifest-path /proj/Cargo.toml --lambda-dir /proj/target/lambda/arm64 --bin api --bin worker"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("args =\n  %s\nwant\n  %s", got, want)
	}
	if lambdaDir != "/proj/target/lambda/arm64" {
		t.Errorf("lambdaDir = %q", lambdaDir)
	}
}

func TestDockerBuildCommand(t *testing.T) {
	t.Setenv("CARGO_HOME", t.TempDir())
	name, args, lambdaDir := dockerBuildCommand(testGroup(), "aarch64-unknown-linux-gnu")
	joined := strings.Join(args, " ")

	if name != "docker" {
		t.Errorf("name = %q", name)
	}
	for _, want := range []string{
		"run --rm",
		"-v /proj:/workspace -w /workspace",
		"/registry:/usr/local/cargo/registry",
		"/git:/usr/local/cargo/git",
		"-e CARGO_TARGET_DIR=/workspace/target/docker",
		"--entrypoint cargo " + cargoLambdaImage + " lambda build --release --target aarch64-unknown-linux-gnu",
		"--lambda-dir /workspace/target/docker/lambda/arm64 --bin api --bin worker",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker args missing %q:\n  %s", want, joined)
		}
	}
	if !strings.Contains(cargoLambdaImage, "@sha256:") {
		t.Errorf("image %q is not pinned by digest", cargoLambdaImage)
	}
	if lambdaDir != "/proj/target/docker/lambda/arm64" {
		t.Errorf("lambdaDir = %q", lambdaDir)
	}
}

func TestSelectRustBuilder(t *testing.T) {
	tests := []struct {
		name       string
		onPath     []string
		version    string // `cargo lambda --version` output; "" makes the command fail
		dockerUp   bool
		wantDocker bool
		wantErr    string
	}{
		{"current cargo-lambda", []string{"cargo-lambda", "docker"}, "cargo-lambda 1.9.2 (67129fc 2026-08-21Z)\n", true, false, ""},
		{"newer cargo-lambda", []string{"cargo-lambda"}, "cargo-lambda 1.10.0 (abc 2026-12-01Z)\n", false, false, ""},
		{"next major", []string{"cargo-lambda"}, "cargo-lambda 2.0.0\n", false, false, ""},
		{"old cargo-lambda falls back to Docker", []string{"cargo-lambda", "docker"}, "cargo-lambda 1.8.0 (5d84825 2025-03-15Z)\n", true, true, ""},
		{"old cargo-lambda without Docker", []string{"cargo-lambda", "docker"}, "cargo-lambda 1.8.0 (5d84825 2025-03-15Z)\n", false, false, "cargo-lambda 1.8.0 is too old to build Rust Lambdas (1.9.2 or newer is needed)"},
		{"unreadable version builds locally", []string{"cargo-lambda"}, "something unexpected\n", false, false, ""},
		{"version command fails builds locally", []string{"cargo-lambda"}, "", false, false, ""},
		{"no cargo-lambda falls back to Docker", []string{"docker"}, "", true, true, ""},
		{"Docker installed but not running", []string{"docker"}, "", false, false, "need cargo-lambda or Docker"},
		{"neither", nil, "", false, false, "need cargo-lambda or Docker"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origLook, origRun := lookPath, runCommand
			t.Cleanup(func() { lookPath, runCommand = origLook, origRun })

			lookPath = func(file string) (string, error) {
				for _, p := range tt.onPath {
					if p == file {
						return "/usr/bin/" + file, nil
					}
				}
				return "", exec.ErrNotFound
			}
			runCommand = func(name string, args ...string) ([]byte, error) {
				switch {
				case name == "docker" && args[0] == "info":
					if !tt.dockerUp {
						return nil, errors.New("Cannot connect to the Docker daemon")
					}
				case name == "cargo" && args[0] == "lambda" && args[1] == "--version":
					if tt.version == "" {
						return nil, errors.New("no such command")
					}
					return []byte(tt.version), nil
				}
				return nil, nil
			}

			useDocker, err := selectRustBuilder()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), cargoLambdaInstallURL) {
					t.Errorf("error should link the install page: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if useDocker != tt.wantDocker {
				t.Errorf("useDocker = %v, want %v", useDocker, tt.wantDocker)
			}
		})
	}
}

func TestDockerImageMeetsMinimumCargoLambda(t *testing.T) {
	m := regexp.MustCompile(`cargo-lambda:(\d+)\.(\d+)\.(\d+)@`).FindStringSubmatch(cargoLambdaImage)
	if m == nil {
		t.Fatalf("can't read the version tag of %s", cargoLambdaImage)
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	if versionLess(v, minCargoLambdaVersion) {
		t.Errorf("Docker image %s is older than the minimum cargo-lambda %s", formatVersion(v), formatVersion(minCargoLambdaVersion))
	}
}

func TestBuildRustLambdasRejectsUnknownArchitecture(t *testing.T) {
	_, failed := BuildRustLambdas([]RustFunction{{Name: "api", Handler: "functions/api", Architecture: "riscv"}})
	if err := failed["api"]; err == nil || !strings.Contains(err.Error(), `unsupported architecture "riscv"`) {
		t.Fatalf("error = %v", err)
	}
}

// TestBuildRustLambdasReal compiles a two-member workspace with the real
// cargo lambda. It downloads crates and takes a minute or two, so it only
// runs when ANVIL_RUST_E2E=1.
func TestBuildRustLambdasReal(t *testing.T) {
	if os.Getenv("ANVIL_RUST_E2E") != "1" {
		t.Skip("set ANVIL_RUST_E2E=1 to run a real cargo lambda build")
	}

	root := t.TempDir()
	write := func(rel, contents string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Cargo.toml", `[workspace]
resolver = "3"
members = ["functions/api", "functions/worker"]

[workspace.package]
edition = "2024"

[workspace.dependencies]
lambda_runtime = "1"
tokio = { version = "1", features = ["macros"] }
serde_json = "1"
`)
	for _, name := range []string{"api", "worker"} {
		write("functions/"+name+"/Cargo.toml", `[package]
name = "`+name+`"
version = "0.1.0"
edition.workspace = true

[dependencies]
lambda_runtime = { workspace = true }
tokio = { workspace = true }
serde_json = { workspace = true }
`)
		write("functions/"+name+"/src/main.rs", `use lambda_runtime::{run, service_fn, Error, LambdaEvent};
use serde_json::{json, Value};

async fn handler(_event: LambdaEvent<Value>) -> Result<Value, Error> {
    Ok(json!({ "ok": true }))
}

#[tokio::main]
async fn main() -> Result<(), Error> {
    run(service_fn(handler)).await
}
`)
	}

	t.Chdir(root)
	results, failed := BuildRustLambdas([]RustFunction{
		{Name: "api", Handler: "functions/api", Architecture: "arm64"},
		{Name: "worker", Handler: "functions/worker/src/main.rs", Architecture: "arm64"},
	})
	for name, err := range failed {
		t.Fatalf("%s failed: %v", name, err)
	}

	for _, name := range []string{"api", "worker"} {
		r, err := zip.OpenReader(results[name].ZipPath)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if len(r.File) != 1 || r.File[0].Name != "bootstrap" || r.File[0].Mode().Perm() != 0o755 {
			t.Fatalf("%s zip: want one 0755 bootstrap, got %v", name, r.File)
		}

		// It must be a Linux arm64 executable, not a host (macOS) binary.
		f, err := r.File[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		binary, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		exe, err := elf.NewFile(bytes.NewReader(binary))
		if err != nil {
			t.Fatalf("%s bootstrap is not a Linux (ELF) binary: %v", name, err)
		}
		if exe.Machine != elf.EM_AARCH64 {
			t.Errorf("%s bootstrap is built for %v, want aarch64", name, exe.Machine)
		}
	}
}
