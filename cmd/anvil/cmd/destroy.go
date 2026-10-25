package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/spf13/cobra"
)

var (
	destroyStage   string
	destroyVerbose bool
	destroyForce   bool
)

var destroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Tear down a deployment",
	Long: `Destroy all resources in the specified stage and remove it from anvil.yaml.

Requires --stage to be set explicitly. The active stage is ignored for safety.

Protected resources (DSQL clusters, DynamoDB tables, Cognito user pools) are
kept by default. Pass --force to remove their protection and delete them too.`,
	RunE: runDestroy,
}

func init() {
	destroyCmd.Flags().StringVar(&destroyStage, "stage", "", "Stage name to destroy (required)")
	destroyCmd.Flags().BoolVar(&destroyVerbose, "verbose", false, "Show underlying cloud resources")
	destroyCmd.Flags().BoolVar(&destroyForce, "force", false, "Also delete protected resources (databases, tables, user pools) and their data")
	rootCmd.AddCommand(destroyCmd)
}

func runDestroy(cmd *cobra.Command, args []string) error {
	if destroyStage == "" {
		fmt.Fprintf(os.Stderr, "  Destroy requires an explicit --stage flag for safety.\n")
		fmt.Fprintf(os.Stderr, "  Example: anvil destroy --stage dev\n")
		return fmt.Errorf("--stage is required for destroy")
	}

	ctx := context.Background()

	s, err := loadStackNoBootstrap(ctx, destroyStage)

	if err != nil {
		return err
	}

	printBanner()

	// ── 0. --force: strip protection so everything can be deleted ──
	if destroyForce {
		deployment, protected, err := findProtectedResources(ctx, s, resolveRegionForStage(destroyStage))
		if err != nil {
			return err
		}
		if len(protected) > 0 {
			fmt.Printf("  %s --force will permanently delete these protected resources and their data:\n\n", yellow("⚠"))
			for _, p := range protected {
				_, name := parseURN(p.urn)
				fmt.Printf("    • %s  (%s)\n", name, p.kind)
			}
			fmt.Println()

			if isTTY() {
				fmt.Printf("  Type the stage name (%s) to confirm: ", destroyStage)
				input, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.TrimSpace(input) != destroyStage {
					fmt.Println("  Cancelled.")
					return nil
				}
				fmt.Println()
			}

			if err := removeProtection(ctx, s, deployment, protected); err != nil {
				return err
			}
			printCheck("Protection removed")
			fmt.Println()
		}
	}

	fmt.Printf("  Destroying %s...\n\n", destroyStage)

	// ── 1. Destroy app resources via Pulumi ──
	handler := NewEventHandler(destroyVerbose, "destroy")
	eventCh := make(chan events.EngineEvent)

	go func() {
		for event := range eventCh {
			handler.HandleEvent(event)
		}
	}()

	// CDC streams aren't Pulumi resources: delete them first, while their role
	// and cluster still exist.
	if projectRoot, rootErr := findProjectRoot(); rootErr == nil {
		if err := deleteAllStreams(ctx, s, resolveProjectName(projectRoot), destroyStage); err != nil {
			return fmt.Errorf("failed to delete CDC streams, nothing was destroyed: %w", err)
		}
	}

	_, err = s.Destroy(ctx,
		optdestroy.EventStreams(eventCh),
		optdestroy.Parallel(10),
	)

	handler.PrintSummary(destroyStage)

	if err != nil {
		return fmt.Errorf("destroy failed")
	}

	// ── Remove stage from anvil.yaml ──
	config, configErr := loadAnvilConfig()
	if configErr != nil {
		return nil
	}

	if _, ok := config.Stages[destroyStage]; ok {
		delete(config.Stages, destroyStage)

		// If active stage was the one destroyed, clear it
		if config.Active == destroyStage {
			config.Active = ""
		}

		if len(config.Stages) == 0 {
			os.Remove("anvil.yaml")
			printCheck("anvil.yaml removed (no stages remaining)")
		} else {
			err = writeAnvilConfig(*config)
			if err != nil {
				fmt.Printf("  %s Could not update anvil.yaml: %s\n", yellow("⚠"), err)
			} else {
				printCheck(fmt.Sprintf("Stage \"%s\" removed from anvil.yaml", destroyStage))
			}
		}
	}

	// Clean up Pulumi stack config file
	os.Remove(fmt.Sprintf("Pulumi.%s.yaml", destroyStage))

	return nil
}
