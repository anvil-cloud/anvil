package cmd

import (
	"bufio"
	"strings"
	"testing"
)

func TestDefaultInitStageIsValid(t *testing.T) {
	if s := defaultInitStage(); !validStageName.MatchString(s) {
		t.Errorf("defaultInitStage() = %q, not a valid stage name", s)
	}
}

func TestResolveInitStageFlag(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(""))

	got, err := resolveInitStage("staging", reader)
	if err != nil || got != "staging" {
		t.Errorf("--stage staging: got %q, %v", got, err)
	}

	for _, bad := range []string{"-dev", "my stage", "dev_1", "dev/1"} {
		if _, err := resolveInitStage(bad, reader); err == nil || !strings.Contains(err.Error(), "Invalid stage name") {
			t.Errorf("--stage %q: error = %v, want invalid stage name", bad, err)
		}
	}
}

func TestResolveInitStageWithoutTerminalUsesSuggestion(t *testing.T) {
	if isTTY() {
		t.Skip("running in a terminal")
	}
	got, err := resolveInitStage("", bufio.NewReader(strings.NewReader("ignored\n")))
	if err != nil || got != defaultInitStage() {
		t.Errorf("got %q, %v; want the suggestion %q", got, err, defaultInitStage())
	}
}
