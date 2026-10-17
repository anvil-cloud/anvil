package bundler

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// zipEpoch is the modification time written on every entry by ZipFile.
// Pulumi detects code changes by hashing the zip, so the zip must be
// byte-identical whenever its contents are: a real timestamp would make
// every build look like a code change. 1980-01-01 is the earliest time
// the zip format can store.
var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// ZipFile creates a zip archive at destPath containing a single file
// with the given name, contents and Unix permission bits.
//
// Used for single-file build output — esbuild's index.js (0644) or a Rust
// bootstrap executable (0755, which Lambda needs to run it). The output is
// deterministic: the same inputs always produce the same bytes.
func ZipFile(destPath, fileName string, contents []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create zip file: %w", err)
	}
	defer f.Close()

	w := zip.NewWriter(f)

	header := &zip.FileHeader{
		Name:     fileName,
		Method:   zip.Deflate,
		Modified: zipEpoch,
	}
	header.SetMode(mode)

	entry, err := w.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("failed to create zip entry: %w", err)
	}

	if _, err := entry.Write(contents); err != nil {
		return fmt.Errorf("failed to write zip entry: %w", err)
	}

	// Close writes the central directory; a failure here leaves a corrupt zip.
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to finalize zip file: %w", err)
	}

	return nil
}

// ZipDirectory creates a zip archive at destPath containing all files
// in sourceDir, preserving relative paths.
//
// Used when bundling produces multiple output files.
func ZipDirectory(destPath, sourceDir string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create zip file: %w", err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	defer w.Close()

	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}

		entry, err := w.Create(rel)
		if err != nil {
			return fmt.Errorf("failed to create zip entry for %s: %w", rel, err)
		}

		src, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", path, err)
		}
		defer src.Close()

		if _, err := io.Copy(entry, src); err != nil {
			return fmt.Errorf("failed to write %s to zip: %w", rel, err)
		}

		return nil
	})
}
