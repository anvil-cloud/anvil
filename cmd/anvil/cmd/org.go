package cmd

// anvil org …: commands for Org projects (anvil.org.yaml + new anvil.Org). The
// work lives in org_project.go, org_preflight.go and org_state.go.

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/spf13/cobra"
)

// orgStackName is the only stack in an Org project: Org has no stages.
const orgStackName = "org"

var (
	orgRegion  string
	orgVerbose bool
)

var orgCmd = &cobra.Command{
	Use:   "org",
	Short: "Manage an AWS Organization (Org projects)",
	Long: `Commands for Org projects: an anvil.org.yaml next to an entry point that
exports new anvil.Org({...}). Run them with management-account credentials.

  anvil org preview   Check the org and show what would change
  anvil org deploy    Deploy the org`,
}

var orgPreviewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Check the organization and preview changes",
	RunE:  func(cmd *cobra.Command, args []string) error { return runOrg(cmd.Context(), false) },
}

var orgDeployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy the organization",
	RunE:  func(cmd *cobra.Command, args []string) error { return runOrg(cmd.Context(), true) },
}

func init() {
	for _, c := range []*cobra.Command{orgPreviewCmd, orgDeployCmd} {
		c.Flags().StringVar(&orgRegion, "region", "", "Home region, used only until anvil.org.yaml records one (default: AWS config, then "+orgDefaultRegion+")")
		c.Flags().BoolVar(&orgVerbose, "verbose", false, "Show underlying cloud resources")
		orgCmd.AddCommand(c)
	}
	rootCmd.AddCommand(orgCmd)
}

func runOrg(ctx context.Context, deploy bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := findOrgRoot()
	if err != nil {
		return err
	}
	cfg, err := loadOrgConfig(root)
	if err != nil {
		return err
	}
	if cfg.Project == "" {
		cfg.Project = filepath.Base(root)
	}

	rt, err := detectRuntime(root)
	if err != nil {
		return err
	}
	workDir, err := writeProjectFile(root, cfg.Project, rt)
	if err != nil {
		return err
	}

	// ── Discovery: the program must define an Org ──
	if err := discoverOrg(ctx, root, workDir); err != nil {
		return err
	}

	// ── Home region ──
	awsCfg, region, err := orgAWSConfig(ctx, cfg.Region)
	if err != nil {
		return err
	}

	printBanner()
	fmt.Printf("  Org %s · home region %s\n\n", cfg.Project, region)

	// ── Preflight ──
	org, err := orgPreflight(ctx, awsCfg)
	if err != nil {
		return err
	}
	printCheck(fmt.Sprintf("Organization %s · management account %s · all features", org.ID, org.ManagementAccountID))

	// ── State bucket (created on first use) ──
	bucket, err := ensureOrgStateBucket(ctx, awsCfg, cfg, org, region)
	if err != nil {
		return err
	}
	changed := cfg.Region != region || cfg.State == nil || cfg.State.Bucket != bucket
	cfg.Region = region
	cfg.State = &orgStateConfig{Bucket: bucket}
	if changed {
		if err := writeOrgConfig(root, cfg); err != nil {
			return err
		}
		printCheck(fmt.Sprintf("Recorded home region and state bucket in %s (commit it)", orgConfigFile))
	}
	fmt.Println()

	// ── Stack ──
	settings := stateSettings{RetainVersions: orgRetainVersions, CompressState: true}
	backendURL := fmt.Sprintf("s3://%s?region=%s", bucket, region)
	s, err := auto.UpsertStackLocalSource(ctx, orgStackName, workDir, auto.EnvVars(pulumiEnv(backendURL, settings)))
	if err != nil {
		return fmt.Errorf("%s", mapError(err.Error()))
	}
	for key, val := range map[string]string{
		"aws:region":                region,
		"anvil:orgId":               org.ID,
		"anvil:orgRootId":           org.RootID,
		"anvil:managementAccountId": org.ManagementAccountID,
	} {
		if err := s.SetConfig(ctx, key, auto.ConfigValue{Value: val}); err != nil {
			return fmt.Errorf("failed to set %s: %w", key, err)
		}
	}

	command := "preview"
	if deploy {
		command = "deploy"
	}
	handler := NewEventHandler(orgVerbose, command)
	eventCh := make(chan events.EngineEvent)
	go func() {
		for event := range eventCh {
			handler.HandleEvent(event)
		}
	}()

	if !deploy {
		_, err = s.Preview(ctx, optpreview.EventStreams(eventCh))
		handler.PrintSummary(orgStackName)
		if err != nil {
			return fmt.Errorf("preview failed")
		}
		return nil
	}

	_, upErr := s.Up(ctx, optup.EventStreams(eventCh), optup.Parallel(10))
	handler.PrintSummary(orgStackName)

	if err := pruneHistoryInBucket(ctx, s3.NewFromConfig(awsCfg), bucket, orgRetainVersions); err != nil {
		printWarn(fmt.Sprintf("Could not remove old state checkpoints: %v", err))
	}
	if handler.HasErrors() {
		return fmt.Errorf("deploy failed")
	}
	if upErr != nil && !isOutputFetchError(upErr) {
		return fmt.Errorf("deploy failed: %w", upErr)
	}
	return nil
}
