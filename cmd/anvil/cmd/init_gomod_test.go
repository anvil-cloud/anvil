package cmd

import (
	"errors"
	"strings"
	"testing"
)

func TestGoSDKVersion(t *testing.T) {
	noLatest := func() (string, error) { return "", errors.New("offline") }
	latest := func(v string) func() (string, error) {
		return func() (string, error) { return v, nil }
	}
	mustNotCall := func() (string, error) {
		t.Fatal("latest() must not be called when the CLI has a release version")
		return "", nil
	}

	tests := []struct {
		name      string
		cli       string
		buildInfo string
		latest    func() (string, error)
		want      string
	}{
		{"release CLI version", "0.1.22", "", mustNotCall, "v0.1.22"},
		{"release CLI version with v prefix", "v0.1.22", "", mustNotCall, "v0.1.22"},
		{"go install build info", "dev", "v0.1.21", mustNotCall, "v0.1.21"},
		{"dev build uses latest published", "dev", "(devel)", latest("v0.1.22\n"), "v0.1.22"},
		{"pseudo-version build info ignored", "dev", "v0.1.23-0.20261024000000-abcdef123456", latest("v0.1.22"), "v0.1.22"},
		{"dirty build info ignored", "dev", "v0.1.22+dirty", latest("v0.1.22"), "v0.1.22"},
		{"prerelease CLI ignored", "0.2.0-rc.1", "", latest("v0.1.22"), "v0.1.22"},
		{"dev build offline leaves it to tidy", "dev", "(devel)", noLatest, ""},
		{"garbage from latest ignored", "dev", "", latest("not a version"), ""},
		{"nil latest", "dev", "", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := goSDKVersion(tt.cli, tt.buildInfo, tt.latest); got != tt.want {
				t.Errorf("goSDKVersion(%q, %q) = %q, want %q", tt.cli, tt.buildInfo, got, tt.want)
			}
		})
	}
}

func TestGoMod(t *testing.T) {
	got := goMod("github.com/acme/demo", "v0.1.22")
	want := "module github.com/acme/demo\n\ngo 1.22\n\nrequire github.com/anvil-cloud/anvil/sdk/go/anvil v0.1.22\n"
	if got != want {
		t.Errorf("goMod() =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "pulumi/pulumi/sdk") {
		t.Error("goMod() must not pin the Pulumi SDK; go mod tidy takes it from the Anvil SDK's requirements")
	}
	if strings.Contains(got, "DamienPace15") {
		t.Error("goMod() references the old module path")
	}
}

func TestGoModDevFallback(t *testing.T) {
	// No resolvable version (dev build, offline): no require line, so
	// `go mod tidy` resolves the SDK itself rather than a stale pin.
	got := goMod("github.com/acme/demo", goSDKVersion("dev", "(devel)", func() (string, error) {
		return "", errors.New("offline")
	}))
	want := "module github.com/acme/demo\n\ngo 1.22\n"
	if got != want {
		t.Errorf("goMod() =\n%q\nwant\n%q", got, want)
	}
}
