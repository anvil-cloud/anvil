package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
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

func TestOrgInit_ScaffoldsProject(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() {
		os.Chdir(old)
		orgInitName, orgInitRegion, orgInitSDKPath = "", "", ""
		orgInitTier, orgInitSandbox, orgInitPolicyStaging = "", true, false
	})
	orgInitName, orgInitRegion = "acme-org", "ap-southeast-2"

	if err := runOrgInit(nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"anvil.config.ts", "package.json", "tsconfig.json", orgConfigFile, ".gitignore"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	cfg, err := loadOrgConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project != "acme-org" || cfg.Region != "ap-southeast-2" {
		t.Errorf("got %+v", cfg)
	}
	entry, _ := os.ReadFile("anvil.config.ts")
	if !strings.Contains(string(entry), "new anvil.Org(") {
		t.Errorf("entry point doesn't construct an Org:\n%s", entry)
	}
	// Default (no terminal, no flags): minimum with Sandbox, every OU written out.
	for _, want := range []string{"    Security: { enable: true },", "    Workloads: {\n      enable: true,\n      ou: {", "        Prod: {\n          enable: true,\n", "          // accounts: {\n          //   'my-app-prod': { enable: true },\n          //   'legacy-app': { enable: true, import: { accountId: '123456789012' } },", "  // email: 'aws+{account}@example.com',", "        NonProd: { enable: true },\n      },\n    },", "    Sandbox: { enable: true },", "    Suspended: { enable: true },"} {
		if !strings.Contains(string(entry), want) {
			t.Errorf("entry point missing %q:\n%s", want, entry)
		}
	}
	if strings.Contains(string(entry), "'Workloads/") || strings.Contains(string(entry), "ou: 'W") {
		t.Errorf("OUs and accounts must be nested, not paths:\n%s", entry)
	}
	if strings.Contains(string(entry), "tier") {
		t.Errorf("entry point must not name a tier:\n%s", entry)
	}

	if err := runOrgInit(nil, nil); err == nil || !strings.Contains(err.Error(), "already an Org project") {
		t.Errorf("second init: got %v, want already-an-Org error", err)
	}
}

func TestOrgInit_RefusesAppProject(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(old) })
	os.WriteFile("anvil.yaml", []byte("project: app\n"), 0644)

	if err := runOrgInit(nil, nil); err == nil || !strings.Contains(err.Error(), "App project") {
		t.Errorf("got %v, want an App-project error", err)
	}
}

func TestEventHandler_OrgLinesNamedAndOnce(t *testing.T) {
	h := &EventHandler{command: "preview", resources: map[string]*trackedResource{}, showNames: true}
	urn := "urn:pulumi:org::damien-org::anvil:org:OrganizationalUnit::Workloads/Prod"
	if got := h.label("anvil:org:OrganizationalUnit", "Workloads/Prod"); got != "OrganizationalUnit  Workloads/Prod" {
		t.Errorf("label: got %q", got)
	}
	tr := &trackedResource{urn: urn, typeName: "anvil:org:OrganizationalUnit", name: "Workloads/Prod", op: "create"}
	h.resources[urn] = tr

	out := captureStdout(t, func() {
		h.handleResOutputs(&apitype.ResOutputsEvent{Metadata: apitype.StepEventMetadata{URN: urn, Op: "create"}})
		h.handleResOutputs(&apitype.ResOutputsEvent{Metadata: apitype.StepEventMetadata{URN: urn, Op: "create"}})
	})
	if n := strings.Count(out, "Workloads/Prod"); n != 1 {
		t.Errorf("line printed %d times, want once:\n%s", n, out)
	}
}

// captureStdout returns what fn prints to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return string(data)
}

func TestRoleNameFromAssumedRole(t *testing.T) {
	got := roleNameFromAssumedRole("arn:aws:sts::732401079015:assumed-role/AWSReservedSSO_AdministratorAccess_abc123/damien")
	if want := "AWSReservedSSO_AdministratorAccess_abc123"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOrgLayoutOus_Tiers(t *testing.T) {
	paths := func(c orgLayoutChoice) string {
		ous, err := orgLayoutOus(c)
		if err != nil {
			t.Fatal(err)
		}
		var p []string
		for _, ou := range ous {
			p = append(p, ou.Path)
		}
		return strings.Join(p, ",")
	}
	cases := map[string]orgLayoutChoice{
		"Security,Workloads,Workloads/Prod,Workloads/NonProd,Sandbox,Suspended":                                  {Tier: "minimum", Sandbox: true},
		"Security,Workloads,Workloads/Prod,Workloads/NonProd,Suspended":                                          {Tier: "minimum"},
		"Security,Infrastructure,Workloads,Workloads/Prod,Workloads/NonProd,Suspended":                           {Tier: "network"},
		"Security,Infrastructure,Deployments,Workloads,Workloads/Prod,Workloads/NonProd,PolicyStaging,Suspended": {Tier: "SRA", PolicyStaging: true},
	}
	for want, c := range cases {
		if got := paths(c); got != want {
			t.Errorf("%+v: got %s, want %s", c, got, want)
		}
	}
	if _, err := orgLayoutOus(orgLayoutChoice{Tier: "huge"}); err == nil || !strings.Contains(err.Error(), "minimum, network, sra") {
		t.Errorf("unknown tier: got %v", err)
	}
}

func TestOrgLayoutTree(t *testing.T) {
	ous, _ := orgLayoutOus(orgLayoutChoice{Tier: "minimum", Sandbox: true})
	got := strings.Join(orgLayoutTree(ous), "\n")
	for _, want := range []string{"Root", "├── Security", "├── Workloads", "│   ├── Prod", "│   └── NonProd", "└── Suspended"} {
		if !strings.Contains(got, want) {
			t.Errorf("tree missing %q:\n%s", want, got)
		}
	}
}
