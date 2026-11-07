package cmd

import (
	"fmt"
	"os"
	"path/filepath"
)

// runtime represents the detected Pulumi runtime for the project.
type runtime struct {
	Name      string // "nodejs", "python", or "go"
	EntryFile string // the file that was detected
}

// entryPoints defines the supported entry point files and their runtimes.
// Order does not imply priority — multiple matches are a fatal error.
var entryPoints = []struct {
	file    string
	runtime string
}{
	{"anvil.config.ts", "nodejs"},
	{"anvil.config.py", "python"},
	{"anvil.config.go", "go"},
}

// detectRuntime scans the project root for a supported entry point file.
// Project root is the directory containing anvil.yaml.
//
// Rules:
//   - Exactly one entry point must exist
//   - Multiple entry points → fatal error
//   - No entry points → fatal error
//   - Only checks project root, no subdirectory scanning
func detectRuntime(projectRoot string) (*runtime, error) {
	var found []runtime

	for _, ep := range entryPoints {
		path := filepath.Join(projectRoot, ep.file)
		if _, err := os.Stat(path); err == nil {
			found = append(found, runtime{
				Name:      ep.runtime,
				EntryFile: ep.file,
			})
		}
	}

	switch len(found) {
	case 0:
		return nil, fmt.Errorf(
			"No entry point found in %s\n"+
				"  Create one of:\n"+
				"    • anvil.config.ts  (TypeScript)\n"+
				"    • anvil.config.py  (Python)\n"+
				"    • anvil.config.go          (Go)",
			projectRoot,
		)
	case 1:
		return &found[0], nil
	default:
		files := ""
		for _, f := range found {
			files += fmt.Sprintf("    • %s\n", f.EntryFile)
		}
		return nil, fmt.Errorf(
			"Multiple entry points found in %s\n"+
				"  Found:\n%s"+
				"  Only one entry point is allowed per project.\n"+
				"  Remove the extras and keep the one for your language.",
			projectRoot, files,
		)
	}
}

// findProjectRoot walks up from the current directory to find anvil.yaml.
// Returns the directory containing anvil.yaml.
func findProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get working directory: %w", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "anvil.yaml")); err == nil {
			return dir, nil
		}
		if _, err := os.Stat(filepath.Join(dir, orgConfigFile)); err == nil {
			return "", fmt.Errorf("This project defines an Org. Use anvil org deploy (or anvil org preview).")
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf(
				"No anvil.yaml found (searched from current directory to root).\n" +
					"  Run `anvil init` to create a new project, or cd into an existing one.",
			)
		}
		dir = parent
	}
}
