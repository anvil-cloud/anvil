package bundler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// BundleResult holds the output of a successful Node.js bundle operation.
type BundleResult struct {
	// JS is the bundled JavaScript output — a single self-contained file.
	JS []byte

	// SourceMap is the optional source map for debugging.
	SourceMap []byte
}

// BundleOptions controls how esbuild bundles the function.
type BundleOptions struct {
	// EntryPoint is the path to the entry file. e.g. "functions/api/index.ts"
	EntryPoint string

	// Architecture determines the esbuild target platform.
	// "arm64" and "x86_64" are both supported — esbuild output is
	// architecture-agnostic JS, but we record it for future use.
	Architecture string

	// SourceMaps enables source map generation for debugging.
	// Not enabled by default — adds size to the zip.
	SourceMaps bool
}

// Bundle runs esbuild on a Node.js/TypeScript entry point and returns
// the bundled output as bytes. No files are written to disk — the caller
// is responsible for zipping and caching.
//
// TypeScript is supported natively via esbuild — no tsc needed.
// All dependencies are inlined into a single file — no node_modules in zip.
func Bundle(opts BundleOptions) (*BundleResult, error) {
	sourcemap := api.SourceMapNone
	if opts.SourceMaps {
		sourcemap = api.SourceMapInline
	}

	result := api.Build(api.BuildOptions{
		EntryPoints: []string{opts.EntryPoint},
		Bundle:      true,
		Platform:    api.PlatformNode,
		Format:      api.FormatCommonJS,
		Target:      api.ESNext,
		Sourcemap:   sourcemap,
		Write:       false, // return bytes, don't write to disk
		Loader: map[string]api.Loader{
			".ts":   api.LoaderTS,
			".tsx":  api.LoaderTSX,
			".js":   api.LoaderJS,
			".jsx":  api.LoaderJSX,
			".json": api.LoaderJSON,
		},
		// Tree-shake aggressively — Lambda cold start depends on bundle size
		TreeShaking: api.TreeShakingTrue,
		// MinifyWhitespace reduces size without affecting correctness
		MinifyWhitespace: true,
	})

	if len(result.Errors) > 0 {
		return nil, formatBuildErrors(opts.EntryPoint, result.Errors)
	}

	if len(result.OutputFiles) == 0 {
		return nil, fmt.Errorf("esbuild produced no output for %s", opts.EntryPoint)
	}

	bundleResult := &BundleResult{
		JS: result.OutputFiles[0].Contents,
	}

	// Source map is second output file when enabled
	if opts.SourceMaps && len(result.OutputFiles) > 1 {
		bundleResult.SourceMap = result.OutputFiles[1].Contents
	}

	return bundleResult, nil
}

// nodeEntryExtensions are tried in order when resolving a handler's file.
var nodeEntryExtensions = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

// ResolveNodeEntry finds the source file for a handler path written without
// an extension, the way Lambda handlers are: "functions/api/index" finds
// functions/api/index.ts (or .js, ...). Exactly one match is allowed, so an
// index.ts next to a stale compiled index.js is reported, not guessed.
func ResolveNodeEntry(pathWithoutExt string) (string, error) {
	var found []string
	for _, ext := range nodeEntryExtensions {
		candidate := pathWithoutExt + ext
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			found = append(found, candidate)
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("no source file %s.ts (or %s) found", pathWithoutExt, strings.Join(nodeEntryExtensions[1:], ", "))
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("ambiguous: %s all exist; keep only one", strings.Join(found, ", "))
	}
}

// BlankBundle returns a minimal Node.js handler for blank Lambda placeholders.
// Deployed when no entry point is provided — returns 200 with a message.
func BlankBundle() *BundleResult {
	js := []byte(`exports.handler = async () => ({
  statusCode: 200,
  body: JSON.stringify({
    message: "This Lambda has no code deployed yet. Deploy your function code to replace this placeholder.",
    anvil: true,
  }),
});
`)
	return &BundleResult{JS: js}
}

// formatBuildErrors converts esbuild error messages into a human-readable
// error with file name and line number, matching the format users expect
// from TypeScript compiler errors.
func formatBuildErrors(entryPoint string, errors []api.Message) error {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("build failed for %s:\n", filepath.Base(entryPoint)))

	for _, e := range errors {
		if e.Location != nil {
			sb.WriteString(fmt.Sprintf("  %s:%d:%d: %s\n",
				e.Location.File,
				e.Location.Line,
				e.Location.Column,
				e.Text,
			))
		} else {
			sb.WriteString(fmt.Sprintf("  %s\n", e.Text))
		}
	}

	return fmt.Errorf("%s", sb.String())
}
