// Package sveltekit provides build orchestration for SvelteKit sites using adapter-node.
// It is provider-agnostic — the output BuildResult is consumed by provider-specific
// components (aws/sveltekitsite, gcp/sveltekitsite) to wire up cloud resources.
package sveltekit

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/anvil-cloud/anvil/provider/sites"
)

// BuildOptions configures the SvelteKit build process.
type BuildOptions struct {
	// Path is the directory containing the SvelteKit project, relative to the project root.
	// The project root is where anvil.yaml lives.
	Path string

	// ProjectRoot is the absolute path to the project root (where anvil.yaml lives).
	// All relative paths are resolved from here, not from the entry point file.
	ProjectRoot string

	// Environment variables to set during the build.
	// Injected as process.env.* — available for static generation and prerendering.
	Environment map[string]string
}

// BuildSvelteKit orchestrates a full SvelteKit build using adapter-node.
//
// Steps:
//  1. Resolve the site path relative to project root
//  2. Detect svelte.config.js
//  3. Check adapter-node is configured
//  4. Ensure Node.js is available
//  5. Run npm install if node_modules is missing
//  6. Run npm run build
//  7. Parse and validate the output structure
//
// Returns a BuildResult the provider layer can consume, or an error with
// a clear message indicating what went wrong.
func BuildSvelteKit(opts BuildOptions) (*sites.BuildResult, error) {
	// Resolve site directory relative to project root.
	siteDir := opts.Path
	if !filepath.IsAbs(siteDir) {
		siteDir = filepath.Join(opts.ProjectRoot, siteDir)
	}
	siteDir = filepath.Clean(siteDir)

	// 1. Check the site directory exists.
	info, err := os.Stat(siteDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("site directory not found: %s\n\nThe 'path' property should be relative to your project root (where anvil.yaml lives), not relative to your anvil.config file.\nExample: path: \"frontend\" resolves to %s/frontend", opts.Path, opts.ProjectRoot)
		}
		return nil, fmt.Errorf("cannot access site directory %s: %w", siteDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("site path is not a directory: %s", siteDir)
	}

	// 2. Detect SvelteKit project via svelte.config.js (or .ts, .mjs).
	configPath, err := findSvelteConfig(siteDir)
	if err != nil {
		return nil, err
	}

	// 3. Check adapter-node is configured.
	if err := checkAdapterNode(siteDir, configPath); err != nil {
		return nil, err
	}

	// 4. Ensure Node.js is installed.
	if err := checkNodeInstalled(); err != nil {
		return nil, err
	}

	// 5. npm install if node_modules is missing.
	if err := ensureDependencies(siteDir); err != nil {
		return nil, err
	}

	// 6. Run the build.
	if err := runBuild(siteDir, opts.Environment); err != nil {
		return nil, err
	}

	// 7. Parse and validate output.
	return parseBuildOutput(siteDir)
}

// findSvelteConfig looks for svelte.config.js, .ts, or .mjs in the site directory.
// SvelteKit 3 moved its config (including the adapter) into the Vite config, so
// vite.config.* is accepted when no svelte.config.* exists.
func findSvelteConfig(siteDir string) (string, error) {
	candidates := []string{
		"svelte.config.js",
		"svelte.config.ts",
		"svelte.config.mjs",
		"vite.config.ts",
		"vite.config.js",
		"vite.config.mts",
		"vite.config.mjs",
	}

	for _, name := range candidates {
		p := filepath.Join(siteDir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf(
		"no svelte.config.js found in %s\n\n"+
			"This doesn't look like a SvelteKit project. Anvil expects a SvelteKit app "+
			"with adapter-node configured.\n\n"+
			"To create a new SvelteKit project:\n"+
			"  npx sv create %s\n"+
			"  cd %s && npm install @sveltejs/adapter-node",
		siteDir,
		filepath.Base(siteDir),
		filepath.Base(siteDir),
	)
}

// checkAdapterNode reads the svelte config and package.json to verify adapter-node is present.
func checkAdapterNode(siteDir string, configPath string) error {
	pkgPath := filepath.Join(siteDir, "package.json")
	pkgData, err := os.ReadFile(pkgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf(
				"no package.json found in %s\n\n"+
					"SvelteKit projects need a package.json with @sveltejs/adapter-node.\n"+
					"Run: cd %s && npm init -y && npm install @sveltejs/adapter-node",
				siteDir, siteDir,
			)
		}
		return fmt.Errorf("cannot read package.json: %w", err)
	}

	hasAdapterDep := hasPackageDependency(pkgData, "@sveltejs/adapter-node")

	configData, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", configPath, err)
	}
	hasAdapterImport := strings.Contains(string(configData), "adapter-node")

	if !hasAdapterDep && !hasAdapterImport {
		return fmt.Errorf(
			"adapter-node not detected in %s\n\n"+
				"Anvil requires @sveltejs/adapter-node to build SvelteKit sites for deployment.\n"+
				"Your svelte.config may be using a different adapter (e.g. adapter-auto, adapter-static).\n\n"+
				"To fix:\n"+
				"  1. npm install @sveltejs/adapter-node\n"+
				"  2. Update svelte.config.js:\n"+
				"     import adapter from '@sveltejs/adapter-node';\n"+
				"     export default { kit: { adapter: adapter() } };",
			siteDir,
		)
	}

	if hasAdapterDep && !hasAdapterImport {
		fmt.Fprintf(os.Stderr, "⚠  @sveltejs/adapter-node is installed but not referenced in %s\n"+
			"   Make sure your svelte.config.js uses adapter-node, not adapter-auto.\n\n",
			filepath.Base(configPath),
		)
	}

	return nil
}

// hasPackageDependency checks if a package name appears in any dependency group.
func hasPackageDependency(pkgData []byte, pkg string) bool {
	var pkgJSON map[string]json.RawMessage
	if err := json.Unmarshal(pkgData, &pkgJSON); err != nil {
		return false
	}

	for _, key := range []string{"dependencies", "devDependencies", "peerDependencies"} {
		raw, ok := pkgJSON[key]
		if !ok {
			continue
		}
		var deps map[string]interface{}
		if err := json.Unmarshal(raw, &deps); err != nil {
			continue
		}
		if _, exists := deps[pkg]; exists {
			return true
		}
	}

	return false
}

// checkNodeInstalled verifies Node.js is available on PATH.
func checkNodeInstalled() error {
	if _, err := exec.LookPath("node"); err != nil {
		return fmt.Errorf(
			"Node.js is not installed or not on PATH\n\n" +
				"SvelteKit builds require Node.js. Install it from https://nodejs.org\n" +
				"or via a version manager like nvm, fnm, or mise.",
		)
	}
	return nil
}

// ensureDependencies runs npm install if node_modules doesn't exist.
func ensureDependencies(siteDir string) error {
	if _, err := os.Stat(filepath.Join(siteDir, "node_modules")); err == nil {
		return nil
	}

	npmPath, err := exec.LookPath("npm")
	if err != nil {
		return fmt.Errorf(
			"npm is not installed or not on PATH\n\n" +
				"Anvil needs npm to install SvelteKit dependencies.\n" +
				"npm is bundled with Node.js — install Node.js from https://nodejs.org",
		)
	}

	fmt.Fprintf(os.Stderr, "📦 Installing dependencies in %s...\n", siteDir)

	cmd := exec.Command(npmPath, "install")
	cmd.Dir = siteDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf(
			"npm install failed in %s\n\n"+
				"Check the output above for details. Common causes:\n"+
				"  - Missing or invalid package.json\n"+
				"  - Network issues (npm registry unreachable)\n"+
				"  - Incompatible Node.js version (check engines in package.json)",
			siteDir,
		)
	}

	return nil
}

// runBuild executes npm run build with the provided environment variables.
func runBuild(siteDir string, env map[string]string) error {
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		return fmt.Errorf("npm not found: %w", err)
	}

	fmt.Fprintf(os.Stderr, "🔨 Building SvelteKit app in %s...\n", siteDir)

	cmd := exec.Command(npmPath, "run", "build")
	cmd.Dir = siteDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf(
			"SvelteKit build failed in %s\n\n"+
				"Check the build output above for details. Common causes:\n"+
				"  - TypeScript errors in your SvelteKit app\n"+
				"  - Missing environment variables expected at build time\n"+
				"  - Incompatible dependency versions\n\n"+
				"You can test the build locally: cd %s && npm run build",
			siteDir, siteDir,
		)
	}

	return nil
}

// parseBuildOutput validates the adapter-node output structure and returns a BuildResult.
//
// adapter-node produces:
//
//	build/
//	├── index.js    → Node.js server entry (what the Lambda runs)
//	├── client/     → static assets (JS, CSS, images, prerendered pages)
//	└── server/     → SvelteKit server code
//
// adapter-node 6 (SvelteKit 3) no longer emits build/server/index.js, so the
// entry is checked at build/index.js, which every adapter-node version emits.
func parseBuildOutput(siteDir string) (*sites.BuildResult, error) {
	buildDir := filepath.Join(siteDir, "build")

	if _, err := os.Stat(buildDir); os.IsNotExist(err) {
		return nil, fmt.Errorf(
			"build output not found at %s\n\n"+
				"The SvelteKit build completed but the expected output directory is missing.\n"+
				"adapter-node should produce a 'build/' directory. Check your svelte.config.js:\n\n"+
				"  adapter: adapter({\n"+
				"    out: 'build'  // This is the default — omit to use default\n"+
				"  })",
			buildDir,
		)
	}

	clientDir := filepath.Join(buildDir, "client")
	serverDir := filepath.Join(buildDir, "server")
	serverEntry := filepath.Join(buildDir, "index.js")

	if _, err := os.Stat(clientDir); os.IsNotExist(err) {
		return nil, fmt.Errorf(
			"static assets directory not found: %s\n\n"+
				"adapter-node should produce a 'build/client/' directory containing static assets.\n"+
				"Check your svelte.config.js uses @sveltejs/adapter-node.",
			clientDir,
		)
	}

	if _, err := os.Stat(serverDir); os.IsNotExist(err) {
		return nil, fmt.Errorf(
			"server directory not found: %s\n\n"+
				"adapter-node should produce a 'build/server/' directory containing the Node.js server.\n"+
				"If you're using adapter-static instead, there's no server to deploy — "+
				"consider using adapter-node for SSR support.",
			serverDir,
		)
	}

	if _, err := os.Stat(serverEntry); os.IsNotExist(err) {
		return nil, fmt.Errorf(
			"server entry point not found: %s\n\n"+
				"adapter-node should produce 'build/index.js' as the server entry point.\n"+
				"The build output structure may have changed — check your @sveltejs/adapter-node version.",
			serverEntry,
		)
	}

	return &sites.BuildResult{
		StaticDir:   clientDir,
		ServerDir:   serverDir,
		ServerEntry: serverEntry,
		Framework:   "sveltekit",
	}, nil
}
