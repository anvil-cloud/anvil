// Package complianceScanner is the per-app half of compliance scanning: one
// EventBridge Scheduler schedule that starts the shared Prowler CodeBuild
// project for this app. The shared half (bucket, roles, project, schedule
// group) is managed by the CLI. See docs/design/compliance-scanning.md.
//
// Internal: App creates this when `compliance` is set. Not meant to be
// constructed directly.
package complianceScanner

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/scheduler"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// Shared-layer names; must match cmd/anvil/compliance/names.go.
const (
	scheduleGroup = "anvil-compliance"
	manifestPath  = ".anvil/build-manifest.json"
)

type ComplianceScannerArgs struct {
	// Frameworks to scan against: soc2, iso27001, cis, nist-800-53, pci, hipaa,
	// fsbp, or a raw Prowler AWS compliance ID.
	Frameworks []string `pulumi:"frameworks"`
	// Schedule: daily (default), weekly, none, or cron (with Cron).
	Schedule string `pulumi:"schedule,optional"`
	// Cron is six AWS cron fields, used when Schedule is "cron".
	Cron string `pulumi:"cron,optional"`
	// Timezone for the schedule (IANA name). Default UTC.
	Timezone string `pulumi:"timezone,optional"`
	// Retention tier for results: 30d, 90d, 180d, 1y (default), 2y, 7y.
	Retention string `pulumi:"retention,optional"`
	// ScanOnDeploy starts a scan after each successful deploy.
	ScanOnDeploy bool `pulumi:"scanOnDeploy,optional"`
	// Regions the app deploys to, in addition to the provider's region.
	Regions []string `pulumi:"regions,optional"`
}

type ComplianceScanner struct {
	pulumi.ResourceState

	// ScheduleName is the schedule's name in the anvil-compliance group ("" when schedule is none).
	ScheduleName pulumi.StringOutput `pulumi:"scheduleName"`
	// ScheduleExpression is the EventBridge Scheduler expression ("" when schedule is none).
	ScheduleExpression pulumi.StringOutput `pulumi:"scheduleExpression"`
}

func (s *ComplianceScanner) Annotate(a infer.Annotator) {
	a.SetToken("aws", "ComplianceScanner")
	a.Describe(&s, "Internal: created by App when `compliance` is set. Schedules this app's Prowler compliance scans on the shared Anvil scanner. Do not construct directly.")
}

// manifestEntry is the `compliance` section of .anvil/build-manifest.json,
// read by the CLI before deploy.
type manifestEntry struct {
	Frameworks         []string `json:"frameworks"`
	Schedule           string   `json:"schedule"`
	ScheduleExpression string   `json:"scheduleExpression,omitempty"`
	Timezone           string   `json:"timezone"`
	Retention          string   `json:"retention"`
	ScanOnDeploy       bool     `json:"scanOnDeploy"`
	Regions            []string `json:"regions"`
	Region             string   `json:"region"`
}

func NewComplianceScanner(ctx *pulumi.Context, name string, args ComplianceScannerArgs, opts ...pulumi.ResourceOption) (*ComplianceScanner, error) {
	s := &ComplianceScanner{}
	stage := c.New(ctx, "anvil").Require("stage")
	project := ctx.Project()

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, s, opts...); err != nil {
		return nil, err
	}

	// The provider this component runs under decides the region; the shared
	// layer and schedule group live there.
	region, err := aws.GetRegion(ctx, &aws.GetRegionArgs{}, pulumi.Parent(s))
	if err != nil {
		return nil, fmt.Errorf("compliance: resolving AWS region: %w", err)
	}

	cfg, err := resolve(args, project, stage, region.Region)
	if err != nil {
		return nil, err
	}

	empty := pulumi.String("").ToStringOutput()

	if os.Getenv("ANVIL_BUILD_MODE") == "true" {
		if err := writeManifest(cfg, region.Region); err != nil {
			return nil, fmt.Errorf("compliance: build manifest write failed: %w", err)
		}
		s.ScheduleName, s.ScheduleExpression = empty, empty
		return s, ctx.RegisterResourceOutputs(s, pulumi.Map{"scheduleName": empty, "scheduleExpression": empty})
	}

	if cfg.Schedule == scheduleNone {
		s.ScheduleName, s.ScheduleExpression = empty, empty
		return s, ctx.RegisterResourceOutputs(s, pulumi.Map{"scheduleName": empty, "scheduleExpression": empty})
	}

	identity, err := aws.GetCallerIdentity(ctx, &aws.GetCallerIdentityArgs{}, pulumi.Parent(s))
	if err != nil {
		return nil, fmt.Errorf("compliance: resolving AWS account: %w", err)
	}

	input, err := startBuildInput(identity.AccountId, region.Region, project, stage, cfg)
	if err != nil {
		return nil, err
	}

	schedName := scheduleName(project, stage)
	sched := &scheduler.Schedule{}
	err = ctx.RegisterResource("aws:scheduler/schedule:Schedule", name+"-schedule", pulumi.Map{
		"name":                       pulumi.String(schedName),
		"groupName":                  pulumi.String(scheduleGroup),
		"description":                pulumi.String(fmt.Sprintf("Anvil compliance scan for %s/%s", project, stage)),
		"scheduleExpression":         pulumi.String(cfg.ScheduleExpression),
		"scheduleExpressionTimezone": pulumi.String(cfg.Timezone),
		"flexibleTimeWindow":         pulumi.Map{"mode": pulumi.String("OFF")},
		"state":                      pulumi.String("ENABLED"),
		"target": pulumi.Map{
			// Universal target: calls codebuild:StartBuild directly.
			"arn":     pulumi.String("arn:aws:scheduler:::aws-sdk:codebuild:startBuild"),
			"roleArn": pulumi.String(fmt.Sprintf("arn:aws:iam::%s:role/anvil-compliance-invoke-%s", identity.AccountId, region.Region)),
			"input":   pulumi.String(input),
			// Retries cover transient StartBuild failures such as the account's
			// CodeBuild concurrency limit.
			"retryPolicy": pulumi.Map{
				"maximumEventAgeInSeconds": pulumi.Int(3600),
				"maximumRetryAttempts":     pulumi.Int(5),
			},
		},
	}, sched, pulumi.Parent(s))
	if err != nil {
		return nil, fmt.Errorf("compliance: creating schedule: %w", err)
	}

	s.ScheduleName = sched.Name
	s.ScheduleExpression = sched.ScheduleExpression
	return s, ctx.RegisterResourceOutputs(s, pulumi.Map{
		"scheduleName":       sched.Name,
		"scheduleExpression": sched.ScheduleExpression,
	})
}

// startBuildInput is the StartBuild request the schedule sends. EventBridge
// Scheduler universal targets take PascalCase parameter names (it rejects
// CodeBuild's camelCase "projectName" at CreateSchedule). The variables mirror
// what `anvil compliance scan` sends (cmd/anvil/compliance/scan.go) and what
// scan.py reads.
func startBuildInput(account, region, project, stage string, cfg config) (string, error) {
	vars := [][2]string{
		{"ANVIL_PROJECT", project},
		{"ANVIL_STAGE", stage},
		{"ANVIL_REGIONS", strings.Join(cfg.Regions, " ")},
		{"ANVIL_FRAMEWORKS", strings.Join(cfg.Frameworks, " ")},
		{"ANVIL_RETENTION", cfg.Retention},
		{"ANVIL_TRIGGER", "scheduled"},
	}
	env := make([]map[string]string, 0, len(vars))
	for _, v := range vars {
		env = append(env, map[string]string{"Name": v[0], "Value": v[1], "Type": "PLAINTEXT"})
	}
	b, err := json.Marshal(map[string]any{
		"ProjectName":                  fmt.Sprintf("anvil-compliance-%s-%s", account, region),
		"EnvironmentVariablesOverride": env,
	})
	return string(b), err
}

func writeManifest(cfg config, region string) error {
	provider.ManifestMu.Lock()
	defer provider.ManifestMu.Unlock()

	if err := os.MkdirAll(".anvil", 0755); err != nil {
		return err
	}
	// Keep every other section (functions, schemas) verbatim.
	m := map[string]json.RawMessage{}
	if data, err := os.ReadFile(manifestPath); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	entry, err := json.Marshal(manifestEntry{
		Frameworks:         cfg.Frameworks,
		Schedule:           cfg.Schedule,
		ScheduleExpression: cfg.ScheduleExpression,
		Timezone:           cfg.Timezone,
		Retention:          cfg.Retention,
		ScanOnDeploy:       cfg.ScanOnDeploy,
		Regions:            cfg.Regions,
		Region:             region,
	})
	if err != nil {
		return err
	}
	m["compliance"] = entry
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, data, 0644)
}
