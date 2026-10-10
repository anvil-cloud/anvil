package dsql

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	dsqlbootstrap "github.com/anvil-cloud/anvil/provider/aws/dsql/bootstrap"
	dsqlgrant "github.com/anvil-cloud/anvil/provider/aws/dsqlgrant"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/anvil-cloud/anvil/provider/internal/transform"
	"github.com/anvil-cloud/anvil/provider/internal/vpcsg"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	awsbackup "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/backup"
	awscloudwatch "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	awsdsql "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/dsql"
	awsdynamodb "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/dynamodb"
	awsec2 "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	awsiam "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	awslambda "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lambda"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// ── Nested arg types ───────────────────────────────────────────────────────

// DSQLMultiRegionArgs configures multi-region active-active replication.
// When set, Anvil creates one cluster per region and links them via
// ClusterPeering. Both regions accept reads and writes with strong consistency.
// When omitted, a single-region cluster is created in the provider's default region.
type DSQLMultiRegionArgs struct {
	Regions       []string `pulumi:"regions"`
	WitnessRegion string   `pulumi:"witnessRegion"`
}

// DSQLRoleArgs defines a database role within a schema. Declared inside a
// schema (schemas[].roles[]) — the role's schema is its parent, so each role
// belongs to exactly one schema. Role names must be globally unique (DSQL
// roles are database-wide). Anvil writes the role definition to DynamoDB; a
// stream triggers the bootstrap Lambda (the only entity with dsql:DbConnectAdmin)
// to run the SQL. The admin endpoint never touches the CI/CD pipeline.
type DSQLRoleArgs struct {
	// Name is the Postgres role name. Must be a valid PostgreSQL identifier
	// and globally unique across all schemas.
	Name string `pulumi:"name" json:"name"`

	// Grants are the table-level privileges granted to this role within its
	// schema, e.g. ["SELECT", "INSERT", "UPDATE", "DELETE"].
	Grants []string `pulumi:"grants" json:"grants"`

	// TableScoping optionally narrows the grants to specific tables instead of
	// the whole schema. When set, the role is granted only on these tables
	// (which must exist — declare them in the same schema's tables). When
	// omitted, grants apply schema-wide (all current + future tables).
	TableScoping []string `pulumi:"tableScoping,optional" json:"tableScoping,omitempty"`
}

// DSQLColumnArgs defines a single column in a table.
// type is a constrained set of DSQL-supported types — arrays, enums, serial,
// money, xml are deliberately excluded because DSQL does not support them.
type DSQLColumnArgs struct {
	// Name is the column name.
	Name string `pulumi:"name" json:"name"`

	// Type is the DSQL column type. One of the supported set:
	// uuid, text, varchar, char, boolean, smallint, integer, bigint,
	// real, double precision, numeric, date, time, timestamp, timestamptz,
	// interval, bytea, json, jsonb.
	Type string `pulumi:"type" json:"type"`

	// Length is the size for char/varchar (e.g. varchar(255)).
	// char ≤ 4096, varchar ≤ 65535. Ignored for other types.
	Length int `pulumi:"length,optional" json:"length,omitempty"`

	// Precision is the total digits for numeric (≤ 38). Ignored for other types.
	Precision int `pulumi:"precision,optional" json:"precision,omitempty"`

	// Scale is the fractional digits for numeric (≤ 37). Ignored for other types.
	Scale int `pulumi:"scale,optional" json:"scale,omitempty"`

	// NotNull adds a NOT NULL constraint. Primary-key columns are auto-NOT-NULL.
	NotNull bool `pulumi:"notNull,optional" json:"notNull,omitempty"`

	// Unique creates a named async unique index on this column.
	// Composite/explicit uniqueness goes in the table's indexes instead.
	Unique bool `pulumi:"unique,optional" json:"unique,omitempty"`

	// Default is a raw SQL default expression, passed through verbatim.
	// String literals need inner quotes: "'active'". Functions: "now()".
	// Anvil cannot validate the expression — it must be DSQL-valid.
	Default string `pulumi:"default,optional" json:"default,omitempty"`
}

// DSQLIndexArgs defines a secondary index on a table.
// All indexes are created with CREATE INDEX ASYNC (DSQL requirement) —
// the build runs in the background; Anvil fires it and logs the job_id.
type DSQLIndexArgs struct {
	// Name is the index name. Required so additive diffing is by name.
	Name string `pulumi:"name" json:"name"`

	// Columns are the indexed columns, in order (order matters for composite).
	Columns []string `pulumi:"columns" json:"columns"`

	// Unique creates a unique index (CREATE UNIQUE INDEX ASYNC).
	Unique bool `pulumi:"unique,optional" json:"unique,omitempty"`
}

// DSQLTableArgs defines a table to create in a schema.
// Additive-only: Anvil creates the table and adds new columns/indexes, but
// never drops or alters existing ones — destructive changes are left to humans.
type DSQLTableArgs struct {
	// Name is the table name (literal — not stage-prefixed).
	Name string `pulumi:"name" json:"name"`

	// Columns are the table's columns.
	Columns []DSQLColumnArgs `pulumi:"columns" json:"columns"`

	// PrimaryKey is the list of column names forming the primary key, in order.
	// Required — DSQL uses the PK as the distribution key and it cannot be added
	// after table creation. Composite keys list multiple columns in order.
	// AWS recommends a uuid PK (generated app-side) to avoid write hotspots.
	PrimaryKey []string `pulumi:"primaryKey" json:"primaryKey"`

	// Indexes are secondary indexes on the table. Optional.
	Indexes []DSQLIndexArgs `pulumi:"indexes,optional" json:"indexes,omitempty"`
}

// DSQLSchemaArgs is the container for a schema, its roles, and its tables.
// Anvil creates the schema (CREATE SCHEMA IF NOT EXISTS), the roles (scoped to
// this schema), and the tables.
type DSQLSchemaArgs struct {
	// Name is the schema name (literal). Must not be "public".
	Name string `pulumi:"name" json:"name"`

	// Roles are the database roles for this schema. Each role's grants apply to
	// this schema. Role names must be globally unique across all schemas.
	Roles []DSQLRoleArgs `pulumi:"roles,optional" json:"roles,omitempty"`

	// Tables are the tables to create in this schema.
	Tables []DSQLTableArgs `pulumi:"tables,optional" json:"tables,omitempty"`
}

// DSQLVpcArgs configures VPC placement and network routing for DSQL.
// When set without HasNat, Anvil creates interface VPC endpoints (PrivateLink)
// per region so traffic stays on the AWS backbone.
// When HasNat is true, endpoint creation is skipped — traffic routes via
// the existing NAT gateway to the public DSQL endpoint.
// Anvil never creates the NAT gateway.
type DSQLVpcArgs struct {
	VpcId            pulumi.StringInput      `pulumi:"vpcId"`
	PrivateSubnetIds pulumi.StringArrayInput `pulumi:"privateSubnetIds"`
	HasNat           bool                    `pulumi:"hasNat,optional"`
}

// DSQLBackupArgs configures AWS Backup for the cluster(s).
// Anvil opts in the DSQL resource type per region, creates a backup plan,
// and for multi-region clusters automatically adds cross-region copy rules.
type DSQLBackupArgs struct {
	RetentionDays      int    `pulumi:"retentionDays,optional"`
	ScheduleExpression string `pulumi:"scheduleExpression,optional"`
	ScheduleTimezone   string `pulumi:"scheduleTimezone,optional"`
	VaultArn           string `pulumi:"vaultArn,optional"`
}

// ── Args ───────────────────────────────────────────────────────────────────

type DSQLArgs struct {
	// MultiRegion enables multi-region active-active replication.
	// When omitted, a single-region cluster is created in the provider's
	// default region.
	MultiRegion *DSQLMultiRegionArgs `pulumi:"multiRegion,optional"`

	// Schemas declares schemas, their roles, and their tables. Anvil creates
	// each schema, its roles (scoped to the schema), and its tables
	// (additive-only — new tables/columns/indexes are applied; removals are
	// left to humans). When omitted, no schema management is performed.
	Schemas []DSQLSchemaArgs `pulumi:"schemas,optional"`

	// Vpc configures VPC placement and network routing.
	// When omitted, the Lambda connects to the public DSQL endpoint.
	Vpc *DSQLVpcArgs `pulumi:"vpc,optional"`

	// Backup configures AWS Backup for the cluster(s).
	// When omitted, no backup resources are created.
	Backup *DSQLBackupArgs `pulumi:"backup,optional"`

	// Transform allows low-level overrides of the underlying aws.dsql.Cluster.
	// Keys: "cluster". Use for KMS key ARN and other cluster-level settings
	// not exposed by the Anvil API.
	Transform map[string]map[string]interface{} `pulumi:"transform,optional"`
}

// ── Component ──────────────────────────────────────────────────────────────

type DSQL struct {
	pulumi.ResourceState

	// Endpoints is a map of region to cluster endpoint.
	// Single-region: one entry keyed by the provider region.
	// Multi-region: one entry per region.
	Endpoints pulumi.StringMapOutput `pulumi:"endpoints"`

	// Endpoint is the single cluster endpoint for the provider's default region.
	// Convenience for the common single-region case — pass it straight to a
	// Lambda env var: { DSQL_ENDPOINT: dsql.endpoint }. For multi-region this
	// is the local-region endpoint; use endpoints[region] to pick another.
	Endpoint pulumi.StringOutput `pulumi:"endpoint"`

	// ClusterArns is a map of region to cluster ARN.
	ClusterArns pulumi.StringMapOutput `pulumi:"clusterArns"`

	// VpcEndpointIds is a map of region to VPC endpoint ID.
	// Only populated when vpc is set and hasNat is false.
	VpcEndpointIds pulumi.StringMapOutput `pulumi:"vpcEndpointIds"`

	// VpcEndpointSecurityGroupIds is a map of region to endpoint SG ID.
	// Only populated when vpc is set and hasNat is false.
	// Pass to LambdaVpcEndpointArgs.securityGroupId.
	VpcEndpointSecurityGroupIds pulumi.StringMapOutput `pulumi:"vpcEndpointSecurityGroupIds"`

	// RolesTableName is the DynamoDB table name for role bootstrap.
	// Only populated when roles is configured. Used by DSQLConnect to
	// create IAM mapping items. Empty when roles is omitted.
	RolesTableName pulumi.StringOutput `pulumi:"rolesTableName"`

	// hasVpcEndpoints tracks whether interface endpoints were created.
	// Used by GrantConnect to decide whether to wire SG egress rules.
	hasVpcEndpoints bool

	// vpcEndpointSgOutputs stores endpoint SG IDs keyed by region as plain
	// StringOutputs. Populated during construction when vpc is set and hasNat
	// is false. Used by GrantConnect to wire egress rules at graph construction
	// time — cannot use VpcEndpointSecurityGroupIds (StringMapOutput) for this
	// because Pulumi resources cannot be created inside an ApplyT.
	vpcEndpointSgOutputs map[string]pulumi.StringOutput

	name string
}

func (d *DSQL) Name() string {
	return d.name
}

func (d *DSQL) Annotate(a infer.Annotator) {
	a.SetToken("aws", "DSQL")
	a.Describe(&d, "An Anvil-managed Aurora DSQL cluster. Serverless distributed PostgreSQL-compatible database with optional multi-region active-active replication, IAM authentication, deploy-time role bootstrapping, and AWS Backup integration. No VPC required.")
}

func NewDSQL(ctx *pulumi.Context, name string, args DSQLArgs, opts ...pulumi.ResourceOption) (*DSQL, error) {
	d := &DSQL{name: name}

	// Validate + normalize schemas/tables before anything else so bad config
	// fails fast with a clear message (auto-NOT-NULL on PK cols is applied
	// here, mutating args.Schemas).
	if err := validateSchemas(args.Schemas); err != nil {
		return nil, fmt.Errorf("invalid DSQL schemas config: %w", err)
	}

	if os.Getenv("ANVIL_BUILD_MODE") == "true" {
		if len(args.Schemas) > 0 {
			if err := appendSchemasToManifest(name, args.Schemas); err != nil {
				return nil, fmt.Errorf("build manifest write failed for %s: %w", name, err)
			}
		}

		// Build mode runs a Pulumi preview to extract schema metadata without
		// creating real cloud resources. We still must register the component
		// and its output properties: returning an unregistered component leaves
		// the pulumi:"..." output fields nil, which panics the provider when he
		// engine serializes the component's outputs (or a consumer such as t
		// Lambda env var references dsql.endpoint / dsql.clusterArns).a
		opts = provider.WithDefault(opts, true)
		if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, d, opts...); err != nil {
			return nil, err
		}

		empty := pulumi.StringMap{}.ToStringMapOutput()
		emptyStr := pulumi.String("").ToStringOutput()
		d.Endpoints = empty
		d.Endpoint = emptyStr
		d.ClusterArns = empty
		d.VpcEndpointIds = empty
		d.VpcEndpointSecurityGroupIds = empty
		d.RolesTableName = emptyStr

		ctx.RegisterResourceOutputs(d, pulumi.Map{
			"endpoints":                   d.Endpoints,
			"endpoint":                    d.Endpoint,
			"clusterArns":                 d.ClusterArns,
			"vpcEndpointIds":              d.VpcEndpointIds,
			"vpcEndpointSecurityGroupIds": d.VpcEndpointSecurityGroupIds,
			"rolesTableName":              d.RolesTableName,
		})

		return d, nil
	}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, d, opts...); err != nil {
		return nil, err
	}

	// ── 1. Resolve default region ──────────────────────────────────────────
	// Used for single-region clusters and as the output map key.
	// Safe to call outside Apply — synchronous SDK call.
	defaultRegion, err := aws.GetRegion(ctx, &aws.GetRegionArgs{}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve AWS region: %w", err)
	}
	providerRegion := defaultRegion.Region

	// ── 2. Determine regions and witness ──────────────────────────────────
	isMultiRegion := args.MultiRegion != nil
	var regions []string
	var witnessRegion string

	if isMultiRegion {
		regions = args.MultiRegion.Regions
		witnessRegion = args.MultiRegion.WitnessRegion
	} else {
		regions = []string{providerRegion}
	}

	// ── 3. Regional providers ──────────────────────────────────────────────
	// One provider per region. Reuses the default provider for the stack's
	// default region to avoid creating a redundant provider.
	regionalProviders := make(map[string]*aws.Provider, len(regions))
	for _, region := range regions {
		if region == providerRegion {
			regionalProviders[region] = nil
			continue
		}
		rp, err := aws.NewProvider(ctx, fmt.Sprintf("%s-provider-%s", name, region), &aws.ProviderArgs{
			Region: pulumi.String(region),
		}, pulumi.Parent(d))
		if err != nil {
			return nil, fmt.Errorf("creating regional provider for %s: %w", region, err)
		}
		regionalProviders[region] = rp
	}

	// resourceOpts returns the correct provider option for a given region.
	// Always includes pulumi.Parent(d) for correct resource hierarchy.
	resourceOpts := func(region string) []pulumi.ResourceOption {
		base := []pulumi.ResourceOption{pulumi.Parent(d)}
		if rp := regionalProviders[region]; rp != nil {
			base = append(base, pulumi.Provider(rp))
		}
		return base
	}

	// ── 4. Clusters ────────────────────────────────────────────────────────
	// Single-region: one cluster, no multiRegionProperties.
	// Multi-region: two clusters each with witnessRegion set.
	// Both enter PENDING_SETUP — peering (phase 5) completes the link.
	clusters := make(map[string]*awsdsql.Cluster, len(regions))

	for _, region := range regions {

		resourceName := name
		if isMultiRegion {
			resourceName = fmt.Sprintf("%s-%s", name, region)
		}

		clusterMap := pulumi.Map{
			"tags": pulumi.StringMap{
				// DSQL clusters have no name field — the console shows the Name tag.
				"Name":      pulumi.String(provider.PhysicalName(stage, resourceName, "dsql", stageId)),
				"ManagedBy": pulumi.String("anvil"),
			},
		}

		if isMultiRegion {
			clusterMap["multiRegionProperties"] = pulumi.Map{
				"witnessRegion": pulumi.String(witnessRegion),
			}
		}

		clusterProps := transform.MergeTransform(args.Transform["cluster"], clusterMap)
		clusterRes := &awsdsql.Cluster{}

		clusterOpts := append(resourceOpts(region), pulumi.Protect(true))

		if err := ctx.RegisterResource(
			"aws:dsql/cluster:Cluster",
			resourceName,
			clusterProps,
			clusterRes,
			clusterOpts...,
		); err != nil {
			return nil, fmt.Errorf("creating DSQL cluster in %s: %w", region, err)
		}

		clusters[region] = clusterRes
	}

	// ── 5. ClusterPeering ──────────────────────────────────────────────────
	// Only for multi-region. Creates two ClusterPeering resources that
	// cross-reference each other's ARN. Pulumi waits for both clusters
	// before running either peering resource — no circular dependency.
	// After peering: PENDING_SETUP → CREATING → ACTIVE.
	if isMultiRegion && len(regions) == 2 {
		regionA := regions[0]
		regionB := regions[1]
		clusterA := clusters[regionA]
		clusterB := clusters[regionB]

		if _, err := awsdsql.NewClusterPeering(ctx,
			fmt.Sprintf("%s-peer-%s", name, regionA),
			&awsdsql.ClusterPeeringArgs{
				Identifier:    clusterA.Identifier,
				Clusters:      pulumi.StringArray{clusterB.Arn},
				WitnessRegion: pulumi.String(witnessRegion),
			},
			resourceOpts(regionA)...,
		); err != nil {
			return nil, fmt.Errorf("creating cluster peering for %s: %w", regionA, err)
		}

		if _, err := awsdsql.NewClusterPeering(ctx,
			fmt.Sprintf("%s-peer-%s", name, regionB),
			&awsdsql.ClusterPeeringArgs{
				Identifier:    clusterB.Identifier,
				Clusters:      pulumi.StringArray{clusterA.Arn},
				WitnessRegion: pulumi.String(witnessRegion),
			},
			resourceOpts(regionB)...,
		); err != nil {
			return nil, fmt.Errorf("creating cluster peering for %s: %w", regionB, err)
		}
	}

	// ── Cluster ARN map ────────────────────────────────────────────────────
	// Assigned here (not in the outputs section) because section 8 (role
	// bootstrap) needs d.ClusterArns to build the dsql:DbConnectAdmin policy.
	// Section 9 reuses this — it must not be reassigned to a zero value.
	clusterArnMap := pulumi.StringMap{}
	for region, cluster := range clusters {
		clusterArnMap[region] = cluster.Arn
	}
	d.ClusterArns = clusterArnMap.ToStringMapOutput()

	// ── 6. VPC endpoints ───────────────────────────────────────────────────
	// Only when vpc is set and hasNat is false.
	// One interface endpoint per region with a dedicated SG.
	// Self-referencing ingress on port 5432 (Postgres).
	// grantConnect wires the egress side on the Lambda SG.
	//
	// The endpoint service name is cluster-specific:
	// com.amazonaws.{region}.dsql-{hex} — exposed as cluster.VpcEndpointServiceName.
	// This differs from standard AWS services which have fixed service names,
	// which is why DSQL cannot use the existing VpcEndpoint component or enum.
	vpcEndpointIds := pulumi.StringMap{}
	vpcEndpointSgIds := pulumi.StringMap{}
	d.vpcEndpointSgOutputs = make(map[string]pulumi.StringOutput)

	if args.Vpc != nil && !args.Vpc.HasNat {
		for _, region := range regions {
			cluster := clusters[region]

			sg, err := vpcsg.CreateSecurityGroup(
				ctx,
				fmt.Sprintf("%s-vpce-sg-%s", name, region),
				stage,
				stageId,
				args.Vpc.VpcId,
				d,
			)
			if err != nil {
				return nil, fmt.Errorf("creating VPC endpoint SG for %s: %w", region, err)
			}

			if err := vpcsg.AddIngressRule(
				ctx,
				fmt.Sprintf("%s-vpce-%s-self-ingress", name, region),
				sg.ID().ToStringOutput(),
				sg.ID().ToStringOutput(),
				5432, 5432,
				"tcp",
				d,
			); err != nil {
				return nil, fmt.Errorf("creating self-referencing ingress for %s: %w", region, err)
			}

			endpointOpts := resourceOpts(region)
			endpoint, err := awsec2.NewVpcEndpoint(ctx,
				fmt.Sprintf("%s-vpce-%s", name, region),
				&awsec2.VpcEndpointArgs{
					VpcId:             args.Vpc.VpcId.ToStringOutput(),
					ServiceName:       cluster.VpcEndpointServiceName,
					VpcEndpointType:   pulumi.String("Interface"),
					PrivateDnsEnabled: pulumi.Bool(true),
					SubnetIds:         args.Vpc.PrivateSubnetIds.ToStringArrayOutput(),
					SecurityGroupIds:  pulumi.StringArray{sg.ID()},
					Tags: pulumi.StringMap{
						"Name":      pulumi.String(provider.PhysicalName(stage, fmt.Sprintf("%s-%s", name, region), "vpce", stageId)),
						"ManagedBy": pulumi.String("anvil"),
					},
				},
				endpointOpts...,
			)
			if err != nil {
				return nil, fmt.Errorf("creating VPC endpoint for %s: %w", region, err)
			}

			vpcEndpointIds[region] = endpoint.ID().ToStringOutput()
			vpcEndpointSgIds[region] = sg.ID().ToStringOutput()
			d.vpcEndpointSgOutputs[region] = sg.ID().ToStringOutput()
		}

		d.hasVpcEndpoints = true
	}

	// ── 7. Backup ──────────────────────────────────────────────────────────
	// Only when backup block is set.
	// Opts in DSQL per region, creates a plan with schedule and retention.
	// Multi-region: cross-region copy rules are wired automatically so
	// restores work in both regions — AWS Backup does not replicate by default.
	if args.Backup != nil {
		retentionDays := args.Backup.RetentionDays
		if retentionDays == 0 {
			retentionDays = 35
		}
		scheduleExpr := args.Backup.ScheduleExpression
		if scheduleExpr == "" {
			scheduleExpr = "cron(0 0 * * ? *)"
		}
		scheduleTz := args.Backup.ScheduleTimezone
		if scheduleTz == "" {
			scheduleTz = "Etc/UTC"
		}
		vaultName := "Default"
		if args.Backup.VaultArn != "" {
			parts := strings.Split(args.Backup.VaultArn, ":")
			vaultName = parts[len(parts)-1]
		}

		// Opt in DSQL resource type per region — required before AWS Backup
		// can protect DSQL clusters in that region.
		for _, region := range regions {
			region := region
			if _, err := awsbackup.NewRegionSettings(ctx,
				fmt.Sprintf("%s-backup-settings-%s", name, region),
				&awsbackup.RegionSettingsArgs{
					ResourceTypeOptInPreference: pulumi.BoolMap{
						"DSQL": pulumi.Bool(true),
					},
					ResourceTypeManagementPreference: pulumi.BoolMap{
						"DSQL": pulumi.Bool(true),
					},
				},
				resourceOpts(region)...,
			); err != nil {
				return nil, fmt.Errorf("enabling DSQL backup region settings for %s: %w", region, err)
			}
		}

		// Cross-region copy actions for multi-region clusters.
		// One copy action per region so each region has its own recovery point.
		// Without this, multi-region restore fails because the peer region
		// has no recovery point to restore from.
		var copyActions awsbackup.PlanRuleCopyActionArray
		if isMultiRegion && len(regions) == 2 {
			for _, region := range regions {
				destVaultArn := clusters[region].Arn.ApplyT(func(arn string) string {
					parts := strings.Split(arn, ":")
					if len(parts) < 5 {
						return ""
					}
					accountId := parts[4]
					return fmt.Sprintf("arn:aws:backup:%s:%s:backup-vault:%s", region, accountId, vaultName)
				}).(pulumi.StringOutput)

				copyActions = append(copyActions, awsbackup.PlanRuleCopyActionArgs{
					DestinationVaultArn: destVaultArn,
					Lifecycle: &awsbackup.PlanRuleCopyActionLifecycleArgs{
						DeleteAfter: pulumi.Int(retentionDays),
					},
				})
			}
		}

		planRule := awsbackup.PlanRuleArgs{
			RuleName:                   pulumi.String(fmt.Sprintf("%s-daily", name)),
			TargetVaultName:            pulumi.String(vaultName),
			Schedule:                   pulumi.String(scheduleExpr),
			ScheduleExpressionTimezone: pulumi.String(scheduleTz),
			Lifecycle: &awsbackup.PlanRuleLifecycleArgs{
				DeleteAfter: pulumi.Int(retentionDays),
			},
		}
		if len(copyActions) > 0 {
			planRule.CopyActions = copyActions
		}

		plan, err := awsbackup.NewPlan(ctx,
			fmt.Sprintf("%s-backup-plan", name),
			&awsbackup.PlanArgs{
				Name:  pulumi.String(provider.PhysicalName(stage, name, "backup-plan", stageId)),
				Rules: awsbackup.PlanRuleArray{planRule},
				Tags: pulumi.StringMap{
					"ManagedBy": pulumi.String("anvil"),
				},
			},
			pulumi.Parent(d),
		)
		if err != nil {
			return nil, fmt.Errorf("creating backup plan: %w", err)
		}

		// Collect all cluster ARNs for the backup selection resource.
		var clusterArns pulumi.StringArray
		for _, cluster := range clusters {
			clusterArns = append(clusterArns, cluster.Arn)
		}

		// IAM role for AWS Backup to access DSQL clusters.
		backupRole, err := awsiam.NewRole(ctx,
			fmt.Sprintf("%s-backup-role", name),
			&awsiam.RoleArgs{
				Name: pulumi.String(provider.PhysicalName(stage, name, "backup-role", stageId)),
				AssumeRolePolicy: pulumi.String(`{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Service": "backup.amazonaws.com" },
    "Action": "sts:AssumeRole"
  }]
}`),
				ManagedPolicyArns: pulumi.StringArray{
					pulumi.String("arn:aws:iam::aws:policy/service-role/AWSBackupServiceRolePolicyForBackup"),
				},
				Tags: pulumi.StringMap{
					"ManagedBy": pulumi.String("anvil"),
				},
			},
			pulumi.Parent(d),
		)
		if err != nil {
			return nil, fmt.Errorf("creating backup IAM role: %w", err)
		}

		if _, err := awsbackup.NewSelection(ctx,
			fmt.Sprintf("%s-backup-selection", name),
			&awsbackup.SelectionArgs{
				Name:       pulumi.String(provider.PhysicalName(stage, name, "backup-sel", stageId)),
				PlanId:     plan.ID(),
				IamRoleArn: backupRole.Arn,
				Resources:  clusterArns,
			},
			pulumi.Parent(d),
		); err != nil {
			return nil, fmt.Errorf("creating backup selection: %w", err)
		}
	}

	// ── 8. Role bootstrap ──────────────────────────────────────────────────
	//
	// ARCHITECTURE (security-first, least-privilege):
	//
	//   Pulumi (deploy) ──► DynamoDB TableItem ──► DynamoDB Stream ──► Bootstrap Lambda
	//                                                                    │
	//                          ┌─────────────────────────────────────────┘
	//                          ▼
	//                   DSQL cluster (SQL)
	//                   - CREATE SCHEMA / ROLE / GRANT (from DEFINITION items)
	//
	// The admin endpoint (dsql:DbConnectAdmin) NEVER runs in the CI/CD pipeline.
	// If the pipeline is compromised, an attacker can only write role definitions
	// to DynamoDB — they cannot execute arbitrary SQL against the cluster.
	//
	// The bootstrap Lambda:
	//   - Can ONLY be triggered by the DynamoDB stream (no lambda:InvokeFunction)
	//   - Has dsql:DbConnectAdmin scoped to these cluster ARNs only
	//   - Reads DSQL_ENDPOINT and DSQL_REGION from its own env vars
	//   - Generates IAM auth tokens per-request (no stored credentials)
	//   - Is idempotent: INSERT creates, MODIFY updates, REMOVE drops
	//
	// Pulumi lifecycle handles diffing: unchanged roles are skipped entirely,
	// no API calls, no stream events, no Lambda invocations.
	//
	// MULTI-CLUSTER ISOLATION:
	//   Each DSQL component creates its own independent DynamoDB table and
	//   bootstrap Lambda. Two clusters = two tables, two Lambdas, each with
	//   their own DSQL_ENDPOINT env var. No cross-cluster confusion.
	//
	//     DSQL("orders")    → dev-orders-dsql-roles-abc123    + dev-orders-dsql-bootstrap-abc123
	//     DSQL("analytics") → dev-analytics-dsql-roles-def456 + dev-analytics-dsql-bootstrap-def456
	//
	// DynamoDB table schema (composite key):
	//
	//   PK: roleName (S)  — Postgres role name, e.g. "app_role"
	//   SK: sk (S)        — item type discriminator, currently "DEFINITION"
	//
	//   Role definition item (sk = "DEFINITION"):
	//     Attributes: schema (S), grants (S, comma-separated)
	//     Stream INSERT → CREATE SCHEMA, CREATE ROLE, GRANT, ALTER DEFAULT PRIVILEGES
	//     Stream MODIFY → REVOKE old, GRANT new
	//     Stream REMOVE → REVOKE ALL, DROP OWNED BY, DROP ROLE
	//
	//   Cluster endpoint/region are NOT stored per-item — they are infrastructure
	//   config set as Lambda env vars (DSQL_ENDPOINT, DSQL_REGION).
	//
	// FRONTEND / DASHBOARD NOTES:
	//   Reading roles: query the DynamoDB table directly. Table name follows
	//   Anvil naming: {stage}-{componentName}-dsql-roles-{stageId}.
	//   Writes should go through Pulumi (code-defined roles), but reads
	//   are safe from any authorized service.
	//
	//   Limits: DSQL allows max 10 schemas per database, 1 database per cluster.
	//
	if len(args.Schemas) > 0 {

		// ── 8a. DynamoDB roles table ──────────────────────────────────────
		rolesTableName := provider.PhysicalName(stage, name, "dsql-roles", stageId)

		rolesTable := &awsdynamodb.Table{}
		if err := ctx.RegisterResource("aws:dynamodb/table:Table",
			fmt.Sprintf("%s-roles-table", name),
			pulumi.Map{
				"name":     pulumi.String(rolesTableName),
				"hashKey":  pulumi.String("roleName"),
				"rangeKey": pulumi.String("sk"),
				"attributes": pulumi.MapArray{
					pulumi.Map{
						"name": pulumi.String("roleName"),
						"type": pulumi.String("S"),
					},
					pulumi.Map{
						"name": pulumi.String("sk"),
						"type": pulumi.String("S"),
					},
				},
				"billingMode":               pulumi.String("PAY_PER_REQUEST"),
				"deletionProtectionEnabled": pulumi.Bool(true),
				"pointInTimeRecovery": pulumi.Map{
					"enabled": pulumi.Bool(true),
				},
				"streamEnabled":  pulumi.Bool(true),
				"streamViewType": pulumi.String("NEW_AND_OLD_IMAGES"),
				"tags": pulumi.StringMap{
					"ManagedBy": pulumi.String("anvil"),
				},
			},
			rolesTable,
			pulumi.Parent(d),
		); err != nil {
			return nil, fmt.Errorf("creating DSQL roles table: %w", err)
		}
		d.RolesTableName = rolesTable.Name

		// ── 8b. Bootstrap Lambda IAM role ─────────────────────────────────
		bootstrapRoleName := provider.PhysicalName(stage, name, "dsql-bootstrap-role", stageId)

		bootstrapRole := &awsiam.Role{}
		if err := ctx.RegisterResource("aws:iam/role:Role",
			fmt.Sprintf("%s-bootstrap-role", name),
			pulumi.Map{
				"name": pulumi.String(bootstrapRoleName),
				"assumeRolePolicy": pulumi.String(`{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Service": "lambda.amazonaws.com" },
    "Action": "sts:AssumeRole"
  }]
}`),
				"tags": pulumi.StringMap{
					"ManagedBy": pulumi.String("anvil"),
				},
			},
			bootstrapRole,
			pulumi.Parent(d),
		); err != nil {
			return nil, fmt.Errorf("creating bootstrap Lambda IAM role: %w", err)
		}

		// Basic execution policy (CloudWatch logs)
		basicExec := &awsiam.RolePolicyAttachment{}
		if err := ctx.RegisterResource("aws:iam/rolePolicyAttachment:RolePolicyAttachment",
			fmt.Sprintf("%s-bootstrap-basic-exec", name),
			pulumi.Map{
				"role":      bootstrapRole.Name,
				"policyArn": pulumi.String("arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"),
			},
			basicExec,
			pulumi.Parent(d),
		); err != nil {
			return nil, fmt.Errorf("attaching basic exec policy to bootstrap role: %w", err)
		}

		// dsql:DbConnectAdmin on all cluster ARNs
		adminPolicyJSON := d.ClusterArns.ApplyT(func(arns map[string]string) (string, error) {
			resources := make([]string, 0, len(arns))
			for _, arn := range arns {
				resources = append(resources, arn)
			}
			type statement struct {
				Effect   string   `json:"Effect"`
				Action   []string `json:"Action"`
				Resource []string `json:"Resource"`
			}
			type doc struct {
				Version   string      `json:"Version"`
				Statement []statement `json:"Statement"`
			}
			b, err := json.Marshal(doc{
				Version: "2012-10-17",
				Statement: []statement{{
					Effect:   "Allow",
					Action:   []string{"dsql:DbConnectAdmin"},
					Resource: resources,
				}},
			})
			if err != nil {
				return "", fmt.Errorf("failed to marshal admin policy: %w", err)
			}
			return string(b), nil
		}).(pulumi.StringOutput)

		if err := ctx.RegisterResource("aws:iam/rolePolicy:RolePolicy",
			fmt.Sprintf("%s-bootstrap-admin-policy", name),
			pulumi.Map{
				"role":   bootstrapRole.Name,
				"policy": adminPolicyJSON,
			},
			&awsiam.RolePolicy{},
			pulumi.Parent(d),
		); err != nil {
			return nil, fmt.Errorf("attaching admin policy to bootstrap role: %w", err)
		}

		// ── 8c. Bootstrap Lambda log group ────────────────────────────────
		bootstrapLambdaName := provider.PhysicalName(stage, name, "dsql-bootstrap", stageId)

		logGroup := &awscloudwatch.LogGroup{}
		if err := ctx.RegisterResource("aws:cloudwatch/logGroup:LogGroup",
			fmt.Sprintf("%s-bootstrap-logs", name),
			pulumi.Map{
				"name":            pulumi.String(fmt.Sprintf("/aws/lambda/%s", bootstrapLambdaName)),
				"retentionInDays": pulumi.Int(30),
				"tags": pulumi.StringMap{
					"ManagedBy": pulumi.String("anvil"),
				},
			},
			logGroup,
			pulumi.Parent(d),
		); err != nil {
			return nil, fmt.Errorf("creating bootstrap log group: %w", err)
		}

		// ── 8d. Bootstrap Lambda function ─────────────────────────────────
		// Code is embedded in the provider binary at build time.
		// Built by: go run build.go build-dsql-lambda (runs before build-provider)
		// Source:   cmd/anvil/dsql-lambda/main.go
		bootstrapZipPath, err := dsqlbootstrap.WriteDSQLLambdaZip()
		if err != nil {
			return nil, fmt.Errorf("extracting bootstrap Lambda zip: %w", err)
		}
		bootstrapCode := pulumi.NewFileArchive(bootstrapZipPath)

		// Resolve cluster endpoint for Lambda env var.
		// Each DSQL component's Lambda gets its own endpoint — no cross-cluster confusion.
		firstRegion := regions[0]
		clusterEndpoint := clusters[firstRegion].Identifier.ApplyT(func(id string) string {
			return fmt.Sprintf("%s.dsql.%s.on.aws", id, firstRegion)
		}).(pulumi.StringOutput)

		bootstrapLambda := &awslambda.Function{}
		if err := ctx.RegisterResource("aws:lambda/function:Function",
			fmt.Sprintf("%s-bootstrap-fn", name),
			pulumi.Map{
				"name":          pulumi.String(bootstrapLambdaName),
				"runtime":       pulumi.String("provided.al2023"),
				"handler":       pulumi.String("bootstrap"),
				"role":          bootstrapRole.Arn,
				"memorySize":    pulumi.Int(256),
				"timeout":       pulumi.Int(30),
				"architectures": pulumi.StringArray{pulumi.String("arm64")},
				"code":          bootstrapCode,
				"environment": pulumi.Map{
					"variables": pulumi.StringMap{
						"DSQL_ENDPOINT": clusterEndpoint,
						"DSQL_REGION":   pulumi.String(firstRegion),
					},
				},
				"tags": pulumi.StringMap{
					"ManagedBy": pulumi.String("anvil"),
				},
			},
			bootstrapLambda,
			pulumi.Parent(d),
			pulumi.DependsOn([]pulumi.Resource{basicExec, logGroup}),
		); err != nil {
			return nil, fmt.Errorf("creating bootstrap Lambda: %w", err)
		}

		// ── 8e. Stream → Lambda wiring ────────────────────────────────────
		// Resource-based policy for DynamoDB Streams to invoke the Lambda.
		if _, err := awslambda.NewPermission(ctx,
			fmt.Sprintf("%s-bootstrap-stream-invoke", name),
			&awslambda.PermissionArgs{
				Action:    pulumi.String("lambda:InvokeFunction"),
				Function:  bootstrapLambda.Arn,
				Principal: pulumi.String("lambda.amazonaws.com"),
				SourceArn: rolesTable.StreamArn,
			},
			pulumi.Parent(d),
		); err != nil {
			return nil, fmt.Errorf("creating stream invoke permission: %w", err)
		}

		// Stream read permissions on bootstrap Lambda role
		streamReadPolicy := rolesTable.StreamArn.ApplyT(func(streamArn string) (string, error) {
			type statement struct {
				Effect   string   `json:"Effect"`
				Action   []string `json:"Action"`
				Resource []string `json:"Resource"`
			}
			type doc struct {
				Version   string      `json:"Version"`
				Statement []statement `json:"Statement"`
			}
			b, err := json.Marshal(doc{
				Version: "2012-10-17",
				Statement: []statement{{
					Effect: "Allow",
					Action: []string{
						"dynamodb:GetRecords",
						"dynamodb:GetShardIterator",
						"dynamodb:DescribeStream",
						"dynamodb:ListStreams",
					},
					Resource: []string{streamArn},
				}},
			})
			if err != nil {
				return "", fmt.Errorf("failed to marshal stream read policy: %w", err)
			}
			return string(b), nil
		}).(pulumi.StringOutput)

		if err := ctx.RegisterResource("aws:iam/rolePolicy:RolePolicy",
			fmt.Sprintf("%s-bootstrap-stream-read", name),
			pulumi.Map{
				"role":   bootstrapRole.Name,
				"policy": streamReadPolicy,
			},
			&awsiam.RolePolicy{},
			pulumi.Parent(d),
		); err != nil {
			return nil, fmt.Errorf("attaching stream read policy: %w", err)
		}

		// Event Source Mapping — DynamoDB Stream → Bootstrap Lambda
		if _, err := awslambda.NewEventSourceMapping(ctx,
			fmt.Sprintf("%s-bootstrap-esm", name),
			&awslambda.EventSourceMappingArgs{
				EventSourceArn:   rolesTable.StreamArn,
				FunctionName:     bootstrapLambda.Arn,
				StartingPosition: pulumi.String("TRIM_HORIZON"),
				BatchSize:        pulumi.Int(1),
				Enabled:          pulumi.Bool(true),
				// Bound retries so a poison-pill record can't loop for the full
				// 24h stream retention (and block records behind it). After the
				// attempts are exhausted the record is dropped and the stream
				// advances. INSERT/MODIFY validation happens at deploy time, so
				// a permanently-failing record is rare; REMOVE is best-effort.
				MaximumRetryAttempts:       pulumi.Int(3),
				BisectBatchOnFunctionError: pulumi.Bool(true),
			},
			pulumi.Parent(d),
		); err != nil {
			return nil, fmt.Errorf("creating bootstrap event source mapping: %w", err)
		}

		// ── 8f. Table definition items ────────────────────────────────────
		// One DynamoDB item per table, kind="table". The bootstrap Lambda
		// creates the schema + table, and additively adds new columns/indexes
		// on change. Removals are no-ops (destructive changes are human-owned).
		// Validated at deploy time (validateSchemas) before reaching here.
		//
		// Created BEFORE role items so that table-scoped role grants
		// (GRANT ... ON schema.table) run after the tables exist — role items
		// DependsOn these so the stream delivers tables first.
		tableItemResources := []pulumi.Resource{rolesTable}
		for _, schema := range args.Schemas {
			for _, table := range schema.Tables {
				table := table
				schemaName := schema.Name

				itemJSON, err := marshalTableItem(schemaName, table)
				if err != nil {
					return nil, fmt.Errorf("marshalling table %s.%s: %w", schemaName, table.Name, err)
				}

				tableItem := &awsdynamodb.TableItem{}
				if err := ctx.RegisterResource("aws:dynamodb/tableItem:TableItem",
					fmt.Sprintf("%s-table-%s-%s", name, schemaName, table.Name),
					pulumi.Map{
						"tableName": rolesTable.Name,
						"hashKey":   pulumi.String("roleName"),
						"rangeKey":  pulumi.String("sk"),
						"item":      pulumi.String(itemJSON),
					},
					tableItem,
					pulumi.Parent(d),
					pulumi.DependsOn([]pulumi.Resource{rolesTable}),
				); err != nil {
					return nil, fmt.Errorf("creating table item for %s.%s: %w", schemaName, table.Name, err)
				}
				tableItemResources = append(tableItemResources, tableItem)
			}
		}

		// ── 8g. Role definition items ─────────────────────────────────────
		// One DynamoDB item per role with sk="DEFINITION".
		// When roles change → PutItem/DeleteItem → stream fires → Lambda acts.
		// Endpoint/region are Lambda env vars, not stored per-item.
		// tableScoping (when set) narrows grants to specific tables — those role
		// items DependsOn all table items so the tables exist before the grant.
		for _, schema := range args.Schemas {
			schemaName := schema.Name
			for _, role := range schema.Roles {
				role := role

				grantsList := make([]map[string]string, len(role.Grants))
				for i, g := range role.Grants {
					grantsList[i] = map[string]string{"S": g}
				}

				item := map[string]interface{}{
					"roleName": map[string]string{"S": role.Name},
					"sk":       map[string]string{"S": "DEFINITION"},
					"schema":   map[string]string{"S": schemaName},
					"grants":   map[string]interface{}{"L": grantsList},
				}
				if len(role.TableScoping) > 0 {
					scoping := make([]map[string]string, len(role.TableScoping))
					for i, t := range role.TableScoping {
						scoping[i] = map[string]string{"S": t}
					}
					item["tableScoping"] = map[string]interface{}{"L": scoping}
				}
				itemBytes, _ := json.Marshal(item)
				itemJSON := string(itemBytes)

				// Table-scoped roles depend on all table items (tables must exist
				// before GRANT ON schema.table). Schema-wide roles only need the table.
				deps := []pulumi.Resource{rolesTable}
				if len(role.TableScoping) > 0 {
					deps = tableItemResources
				}

				roleItem := &awsdynamodb.TableItem{}
				if err := ctx.RegisterResource("aws:dynamodb/tableItem:TableItem",
					fmt.Sprintf("%s-role-%s", name, role.Name),
					pulumi.Map{
						"tableName": rolesTable.Name,
						"hashKey":   pulumi.String("roleName"),
						"rangeKey":  pulumi.String("sk"),
						"item":      pulumi.String(itemJSON),
					},
					roleItem,
					pulumi.Parent(d),
					pulumi.DependsOn(deps),
				); err != nil {
					return nil, fmt.Errorf("creating role item for %s: %w", role.Name, err)
				}
			}
		}
	}

	// ── 9. Outputs ─────────────────────────────────────────────────────────
	// The standard aws provider does not expose an Endpoint output field on
	// aws.dsql.Cluster. The endpoint follows a fixed pattern:
	// {identifier}.dsql.{region}.on.aws
	// Constructed from the Identifier output and the known region string.
	endpointMap := pulumi.StringMap{}
	for region, cluster := range clusters {
		endpointMap[region] = cluster.Identifier.ApplyT(func(id string) string {
			return fmt.Sprintf("%s.dsql.%s.on.aws", id, region)
		}).(pulumi.StringOutput)
	}

	d.Endpoints = endpointMap.ToStringMapOutput()
	// Singular convenience endpoint: the provider/default region's cluster.
	d.Endpoint = clusters[providerRegion].Identifier.ApplyT(func(id string) string {
		return fmt.Sprintf("%s.dsql.%s.on.aws", id, providerRegion)
	}).(pulumi.StringOutput)
	// d.ClusterArns already assigned earlier (before section 6) — do not reassign.
	d.VpcEndpointIds = vpcEndpointIds.ToStringMapOutput()
	d.VpcEndpointSecurityGroupIds = vpcEndpointSgIds.ToStringMapOutput()
	if len(args.Schemas) == 0 {
		d.RolesTableName = pulumi.String("").ToStringOutput()
	}

	outputMap := pulumi.Map{
		"endpoints":                   endpointMap,
		"endpoint":                    d.Endpoint,
		"clusterArns":                 clusterArnMap,
		"vpcEndpointIds":              vpcEndpointIds,
		"vpcEndpointSecurityGroupIds": vpcEndpointSgIds,
		"rolesTableName":              d.RolesTableName,
	}
	ctx.RegisterResourceOutputs(d, outputMap)

	return d, nil
}

// ── Grant methods ──────────────────────────────────────────────────────────

// DSQLConnectRole identifies a role to grant a compute, by schema + name.
// The schema is for readability/validation; the IAM mapping uses the role name.
type DSQLConnectRole struct {
	Schema   string
	RoleName string
}

// GrantConnect grants a compute resource connect access to this DSQL cluster.
// Pass one or more { schema, roleName } pairs — the compute may be mapped to
// multiple roles and connects as whichever one it chooses at runtime.
// Creates an anvil:aws:DSQLConnect component that handles:
//
//  1. IAM policy — dsql:DbConnect on all cluster ARNs
//  2. IAM-to-role mappings — one DynamoDB item per role, triggering the
//     bootstrap Lambda to run: AWS IAM GRANT "<role>" TO 'arn:...'
//
// When vpc endpoints were created (hasNat false), also wires egress rules
// from the target's SG to each endpoint SG on port 5432.
//
// The target must declare the endpoint SGs in vpc.vpcEndpoints at construction
// time — GrantConnect cannot attach SGs to an already-constructed target.
func (d *DSQL) GrantConnect(
	ctx *pulumi.Context,
	target provider.GrantTarget,
	roles []DSQLConnectRole,
) error {
	dbRoles := make([]string, len(roles))
	for i, r := range roles {
		dbRoles[i] = r.RoleName
	}

	grantArgs := dsqlgrant.DSQLConnectArgs{
		ClusterArns:   d.ClusterArns,
		TargetRoleArn: target.RoleARN(),
		TargetName:    target.Name(),
		DbRoles:       dbRoles,
	}

	if d.RolesTableName != (pulumi.StringOutput{}) {
		grantArgs.RolesTableName = d.RolesTableName
	}

	// Not parented to the DSQL component: DSQLConnect depends on this
	// component's outputs (ClusterArns, RolesTableName). Parenting would
	// create a dependency cycle — the parent's outputs only finalize after
	// its children complete, but this child needs those outputs to construct.
	if _, err := dsqlgrant.NewDSQLConnect(ctx,
		fmt.Sprintf("%s-%s-dsql-connect", d.name, target.Name()),
		grantArgs,
	); err != nil {
		return fmt.Errorf("granting dsql:DbConnect to %s: %w", target.Name(), err)
	}

	// Wire egress rules from target SG to each endpoint SG on port 5432.
	// Only when interface endpoints were created during component construction.
	if d.hasVpcEndpoints {
		for region, sgIdOutput := range d.vpcEndpointSgOutputs {
			if err := vpcsg.AddEgressRule(
				ctx,
				fmt.Sprintf("%s-%s-vpce-egress-%s", d.name, target.Name(), region),
				target.SecurityGroupId().ToStringOutput(),
				sgIdOutput,
				5432, 5432,
				"tcp",
				d,
			); err != nil {
				return fmt.Errorf("wiring egress to DSQL endpoint SG in %s: %w", region, err)
			}
		}
	}

	return nil
}

// ── Table definition serialization ───────────────────────────────────────────
//
// A table item stores the full table definition as a single JSON string in the
// "definition" attribute. The bootstrap Lambda unmarshals it into an identical
// struct set (cmd/anvil/dsql-lambda) and generates DDL. Keep the json tags here
// in sync with the Lambda's structs.

type columnDefPayload struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Length    int    `json:"length,omitempty"`
	Precision int    `json:"precision,omitempty"`
	Scale     int    `json:"scale,omitempty"`
	NotNull   bool   `json:"notNull,omitempty"`
	Unique    bool   `json:"unique,omitempty"`
	Default   string `json:"default,omitempty"`
}

type indexDefPayload struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
}

type tableDefPayload struct {
	Schema     string             `json:"schema"`
	Name       string             `json:"name"`
	PrimaryKey []string           `json:"primaryKey"`
	Columns    []columnDefPayload `json:"columns"`
	Indexes    []indexDefPayload  `json:"indexes,omitempty"`
}

// marshalTableItem builds the DynamoDB item JSON for a table definition.
// PK reuses the "roleName" attribute (generic id), sk="DEFINITION", kind="table".
// The full definition is a JSON string in "definition" — the Lambda parses it.
func marshalTableItem(schema string, t DSQLTableArgs) (string, error) {
	cols := make([]columnDefPayload, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = columnDefPayload{
			Name: c.Name, Type: c.Type, Length: c.Length,
			Precision: c.Precision, Scale: c.Scale,
			NotNull: c.NotNull, Unique: c.Unique, Default: c.Default,
		}
	}
	idxs := make([]indexDefPayload, len(t.Indexes))
	for i, x := range t.Indexes {
		idxs[i] = indexDefPayload{Name: x.Name, Columns: x.Columns, Unique: x.Unique}
	}

	def := tableDefPayload{
		Schema:     schema,
		Name:       t.Name,
		PrimaryKey: t.PrimaryKey,
		Columns:    cols,
		Indexes:    idxs,
	}
	defBytes, err := json.Marshal(def)
	if err != nil {
		return "", err
	}

	item := map[string]map[string]string{
		"roleName":   {"S": fmt.Sprintf("table#%s.%s", schema, t.Name)},
		"sk":         {"S": "DEFINITION"},
		"kind":       {"S": "table"},
		"definition": {"S": string(defBytes)},
	}
	itemBytes, err := json.Marshal(item)
	if err != nil {
		return "", err
	}
	return string(itemBytes), nil
}

// ── Schema/table validation ──────────────────────────────────────────────────

// dsqlSupportedTypes is the set of DSQL-supported column types.
// Arrays, enums, serial, money, xml, etc. are deliberately excluded.
var dsqlSupportedTypes = map[string]bool{
	"uuid": true, "text": true, "varchar": true, "char": true,
	"boolean": true, "smallint": true, "integer": true, "bigint": true,
	"real": true, "double precision": true, "numeric": true,
	"date": true, "time": true, "timestamp": true, "timestamptz": true,
	"interval": true, "bytea": true, "json": true, "jsonb": true,
}

// validateSchemas checks the schemas/roles/tables config at deploy time and
// applies normalization (auto-NOT-NULL on primary-key columns). Returns a clear
// error so misconfiguration fails at `anvil deploy`, not silently in the Lambda.
// Mutates args.Schemas in place to apply auto-NOT-NULL.
func validateSchemas(schemas []DSQLSchemaArgs) error {
	distinct := map[string]bool{}
	roleSchema := map[string]string{} // role name → schema, to enforce global uniqueness

	for si := range schemas {
		s := &schemas[si]
		if s.Name == "" {
			return fmt.Errorf("schema name must not be empty")
		}
		if s.Name == "public" {
			return fmt.Errorf("schema %q: cannot manage the public schema (owned by admin)", s.Name)
		}
		distinct[s.Name] = true

		// Roles — names must be globally unique (DSQL roles are database-wide).
		for ri := range s.Roles {
			r := &s.Roles[ri]
			if r.Name == "" {
				return fmt.Errorf("schema %q: role name must not be empty", s.Name)
			}
			if len(r.Grants) == 0 {
				return fmt.Errorf("schema %q role %q: grants must not be empty", s.Name, r.Name)
			}
			if existing, dup := roleSchema[r.Name]; dup {
				return fmt.Errorf("role %q is declared in both schema %q and %q — role names must be globally unique (DSQL roles are database-wide)", r.Name, existing, s.Name)
			}
			roleSchema[r.Name] = s.Name
		}

		tableNames := map[string]bool{}
		for ti := range s.Tables {
			t := &s.Tables[ti]
			if t.Name == "" {
				return fmt.Errorf("schema %q: table name must not be empty", s.Name)
			}
			tableNames[t.Name] = true
			if len(t.Columns) == 0 {
				return fmt.Errorf("table %s.%s: must have at least one column", s.Name, t.Name)
			}
			if len(t.PrimaryKey) == 0 {
				return fmt.Errorf("table %s.%s: primaryKey is required (DSQL needs it as the distribution key and it cannot be added later)", s.Name, t.Name)
			}

			colByName := map[string]*DSQLColumnArgs{}
			for ci := range t.Columns {
				c := &t.Columns[ci]
				if c.Name == "" {
					return fmt.Errorf("table %s.%s: column name must not be empty", s.Name, t.Name)
				}
				if !dsqlSupportedTypes[c.Type] {
					return fmt.Errorf("table %s.%s column %q: type %q is not supported by DSQL (no arrays/enums/serial; use uuid for ids)", s.Name, t.Name, c.Name, c.Type)
				}
				switch c.Type {
				case "varchar":
					if c.Length > 65535 {
						return fmt.Errorf("table %s.%s column %q: varchar length %d exceeds DSQL max 65535", s.Name, t.Name, c.Name, c.Length)
					}
				case "char":
					if c.Length > 4096 {
						return fmt.Errorf("table %s.%s column %q: char length %d exceeds DSQL max 4096", s.Name, t.Name, c.Name, c.Length)
					}
				case "numeric":
					if c.Precision > 38 {
						return fmt.Errorf("table %s.%s column %q: numeric precision %d exceeds DSQL max 38", s.Name, t.Name, c.Name, c.Precision)
					}
					if c.Scale > 37 {
						return fmt.Errorf("table %s.%s column %q: numeric scale %d exceeds DSQL max 37", s.Name, t.Name, c.Name, c.Scale)
					}
				}
				colByName[c.Name] = c
			}

			// Primary key columns must exist; auto-apply NOT NULL.
			for _, pk := range t.PrimaryKey {
				col, ok := colByName[pk]
				if !ok {
					return fmt.Errorf("table %s.%s: primaryKey column %q is not defined in columns", s.Name, t.Name, pk)
				}
				col.NotNull = true // auto-NOT-NULL: PK columns are NOT NULL by definition
			}

			// Index columns must exist; index needs a name.
			for _, idx := range t.Indexes {
				if idx.Name == "" {
					return fmt.Errorf("table %s.%s: every index requires a name", s.Name, t.Name)
				}
				if len(idx.Columns) == 0 {
					return fmt.Errorf("table %s.%s index %q: must reference at least one column", s.Name, t.Name, idx.Name)
				}
				for _, ic := range idx.Columns {
					if _, ok := colByName[ic]; !ok {
						return fmt.Errorf("table %s.%s index %q: column %q is not defined", s.Name, t.Name, idx.Name, ic)
					}
				}
			}
		}

		// Role tableScoping must reference tables declared in this schema
		// (they must exist before the scoped GRANT runs).
		for ri := range s.Roles {
			r := &s.Roles[ri]
			for _, scoped := range r.TableScoping {
				if !tableNames[scoped] {
					return fmt.Errorf("schema %q role %q: tableScoping references table %q which is not declared in this schema's tables", s.Name, r.Name, scoped)
				}
			}
		}
	}

	if len(distinct) > 10 {
		return fmt.Errorf("DSQL allows at most 10 schemas per database; %d declared", len(distinct))
	}
	return nil
}

// ── Build manifest ────────────────────────────────────────────────────────

type schemaManifestEntry struct {
	Component string           `json:"component"`
	Type      string           `json:"type"`
	Schemas   []DSQLSchemaArgs `json:"schemas"`
}

type schemaManifest struct {
	Functions []json.RawMessage     `json:"functions,omitempty"`
	Schemas   []schemaManifestEntry `json:"schemas,omitempty"`
	// Compliance is preserved verbatim so a DSQL write doesn't drop it.
	Compliance json.RawMessage `json:"compliance,omitempty"`
}

func appendSchemasToManifest(name string, schemas []DSQLSchemaArgs) error {
	const manifestPath = ".anvil/build-manifest.json"

	provider.ManifestMu.Lock()
	defer provider.ManifestMu.Unlock()

	if err := os.MkdirAll(".anvil", 0755); err != nil {
		return err
	}

	m := schemaManifest{}
	if data, err := os.ReadFile(manifestPath); err == nil {
		_ = json.Unmarshal(data, &m)
	}

	m.Schemas = append(m.Schemas, schemaManifestEntry{
		Component: name,
		Type:      "anvil:aws:DSQL",
		Schemas:   schemas,
	})

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(manifestPath, data, 0644)
}

// ── Doc notes ──────────────────────────────────────────────────────────────
//
// ROLE BOOTSTRAP ARCHITECTURE (docs):
//   Roles are managed via a DynamoDB table + stream + Lambda pattern.
//   The bootstrap Lambda is the ONLY entity with dsql:DbConnectAdmin — the
//   CI/CD pipeline and deployer never hold admin credentials.
//
//   Deploy flow:
//     1. Pulumi writes role DEFINITION items to DynamoDB (from roles[] array)
//     2. GrantConnect creates DSQLConnect components that write IAM mapping
//        items to the same DynamoDB table
//     3. DynamoDB stream fires for each INSERT/MODIFY/REMOVE
//     4. Bootstrap Lambda processes events and runs SQL against DSQL
//
//   The Lambda reads DSQL_ENDPOINT and DSQL_REGION from its own env vars —
//   these are NOT stored in the DynamoDB items. Each DSQL component has its
//   own table and Lambda, so multi-cluster deployments are fully isolated.
//
//   Source: cmd/anvil/dsql-lambda/main.go
//   Build:  go run build.go build-dsql-lambda
//   Deploy: anvil deploy (auto-builds and sets Pulumi config)
//
// MULTI-CLUSTER ISOLATION (docs):
//   Each DSQL component creates its own DynamoDB table and bootstrap Lambda.
//   The endpoint is an env var on the Lambda, not stored in items. This means:
//   - Two clusters = two tables, two Lambdas, zero shared state
//   - Renaming or replacing a cluster only updates one Lambda's env var
//   - No item migration or cross-table coordination needed
//
// ADMIN CREDENTIALS — BLAST RADIUS (docs + security):
//   The bootstrap Lambda has dsql:DbConnectAdmin scoped to the component's
//   cluster ARNs. It cannot be invoked directly — no lambda:InvokeFunction
//   is granted. The only trigger is the DynamoDB stream. If the CI/CD
//   pipeline is compromised, the attacker can write items to DynamoDB
//   (role definitions or IAM mappings) but cannot execute arbitrary SQL.
//
// GRANTCONNECT — DSQLConnect COMPONENT (docs):
//   dsql.grantConnect(target, "app_role") creates an anvil:aws:DSQLConnect
//   component (provider/aws/dsqlgrant/) that handles:
//     1. IAM policy — dsql:DbConnect on all cluster ARNs
//     2. DynamoDB IAM mapping item (sk = IAM ARN) — triggers bootstrap Lambda
//        to run: AWS IAM GRANT "app_role" TO 'arn:...'
//   Removing the target cleans up both: Pulumi deletes the policy and the
//   DynamoDB item, the stream fires REMOVE, bootstrap Lambda runs AWS IAM REVOKE.
//   DSQLConnect works with any GrantTarget (Lambda, ECS, future compute types).
//   It can also be used standalone: new anvil.aws.DSQLConnect(...) for advanced cases.
//
// GRANTCONNECT WITHOUT ROLES (docs):
//   When roles is omitted (no roles table), GrantConnect only creates the
//   IAM policy (dsql:DbConnect). The DynamoDB mapping item is skipped because
//   rolesTableName is empty. The database-level mapping (AWS IAM GRANT)
//   must be handled separately — intended for teams using migration tools.
//
// LAMBDA CONNECTION FLOW (docs):
//   The user's Lambda connects to DSQL as a custom role (not admin):
//     1. Generate token: signer.getDbConnectAuthToken() (NOT admin)
//     2. Connect: user="app_role", password=token, port=5432, ssl=true
//   This requires:
//     - dsql:DbConnect IAM permission (from DSQLConnect)
//     - AWS IAM GRANT mapping in the database (from bootstrap Lambda)
//     - The database role must exist with GRANT privileges (from DEFINITION item)
//
// SINGLE-REGION VS MULTI-REGION (docs):
//   Single-region is the default — zero config required for a working cluster.
//   Multi-region is opt-in via the multiRegion block. Tradeoffs:
//   ~$43-87/month in PrivateLink costs (if VPC is used), cross-region data
//   transfer charges, and OCC retry handling at the application layer.
//   For multi-region, the bootstrap Lambda connects to the first region's
//   cluster — DSQL replicates the system catalog across regions.
//
// OCC RETRY BEHAVIOR (docs):
//   DSQL uses Optimistic Concurrency Control. Under contention, transactions
//   abort at commit time rather than blocking. Applications must implement
//   retry logic — fundamentally different from standard Postgres locking.
//
// BOOTSTRAP IDEMPOTENCY (docs):
//   Pulumi state diffing prevents re-runs when inputs are unchanged.
//   CREATE SCHEMA IF NOT EXISTS and CREATE ROLE (without IF NOT EXISTS)
//   are safety nets. ALTER DEFAULT PRIVILEGES is critical — without it,
//   grants only apply to tables that exist at bootstrap time. New tables
//   added later are automatically accessible because of this statement.
//
// DSQL LIMITS (docs):
//   Max 10 schemas per database. Max 1 database per cluster. No published
//   limit on number of database roles or IAM-to-role mappings. Each role
//   in the roles[] array requires a schema, so you're effectively capped
//   at 10 roles unless multiple roles share a schema.
//
// VPC ENDPOINT SERVICE NAME (implementation note):
//   Unlike SSM, SQS, and other standard services where the service name is
//   fixed (com.amazonaws.{region}.sqs), DSQL's service name is cluster-specific:
//   com.amazonaws.{region}.dsql-{hex}. It is exposed as cluster.VpcEndpointServiceName.
//   This is why DSQL cannot use the existing AwsVpcEndpointService enum or
//   the existing VpcEndpoint component.
//
// BACKUP CROSS-REGION COPY (docs):
//   AWS Backup does not automatically replicate across regions. Without a
//   cross-region copy rule, multi-region restore fails because the peer region
//   has no recovery point. Anvil wires copy rules automatically when multiRegion
//   is set. Single-region clusters only get a local recovery point.
//
// PRIVATELINK PORT (docs):
//   DSQL VPC endpoints use port 5432 (Postgres wire protocol), not 443 like
//   most AWS service endpoints (SSM, SQS, Secrets Manager). The self-referencing
//   ingress rule and Lambda egress rules are wired on 5432. Do not change to 443.
//
// GRANTCONNECT AND VPC SG ATTACHMENT (docs):
//   GrantConnect wires the egress rule from the target SG to the DSQL endpoint
//   SG. However, it cannot attach the endpoint SG to the target's vpcConfig —
//   that is set at construction time and cannot be modified post-deploy
//   without replacing the resource. The user must declare the DSQL endpoint SGs
//   in vpc.vpcEndpoints on the target when constructing it.
//
// DYNAMODB TABLE NAMING (frontend reference):
//   Table name: {stage}-{componentName}-dsql-roles-{stageId}
//   Example:    dev-orders-dsql-roles-a1b2c3
//   Query patterns:
//     - List all role definitions: Scan with FilterExpression sk = "DEFINITION"
//     - Get one role:   GetItem(roleName="app_role", sk="DEFINITION")
//     - List IAM mappings for a role: Query(roleName="app_role", sk begins_with "arn:")
//     - List all mappings: Scan with FilterExpression sk begins_with "arn:"

// ════════════════════════════════════════════════════════════════════════════
// DSQL — FULL FEATURE REFERENCE (for SDK docs / frontend dashboard authors)
// ════════════════════════════════════════════════════════════════════════════
//
// This is the canonical reference for everything the DSQL component implements:
// the cluster, declarative schema/role/table management, and compute access via
// grantConnect. Use it as the source of truth when writing user-facing docs.
//
// ── 1. WHAT IT IS ───────────────────────────────────────────────────────────
//
// An Anvil-managed Aurora DSQL cluster (serverless, distributed, Postgres-
// compatible) with:
//   - single- or multi-region active-active clusters
//   - declarative, additive-only schema + table management
//   - declarative database roles with schema-wide or per-table grants
//   - compute-to-database access wiring (grantConnect) with zero admin
//     credentials in the CI/CD pipeline
//   - optional AWS Backup and VPC PrivateLink
//
// ── 2. THE API (single source of truth) ─────────────────────────────────────
//
//   new anvil.aws.DSQL("orders", {
//     // optional: multiRegion, vpc, backup, transform
//     schemas: [
//       {
//         name: "myapp",                              // schema (namespace)
//         roles: [
//           { name: "app_role", grants: ["SELECT","INSERT","UPDATE"] },        // schema-wide
//           { name: "reader",   grants: ["SELECT"], tableScoping: ["users"] }, // table-scoped
//         ],
//         tables: [
//           {
//             name: "users",
//             columns: [
//               { name: "id",         type: "uuid", notNull: true },
//               { name: "email",      type: "varchar", length: 255, notNull: true, unique: true },
//               { name: "created_at", type: "timestamptz", default: "now()" },
//             ],
//             primaryKey: ["id"],                       // required; composite via order
//             indexes: [{ name: "users_created_idx", columns: ["created_at"] }],
//           },
//         ],
//       },
//     ],
//   });
//
//   // Access — map a compute resource to one or more roles. The compute may
//   // connect as any of them (picks via the connection's `user` field).
//   dsql.grantConnect(lambda, {
//     schemas: [
//       { schema: "myapp", roleName: "app_role" },
//       { schema: "myapp", roleName: "reader" },
//     ],
//   });
//
// STRUCTURE: schemas[] is the container. Each schema holds roles[] (scoped to
// that schema) and tables[]. Roles are NOT repeated per schema — a role belongs
// to exactly one schema, and role NAMES must be globally unique (DSQL roles are
// database-wide). The schema in grantConnect pairs is for readability; the IAM
// mapping itself only needs the role name.
//
// ── 3. SUPPORTED COLUMN TYPES (constrained enum: DSQLColumnType) ─────────────
//
//   uuid, text, varchar, char, boolean,
//   smallint, integer, bigint, real, double precision, numeric,
//   date, time, timestamp, timestamptz, interval,
//   bytea, json, jsonb
//
//   Parameterized via structured fields (NOT inline):
//     varchar/char → length    (char ≤ 4096, varchar ≤ 65535)
//     numeric      → precision (≤ 38), scale (≤ 37)
//
//   Deliberately UNSUPPORTED (DSQL limitation — rejected at deploy time):
//     arrays (text[]), enums, serial (use uuid + app-generated id), money, xml.
//
//   Column options: notNull, unique (→ named async unique index), default
//   (raw passthrough SQL expression — string literals need inner quotes:
//   "'active'"; functions: "now()").
//
// ── 4. PRIMARY KEYS ─────────────────────────────────────────────────────────
//
//   - Required on every table (DSQL uses the PK as the distribution key).
//   - Cannot be added after creation — get it right up front.
//   - Composite: primaryKey: ["tenant_id", "id"] (order matters).
//   - PK columns are auto-NOT-NULL.
//   - AWS recommends a uuid PK generated app-side (crypto.randomUUID()) to
//     avoid write hotspots — sequential/identity PKs are an anti-pattern on
//     a distributed DB. Anvil does not provide an identity/serial shorthand.
//
// ── 5. GRANTS ───────────────────────────────────────────────────────────────
//
//   Schema-wide (no tableScoping): role gets the grants on ALL current AND
//   future tables in the schema (GRANT ON ALL TABLES + ALTER DEFAULT PRIVILEGES).
//
//   Table-scoped (tableScoping: [...]): role gets the grants ONLY on the named
//   tables (which must be declared in the same schema). Does NOT auto-cover
//   future tables — add them to tableScoping explicitly. Tables are created
//   before the scoped grant runs (Pulumi DependsOn ordering).
//
// ── 6. COMPUTE ACCESS — grantConnect ────────────────────────────────────────
//
//   grantConnect(target, { schemas: [{ schema, roleName }, ...] }) does two
//   things for the target compute (Lambda, ECS, anything implementing the
//   GrantTarget interface):
//     1. IAM-level: attaches a dsql:DbConnect policy scoped to the cluster ARNs
//        so the compute can mint a connection token.
//     2. Database-level: writes one DynamoDB mapping item per role; the
//        bootstrap Lambda runs AWS IAM GRANT "<role>" TO '<compute-iam-arn>'.
//   Both are required — miss either and the runtime gets `access denied`.
//
//   Multiple roles: a compute can be mapped to several roles and connects as
//   whichever one it chooses per connection (one role per connection).
//
//   Removing a grant (or the compute) deletes the policy + mapping item; the
//   stream fires REMOVE → bootstrap Lambda runs AWS IAM REVOKE.
//
// ── 7. RUNTIME — HOW A COMPUTE CONNECTS ─────────────────────────────────────
//
//   The compute connects as a CUSTOM role (not admin):
//     1. token = getDbConnectAuthToken()      // NON-admin token (TS/JS)
//                                              // Go: auth.GenerateDbConnectAuthToken
//     2. connect: user=<roleName>, password=token, port=5432, sslmode=verify-full,
//                 database=postgres
//   The cluster endpoint is dsql.endpoint (single, provider-region) or
//   dsql.endpoints[region] (multi-region map). Wire it to the compute as an
//   env var; Anvil does NOT inject it automatically.
//
// ── 8. COMPONENT OUTPUTS ────────────────────────────────────────────────────
//
//   endpoint        string                 - provider-region cluster endpoint (convenience)
//   endpoints       map[region]string      - all cluster endpoints
//   clusterArns     map[region]string      - cluster ARNs (one per region)
//   rolesTableName  string                 - DynamoDB bootstrap table name (empty if no schemas)
//   vpcEndpointIds, vpcEndpointSecurityGroupIds - only when vpc set & hasNat=false
//
// ── 9. ARCHITECTURE / SECURITY MODEL ────────────────────────────────────────
//
//   Pulumi (deploy) ──► DynamoDB item ──► DynamoDB Stream ──► Bootstrap Lambda ──► DSQL
//                       (definitions)                          (dsql:DbConnectAdmin)
//
//   - The bootstrap Lambda is the ONLY holder of dsql:DbConnectAdmin and can
//     ONLY be triggered by the stream (no lambda:InvokeFunction granted).
//   - The pipeline only writes definitions to DynamoDB — it never holds admin
//     DB credentials. Pipeline compromise can at most alter definitions, never
//     run arbitrary SQL.
//   - Each DSQL component has its own DynamoDB table + bootstrap Lambda. The
//     cluster endpoint/region are Lambda env vars, not per-item — multi-cluster
//     deployments are fully isolated.
//
// ── 10. DYNAMODB ITEM SCHEMA (frontend reads this for live state) ────────────
//
//   Table name: {stage}-{componentName}-dsql-roles-{stageId}
//   Composite key: PK "roleName", SK "sk".
//
//   Role definition:  roleName=<role>, sk="DEFINITION", schema=<s>,
//                     grants=[...], tableScoping=[...] (optional)
//   IAM mapping:      roleName=<role>, sk=<iam-arn>
//   Table definition: roleName="table#<schema>.<name>", sk="DEFINITION",
//                     kind="table", definition=<JSON: columns, primaryKey, indexes>
//
//   Frontend query patterns:
//     - List roles:    Scan, FilterExpression sk = "DEFINITION" AND attribute_not_exists(kind)
//     - List tables:   Scan, FilterExpression kind = "table"
//     - Role mappings: Query roleName=<role>, sk begins_with "arn:"
//
// ── 11. ADDITIVE-ONLY SEMANTICS (tables & columns) ──────────────────────────
//
//   Anvil only ever moves the schema FORWARD. Destructive changes are the
//   operator's responsibility — Anvil never runs them.
//     - New table      → CREATE TABLE
//     - New column     → ALTER TABLE ADD COLUMN
//     - New index      → CREATE INDEX ASYNC
//     - Removed column → no-op (column stays in DSQL)
//     - Removed table  → no-op (table stays in DSQL)
//     - Type change    → no-op (not additive)
//   Roles ARE fully reconciled (revoke-all-then-re-grant on change; dropped on
//   removal) — that's safe because roles carry no data.
//
// ── 12. DSQL CONSTRAINTS THAT SHAPE THE DESIGN ──────────────────────────────
//
//   - Max 10 schemas per database; max 1 database per cluster.
//   - One DDL statement per transaction; DDL and DML cannot mix.
//   - CREATE INDEX must be ASYNC (background build; returns job_id; monitor
//     via sys.jobs). Anvil fires-and-logs, does not block.
//   - No CREATE ROLE IF NOT EXISTS — bootstrap checks pg_roles then conditionally
//     creates (idempotent).
//   - No DROP OWNED BY — role removal revokes grants then DROP ROLE (best-effort).
//   - AWS IAM GRANT/REVOKE is a DSQL extension — works over pgx/psql but many
//     GUI SQL clients reject it client-side (use psql / CloudShell / DSQL console).
//   - OCC: transactions abort at commit under contention; apps must retry.
//
// ── 13. SOURCE MAP (where each piece lives) ─────────────────────────────────
//
//   provider/aws/dsql/dsql.go        - DSQL component, args, validation, bootstrap wiring
//   provider/aws/dsqlgrant/          - DSQLConnect component (grantConnect under the hood)
//   provider/aws/dsql/bootstrap/     - embedded bootstrap Lambda zip (//go:embed)
//   cmd/anvil/dsql-lambda/main.go    - bootstrap Lambda source (stream handler + SQL)
//   scripts/grants/configs/dsql-config.ts - SDK grantConnect method generation
//   build.go: build-dsql-lambda      - cross-compiles + embeds the bootstrap Lambda
