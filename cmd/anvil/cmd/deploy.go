package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/spf13/cobra"
)

var (
	deployStage      string
	deployVerbose    bool
	deployPartial    bool
	deployForceCache bool
	deployRefresh    bool
	deployYes        bool
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy your infrastructure",
	Long:  `Deploy infrastructure to your cloud account. Uses the Anvil provider to create secure-by-default resources.`,
	RunE:  runDeploy,
}

func init() {
	deployCmd.Flags().StringVar(&deployStage, "stage", "", "Stage name for this deployment")
	deployCmd.Flags().BoolVar(&deployVerbose, "verbose", false, "Show underlying cloud resources")
	deployCmd.Flags().BoolVar(&deployPartial, "partial", false, "Deploy successfully built functions even if some builds fail")
	deployCmd.Flags().BoolVar(&deployForceCache, "force-cache", false, "Skip rebuild and use last cached build artifacts")
	deployCmd.Flags().BoolVar(&deployRefresh, "refresh", false, "Refresh state from cloud before deploying")
	deployCmd.Flags().BoolVar(&deployYes, "yes", false, "Create shared account resources (e.g. the compliance scanner) without prompting")
	rootCmd.AddCommand(deployCmd)
}

func runDeploy(cmd *cobra.Command, args []string) error {
	stage := resolveStage(deployStage)
	ctx := context.Background()

	// ── Step 1: Discover Lambda functions ─────────────
	// Runs the program with ANVIL_BUILD_MODE=true using a separate
	// stack instance so the real deploy stack is never contaminated.
	// Makes no AWS API calls — purely a manifest discovery step.
	functions, err := discoverFunctions(ctx, stage)
	if err != nil {
		return fmt.Errorf("function discovery failed: %w", err)
	}

	// ── Step 1b: Generate ──────────────────────────────
	// Before the build, so it bundles current code: typed database clients
	// (incl. the gen/db crate Rust Lambdas use) from the schemas discovery
	// just read, and packages for any declared Rust Lambda that doesn't exist
	// yet. Existing user code is never touched.
	projectRoot, err := findProjectRoot()
	if err != nil {
		return err
	}
	discovered, err := readFullManifest()
	if err != nil {
		return err
	}
	// Fails only with nothing saved and no detectable language; Rust Lambdas
	// still get gen/db, since generateProject adds the Rust target.
	targets, _ := generateTargetsFor(projectRoot, stage, false)
	if err := generateProject(projectRoot, targets, functions, discovered, "Updated"); err != nil {
		return fmt.Errorf("generate failed: %w", err)
	}

	// ── Step 2: Load real deploy stack ────────────────
	s, err := loadStack(ctx, stage)
	if err != nil {
		return err
	}

	printBanner()
	fmt.Printf("  Deploying to %s...\n\n", stage)

	// ── Step 3: Build functions ────────────────────────
	// Bundle each discovered function and collect the zip paths.
	// Skips rebuild if source hash matches cached hash (unless --force-cache).
	if len(functions) > 0 {
		fmt.Printf("Building %d Lambda function(s)...\n\n", len(functions))

		artifacts, buildErrors := buildFunctions(functions, deployForceCache, deployPartial)

		// Surface build errors
		if len(buildErrors) > 0 {
			for _, e := range buildErrors {
				fmt.Printf("  ❌ Build failed: %s\n", e)
			}
			if !deployPartial {
				return fmt.Errorf("build failed — use --partial to deploy successfully built functions, or --force-cache to skip rebuild")
			}
			fmt.Printf("\n  ⚠️  --partial: deploying %d successfully built function(s), skipping %d failed\n\n",
				len(artifacts), len(buildErrors))
		}

		// ── Step 4: Set artifact paths in Pulumi config ─
		// Provider reads anvil:fn:{name} to locate the zip during deploy.
		if err := setFunctionArtifacts(ctx, s, artifacts); err != nil {
			return fmt.Errorf("failed to set function artifacts: %w", err)
		}
	}

	// ── Step 4b: Shared compliance scanner ─────────────
	// The app's schedule targets the shared scanner, so it must exist first.
	// Only prompts when it would be created for the first time.
	compliance := discoveredCompliance()
	if compliance != nil {
		complianceYes = deployYes
		shared, err := loadSharedIn(ctx, compliance.Region)
		if err != nil {
			return err
		}
		if err := ensureComplianceShared(ctx, shared); err != nil {
			return err
		}
		fmt.Println()
	}

	// ── Step 5: Real deploy ────────────────────────────
	handler := NewEventHandler(deployVerbose, "deploy")
	eventCh := make(chan events.EngineEvent)

	go func() {
		for event := range eventCh {
			handler.HandleEvent(event)
		}
	}()

	upOpts := []optup.Option{
		optup.EventStreams(eventCh),
		optup.Parallel(10),
	}
	if deployRefresh {
		upOpts = append(upOpts, optup.Refresh())
	}

	_, upErr := s.Up(ctx, upOpts...)

	handler.PrintSummary(stage)

	if handler.HasErrors() {
		return fmt.Errorf("deploy failed")
	}

	if upErr != nil {
		// After a successful update, Automation API reads the stack outputs with
		// two extra `pulumi stack output` calls. Anvil doesn't use them, so a
		// failure there mustn't turn a completed deploy into a failed one.
		if isOutputFetchError(upErr) {
			printWarn(fmt.Sprintf("Deploy succeeded, but reading stack outputs failed: %v", upErr))
		} else {
			return fmt.Errorf("deploy failed: %w", upErr)
		}
	}

	// ── Step 5b: Scan on deploy ────────────────────────
	// Fire and forget: a scan failure never fails a completed deploy.
	if compliance != nil && compliance.ScanOnDeploy {
		startDeployScan(ctx, stage, compliance)
	}

	return nil
}

// isOutputFetchError reports whether err came from Automation API's post-update
// `pulumi stack output` calls rather than from the update itself.
func isOutputFetchError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "could not get outputs") || strings.Contains(msg, "could not get secret outputs")
}
