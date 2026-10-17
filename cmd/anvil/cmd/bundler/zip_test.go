package bundler

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestZipFileIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.zip")
	second := filepath.Join(dir, "second.zip")
	contents := []byte("binary contents")

	if err := ZipFile(first, "bootstrap", contents, 0o755); err != nil {
		t.Fatal(err)
	}
	// A different wall-clock time must not change the output.
	time.Sleep(1100 * time.Millisecond)
	if err := ZipFile(second, "bootstrap", contents, 0o755); err != nil {
		t.Fatal(err)
	}

	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("zips of identical contents differ")
	}
}

func TestZipFileEntry(t *testing.T) {
	tests := []struct {
		name string
		file string
		mode os.FileMode
	}{
		{"rust bootstrap is executable", "bootstrap", 0o755},
		{"node bundle is not", "index.js", 0o644},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.zip")
			contents := []byte("contents of " + tt.file)
			if err := ZipFile(path, tt.file, contents, tt.mode); err != nil {
				t.Fatal(err)
			}

			r, err := zip.OpenReader(path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			if len(r.File) != 1 {
				t.Fatalf("got %d entries, want 1", len(r.File))
			}
			f := r.File[0]
			if f.Name != tt.file {
				t.Errorf("name = %q, want %q", f.Name, tt.file)
			}
			if got := f.Mode().Perm(); got != tt.mode {
				t.Errorf("mode = %o, want %o", got, tt.mode)
			}
			if !f.Modified.Equal(zipEpoch) {
				t.Errorf("modified = %v, want %v", f.Modified, zipEpoch)
			}

			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, contents) {
				t.Errorf("contents = %q, want %q", got, contents)
			}
		})
	}
}
