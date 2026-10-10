package aws

import (
	"fmt"
	"path/filepath"

	"github.com/anvil-cloud/anvil/cmd/anvil/cmd/bundler"
)

// BuildResult holds the outcome of building a single Lambda function.
type BuildResult struct {
	// FunctionName is the logical Anvil name of the function.
	FunctionName string

	// ZipPath is the path to the produced zip file.
	ZipPath string

	// Skipped is true when the build was skipped due to cache hit.
	Skipped bool
}

// BuildLambda bundles a Node.js Lambda function and produces a zip at
// .anvil/build/{functionName}.zip. Handles caching, blank placeholders,
// and AWS-specific zip format (index.js at root of zip).
//
// Returns the zip path on success. Returns an error on build failure.
func BuildLambda(functionName, entryPoint, architecture string, forceCache bool) (*BuildResult, error) {
	zipPath := bundler.ZipFilePath(functionName)

	// ── Blank placeholder ──────────────────────────────
	// No entry point — deploy a minimal handler returning 200.
	if entryPoint == "" {
		blank := bundler.BlankBundle()
		if err := bundler.ZipFile(zipPath, "index.js", blank.JS); err != nil {
			return nil, fmt.Errorf("failed to create blank zip for %s: %w", functionName, err)
		}
		return &BuildResult{FunctionName: functionName, ZipPath: zipPath}, nil
	}

	// ── Resolve source directory ───────────────────────
	// Hash the directory containing the entry point, not just the file,
	// so changes to imported modules are detected.
	sourceDir := filepath.Dir(entryPoint)

	// ── Cache check ────────────────────────────────────
	if !forceCache {
		needsRebuild, err := bundler.NeedsRebuild(functionName, sourceDir)
		if err != nil {
			// Hash failure is non-fatal — rebuild to be safe
			fmt.Printf("Cache check failed for %s, rebuilding: %v\n", functionName, err)
		} else if !needsRebuild {
			fmt.Printf("%s — no changes detected, using cached build\n", functionName)
			return &BuildResult{
				FunctionName: functionName,
				ZipPath:      zipPath,
				Skipped:      true,
			}, nil
		}
	} else {
		fmt.Printf("  ⚡ %s — --force-cache skipping rebuild\n", functionName)
		return &BuildResult{
			FunctionName: functionName,
			ZipPath:      zipPath,
			Skipped:      true,
		}, nil
	}

	// ── Bundle ─────────────────────────────────────────
	fmt.Printf("  🔨 Building %s (%s)...\n", functionName, filepath.Base(entryPoint))

	result, err := bundler.Bundle(bundler.BundleOptions{
		EntryPoint:   entryPoint,
		Architecture: architecture,
	})
	if err != nil {
		return nil, err
	}

	// ── Zip ────────────────────────────────────────────
	// AWS Lambda expects the handler file at the root of the zip.
	// esbuild produces a single bundled file — always named index.js.
	if err := bundler.ZipFile(zipPath, "index.js", result.JS); err != nil {
		return nil, fmt.Errorf("failed to zip %s: %w", functionName, err)
	}

	// ── Update cache ───────────────────────────────────
	hash, err := bundler.HashDirectory(sourceDir)
	if err != nil {
		// Non-fatal — just means next deploy will rebuild
		fmt.Printf("  ⚠️  Failed to write cache for %s: %v\n", functionName, err)
	} else {
		if err := bundler.WriteCachedHash(functionName, hash); err != nil {
			fmt.Printf("  ⚠️  Failed to write cache for %s: %v\n", functionName, err)
		}
	}

	fmt.Printf("  ✅ %s built successfully\n", functionName)

	return &BuildResult{
		FunctionName: functionName,
		ZipPath:      zipPath,
	}, nil
}
