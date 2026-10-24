package cmd

import "testing"

func TestStackNameFromKey(t *testing.T) {
	cases := map[string]string{
		"dev.json":         "dev",
		"dev.json.gz":      "dev",
		"dev.json.zst":     "dev",
		"proj/dev.json.gz": "proj/dev",
		"dev.json.bak":     "",
		"dev.json.gz.bak":  "",
		".json":            "",
		"meta.yaml":        "",
	}
	for key, want := range cases {
		got, ok := stackNameFromKey(key)
		if ok != (want != "") || got != want {
			t.Errorf("stackNameFromKey(%q) = %q, %v; want %q", key, got, ok, want)
		}
	}
}
