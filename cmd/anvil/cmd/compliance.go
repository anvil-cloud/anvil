package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"

	"github.com/anvil-cloud/anvil/cmd/anvil/compliance"
)

var (
	complianceStage         string
	complianceYes           bool
	complianceFrameworks    []string
	complianceRegions       []string
	complianceRetention     string
	complianceWait          bool
	complianceDeleteResults bool
)

var complianceCmd = &cobra.Command{
	Use:   "compliance",
	Short: "Compliance scanning (Prowler) for your app",
	Long: `Scan your deployed app against compliance frameworks with Prowler.

Prowler runs in CodeBuild inside your account; nothing is installed locally.
The scanner is shared by every Anvil app in the account and region.`,
}

var complianceSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Create or update the shared compliance scanner for this account and region",
	RunE:  runComplianceSetup,
}

var complianceScanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Scan this app's deployed resources now",
	RunE:  runComplianceScan,
}

var complianceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the shared scanner and this app's latest scan",
	RunE:  runComplianceStatus,
}

var complianceTeardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Remove the shared compliance scanner from this account and region",
	RunE:  runComplianceTeardown,
}

func init() {
	for _, c := range []*cobra.Command{complianceSetupCmd, complianceScanCmd, complianceStatusCmd, complianceTeardownCmd} {
		c.Flags().StringVar(&complianceStage, "stage", "", "Stage (defaults to the active stage)")
		complianceCmd.AddCommand(c)
	}
	complianceSetupCmd.Flags().BoolVar(&complianceYes, "yes", false, "Create shared resources without prompting (CI)")
	complianceTeardownCmd.Flags().BoolVar(&complianceYes, "yes", false, "Tear down without prompting")
	complianceTeardownCmd.Flags().BoolVar(&complianceDeleteResults, "delete-results", false, "Also delete the results bucket and every stored scan")

	complianceScanCmd.Flags().StringSliceVar(&complianceFrameworks, "frameworks", nil,
		"Frameworks to scan against, e.g. soc2,iso27001,cis (default: your App's compliance config)")
	complianceScanCmd.Flags().StringSliceVar(&complianceRegions, "regions", nil, "Regions to scan (default: your App's regions; us-east-1 is always included)")
	complianceScanCmd.Flags().StringVar(&complianceRetention, "retention", "1y", "How long to keep results: "+strings.Join(compliance.RetentionTiers, ", ")+" (default: your App's setting)")
	complianceScanCmd.Flags().BoolVar(&complianceWait, "wait", false, "Wait for the scan to finish and print a summary")

	rootCmd.AddCommand(complianceCmd)
}

func complianceLogger() compliance.Logger {
	return compliance.Logger{Check: printCheck, Warn: printWarn}
}

// loadShared resolves the stage's region and opens the shared layer there.
func loadShared(ctx context.Context) (*compliance.Shared, string, error) {
	stage := resolveStage(complianceStage)
	shared, err := loadSharedIn(ctx, resolveRegionForStage(stage))
	return shared, stage, err
}

// loadSharedIn opens the shared layer in a specific region.
func loadSharedIn(ctx context.Context, region string) (*compliance.Shared, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("AWS credentials not found or expired.\n  Run `aws configure` or set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.")
	}
	return compliance.New(ctx, cfg, complianceLogger())
}

// discoveredCompliance returns the App's compliance config from the build
// manifest written by the last discovery run, or nil when it isn't set.
func discoveredCompliance() *ComplianceSpec {
	m, err := readFullManifest()
	if err != nil || m == nil {
		return nil
	}
	return m.Compliance
}

// startDeployScan starts the post-deploy scan. Problems are warnings: the
// deploy itself already succeeded.
func startDeployScan(ctx context.Context, stage string, spec *ComplianceSpec) {
	project, err := readProjectName()
	if err != nil {
		printWarn(fmt.Sprintf("Compliance scan not started: %v", err))
		return
	}
	shared, err := loadSharedIn(ctx, spec.Region)
	if err != nil {
		printWarn(fmt.Sprintf("Compliance scan not started: %v", err))
		return
	}
	if _, err := shared.StartScan(ctx, scanRequest(project, stage, spec, "deploy")); err != nil {
		printWarn(fmt.Sprintf("Compliance scan not started: %v", err))
		return
	}
	printCheck("Compliance scan started — run `anvil compliance status` to see the result")
}

func scanRequest(project, stage string, spec *ComplianceSpec, trigger string) compliance.ScanRequest {
	return compliance.ScanRequest{
		Project: project, Stage: stage, Regions: spec.Regions,
		Frameworks: spec.Frameworks, Retention: spec.Retention, Trigger: trigger,
	}
}

// confirm asks a yes/no question. Without a terminal it refuses unless --yes
// was passed, so CI never creates or deletes shared resources silently.
func confirm(question, nonInteractiveHint string) error {
	if complianceYes {
		return nil
	}
	if !isTTY() {
		return fmt.Errorf("%s", nonInteractiveHint)
	}
	p := promptui.Prompt{Label: question, IsConfirm: true}
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("cancelled — nothing was changed")
	}
	return nil
}

// ── setup ───────────────────────────────────────────────────

func runComplianceSetup(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	shared, _, err := loadShared(ctx)
	if err != nil {
		return err
	}
	if err := ensureComplianceShared(ctx, shared); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println(dim("  Ready. Add `compliance` to your App and deploy, or run `anvil compliance scan --frameworks soc2 --wait`."))
	return nil
}

// ensureComplianceShared converges the shared layer when it's missing or older
// than this CLI, prompting only when it would be created for the first time.
func ensureComplianceShared(ctx context.Context, shared *compliance.Shared) error {
	st, err := shared.Status(ctx)
	if err != nil {
		return err
	}
	if !compliance.NeedsConverge(st, version) {
		printCheck(fmt.Sprintf("Compliance scanner up to date (%s)", st.Version))
		return nil
	}

	n := shared.Names
	if !st.Exists {
		fmt.Printf("\n  No compliance scanner exists yet in account %s (%s).\n", n.Account, n.Region)
		fmt.Println("  Anvil will create shared resources, used by every Anvil app in this account and region:")
		fmt.Println()
		fmt.Printf("    • CodeBuild project + read-only scan role   %s, %s\n", n.Project(), n.ScanRole())
		fmt.Printf("    • Results bucket (encrypted, versioned)     %s\n", n.Bucket())
		fmt.Printf("    • Scheduler group + invoke role             %s, %s\n", n.ScheduleGroup(), n.InvokeRole())
		fmt.Printf("    • Build log group (30-day retention)        %s\n", n.LogGroup())
		fmt.Println()
		if err := confirm("Create them", "Compliance requires shared account resources. Re-run with --yes to create them."); err != nil {
			return err
		}
		fmt.Println()
	} else {
		printCheck(fmt.Sprintf("Updating compliance scanner (%s → %s)", orUnknown(st.Version), version))
	}
	return shared.Ensure(ctx, version)
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// ── scan ────────────────────────────────────────────────────

func runComplianceScan(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	project, err := readProjectName()
	if err != nil {
		return err
	}
	stage := resolveStage(complianceStage)

	// Settings come from the App's `compliance` config; flags override them.
	spec, err := scanSpec(ctx, cmd, stage)
	if err != nil {
		return err
	}
	frameworks := spec.Frameworks

	shared, err := loadSharedIn(ctx, spec.Region)
	if err != nil {
		return err
	}

	st, err := shared.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Exists {
		return fmt.Errorf("no compliance scanner in account %s (%s).\n  Run `anvil compliance setup` first.", shared.Names.Account, shared.Names.Region)
	}

	buildID, err := shared.StartScan(ctx, scanRequest(project, stage, spec, "manual"))
	if err != nil {
		return err
	}
	printCheck(fmt.Sprintf("Scan started for %s/%s (%s)", project, stage, strings.Join(frameworks, ", ")))

	if !complianceWait {
		fmt.Println(dim("  Run `anvil compliance status` to see the result once it finishes."))
		return nil
	}

	res, err := shared.WaitForScan(ctx, buildID, func(phase string) {
		fmt.Printf("  %s %s\n", dim("…"), strings.ToLower(strings.ReplaceAll(phase, "_", " ")))
	})
	if err != nil {
		return err
	}
	if res.Status != "SUCCEEDED" {
		fmt.Println()
		for _, line := range res.LogTail {
			fmt.Println("    " + dim(line))
		}
		if res.LogLink != "" {
			fmt.Printf("\n  Full log: %s\n", res.LogLink)
		}
		return fmt.Errorf("scan %s", strings.ToLower(res.Status))
	}

	m, err := shared.LatestManifest(ctx, project, stage)
	if err != nil {
		return err
	}
	if m != nil {
		fmt.Println()
		printManifest(m)
	}
	return nil
}

// scanSpec builds the scan settings: the App's `compliance` config (found by
// running discovery), with any --frameworks/--regions/--retention flags on top.
// Without the config, --frameworks is required.
func scanSpec(ctx context.Context, cmd *cobra.Command, stage string) (*ComplianceSpec, error) {
	spec := &ComplianceSpec{Region: resolveRegionForStage(stage), Retention: "1y"}

	if !cmd.Flags().Changed("frameworks") {
		fmt.Println(dim("  Reading compliance settings from your app..."))
		if _, err := discoverFunctions(ctx, stage); err != nil {
			return nil, fmt.Errorf("reading your app's compliance settings: %w", err)
		}
		found := discoveredCompliance()
		if found == nil {
			return nil, fmt.Errorf("this app has no `compliance` config.\n  Add `compliance: { frameworks: [...] }` to your App, or pass --frameworks.")
		}
		spec = found
	}

	if cmd.Flags().Changed("frameworks") {
		ids, err := compliance.ResolveFrameworks(complianceFrameworks)
		if err != nil {
			return nil, err
		}
		spec.Frameworks = ids
	}
	if cmd.Flags().Changed("regions") {
		spec.Regions = complianceRegions
	}
	if cmd.Flags().Changed("retention") {
		spec.Retention = complianceRetention
	}
	if len(spec.Regions) == 0 {
		spec.Regions = []string{spec.Region}
	}
	if spec.Region == "" {
		spec.Region = resolveRegionForStage(stage)
	}
	return spec, nil
}

// ── status ──────────────────────────────────────────────────

func runComplianceStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	shared, stage, err := loadShared(ctx)
	if err != nil {
		return err
	}
	st, err := shared.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Exists {
		printWarn(fmt.Sprintf("No compliance scanner in account %s (%s). Run `anvil compliance setup`.", shared.Names.Account, shared.Names.Region))
		return nil
	}
	printCheck(fmt.Sprintf("Scanner %s (version %s, Prowler %s)", shared.Names.Project(), orUnknown(st.Version), compliance.ProwlerVersion))

	apps, err := shared.RegisteredApps(ctx)
	if err != nil {
		return err
	}
	sort.Strings(apps)
	if len(apps) > 0 {
		printCheck(fmt.Sprintf("Scheduled apps: %s", strings.Join(apps, ", ")))
	}

	project, err := readProjectName()
	if err != nil {
		return nil // outside a project: shared status only
	}
	m, err := shared.LatestManifest(ctx, project, stage)
	if err != nil {
		return err
	}
	fmt.Println()
	if m == nil {
		fmt.Printf("  %s/%s has not been scanned yet.\n", project, stage)
		return nil
	}
	printManifest(m)
	return nil
}

func printManifest(m *compliance.Manifest) {
	fmt.Printf("  %s  %s  (%s, %.0fs)\n", bold("Latest scan"), m.ScanID, m.Status, m.DurationSeconds)
	fmt.Printf("    Frameworks: %s\n", strings.Join(m.Frameworks, ", "))
	fmt.Printf("    Resources:  %d scanned\n", m.Resources.Count)
	if m.Status == "empty" {
		printWarn("No resources matched this app's tags")
		return
	}
	f := m.Findings
	fmt.Printf("    Findings:   %d total — %s, %s", f.Total, green(fmt.Sprintf("%d pass", f.Pass)), red(fmt.Sprintf("%d fail", f.Fail)))
	if f.Manual > 0 {
		fmt.Printf(", %d manual", f.Manual)
	}
	fmt.Println()
	var sev []string
	for _, s := range []string{"critical", "high", "medium", "low", "informational"} {
		if n := f.FailBySeverity[s]; n > 0 {
			sev = append(sev, fmt.Sprintf("%d %s", n, s))
		}
	}
	if len(sev) > 0 {
		fmt.Printf("    Failing:    %s\n", strings.Join(sev, ", "))
	}
}

// ── teardown ────────────────────────────────────────────────

func runComplianceTeardown(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	shared, _, err := loadShared(ctx)
	if err != nil {
		return err
	}
	what := "the shared compliance scanner"
	if complianceDeleteResults {
		what += " AND every stored scan result"
	}
	if err := confirm(fmt.Sprintf("Remove %s from account %s (%s)", what, shared.Names.Account, shared.Names.Region),
		"Re-run with --yes to tear down the compliance scanner."); err != nil {
		return err
	}
	return shared.Teardown(ctx, complianceDeleteResults)
}
