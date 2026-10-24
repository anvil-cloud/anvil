package cmd

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestAnvilYAML creates anvil.yaml in a temp dir and chdirs into it,
// since loadAnvilConfig reads ./anvil.yaml.
func writeTestAnvilYAML(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "anvil.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

const twoStageYAML = `project: demo
active: alice
stages:
  alice:
    id: abc123
    region: us-east-1
    environment: nonprod
  prod:
    id: def456
    region: us-east-1
    environment: prod
`

func TestStageFlagsDefaultEmpty(t *testing.T) {
	for _, c := range []struct {
		name string
		f    func() string
	}{
		{"bootstrap", func() string { return bootstrapCmd.Flags().Lookup("stage").DefValue }},
		{"unlock", func() string { return unlockCmd.Flags().Lookup("stage").DefValue }},
	} {
		if got := c.f(); got != "" {
			t.Errorf("%s --stage default = %q, want empty (resolved via resolveStage)", c.name, got)
		}
	}
}

func TestResolveStageUsesFlagThenActive(t *testing.T) {
	writeTestAnvilYAML(t, twoStageYAML)

	if got := resolveStage("prod"); got != "prod" {
		t.Errorf("resolveStage(\"prod\") = %q, want prod", got)
	}
	if got := resolveStage(""); got != "alice" {
		t.Errorf("resolveStage(\"\") = %q, want active stage alice", got)
	}
}

func TestResolveStageFallsBackToUsername(t *testing.T) {
	writeTestAnvilYAML(t, "project: demo\nstages: {}\n")

	u, err := user.Current()
	if err != nil || u.Username == "" {
		t.Skip("no OS username available")
	}
	if got := resolveStage(""); got != u.Username {
		t.Errorf("resolveStage(\"\") = %q, want OS username %q", got, u.Username)
	}
}

func TestResolveUnlockStage(t *testing.T) {
	writeTestAnvilYAML(t, twoStageYAML)

	if got, err := resolveUnlockStage(""); err != nil || got != "alice" {
		t.Errorf("resolveUnlockStage(\"\") = %q, %v; want alice, nil", got, err)
	}
	if got, err := resolveUnlockStage("prod"); err != nil || got != "prod" {
		t.Errorf("resolveUnlockStage(\"prod\") = %q, %v; want prod, nil", got, err)
	}

	_, err := resolveUnlockStage("nope")
	if err == nil || !strings.Contains(err.Error(), `stage "nope" doesn't exist`) {
		t.Errorf("resolveUnlockStage(\"nope\") err = %v; want doesn't-exist error", err)
	}
}

func TestResolveUnlockStageNoConfig(t *testing.T) {
	writeTestAnvilYAML(t, "")

	if _, err := resolveUnlockStage("dev"); err == nil || !strings.Contains(err.Error(), "nothing to unlock") {
		t.Errorf("resolveUnlockStage without anvil.yaml err = %v; want nothing-to-unlock error", err)
	}
	if _, err := os.Stat("anvil.yaml"); !os.IsNotExist(err) {
		t.Errorf("anvil.yaml was created as a side effect")
	}
}
