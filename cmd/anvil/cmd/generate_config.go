package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// defaultGenPath is the output directory used when the user does not pass
// --path and has no saved generate config. Relative to the project root.
const defaultGenPath = "gen"

// generateConfigKey is the Pulumi stack config key under which Anvil persists
// the list of generate targets, so `anvil generate` (no flags) and the
// generate step of every deploy can reproduce the same outputs.
const generateConfigKey = "anvil:generate"

// generateTarget is one language/output-path pair. A project can have several
// (e.g. typescript -> backend/types and python -> services/api/db).
type generateTarget struct {
	Lang string `json:"lang"`
	Path string `json:"path"`
}

// pulumiStackConfigPath returns the path to .anvil/Pulumi.<stage>.yaml, the
// Pulumi stack config file Anvil manages internally for the given stage.
func pulumiStackConfigPath(projectRoot, stage string) string {
	return filepath.Join(projectRoot, ".anvil", fmt.Sprintf("Pulumi.%s.yaml", stage))
}

// loadGenerateTargets reads the saved generate targets from the stage's Pulumi
// config file. Returns nil (no error) when the file or key is absent.
func loadGenerateTargets(projectRoot, stage string) ([]generateTarget, error) {
	data, err := os.ReadFile(pulumiStackConfigPath(projectRoot, stage))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid stack config %s: %w", pulumiStackConfigPath(projectRoot, stage), err)
	}

	cfg, ok := doc["config"].(map[string]any)
	if !ok {
		return nil, nil
	}

	raw, ok := cfg[generateConfigKey].(string)
	if !ok || raw == "" {
		return nil, nil
	}

	var targets []generateTarget
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		return nil, fmt.Errorf("invalid %s config: %w", generateConfigKey, err)
	}
	return targets, nil
}

// saveGenerateTargets writes the generate targets into the stage's Pulumi config
// file under generateConfigKey, preserving all other config entries. The targets
// are stored as a JSON string (Pulumi config values are scalars).
func saveGenerateTargets(projectRoot, stage string, targets []generateTarget) error {
	path := pulumiStackConfigPath(projectRoot, stage)

	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("invalid stack config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	cfg, ok := doc["config"].(map[string]any)
	if !ok {
		cfg = map[string]any{}
		doc["config"] = cfg
	}

	encoded, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	cfg[generateConfigKey] = string(encoded)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// upsertGenerateTarget merges a target into an existing list, replacing any
// entry with the same language (one output path per language).
func upsertGenerateTarget(existing []generateTarget, t generateTarget) []generateTarget {
	for i := range existing {
		if existing[i].Lang == t.Lang {
			existing[i].Path = t.Path
			return existing
		}
	}
	return append(existing, t)
}

// normalizeLang maps a runtime/user language alias to the canonical generator
// key. nodejs is treated as typescript for generation and storage.
func normalizeLang(lang string) string {
	if lang == "nodejs" {
		return "typescript"
	}
	return lang
}
