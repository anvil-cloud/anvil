package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrgStateBucketName_ValidAndStable(t *testing.T) {
	got := orgStateBucketName("o-ab12cd34ef")
	if want := "anvil-org-state-o-ab12cd34ef"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !validBucketName.MatchString(got) {
		t.Errorf("%q is not a valid bucket name", got)
	}
}

func TestOrgStateBucketPolicy_DeniesNonTLSAndHistoryDeletes(t *testing.T) {
	var doc struct {
		Statement []struct {
			Sid    string `json:"Sid"`
			Effect string `json:"Effect"`
			Action any    `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(orgStateBucketPolicy("anvil-org-state-o-x")), &doc); err != nil {
		t.Fatalf("policy isn't valid JSON: %v", err)
	}
	sids := map[string]bool{}
	for _, s := range doc.Statement {
		if s.Effect != "Deny" {
			t.Errorf("%s: want Deny, got %s", s.Sid, s.Effect)
		}
		sids[s.Sid] = true
	}
	for _, want := range []string{"DenyNonTLS", "DenyDeletingStateHistory"} {
		if !sids[want] {
			t.Errorf("missing statement %s", want)
		}
	}
}

func TestReadProjectKind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if kind, err := readProjectKind(path); err != nil || kind != "" {
		t.Errorf("missing file: got %q, %v; want \"\", nil", kind, err)
	}
	for content, want := range map[string]string{
		`{"kind":"org"}`:                 "org",
		`{"state":{"retainVersions":3}}`: "app",
		`{"kind":"org","state":{"x":1}}`: "org",
	} {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if got, err := readProjectKind(path); err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", content, got, err, want)
		}
	}
}

func TestFindOrgRoot(t *testing.T) {
	inDir := func(t *testing.T, dir string) {
		t.Helper()
		old, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chdir(old) })
	}

	t.Run("finds anvil.org.yaml from a subdirectory", func(t *testing.T) {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, orgConfigFile), []byte("project: acme-org\n"), 0644)
		sub := filepath.Join(root, "policies")
		os.Mkdir(sub, 0755)
		inDir(t, sub)
		got, err := findOrgRoot()
		if err != nil {
			t.Fatal(err)
		}
		want, _ := filepath.EvalSymlinks(root)
		if got, _ = filepath.EvalSymlinks(got); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("App project points to anvil deploy", func(t *testing.T) {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "anvil.yaml"), []byte("project: app\n"), 0644)
		inDir(t, root)
		if _, err := findOrgRoot(); err == nil || !strings.Contains(err.Error(), "defines an App") {
			t.Errorf("got %v, want an App error", err)
		}
	})

	t.Run("both files is an error", func(t *testing.T) {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "anvil.yaml"), []byte("project: app\n"), 0644)
		os.WriteFile(filepath.Join(root, orgConfigFile), []byte("project: org\n"), 0644)
		inDir(t, root)
		if _, err := findOrgRoot(); err == nil || !strings.Contains(err.Error(), "both") {
			t.Errorf("got %v, want a both-files error", err)
		}
	})
}

func TestFindProjectRoot_RefusesOrgProject(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, orgConfigFile), []byte("project: acme-org\n"), 0644)
	old, _ := os.Getwd()
	os.Chdir(root)
	t.Cleanup(func() { os.Chdir(old) })

	if _, err := findProjectRoot(); err == nil || !strings.Contains(err.Error(), "anvil org deploy") {
		t.Errorf("got %v, want a pointer to anvil org deploy", err)
	}
}

func TestOrgConfig_RoundTrip(t *testing.T) {
	root := t.TempDir()
	in := &orgFileConfig{Project: "acme-org", Region: "ap-southeast-2", State: &orgStateConfig{Bucket: "anvil-org-state-o-x"}}
	if err := writeOrgConfig(root, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadOrgConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if out.Project != in.Project || out.Region != in.Region || out.State == nil || out.State.Bucket != in.State.Bucket {
		t.Errorf("round trip lost data: %+v", out)
	}
}

func TestProgramError(t *testing.T) {
	out := "Diagnostics:\n  pulumi:pulumi:Stack (testorg-org):\n    Error: ou['A/B']: parent OU 'A' doesn't exist.\n        at planOrgOus (org.js:10)\n"
	if got, want := programError(out), "ou['A/B']: parent OU 'A' doesn't exist."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := programError("no errors here"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
