package cmd

// Org project files: anvil.org.yaml, finding the project root, and the
// discovery run that checks the program defines an Org.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"gopkg.in/yaml.v3"
)

// orgConfigFile marks an Org project and holds what the CLI needs before the
// program runs. It sits next to the entry point and is committed.
const orgConfigFile = "anvil.org.yaml"

// orgFileConfig is anvil.org.yaml.
type orgFileConfig struct {
	Project string `yaml:"project"`
	// Region is the home region: the state bucket and regional home resources.
	Region   string          `yaml:"region,omitempty"`
	State    *orgStateConfig `yaml:"state,omitempty"`
	Baseline string          `yaml:"baseline,omitempty"`
}

type orgStateConfig struct {
	Bucket string `yaml:"bucket,omitempty"`
}

// findOrgRoot walks up from the current directory to the nearest directory
// holding anvil.org.yaml (an Org project) or anvil.yaml (an App project).
func findOrgRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get working directory: %w", err)
	}
	for {
		_, orgErr := os.Stat(filepath.Join(dir, orgConfigFile))
		_, appErr := os.Stat(filepath.Join(dir, "anvil.yaml"))
		switch {
		case orgErr == nil && appErr == nil:
			return "", fmt.Errorf("%s has both anvil.yaml and %s.\n"+
				"  A project is either an App (anvil.yaml) or an Org (%s). Remove the one that doesn't apply.",
				dir, orgConfigFile, orgConfigFile)
		case orgErr == nil:
			return dir, nil
		case appErr == nil:
			return "", fmt.Errorf("This project defines an App. Use anvil deploy --stage <stage>.")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("No %s found (searched from current directory to root).\n"+
				"  Create one next to anvil.config.ts with a project name, e.g.:\n"+
				"    project: acme-org", orgConfigFile)
		}
		dir = parent
	}
}

func loadOrgConfig(root string) (*orgFileConfig, error) {
	data, err := os.ReadFile(filepath.Join(root, orgConfigFile))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", orgConfigFile, err)
	}
	var cfg orgFileConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", orgConfigFile, err)
	}
	return &cfg, nil
}

func writeOrgConfig(root string, cfg *orgFileConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	header := "# Anvil Org project. Written by the CLI; commit it.\n"
	return os.WriteFile(filepath.Join(root, orgConfigFile), append([]byte(header), data...), 0644)
}

// discoverOrg runs the program in build mode (no AWS calls) and checks it
// constructs an Org. Discovery state is local and never touches the org's
// state bucket.
func discoverOrg(ctx context.Context, root, workDir string) error {
	settingsPath := appSettingsPath(root)
	if err := os.Remove(settingsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to clean discovery settings: %w", err)
	}
	discoveryDir := filepath.Join(root, ".anvil", "org-discovery")
	if err := os.MkdirAll(discoveryDir, 0755); err != nil {
		return fmt.Errorf("failed to create discovery directory: %w", err)
	}

	env := pulumiEnv("file://"+discoveryDir, defaultStateSettings())
	env["ANVIL_BUILD_MODE"] = "true"
	env["ANVIL_APP_SETTINGS_PATH"] = settingsPath
	s, err := auto.UpsertStackLocalSource(ctx, orgStackName, workDir, auto.EnvVars(env))
	if err != nil {
		return fmt.Errorf("failed to create discovery stack: %w", err)
	}
	if err := s.SetConfig(ctx, "aws:region", auto.ConfigValue{Value: orgDefaultRegion}); err != nil {
		return fmt.Errorf("failed to set discovery config: %w", err)
	}
	_, previewErr := s.Preview(ctx, optpreview.SuppressProgress(), optpreview.SuppressOutputs())

	kind, err := readProjectKind(settingsPath)
	if err != nil {
		return err
	}
	switch kind {
	case "org":
		return nil
	case "app":
		return fmt.Errorf("This project defines an App, but has an %s.\n"+
			"  Use anvil deploy --stage <stage>, and replace %s with anvil.yaml.", orgConfigFile, orgConfigFile)
	default:
		if previewErr != nil {
			if msg := programError(previewErr.Error()); msg != "" {
				return fmt.Errorf("%s", msg)
			}
			return fmt.Errorf("the program failed before defining an Org:\n%s", mapError(previewErr.Error()))
		}
		return fmt.Errorf("The entry point doesn't define an Org.\n" +
			"  Export one: export default new anvil.Org({ tier: 'minimum' });")
	}
}

// readProjectKind reads what the discovery run constructed: "org", "app"
// (App writes its settings without a kind), or "" when nothing was written.
func readProjectKind(settingsPath string) (string, error) {
	data, err := os.ReadFile(settingsPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read discovery settings: %w", err)
	}
	var raw struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("invalid discovery settings: %w", err)
	}
	if raw.Kind == "" {
		return "app", nil
	}
	return raw.Kind, nil
}

// programError pulls the first "Error: ..." line a Node program threw out of
// Pulumi's output, so config errors from new Org({...}) read cleanly instead of
// as a stack trace. Returns "" when there's none.
func programError(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(line), "Error: "); ok && msg != "" {
			return msg
		}
	}
	return ""
}
