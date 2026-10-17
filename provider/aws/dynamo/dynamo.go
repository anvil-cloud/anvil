package dynamo

import (
	"encoding/json"
	"fmt"

	"github.com/anvil-cloud/anvil/provider/internal/hatch"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	awsdynamodb "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/dynamodb"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	awslambda "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lambda"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/pipes"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// ── Nested arg types ───────────────────────────────────────────────────────

// KeyAttribute is an explicit { name, type } pair for a DynamoDB key.
// All keys — table and GSI — must be declared this way.
// Anvil derives attributeDefinitions automatically by collecting all declared
// keys. Users never declare attributeDefinitions directly — this is a known
// Pulumi/Terraform DynamoDB papercut that Anvil hides completely.
type KeyAttribute struct {
	Name string `pulumi:"name"`
	Type string `pulumi:"type"` // S | N | B
}

// GlobalSecondaryIndex defines a GSI on the table.
// All key attributes must use the explicit { name, type } shape — Anvil merges
// them into the table attributeDefinitions automatically. projectionType
// defaults to ALL so the simple case needs no config.
type GlobalSecondaryIndex struct {
	Name             string        `pulumi:"name"`
	HashKey          KeyAttribute  `pulumi:"hashKey"`
	RangeKey         *KeyAttribute `pulumi:"rangeKey,optional"`
	ProjectionType   string        `pulumi:"projectionType,optional"` // ALL | KEYS_ONLY | INCLUDE — default ALL
	NonKeyAttributes []string      `pulumi:"nonKeyAttributes,optional"`
}

// StreamLambdaConsumer wires a Lambda as the stream consumer via AWS-managed ESM.
// Anvil creates the ESM, lambda:InvokeFunction permission, and attaches stream
// read permissions to the Lambda role. Note: stream consumer wiring does NOT
// grant table read/write access — call table.GrantRead/Write separately if needed.
type StreamLambdaConsumer struct {
	Arn           pulumi.StringInput `pulumi:"arn"`
	LambdaRoleArn pulumi.StringInput `pulumi:"lambdaRoleArn"`
}

// StreamEventBridgeConsumer wires an EventBridge bus as the stream consumer
// via an EventBridge Pipe. Anvil creates the Pipe and a scoped IAM role for it.
// Use this for fanout — the bus routes to multiple targets via rules, bypassing
// the 2-consumer-per-shard limit of direct Lambda ESM.
//
// TICKET: v2 — add optional DLQ for failed Pipe deliveries. DynamoDB stream
// records are only retained for 24 hours, so dropped events are unrecoverable.
// Consider whether a Pipe DLQ should be Tier 1 for production safety.
type StreamEventBridgeConsumer struct {
	Name   pulumi.StringInput `pulumi:"name"`
	BusArn pulumi.StringInput `pulumi:"busArn"`
}

// StreamConsumer is a discriminated union — exactly one of Lambda or EventBridge.
type StreamConsumer struct {
	Lambda      *StreamLambdaConsumer      `pulumi:"lambda,optional"`
	EventBridge *StreamEventBridgeConsumer `pulumi:"eventBridge,optional"`
}

// Stream configures DynamoDB Streams on the table. Opt-in.
type Stream struct {
	ViewType         string         `pulumi:"viewType"`                  // NEW_IMAGE | OLD_IMAGE | NEW_AND_OLD_IMAGES | KEYS_ONLY
	StartingPosition string         `pulumi:"startingPosition,optional"` // TRIM_HORIZON | LATEST — default TRIM_HORIZON (AWS default)
	BatchSize        int            `pulumi:"batchSize,optional"`        // default 100
	Consumer         StreamConsumer `pulumi:"consumer"`
}

// ── Args ───────────────────────────────────────────────────────────────────

type DynamoDBArgs struct {
	HashKey                KeyAttribute           `pulumi:"hashKey"`
	RangeKey               *KeyAttribute          `pulumi:"rangeKey,optional"`
	GlobalSecondaryIndexes []GlobalSecondaryIndex `pulumi:"globalSecondaryIndexes,optional"`
	TtlAttribute           string                 `pulumi:"ttlAttribute,optional"`
	Stream                 *Stream                `pulumi:"stream,optional"`

	// KmsKeyArn is Tier 2 opt-in. ARN of a KMS CMK for encryption at rest.
	// If omitted, AWS_OWNED_KMS is used (Tier 1 default — always on, zero cost).
	// Use for compliance workloads needing key rotation control, CloudTrail
	// audit trail per decrypt, or the ability to revoke access by disabling the key.
	// CMK costs ~$1/month + API call charges.
	KmsKeyArn string `pulumi:"kmsKeyArn,optional"`

	// Hatch is the escape hatch for low-level overrides of the underlying DynamoDB table resource.
	// Keys: "dynamo". Use sparingly — Anvil's defaults should cover most cases.
	Hatch map[string]map[string]interface{} `pulumi:"hatch,optional"`
}

// ── Component ──────────────────────────────────────────────────────────────

type DynamoDB struct {
	pulumi.ResourceState

	TableName pulumi.StringOutput `pulumi:"tableName"`
	TableArn  pulumi.StringOutput `pulumi:"tableArn"`
	StreamArn pulumi.StringOutput `pulumi:"streamArn"` // empty when stream not enabled

	name  string
	table *awsdynamodb.Table
}

func (d *DynamoDB) Name() string {
	return d.name
}

func (d *DynamoDB) Annotate(a infer.Annotator) {
	a.SetToken("aws", "DynamoDB")
	a.Describe(&d, "An Anvil-managed DynamoDB table. Serverless key-value and document store. Secure-by-default: PAY_PER_REQUEST billing, deletion protection, point-in-time recovery, and AWS_OWNED_KMS encryption are always on. Pairs naturally with anvil.aws.Lambda. No VPC required.")
}

func NewDynamoDB(ctx *pulumi.Context, name string, args DynamoDBArgs, opts ...pulumi.ResourceOption) (*DynamoDB, error) {
	d := &DynamoDB{name: name}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, d, opts...); err != nil {
		return nil, err
	}

	physicalName := provider.PhysicalName(stage, name, "table", stageId)

	// ── 1. Attribute definitions ───────────────────────────────────────────
	// Anvil derives attributeDefinitions by collecting all { name, type } pairs
	// from the table keys and all GSI keys, then deduplicating. This hides a
	// well-known Pulumi/Terraform DynamoDB papercut where users must manually
	// redeclare every key attribute at the top-level table definition.
	attrMap := map[string]string{}
	attrMap[args.HashKey.Name] = args.HashKey.Type
	if args.RangeKey != nil {
		attrMap[args.RangeKey.Name] = args.RangeKey.Type
	}
	for _, gsi := range args.GlobalSecondaryIndexes {
		attrMap[gsi.HashKey.Name] = gsi.HashKey.Type
		if gsi.RangeKey != nil {
			attrMap[gsi.RangeKey.Name] = gsi.RangeKey.Type
		}
	}

	var attrDefs awsdynamodb.TableAttributeArray
	for attrName, attrType := range attrMap {
		attrDefs = append(attrDefs, awsdynamodb.TableAttributeArgs{
			Name: pulumi.String(attrName),
			Type: pulumi.String(attrType),
		})
	}

	// ── 2. GSIs ────────────────────────────────────────────────────────────
	// Uses keySchema instead of deprecated hashKey/rangeKey fields on GSI args.
	var gsiDefs pulumi.Array
	for _, gsi := range args.GlobalSecondaryIndexes {
		projType := gsi.ProjectionType
		if projType == "" {
			projType = "ALL"
		}

		keySchema := pulumi.Array{
			pulumi.Map{
				"attributeName": pulumi.String(gsi.HashKey.Name),
				"keyType":       pulumi.String("HASH"),
			},
		}
		if gsi.RangeKey != nil {
			keySchema = append(keySchema, pulumi.Map{
				"attributeName": pulumi.String(gsi.RangeKey.Name),
				"keyType":       pulumi.String("RANGE"),
			})
		}

		gsiMap := pulumi.Map{
			"name":           pulumi.String(gsi.Name),
			"keySchema":      keySchema,
			"projectionType": pulumi.String(projType),
		}
		if projType == "INCLUDE" && len(gsi.NonKeyAttributes) > 0 {
			gsiMap["nonKeyAttributes"] = pulumi.ToStringArray(gsi.NonKeyAttributes)
		}
		gsiDefs = append(gsiDefs, gsiMap)
	}

	// ── 3. Stream spec ─────────────────────────────────────────────────────
	streamEnabled := args.Stream != nil
	streamViewType := ""
	if streamEnabled {
		streamViewType = args.Stream.ViewType
	}

	// ── 4. Table ───────────────────────────────────────────────────────────
	// Encryption is handled in tableMap below.
	// Tier 1: AWS_OWNED_KMS always on (enabled: true, no kmsKeyArn).
	// Tier 2: kmsKeyArn opt-in for CMK. See KmsKeyArn field comment above.
	tableMap := pulumi.Map{
		"name":                      pulumi.String(physicalName),
		"billingMode":               pulumi.String("PAY_PER_REQUEST"),
		"hashKey":                   pulumi.String(args.HashKey.Name),
		"attributes":                attrDefs,
		"deletionProtectionEnabled": pulumi.Bool(true),
		"pointInTimeRecovery": pulumi.Map{
			"enabled": pulumi.Bool(true),
		},
		"serverSideEncryption": pulumi.Map{
			"enabled": pulumi.Bool(true),
		},
		"streamEnabled": pulumi.Bool(streamEnabled),
		"tags": pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}
	if streamEnabled {
		tableMap["streamViewType"] = pulumi.String(streamViewType)
	}
	if args.RangeKey != nil {
		tableMap["rangeKey"] = pulumi.String(args.RangeKey.Name)
	}
	if args.KmsKeyArn != "" {
		tableMap["serverSideEncryption"] = pulumi.Map{
			"enabled":   pulumi.Bool(true),
			"kmsKeyArn": pulumi.String(args.KmsKeyArn),
		}
	}
	if len(gsiDefs) > 0 {
		tableMap["globalSecondaryIndexes"] = gsiDefs
	}
	if args.TtlAttribute != "" {
		tableMap["ttl"] = pulumi.Map{
			"attributeName": pulumi.String(args.TtlAttribute),
			"enabled":       pulumi.Bool(true),
		}
	}

	tablePropsMerged := hatch.Merge(args.Hatch["dynamo"], tableMap)
	tableRes := &awsdynamodb.Table{}
	if err := ctx.RegisterResource("aws:dynamodb/table:Table", name, tablePropsMerged, tableRes, pulumi.Parent(d)); err != nil {
		return nil, fmt.Errorf("creating DynamoDB table %q: %w", name, err)
	}
	d.table = tableRes

	// ── 6. Stream consumer wiring ──────────────────────────────────────────
	if streamEnabled {
		batchSize := args.Stream.BatchSize
		if batchSize == 0 {
			batchSize = 100
		}
		startingPosition := args.Stream.StartingPosition
		if startingPosition == "" {
			startingPosition = "TRIM_HORIZON"
		}

		consumer := args.Stream.Consumer
		if consumer.Lambda != nil {
			if err := wireLambdaConsumer(ctx, name, tableRes, consumer.Lambda, batchSize, startingPosition, d); err != nil {
				return nil, err
			}
		}
		if consumer.EventBridge != nil {
			if err := wireEventBridgeConsumer(ctx, name, stage, tableRes, consumer.EventBridge, d); err != nil {
				return nil, err
			}
		}
	}

	// ── 7. Outputs ─────────────────────────────────────────────────────────
	d.TableName = tableRes.Name
	d.TableArn = tableRes.Arn
	if streamEnabled {
		d.StreamArn = tableRes.StreamArn
	}

	ctx.RegisterResourceOutputs(d, pulumi.Map{
		"tableName": tableRes.Name,
		"tableArn":  tableRes.Arn,
		"streamArn": tableRes.StreamArn,
	})

	return d, nil
}

// ── Lambda consumer ────────────────────────────────────────────────────────

func wireLambdaConsumer(
	ctx *pulumi.Context,
	name string,
	table *awsdynamodb.Table,
	consumer *StreamLambdaConsumer,
	batchSize int,
	startingPosition string,
	parent pulumi.Resource,
) error {
	// 1. Resource-based policy — allows Lambda service to invoke the function
	_, err := awslambda.NewPermission(ctx, name+"-stream-invoke", &awslambda.PermissionArgs{
		Action:    pulumi.String("lambda:InvokeFunction"),
		Function:  consumer.Arn.ToStringOutput(),
		Principal: pulumi.String("lambda.amazonaws.com"),
		SourceArn: table.StreamArn,
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("creating lambda stream permission for %q: %w", name, err)
	}

	// 2. Inline policy on Lambda role — minimum stream read permissions.
	// Note: this does NOT grant table read/write access. Call grantRead/Write
	// separately if the Lambda also needs to interact with the table directly.
	policyJSON := table.StreamArn.ApplyT(func(streamArn string) (string, error) {
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

	roleName := consumer.LambdaRoleArn.ToStringOutput().ApplyT(func(arn string) string {
		for i := len(arn) - 1; i >= 0; i-- {
			if arn[i] == '/' {
				return arn[i+1:]
			}
		}
		return arn
	}).(pulumi.StringOutput)

	_, err = iam.NewRolePolicy(ctx, name+"-stream-read", &iam.RolePolicyArgs{
		Role:   roleName,
		Policy: policyJSON,
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("attaching stream read policy for %q: %w", name, err)
	}

	// 3. Event Source Mapping
	_, err = awslambda.NewEventSourceMapping(ctx, name+"-esm", &awslambda.EventSourceMappingArgs{
		EventSourceArn:   table.StreamArn,
		FunctionName:     consumer.Arn.ToStringOutput(),
		StartingPosition: pulumi.String(startingPosition),
		BatchSize:        pulumi.Int(batchSize),
		Enabled:          pulumi.Bool(true),
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("creating event source mapping for %q: %w", name, err)
	}

	return nil
}

// ── EventBridge Pipe consumer ──────────────────────────────────────────────

func wireEventBridgeConsumer(
	ctx *pulumi.Context,
	name string,
	stage string,
	table *awsdynamodb.Table,
	consumer *StreamEventBridgeConsumer,
	parent pulumi.Resource,
) error {
	// Pipe role name: {name}-{stage}-pipe-role
	pipeRoleName := fmt.Sprintf("%s-%s-pipe-role", name, stage)

	// 1. IAM role for the Pipe — Anvil managed, invisible to the user
	pipeRole, err := iam.NewRole(ctx, pipeRoleName, &iam.RoleArgs{
		Name: pulumi.String(pipeRoleName),
		AssumeRolePolicy: pulumi.String(`{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Service": "pipes.amazonaws.com" },
    "Action": "sts:AssumeRole"
  }]
}`),
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("creating pipe IAM role for %q: %w", name, err)
	}

	// 2. Inline policy — stream read + PutEvents on target bus
	pipePolicy := pulumi.All(table.StreamArn, consumer.BusArn.ToStringOutput()).ApplyT(func(args []interface{}) (string, error) {
		streamArn := args[0].(string)
		busArn := args[1].(string)

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
			Statement: []statement{
				{
					Effect: "Allow",
					Action: []string{
						"dynamodb:GetRecords",
						"dynamodb:GetShardIterator",
						"dynamodb:DescribeStream",
						"dynamodb:ListStreams",
					},
					Resource: []string{streamArn},
				},
				{
					Effect:   "Allow",
					Action:   []string{"events:PutEvents"},
					Resource: []string{busArn},
				},
			},
		})
		if err != nil {
			return "", fmt.Errorf("failed to marshal pipe policy: %w", err)
		}
		return string(b), nil
	}).(pulumi.StringOutput)

	_, err = iam.NewRolePolicy(ctx, name+"-pipe-policy", &iam.RolePolicyArgs{
		Role:   pipeRole.Name,
		Policy: pipePolicy,
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("attaching pipe role policy for %q: %w", name, err)
	}

	// 3. EventBridge Pipe — DynamoDB stream → EventBus
	pipeName := fmt.Sprintf("%s-%s-pipe", name, stage)
	_, err = pipes.NewPipe(ctx, name+"-pipe", &pipes.PipeArgs{
		Name:    pulumi.String(pipeName),
		RoleArn: pipeRole.Arn,
		Source:  table.StreamArn,
		SourceParameters: pipes.PipeSourceParametersArgs{
			DynamodbStreamParameters: &pipes.PipeSourceParametersDynamodbStreamParametersArgs{
				StartingPosition: pulumi.String("LATEST"),
			},
		},
		Target: consumer.BusArn.ToStringOutput(),
		TargetParameters: pipes.PipeTargetParametersArgs{
			EventbridgeEventBusParameters: &pipes.PipeTargetParametersEventbridgeEventBusParametersArgs{
				DetailType: pulumi.String(fmt.Sprintf("%s.stream", name)),
				Source:     pulumi.String(fmt.Sprintf("anvil.dynamodb.%s", name)),
			},
		},
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("creating EventBridge pipe for %q: %w", name, err)
	}

	return nil
}

// ── Grant methods ──────────────────────────────────────────────────────────
// Grants are handled entirely in the SDK layer via fix-sdk-grants.ts.
// The DYNAMODB_GRANT_CONFIG entry in scripts/grants/configs.ts injects
// grantRead, grantWrite, grantReadWrite, and grantDelete into the generated
// SDK classes (TypeScript + Python). No grant methods live in this Go file.
//
// Grant actions are scoped to tableArn + tableArn/index/* in the SDK helpers
// to cover both direct table access and GSI queries.
//
// Stream consumer wiring and table grants are independent — a Lambda wired as
// a stream consumer does NOT automatically receive table read/write access.
// Users must call grantRead/Write explicitly. This is intentional: least-privilege by default.
//
// grantDelete covers dynamodb:DeleteItem only (row-level delete).
// dynamodb:DeleteTable is never granted — table destruction is handled
// exclusively by Anvil's destroy command.

// ── Doc notes ──────────────────────────────────────────────────────────────
//
// PROVISIONED BILLING (docs):
//   Default is PAY_PER_REQUEST (serverless, no capacity planning).
//   PROVISIONED mode is available via the hatch for workloads with predictable
//   traffic needing cost optimisation. When using PROVISIONED, GSIs require
//   their own read/write capacity units — call this out clearly in docs as it
//   is a common source of confusion for users migrating from PAY_PER_REQUEST.
//
// STARTING POSITION (docs):
//   Default is TRIM_HORIZON (AWS default) — replays ALL existing stream records
//   from the start of the 24hr retention window. For most production workloads
//   LATEST is safer as it only processes new events from consumer creation.
//   Call this out prominently in docs so users make a conscious choice.
//
// EVENTBRIDGE PIPE DLQ (ticket v2):
//   When stream.consumer.eventBridge is configured, the Pipe has no failure
//   destination in v1. Failed deliveries are retried per Pipe retry policy but
//   ultimately dropped. DynamoDB stream records are retained for only 24 hours
//   so dropped events are unrecoverable. Future: add optional Tier 2 dlq field
//   under stream.consumer.eventBridge, or auto-create a scoped SQS DLQ.
//   Consider whether this should be Tier 1 given the data loss risk.
//
// ENCRYPTION (docs):
//   Tier 1: AWS_OWNED_KMS — always on, zero cost, no config required.
//   Tier 2: kmsKeyArn — CMK for compliance workloads requiring key rotation
//   control, CloudTrail audit trail per decrypt, or key revocation capability.
//   CMK costs ~$1/month + API call charges. Document the cost tradeoff clearly.
