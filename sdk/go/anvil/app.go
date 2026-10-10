package anvil

import (
	"fmt"
	"sort"

	anvilaws "github.com/anvil-cloud/anvil/sdk/go/anvil/aws"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// ── Context ────────────────────────────────────────────────

// Context is passed to the App's Run callback.
// It wraps pulumi.Context with Anvil-specific information.
type Context struct {
	ctx *pulumi.Context

	// Stage is the current deployment stage.
	Stage string

	// Project is the project name from anvil.yaml.
	Project string

	// Environment is "prod" or "nonprod".
	Environment string

	// IsProduction is true when environment is "prod".
	IsProduction bool

	// Providers holds named providers keyed by config name (e.g. "aws", "aws.us", "gcp").
	Providers map[string]pulumi.ProviderResource
}

// PulumiCtx returns the underlying pulumi.Context.
func (c *Context) PulumiCtx() *pulumi.Context {
	return c.ctx
}

// Export exports a stack output value.
func (c *Context) Export(name string, value pulumi.Input) {
	c.ctx.Export(name, value)
}

// Provider returns the pulumi.Provider ResourceOption for a named provider.
// Use this when creating resources to attach the correct provider:
//
//	anvilaws.NewBucket(ctx.PulumiCtx(), "data", &args, ctx.Provider("aws"))
func (c *Context) Provider(name string) pulumi.ResourceOption {
	if p, ok := c.Providers[name]; ok {
		return pulumi.Provider(p)
	}
	return pulumi.Provider(nil)
}

// ── Config types ───────────────────────────────────────────

// AwsProviderConfig configures an AWS provider.
type AwsProviderConfig struct {
	Region  string
	Profile string
}

// GcpProviderConfig configures a GCP provider.
type GcpProviderConfig struct {
	Project     string
	Region      string
	Zone        string
	Credentials string
}

// DefaultsConfig holds default options applied to all resources.
type DefaultsConfig struct {
	// Tags merged into every taggable resource via defaultTags (AWS) / defaultLabels (GCP).
	// "stage" and "project" are auto-injected. User tags override auto-injected ones.
	Tags map[string]string
}

// ComplianceConfig enables scheduled compliance scans (Prowler) of this app's
// deployed resources. The first deploy with it set creates a scanner shared by
// every Anvil app in the account and region (you'll be asked to confirm).
type ComplianceConfig struct {
	// Frameworks to scan against. Required. Friendly names always point at the
	// newest version Anvil pins: soc2, iso27001, cis, nist-800-53, nist-800-171,
	// nist-csf, pci, hipaa, fsbp, gdpr, nis2, dora, fedramp-low,
	// fedramp-moderate, cmmc, essential-eight, well-architected, c5, csa-ccm.
	// Raw Prowler AWS compliance IDs (e.g. "cis_6.0_aws") are accepted too and
	// stay fixed.
	Frameworks []string

	// Schedule is "daily" (default), "weekly" or "none". Presets run at a fixed
	// time derived from the project and stage. Ignored when Cron is set.
	Schedule string

	// Cron is six AWS cron fields, e.g. "0 3 * * ? *" (at most once an hour).
	Cron string

	// Timezone for Cron, an IANA name such as "Australia/Sydney". Default UTC.
	Timezone string

	// Retention is how long results are kept: 30d, 90d, 180d, 1y (default), 2y, 7y.
	Retention string

	// ScanOnDeploy starts a scan after each successful deploy.
	ScanOnDeploy bool
}

// AppConfig is the configuration for the App.
type AppConfig struct {
	// Run is the infrastructure definition callback. Required.
	Run func(ctx *Context) error

	// Defaults holds default resource options.
	Defaults *DefaultsConfig

	// AwsProviders configures AWS providers.
	// Keys: "aws" (default), "aws.us" (named).
	AwsProviders map[string]*AwsProviderConfig

	// GcpProviders configures GCP providers.
	// Keys: "gcp" (default), "gcp.eu" (named).
	GcpProviders map[string]*GcpProviderConfig

	// Compliance enables scheduled compliance scans. Optional.
	Compliance *ComplianceConfig
}

// ── Run ────────────────────────────────────────────────────

// Run is the entry point for an Anvil infrastructure program.
// It wraps pulumi.Run so users never call it directly.
//
// Example:
//
//	func main() {
//	    anvil.Run(anvil.AppConfig{
//	        Defaults: &anvil.DefaultsConfig{
//	            Tags: map[string]string{"team": "platform"},
//	        },
//	        AwsProviders: map[string]*anvil.AwsProviderConfig{
//	            "aws": {Region: "ap-southeast-2"},
//	        },
//	        Run: func(ctx *anvil.Context) error {
//	            _, err := anvilaws.NewBucket(ctx.PulumiCtx(), "data", &anvilaws.BucketArgs{
//	                DataClassification: pulumi.String("sensitive"),
//	            }, ctx.Provider("aws"))
//	            return err
//	        },
//	    })
//	}
func Run(appConfig AppConfig) {
	pulumi.Run(func(ctx *pulumi.Context) error {
		// ── Read config from Pulumi ────────────────────────
		cfg := config.New(ctx, "anvil")
		stage := cfg.Require("stage")
		environment := cfg.Require("environment")

		// ── Build default tags ─────────────────────────────
		autoTags := map[string]string{
			"stage":   stage,
			"project": ctx.Project(),
		}
		if appConfig.Defaults != nil && appConfig.Defaults.Tags != nil {
			for k, v := range appConfig.Defaults.Tags {
				autoTags[k] = v
			}
		}

		pulumiTags := pulumi.StringMap{}
		for k, v := range autoTags {
			pulumiTags[k] = pulumi.String(v)
		}

		// ── Create providers ───────────────────────────────
		providers := map[string]pulumi.ProviderResource{}

		// AWS providers
		for key, providerCfg := range appConfig.AwsProviders {
			providerName := fmt.Sprintf("anvil-provider-%s", key)

			awsArgs := &aws.ProviderArgs{
				DefaultTags: &aws.ProviderDefaultTagsArgs{
					Tags: pulumiTags,
				},
			}
			if providerCfg.Region != "" {
				awsArgs.Region = pulumi.StringPtr(providerCfg.Region)
			}
			if providerCfg.Profile != "" {
				awsArgs.Profile = pulumi.StringPtr(providerCfg.Profile)
			}

			provider, err := aws.NewProvider(ctx, providerName, awsArgs)
			if err != nil {
				return fmt.Errorf("failed to create AWS provider %q: %w", key, err)
			}

			providers[key] = provider
		}

		// GCP providers
		for key, providerCfg := range appConfig.GcpProviders {
			providerName := fmt.Sprintf("anvil-provider-%s", key)

			gcpArgs := &gcp.ProviderArgs{
				DefaultLabels: pulumiTags,
			}
			if providerCfg.Project != "" {
				gcpArgs.Project = pulumi.StringPtr(providerCfg.Project)
			}
			if providerCfg.Region != "" {
				gcpArgs.Region = pulumi.StringPtr(providerCfg.Region)
			}
			if providerCfg.Zone != "" {
				gcpArgs.Zone = pulumi.StringPtr(providerCfg.Zone)
			}
			if providerCfg.Credentials != "" {
				gcpArgs.Credentials = pulumi.StringPtr(providerCfg.Credentials)
			}

			provider, err := gcp.NewProvider(ctx, providerName, gcpArgs)
			if err != nil {
				return fmt.Errorf("failed to create GCP provider %q: %w", key, err)
			}

			providers[key] = provider
		}

		// ── Create Context ─────────────────────────────────
		anvilCtx := &Context{
			ctx:          ctx,
			Stage:        stage,
			Project:      ctx.Project(),
			Environment:  environment,
			IsProduction: environment == "prod",
			Providers:    providers,
		}

		// ── Execute ────────────────────────────────────────
		if err := appConfig.Run(anvilCtx); err != nil {
			return err
		}

		// ── Compliance ─────────────────────────────────────
		if appConfig.Compliance != nil {
			return createComplianceScanner(ctx, appConfig.Compliance, appConfig.AwsProviders, providers)
		}
		return nil
	})
}

// createComplianceScanner schedules this app's scans on the shared scanner,
// under the app's default AWS provider when one is configured.
func createComplianceScanner(ctx *pulumi.Context, c *ComplianceConfig, awsConfigs map[string]*AwsProviderConfig, providers map[string]pulumi.ProviderResource) error {
	schedule := c.Schedule
	if c.Cron != "" {
		schedule = "cron"
	}

	regionSet := map[string]bool{}
	for _, cfg := range awsConfigs {
		if cfg != nil && cfg.Region != "" {
			regionSet[cfg.Region] = true
		}
	}
	regions := make([]string, 0, len(regionSet))
	for r := range regionSet {
		regions = append(regions, r)
	}
	sort.Strings(regions)

	args := &anvilaws.ComplianceScannerArgs{
		Frameworks:   pulumi.ToStringArray(c.Frameworks),
		ScanOnDeploy: pulumi.BoolPtr(c.ScanOnDeploy),
		Regions:      pulumi.ToStringArray(regions),
	}
	if schedule != "" {
		args.Schedule = pulumi.StringPtr(schedule)
	}
	if c.Cron != "" {
		args.Cron = pulumi.StringPtr(c.Cron)
	}
	if c.Timezone != "" {
		args.Timezone = pulumi.StringPtr(c.Timezone)
	}
	if c.Retention != "" {
		args.Retention = pulumi.StringPtr(c.Retention)
	}

	var opts []pulumi.ResourceOption
	if p, ok := providers["aws"]; ok {
		opts = append(opts, pulumi.Providers(p))
	}
	if _, err := anvilaws.NewComplianceScanner(ctx, "anvil-compliance", args, opts...); err != nil {
		return fmt.Errorf("compliance: %w", err)
	}
	return nil
}
