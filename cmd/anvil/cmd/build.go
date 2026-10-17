package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anvil-cloud/anvil/cmd/anvil/cmd/bundler"
	awsbundler "github.com/anvil-cloud/anvil/cmd/anvil/cmd/bundler/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
)

const manifestPath = ".anvil/build-manifest.json"

// FunctionSpec holds the metadata for a single Lambda function
// discovered from the build manifest.
type FunctionSpec struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Runtime      string `json:"runtime"`
	Handler      string `json:"handler"`
	Entry        string `json:"entry"`
	Architecture string `json:"architecture"`
}

// SchemaSpec holds the metadata for a single DSQL component's schemas
// discovered from the build manifest.
type SchemaSpec struct {
	Component string          `json:"component"`
	Type      string          `json:"type"`
	Schemas   json.RawMessage `json:"schemas"`
}

// ComplianceSpec is the App's `compliance` config, written by the
// ComplianceScanner component during discovery.
type ComplianceSpec struct {
	Frameworks         []string `json:"frameworks"` // Prowler IDs
	Schedule           string   `json:"schedule"`
	ScheduleExpression string   `json:"scheduleExpression"`
	Timezone           string   `json:"timezone"`
	Retention          string   `json:"retention"`
	ScanOnDeploy       bool     `json:"scanOnDeploy"`
	Regions            []string `json:"regions"`
	Region             string   `json:"region"` // where the schedule and shared scanner live
}

type buildManifest struct {
	Functions  []FunctionSpec  `json:"functions"`
	Schemas    []SchemaSpec    `json:"schemas,omitempty"`
	Compliance *ComplianceSpec `json:"compliance,omitempty"`
}

// discoverFunctions runs the user's Pulumi program with ANVIL_BUILD_MODE=true,
// which causes NewLambda to write function metadata to .anvil/build-manifest.json
// instead of registering Pulumi resources. The manifest is then read and returned.
//
// A separate stack instance is used for discovery so the real deploy stack
// is never contaminated with build mode env vars.
func discoverFunctions(ctx context.Context, stage string) ([]FunctionSpec, error) {
	// ── Clean previous manifest ────────────────────────
	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to clean build manifest: %w", err)
	}

	// ── Build discovery stack ──────────────────────────
	projectRoot, err := findProjectRoot()
	if err != nil {
		return nil, err
	}

	rt, err := detectRuntime(projectRoot)
	if err != nil {
		return nil, err
	}

	projectName := resolveProjectName(projectRoot)
	workDir, err := writeProjectFile(projectRoot, projectName, rt)
	if err != nil {
		return nil, err
	}

	backendURL := resolveBackendForStage(stage)
	region := resolveRegionForStage(stage)

	execPath, _ := os.Executable()
	pluginDir := filepath.Dir(execPath)

	discoveryStack, err := auto.UpsertStackLocalSource(ctx, stage, workDir,
		auto.EnvVars(map[string]string{
			"PULUMI_BACKEND_URL":       backendURL,
			"PULUMI_CONFIG_PASSPHRASE": "",
			"PATH":                     pluginDir + ":" + os.Getenv("PATH"),
			"ANVIL_BUILD_MODE":         "true",
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery stack: %w", err)
	}

	anvilConfig, _ := loadAnvilConfig()
	stageId := ""
	if sc, ok := anvilConfig.Stages[stage]; ok {
		stageId = sc.ID
	}

	for key, val := range map[string]string{
		"aws:region":        region,
		"anvil:stage":       stage,
		"anvil:stageId":     stageId,
		"anvil:environment": resolveEnvironmentForStage(stage),
	} {
		if err := discoveryStack.SetConfig(ctx, key, auto.ConfigValue{Value: val}); err != nil {
			return nil, fmt.Errorf("failed to set discovery config %s: %w", key, err)
		}
	}

	_, err = discoveryStack.Preview(ctx,
		optpreview.SuppressProgress(),
		optpreview.SuppressOutputs(),
	)

	// Preview errors are expected in build mode — resources return empty structs.
	// Only fail if no manifest was written and there was an error.
	if err != nil {
		if _, statErr := os.Stat(manifestPath); os.IsNotExist(statErr) {
			return nil, nil
		}
	}

	return readManifest()
}

// readManifest reads and parses .anvil/build-manifest.json.
// Returns nil if the manifest does not exist.
func readManifest() ([]FunctionSpec, error) {
	m, err := readFullManifest()
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	return m.Functions, nil
}

// readFullManifest reads the complete build manifest including schemas.
func readFullManifest() (*buildManifest, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read build manifest: %w", err)
	}

	var m buildManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid build manifest: %w", err)
	}

	return &m, nil
}

// buildFunctions bundles all discovered Lambda functions and returns
// a map of function name → zip path for successful builds, and a slice
// of errors for failed builds.
//
// When partial is true, failed builds are collected and returned rather
// than aborting immediately — the caller decides whether to continue.
func buildFunctions(functions []FunctionSpec, forceCache bool, partial bool) (map[string]string, []error) {
	artifacts := make(map[string]string)
	var errs []error

	// Rust functions build together: one cargo lambda run per workspace and
	// architecture, so they're collected and built after the loop.
	var rust []awsbundler.RustFunction

	for _, fn := range functions {
		if fn.Type == "anvil:aws:Lambda" && fn.Runtime == rustRuntime {
			rust = append(rust, awsbundler.RustFunction{Name: fn.Name, Handler: fn.Entry, Architecture: fn.Architecture})
			continue
		}

		result, err := buildFunction(fn, forceCache)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", fn.Name, err))
			if !partial {
				return artifacts, errs
			}
			continue
		}
		artifacts[fn.Name] = result.ZipPath
	}

	// --force-cache reuses existing artifacts for Rust too.
	if forceCache {
		var unbuilt []awsbundler.RustFunction
		for _, fn := range rust {
			zipPath := bundler.ZipFilePath(fn.Name)
			if _, err := os.Stat(zipPath); err == nil {
				fmt.Printf("  ⚡ %s — --force-cache skipping rebuild\n", fn.Name)
				artifacts[fn.Name] = zipPath
				continue
			}
			unbuilt = append(unbuilt, fn)
		}
		rust = unbuilt
	}

	results, failed := awsbundler.BuildRustLambdas(rust)
	for _, fn := range rust { // manifest order, so errors print stably
		if err, ok := failed[fn.Name]; ok {
			errs = append(errs, fmt.Errorf("%s: %w", fn.Name, err))
			continue
		}
		artifacts[fn.Name] = results[fn.Name].ZipPath
	}

	return artifacts, errs
}

// buildFunction routes a single function to the correct bundler based
// on its runtime and cloud type (determined from the Pulumi type token).
func buildFunction(fn FunctionSpec, forceCache bool) (*awsbundler.BuildResult, error) {
	switch {
	case fn.Type == "anvil:aws:Lambda" && isNodeRuntime(fn.Runtime):
		// The manifest's entry is the handler's path without extension.
		entryFile, err := bundler.ResolveNodeEntry(fn.Entry)
		if err != nil {
			return nil, fmt.Errorf("handler %q: %w", fn.Handler, err)
		}
		return awsbundler.BuildLambda(fn.Name, entryFile, fn.Architecture, forceCache)

	default:
		return nil, fmt.Errorf("unsupported runtime %q for type %q", fn.Runtime, fn.Type)
	}
}

// rustRuntime is the Lambda runtime value for Rust functions.
const rustRuntime = "rust"

// isNodeRuntime returns true for Node.js runtime identifiers.
func isNodeRuntime(runtime string) bool {
	return strings.HasPrefix(runtime, "nodejs")
}

// setFunctionArtifacts writes the built zip paths into Pulumi config so
// the provider can read them during the real deploy via cfg.Get("fn:{name}").
func setFunctionArtifacts(ctx context.Context, s auto.Stack, artifacts map[string]string) error {
	for name, zipPath := range artifacts {
		absPath, err := filepath.Abs(zipPath)
		if err != nil {
			return fmt.Errorf("failed to resolve zip path for %s: %w", name, err)
		}
		key := fmt.Sprintf("anvil:fn-%s", name)
		if err := s.SetConfig(ctx, key, auto.ConfigValue{Value: absPath}); err != nil {
			return fmt.Errorf("failed to set artifact config for %s: %w", name, err)
		}
	}
	return nil
}
