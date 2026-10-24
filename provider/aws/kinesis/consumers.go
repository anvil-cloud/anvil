package kinesis

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	awslambda "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lambda"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/sqs"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	defaultBatchSize = 100
	maxBatchSize     = 10000

	// maxRetryAttempts caps retries of a failing batch. AWS's default is -1
	// (retry until the record expires), which blocks the shard for up to the
	// whole retention period.
	maxRetryAttempts = 3

	dlqRetentionSeconds = 1209600 // 14 days, the SQS maximum

	// maxConsumerNameLength keeps the derived DLQ name inside the SQS limit.
	maxConsumerNameLength = 32

	// maxFilterPatterns is Lambda's default quota of filters per event source
	// mapping. Patterns are OR'd: a record that matches any one is delivered.
	maxFilterPatterns = 5
	maxPatternLength  = 4096

	// Starting positions use the AWS values, matching DynamoDB's stream consumer.
	startTrimHorizon = "TRIM_HORIZON"
	startLatest      = "LATEST"
)

var consumerNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]*$`)

// KinesisLambdaConsumerArgs identifies the Lambda function that consumes the stream.
type KinesisLambdaConsumerArgs struct {
	// Arn is the ARN of the Lambda function to trigger.
	// Use fn.arn from an Anvil Lambda component.
	Arn pulumi.StringInput `pulumi:"arn"`

	// LambdaRoleArn is the ARN of the Lambda's execution role.
	// Use fn.roleArn from an Anvil Lambda component.
	// Required so Anvil can attach the grantRead and DLQ permissions.
	LambdaRoleArn pulumi.StringInput `pulumi:"lambdaRoleArn"`
}

// KinesisConsumerArgs configures one consumer of this stream. Anvil creates
// the event source mapping, a dead letter queue for records that still fail
// after retries, sqs:SendMessage on that DLQ, and — when grantRead is true —
// the grantRead permissions.
type KinesisConsumerArgs struct {
	// ConsumerName identifies the consumer and names its resources
	// (<stream>-<consumerName>-dlq, -esm, -policy), so adding, removing or
	// reordering consumers only touches the one that changed. Usually the
	// Lambda's name, e.g. "indexer". Letters, numbers and hyphens, up to 32
	// characters, unique per stream. Renaming a consumer recreates its resources.
	ConsumerName string `pulumi:"consumerName"`

	// Lambda wires an Anvil Lambda function as the consumer.
	Lambda *KinesisLambdaConsumerArgs `pulumi:"lambda"`

	// GrantRead attaches the grantRead permissions on this stream to the
	// consumer's execution role. Required, so the access is visible where the
	// consumer is declared. It is created inside the component so the event
	// source mapping can wait for it — a separate grantRead call can't be
	// ordered before the mapping. false skips it with a warning: the role must
	// then get read access some other way, or the mapping fails to create.
	GrantRead bool `pulumi:"grantRead"`

	// BatchSize is the most records sent to the Lambda in one invocation,
	// 1–10000. Default: 100.
	BatchSize int `pulumi:"batchSize,optional"`

	// ReportBatchItemFailures lets the Lambda report which records in a batch
	// failed, so only those are retried. The handler must return
	// { batchItemFailures: [{ itemIdentifier: <sequenceNumber> }] }.
	// Default: false (a failure retries the whole batch, split in half each time).
	ReportBatchItemFailures bool `pulumi:"reportBatchItemFailures,optional"`

	// Filter limits which records invoke the Lambda. Records that don't match
	// are skipped (not invoked, not billed) and the consumer moves past them.
	// When omitted, every record is delivered.
	Filter *KinesisConsumerFilterArgs `pulumi:"filter,optional"`

	// StartingPosition is where the consumer starts reading when it's first
	// created: "TRIM_HORIZON" (default) processes every record still in the
	// stream, so a consumer that builds state misses nothing from before it
	// existed; "LATEST" only processes records written after it's created,
	// for consumers that only care about what happens from now on. Changing it
	// later recreates the event source mapping, which resets its position.
	StartingPosition string `pulumi:"startingPosition,optional"`

	// Enabled pauses the consumer when false: the event source mapping stops
	// polling but keeps its position, so it resumes where it left off. Records
	// older than the stream's retention are lost while paused. Default: true.
	Enabled *bool `pulumi:"enabled,optional"`
}

// KinesisConsumerFilterArgs limits which records invoke a consumer. tables
// and patterns can be combined; a record matching any of them is delivered.
// Together they can produce at most 5 filter patterns (Lambda's quota).
type KinesisConsumerFilterArgs struct {
	// Tables delivers only Aurora DSQL CDC events for these tables, matched
	// against the event's source.table. Qualify with a schema to also match
	// source.schema: "orders" matches orders in any schema, "app.orders" only
	// app.orders. Unqualified tables share one pattern; each qualified table
	// takes one.
	Tables []string `pulumi:"tables,optional"`

	// Patterns are raw Lambda event filter patterns (JSON), for records that
	// aren't DSQL CDC events. Kinesis record data is matched under "data",
	// e.g. {"data":{"type":["order.created"]}}. Each pattern takes one filter.
	Patterns []string `pulumi:"patterns,optional"`
}

// filterPatterns turns a consumer's filter into Lambda filter patterns.
func filterPatterns(filter *KinesisConsumerFilterArgs) ([]string, error) {
	if filter == nil {
		return nil, nil
	}
	if len(filter.Tables) == 0 && len(filter.Patterns) == 0 {
		return nil, fmt.Errorf("filter must set tables or patterns — omit filter to deliver every record")
	}

	var patterns []string
	marshal := func(source map[string][]string) error {
		b, err := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"source": source}})
		if err != nil {
			return fmt.Errorf("failed to marshal table filter: %w", err)
		}
		patterns = append(patterns, string(b))
		return nil
	}

	var unqualified []string
	for _, table := range filter.Tables {
		schema, tbl, qualified := strings.Cut(table, ".")
		if !qualified {
			tbl, schema = schema, ""
		}
		if tbl == "" || (qualified && schema == "") || strings.Contains(tbl, ".") {
			return nil, fmt.Errorf("filter.tables entry %q must be \"table\" or \"schema.table\"", table)
		}
		if !qualified {
			unqualified = append(unqualified, tbl)
			continue
		}
		if err := marshal(map[string][]string{"schema": {schema}, "table": {tbl}}); err != nil {
			return nil, err
		}
	}
	if len(unqualified) > 0 {
		if err := marshal(map[string][]string{"table": unqualified}); err != nil {
			return nil, err
		}
	}

	for _, pattern := range filter.Patterns {
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(pattern), &parsed); err != nil {
			return nil, fmt.Errorf("filter.patterns entry %q is not a JSON object: %w", pattern, err)
		}
		if len(pattern) > maxPatternLength {
			return nil, fmt.Errorf("filter.patterns entry is %d characters, Lambda's limit is %d", len(pattern), maxPatternLength)
		}
		patterns = append(patterns, pattern)
	}

	if len(patterns) > maxFilterPatterns {
		return nil, fmt.Errorf("filter produces %d patterns, Lambda allows %d per consumer — group unqualified tables or combine patterns", len(patterns), maxFilterPatterns)
	}
	return patterns, nil
}

// resolvedConsumer is the validated, defaulted form of a KinesisConsumerArgs.
type resolvedConsumer struct {
	batchSize        int
	filterPatterns   []string
	enabled          bool
	startingPosition string
}

// resolveConsumers validates the consumers and returns each one resolved,
// plus warnings to surface for consumers with grantRead: false.
func resolveConsumers(name string, consumers []KinesisConsumerArgs) ([]resolvedConsumer, []string, error) {
	resolved := make([]resolvedConsumer, len(consumers))
	var warnings []string
	seen := make(map[string]int, len(consumers))
	for i, consumer := range consumers {
		if consumer.ConsumerName == "" {
			return nil, nil, fmt.Errorf("kinesis stream %q: consumers[%d].consumerName is required, e.g. the Lambda's name", name, i)
		}
		if !consumerNamePattern.MatchString(consumer.ConsumerName) || len(consumer.ConsumerName) > maxConsumerNameLength {
			return nil, nil, fmt.Errorf("kinesis stream %q: consumers[%d].consumerName %q must start with a letter, contain only letters, numbers and hyphens, and be at most %d characters", name, i, consumer.ConsumerName, maxConsumerNameLength)
		}
		if first, ok := seen[consumer.ConsumerName]; ok {
			return nil, nil, fmt.Errorf("kinesis stream %q: consumers[%d].consumerName %q is already used by consumers[%d] — consumer names must be unique", name, i, consumer.ConsumerName, first)
		}
		seen[consumer.ConsumerName] = i
		if consumer.Lambda == nil {
			return nil, nil, fmt.Errorf("kinesis stream %q: consumer %q: lambda is required", name, consumer.ConsumerName)
		}
		if !consumer.GrantRead {
			warnings = append(warnings, fmt.Sprintf(
				"⚠ %s → consumer %q has grantRead: false, so Anvil is not granting it read access to the stream. "+
					"Its role needs kinesis:GetRecords, GetShardIterator, DescribeStream, DescribeStreamSummary and ListShards "+
					"some other way, or the event source mapping will fail to create.",
				name, consumer.ConsumerName,
			))
		}
		batchSize := consumer.BatchSize
		if batchSize == 0 {
			batchSize = defaultBatchSize
		}
		if batchSize < 1 || batchSize > maxBatchSize {
			return nil, nil, fmt.Errorf("kinesis stream %q: consumer %q: batchSize must be between 1 and %d, got %d", name, consumer.ConsumerName, maxBatchSize, batchSize)
		}
		patterns, err := filterPatterns(consumer.Filter)
		if err != nil {
			return nil, nil, fmt.Errorf("kinesis stream %q: consumer %q: %w", name, consumer.ConsumerName, err)
		}
		startingPosition := consumer.StartingPosition
		if startingPosition == "" {
			startingPosition = startTrimHorizon
		}
		if startingPosition != startTrimHorizon && startingPosition != startLatest {
			return nil, nil, fmt.Errorf("kinesis stream %q: consumer %q: startingPosition must be %q or %q, got %q", name, consumer.ConsumerName, startTrimHorizon, startLatest, startingPosition)
		}
		enabled := consumer.Enabled == nil || *consumer.Enabled
		resolved[i] = resolvedConsumer{batchSize: batchSize, filterPatterns: patterns, enabled: enabled, startingPosition: startingPosition}
	}
	return resolved, warnings, nil
}

// consumerPolicy is the inline policy attached to a consumer's execution role:
// send on its DLQ and, when grantRead is set, read on the stream.
func consumerPolicy(streamArn, dlqArn string, grantRead bool) (string, error) {
	type statement struct {
		Effect   string   `json:"Effect"`
		Action   []string `json:"Action"`
		Resource []string `json:"Resource"`
	}
	type doc struct {
		Version   string      `json:"Version"`
		Statement []statement `json:"Statement"`
	}
	statements := []statement{
		{Effect: "Allow", Action: []string{"sqs:SendMessage"}, Resource: []string{dlqArn}},
	}
	if grantRead {
		statements = append(statements, statement{Effect: "Allow", Action: readActions, Resource: []string{streamArn}})
	}
	b, err := json.Marshal(doc{Version: "2012-10-17", Statement: statements})
	if err != nil {
		return "", fmt.Errorf("failed to marshal consumer policy: %w", err)
	}
	return string(b), nil
}

// roleNameFromArn returns the role name from an IAM role ARN, dropping any path.
func roleNameFromArn(arn string) string {
	return arn[strings.LastIndex(arn, "/")+1:]
}

// wireConsumers creates, per consumer, a DLQ, the IAM policy and the event
// source mapping, all keyed by the consumer's consumerName. It returns the DLQ ARNs
// and URLs keyed by consumer name.
func wireConsumers(ctx *pulumi.Context, k *KinesisStream, name, stage, stageId string, streamArn pulumi.StringOutput, consumers []KinesisConsumerArgs, resolved []resolvedConsumer) (pulumi.StringMap, pulumi.StringMap, error) {
	dlqArns := pulumi.StringMap{}
	dlqUrls := pulumi.StringMap{}

	for i, consumer := range consumers {
		consumerName := fmt.Sprintf("%s-%s", name, consumer.ConsumerName)

		// Records that still fail after retries land here instead of being dropped.
		dlq, err := sqs.NewQueue(ctx, consumerName+"-dlq", &sqs.QueueArgs{
			Name:                    pulumi.String(provider.PhysicalName(stage, consumerName+"-dlq", "queue", stageId)),
			MessageRetentionSeconds: pulumi.Int(dlqRetentionSeconds),
			SqsManagedSseEnabled:    pulumi.Bool(true),
			Tags: pulumi.StringMap{
				"ManagedBy": pulumi.String("anvil"),
				"Role":      pulumi.String("dlq"),
			},
		}, pulumi.Parent(k))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create DLQ for kinesis stream %q consumer %q: %w", name, consumer.ConsumerName, err)
		}

		policyJSON := pulumi.All(streamArn, dlq.Arn).ApplyT(func(arns []interface{}) (string, error) {
			return consumerPolicy(arns[0].(string), arns[1].(string), consumer.GrantRead)
		}).(pulumi.StringOutput)

		roleName := consumer.Lambda.LambdaRoleArn.ToStringOutput().ApplyT(roleNameFromArn).(pulumi.StringOutput)

		policy, err := iam.NewRolePolicy(ctx, consumerName+"-policy", &iam.RolePolicyArgs{
			Role:   roleName,
			Policy: policyJSON,
		}, pulumi.Parent(k))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to attach consumer policy for kinesis stream %q consumer %q: %w", name, consumer.ConsumerName, err)
		}

		esmArgs := &awslambda.EventSourceMappingArgs{
			EventSourceArn:             streamArn,
			FunctionName:               consumer.Lambda.Arn.ToStringOutput(),
			StartingPosition:           pulumi.String(resolved[i].startingPosition),
			BatchSize:                  pulumi.Int(resolved[i].batchSize),
			BisectBatchOnFunctionError: pulumi.Bool(true),
			MaximumRetryAttempts:       pulumi.Int(maxRetryAttempts),
			DestinationConfig: &awslambda.EventSourceMappingDestinationConfigArgs{
				OnFailure: &awslambda.EventSourceMappingDestinationConfigOnFailureArgs{
					DestinationArn: dlq.Arn,
				},
			},
			Enabled: pulumi.Bool(resolved[i].enabled),
		}
		if consumer.ReportBatchItemFailures {
			esmArgs.FunctionResponseTypes = pulumi.StringArray{pulumi.String("ReportBatchItemFailures")}
		}
		if len(resolved[i].filterPatterns) > 0 {
			filters := awslambda.EventSourceMappingFilterCriteriaFilterArray{}
			for _, pattern := range resolved[i].filterPatterns {
				filters = append(filters, &awslambda.EventSourceMappingFilterCriteriaFilterArgs{Pattern: pulumi.String(pattern)})
			}
			esmArgs.FilterCriteria = &awslambda.EventSourceMappingFilterCriteriaArgs{Filters: filters}
		}

		// Lambda checks it can read the stream when the mapping is created, so
		// the policy must exist first.
		if _, err := awslambda.NewEventSourceMapping(ctx, consumerName+"-esm", esmArgs, pulumi.Parent(k), pulumi.DependsOn([]pulumi.Resource{policy})); err != nil {
			return nil, nil, fmt.Errorf("failed to create event source mapping for kinesis stream %q consumer %q: %w", name, consumer.ConsumerName, err)
		}

		dlqArns[consumer.ConsumerName] = dlq.Arn
		dlqUrls[consumer.ConsumerName] = dlq.Url
	}

	return dlqArns, dlqUrls, nil
}
