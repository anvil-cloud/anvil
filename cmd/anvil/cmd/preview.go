package cmd

import (
	"context"
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/spf13/cobra"
)

var (
	previewStage   string
	previewVerbose bool
)

var previewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Preview changes without deploying",
	Long:  `Show a diff of what would change in your infrastructure without actually applying it.`,
	RunE:  runPreview,
}

func init() {
	previewCmd.Flags().StringVar(&previewStage, "stage", "", "Stage name for this deployment")
	previewCmd.Flags().BoolVar(&previewVerbose, "verbose", false, "Show underlying cloud resources")
	rootCmd.AddCommand(previewCmd)
}

func runPreview(cmd *cobra.Command, args []string) error {
	stage := resolveStage(previewStage)

	ctx := context.Background()

	// Discovery records which DSQL components want a CDC stream, so the
	// preview can say what deploy will do to them.
	if _, err := discoverFunctions(ctx, stage); err != nil {
		return fmt.Errorf("discovery failed: %w", err)
	}

	s, err := loadStack(ctx, stage)
	if err != nil {
		return err
	}

	fmt.Printf("\n  Previewing %s...\n\n", stage)

	handler := NewEventHandler(previewVerbose, "preview")
	eventCh := make(chan events.EngineEvent)

	go func() {
		for event := range eventCh {
			handler.HandleEvent(event)
		}
	}()

	_, err = s.Preview(ctx, optpreview.EventStreams(eventCh))

	handler.PrintSummary(stage)

	if err != nil {
		return fmt.Errorf("preview failed")
	}

	if err := previewStreams(ctx, s, stage); err != nil {
		printWarn(fmt.Sprintf("Could not check CDC streams: %v", err))
	}

	return nil
}
