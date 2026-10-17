package bundler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveNodeEntry(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		want    string
		wantErr string
	}{
		{"typescript", []string{"index.ts"}, "index.ts", ""},
		{"javascript", []string{"index.js"}, "index.js", ""},
		{"esm javascript", []string{"index.mjs"}, "index.mjs", ""},
		{"missing", nil, "", "no source file"},
		{"ts next to a stale js", []string{"index.ts", "index.js"}, "", "ambiguous"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("export const handler = () => {}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// A directory named like a candidate must not count as the file.
			if err := os.Mkdir(filepath.Join(dir, "index.tsx"), 0o755); err != nil {
				t.Fatal(err)
			}

			got, err := ResolveNodeEntry(filepath.Join(dir, "index"))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(dir, tt.want); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}
