package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

var (
	unlockStage string
)

var unlockCmd = &cobra.Command{
	Use:   "unlock",
	Short: "Release a locked stage",
	Long: `Release the lock on a stage that is stuck from a previous failed or interrupted operation.

This is a dangerous operation — only use it if you are sure no other deployment
is currently running against this stage.`,
	RunE: runUnlock,
}

func init() {
	unlockCmd.Flags().StringVar(&unlockStage, "stage", "", "Stage name to unlock (defaults to the active stage)")
	rootCmd.AddCommand(unlockCmd)
}

// resolveUnlockStage resolves the stage to unlock (flag → active stage →
// OS username → "dev") and refuses stages that aren't in anvil.yaml, so
// unlocking can never create a stage as a side effect.
func resolveUnlockStage(flagValue string) (string, error) {
	stage := resolveStage(flagValue)

	config, err := loadAnvilConfig()
	if err != nil || config.Stages[stage] == nil {
		return "", fmt.Errorf("stage %q doesn't exist — nothing to unlock", stage)
	}

	return stage, nil
}

func runUnlock(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	stage, err := resolveUnlockStage(unlockStage)
	if err != nil {
		return err
	}

	s, err := loadStackNoBootstrap(ctx, stage)
	if err != nil {
		return err
	}

	fmt.Printf("  Unlocking stage \"%s\"...\n", stage)

	err = s.Cancel(ctx)
	if err != nil {
		return fmt.Errorf("failed to unlock stage \"%s\": %w", stage, err)
	}

	if isTTY() {
		fmt.Printf("\n  %s Stage \"%s\" unlocked.\n", green("✔"), stage)
	} else {
		fmt.Printf("\n  Stage \"%s\" unlocked.\n", stage)
	}

	return nil
}
