package kinesis

import (
	"fmt"

	"github.com/anvil-cloud/anvil/provider/internal/hatch"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	awskinesis "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/kinesis"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

const (
	modeOnDemand    = "onDemand"
	modeProvisioned = "provisioned"

	defaultRetentionHours   = 24
	minRetentionHours       = 24
	maxRetentionHours       = 8760 // 365 days
	defaultMaxRecordSizeKib = 2048
	minMaxRecordSizeKib     = 1024
	maxMaxRecordSizeKib     = 10240
	defaultShardCount       = 1

	// awsManagedKey is the AWS-owned KMS key for Kinesis. Free, no key policy
	// to manage, and producers/consumers in the account need no kms:* grants.
	awsManagedKey = "alias/aws/kinesis"
)

// KinesisProvisionedArgs switches the stream to provisioned capacity.
type KinesisProvisionedArgs struct {
	// ShardCount is the number of shards. Each shard takes 1 MB/s or 1,000
	// records/s in and serves 2 MB/s out. Default: 1.
	ShardCount int `pulumi:"shardCount,optional"`
}

// KinesisStreamArgs defines the inputs for an Anvil-managed Kinesis Data Stream.
type KinesisStreamArgs struct {
	// Provisioned switches the stream from on-demand to provisioned capacity
	// with a fixed shard count. When omitted, the stream is on-demand and
	// scales shards automatically with traffic — the right choice unless
	// throughput is steady and predictable.
	Provisioned *KinesisProvisionedArgs `pulumi:"provisioned,optional"`

	// RetentionHours is how long records stay readable, 24–8760 (365 days).
	// Default: 24. Retention beyond 24h is billed per shard-hour.
	RetentionHours int `pulumi:"retentionHours,optional"`

	// MaxRecordSizeKib is the largest record the stream accepts, in KiB,
	// 1024–10240. Default: 2048, so every stream can take Aurora DSQL CDC
	// events (rows up to 2 MiB) without extra config.
	MaxRecordSizeKib int `pulumi:"maxRecordSizeKib,optional"`

	// Hatch is the escape hatch for low-level overrides of the underlying
	// aws.kinesis.Stream. Keys: "kinesis". Use for a customer-managed KMS key
	// (encryptionType + kmsKeyId) or shard-level metrics.
	Hatch map[string]map[string]interface{} `pulumi:"hatch,optional"`
}

// KinesisStream is the Anvil-managed Kinesis Data Stream component resource.
type KinesisStream struct {
	pulumi.ResourceState

	// Arn is the ARN of the Kinesis stream.
	Arn pulumi.StringOutput `pulumi:"arn"`

	// StreamName is the physical name of the Kinesis stream.
	StreamName pulumi.StringOutput `pulumi:"name"`

	name string
}

func (k *KinesisStream) Name() string {
	return k.name
}

func (k *KinesisStream) Annotate(a infer.Annotator) {
	a.SetToken("aws", "KinesisStream")
	a.Describe(&k, "An Anvil-managed Kinesis Data Stream. On-demand capacity by default (set provisioned for a fixed shard count), and server-side encryption with the AWS-managed KMS key is always on.")
}

// streamConfig is the validated, defaulted form of KinesisStreamArgs.
type streamConfig struct {
	mode             string
	shardCount       int
	retentionHours   int
	maxRecordSizeKib int
}

// resolveConfig applies defaults and validates the args.
func resolveConfig(name string, args KinesisStreamArgs) (streamConfig, error) {
	cfg := streamConfig{
		mode:             modeOnDemand,
		retentionHours:   args.RetentionHours,
		maxRecordSizeKib: args.MaxRecordSizeKib,
	}

	if args.Provisioned != nil {
		cfg.mode = modeProvisioned
		cfg.shardCount = args.Provisioned.ShardCount
		if cfg.shardCount < 0 {
			return cfg, fmt.Errorf("kinesis stream %q: provisioned.shardCount must be positive, got %d", name, cfg.shardCount)
		}
		if cfg.shardCount == 0 {
			cfg.shardCount = defaultShardCount
		}
	}

	if cfg.retentionHours == 0 {
		cfg.retentionHours = defaultRetentionHours
	}
	if cfg.retentionHours < minRetentionHours || cfg.retentionHours > maxRetentionHours {
		return cfg, fmt.Errorf("kinesis stream %q: retentionHours must be between %d and %d, got %d", name, minRetentionHours, maxRetentionHours, cfg.retentionHours)
	}

	if cfg.maxRecordSizeKib == 0 {
		cfg.maxRecordSizeKib = defaultMaxRecordSizeKib
	}
	if cfg.maxRecordSizeKib < minMaxRecordSizeKib || cfg.maxRecordSizeKib > maxMaxRecordSizeKib {
		return cfg, fmt.Errorf("kinesis stream %q: maxRecordSizeKib must be between %d and %d, got %d", name, minMaxRecordSizeKib, maxMaxRecordSizeKib, cfg.maxRecordSizeKib)
	}

	return cfg, nil
}

// streamProps builds the aws:kinesis/stream:Stream inputs, before hatch overrides.
func streamProps(physicalName string, cfg streamConfig) pulumi.Map {
	streamMode := "ON_DEMAND"
	if cfg.mode == modeProvisioned {
		streamMode = "PROVISIONED"
	}

	props := pulumi.Map{
		"name": pulumi.String(physicalName),
		"streamModeDetails": pulumi.Map{
			"streamMode": pulumi.String(streamMode),
		},
		"retentionPeriod":    pulumi.Int(cfg.retentionHours),
		"maxRecordSizeInKib": pulumi.Int(cfg.maxRecordSizeKib),
		"encryptionType":     pulumi.String("KMS"),
		"kmsKeyId":           pulumi.String(awsManagedKey),
		"tags": pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}
	if cfg.mode == modeProvisioned {
		props["shardCount"] = pulumi.Int(cfg.shardCount)
	}
	return props
}

func NewKinesisStream(ctx *pulumi.Context, name string, args KinesisStreamArgs, opts ...pulumi.ResourceOption) (*KinesisStream, error) {
	k := &KinesisStream{name: name}

	streamCfg, err := resolveConfig(name, args)
	if err != nil {
		return nil, err
	}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, k, opts...); err != nil {
		return nil, err
	}

	// ── 1. Stream ──────────────────────────────────────
	props := streamProps(provider.PhysicalName(stage, name, "stream", stageId), streamCfg)
	streamPropsMerged := hatch.Merge(args.Hatch["kinesis"], props)
	streamRes := &awskinesis.Stream{}
	if err := ctx.RegisterResource("aws:kinesis/stream:Stream", name, streamPropsMerged, streamRes, pulumi.Parent(k)); err != nil {
		return nil, fmt.Errorf("failed to create kinesis stream %q: %w", name, err)
	}

	// ── 2. Wire outputs ────────────────────────────────
	k.Arn = streamRes.Arn
	k.StreamName = streamRes.Name

	ctx.RegisterResourceOutputs(k, pulumi.Map{
		"arn":  streamRes.Arn,
		"name": streamRes.Name,
	})

	return k, nil
}
