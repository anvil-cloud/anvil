package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestStateLifecycleRulesSavesAndHistory(t *testing.T) {
	rules, err := stateLifecycleRules(defaultStateSettings())
	if err != nil {
		t.Fatal(err)
	}
	byPrefix := map[string]int{}
	for i, r := range rules {
		byPrefix[aws.ToString(r.Filter.Prefix)] = i
	}

	// Old versions of the state file are in-deploy saves; deploy history lives
	// in .pulumi/history/, so they only need to outlast a day.
	for _, prefix := range []string{".pulumi/stacks/", ".pulumi/history/"} {
		i, ok := byPrefix[prefix]
		if !ok {
			t.Fatalf("no rule for %s", prefix)
		}
		r := rules[i]
		if r.Expiration != nil {
			t.Errorf("%s: must never expire current objects: %+v", prefix, r.Expiration)
		}
		if r.NoncurrentVersionExpiration == nil || aws.ToInt32(r.NoncurrentVersionExpiration.NoncurrentDays) != 1 || r.NoncurrentVersionExpiration.NewerNoncurrentVersions != nil {
			t.Errorf("%s: want old versions deleted after 1 day: %+v", prefix, r.NoncurrentVersionExpiration)
		}
	}
}

func TestHistoryCheckpointsToDelete(t *testing.T) {
	h := ".pulumi/history/app/dev/"
	keys := []string{
		h + "dev-100.checkpoint.json",
		h + "dev-100.history.json",
		h + "dev-400.checkpoint.json.gz",
		h + "dev-400.history.json.gz",
		h + "dev-300.checkpoint.json.gz",
		h + "dev-200.checkpoint.json",
		h + "dev-500.checkpoint.json.gz",
		h + "dev-notanumber.checkpoint.json",
		".pulumi/history/app/prod/prod-1.checkpoint.json",
		".pulumi/history/app/prod/prod-2.checkpoint.json",
		".pulumi/stacks/app/dev-1.checkpoint.json",
	}

	got := historyCheckpointsToDelete(keys, 3)
	want := []string{h + "dev-100.checkpoint.json", h + "dev-200.checkpoint.json"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("keep 3: deleted %v, want %v", got, want)
	}

	// Plain and compressed checkpoints count together, newest first.
	got = historyCheckpointsToDelete(keys, 2)
	want = []string{h + "dev-100.checkpoint.json", h + "dev-200.checkpoint.json", h + "dev-300.checkpoint.json.gz"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("keep 2: deleted %v, want %v", got, want)
	}

	for _, key := range historyCheckpointsToDelete(keys, 2) {
		if strings.Contains(key, ".history.json") || strings.Contains(key, "notanumber") || strings.Contains(key, "/prod/") || strings.HasPrefix(key, ".pulumi/stacks/") {
			t.Errorf("must not delete %s", key)
		}
	}
	if got := historyCheckpointsToDelete(keys[:2], 3); len(got) != 0 {
		t.Errorf("fewer checkpoints than keep: deleted %v", got)
	}
}

func TestStateLifecycleRulesRejectsTooFewVersions(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 102} {
		if _, err := stateLifecycleRules(stateSettings{RetainVersions: n}); err == nil {
			t.Errorf("retainVersions %d accepted", n)
		}
	}
}

func TestStateLifecycleRulesCleanup(t *testing.T) {
	rules, _ := stateLifecycleRules(defaultStateSettings())
	byID := map[string]int{}
	for i, r := range rules {
		byID[aws.ToString(r.ID)] = i
	}

	b := rules[byID["anvil-checkpoint-backups-cleanup"]]
	if aws.ToString(b.Filter.Prefix) != ".pulumi/backups/" || aws.ToInt32(b.Expiration.Days) != 1 || aws.ToInt32(b.NoncurrentVersionExpiration.NoncurrentDays) != 1 {
		t.Errorf("backups rule must delete current and noncurrent backups: %+v", b)
	}

	c := rules[byID["anvil-bucket-cleanup"]]
	if !aws.ToBool(c.Expiration.ExpiredObjectDeleteMarker) || aws.ToInt32(c.AbortIncompleteMultipartUpload.DaysAfterInitiation) != 1 || c.Expiration.Days != nil {
		t.Errorf("cleanup rule must only remove expired delete markers and incomplete uploads: %+v", c)
	}

	// Only leftover backups may have their current objects expired.
	for _, r := range rules {
		p := aws.ToString(r.Filter.Prefix)
		if r.Expiration != nil && r.Expiration.Days != nil && p != ".pulumi/backups/" {
			t.Errorf("rule %s would expire current objects under %q", aws.ToString(r.ID), p)
		}
	}
}

func TestPulumiEnv(t *testing.T) {
	on := pulumiEnv("s3://b", stateSettings{RetainVersions: 3, CompressState: true})
	off := pulumiEnv("s3://b", stateSettings{RetainVersions: 3, CompressState: false})
	if on["PULUMI_DIY_BACKEND_GZIP"] != "true" || off["PULUMI_DIY_BACKEND_GZIP"] != "false" {
		t.Errorf("gzip env: on=%q off=%q", on["PULUMI_DIY_BACKEND_GZIP"], off["PULUMI_DIY_BACKEND_GZIP"])
	}
	for _, env := range []map[string]string{on, off} {
		if env["PULUMI_DIY_BACKEND_DISABLE_CHECKPOINT_BACKUPS"] != "true" {
			t.Error("checkpoint backups not disabled")
		}
		if env["PULUMI_BACKEND_URL"] != "s3://b" {
			t.Errorf("backend = %q", env["PULUMI_BACKEND_URL"])
		}
		if _, ok := env["PULUMI_SKIP_CHECKPOINTS"]; ok {
			t.Error("checkpoints must never be skipped")
		}
	}
}

func TestReadStateSettings(t *testing.T) {
	root := t.TempDir()
	if s, err := readStateSettings(root); err != nil || s != defaultStateSettings() {
		t.Fatalf("no file: %+v, %v; want defaults", s, err)
	}

	write := func(v any) {
		data, _ := json.Marshal(v)
		os.MkdirAll(filepath.Join(root, ".anvil"), 0o755)
		if err := os.WriteFile(appSettingsPath(root), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(map[string]any{"state": map[string]any{"retainVersions": 5, "compressState": false}})
	if s, err := readStateSettings(root); err != nil || s != (stateSettings{5, false}) {
		t.Errorf("explicit: %+v, %v", s, err)
	}

	write(map[string]any{"state": map[string]any{"compressState": false}})
	if s, err := readStateSettings(root); err != nil || s != (stateSettings{3, false}) {
		t.Errorf("partial: %+v, %v", s, err)
	}

	write(map[string]any{"state": map[string]any{"retainVersions": 1}})
	if _, err := readStateSettings(root); err == nil || !strings.Contains(err.Error(), "retainVersions") {
		t.Errorf("retainVersions 1 accepted: %v", err)
	}
}

func TestStateBucketFromBackend(t *testing.T) {
	cases := map[string]string{
		"s3://dev-anvil-state-app-abc123":   "dev-anvil-state-app-abc123",
		"s3://bucket?region=ap-southeast-2": "bucket",
		"s3://bucket/prefix":                "bucket",
		"file://~":                          "",
		"https://api.pulumi.com":            "",
	}
	for url, want := range cases {
		if got := stateBucketFromBackend(url); got != want {
			t.Errorf("stateBucketFromBackend(%q) = %q, want %q", url, got, want)
		}
	}
}
