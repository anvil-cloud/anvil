package queue

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anvil-cloud/anvil/provider/internal/hatch"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	awslambda "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lambda"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/sqs"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// QueueDlqArgs configures the dead letter queue for this queue.
// Omit to use a managed DLQ with defaults.
// Set Arn to reuse an existing queue as the DLQ.
type QueueDlqArgs struct {
	// Arn is the ARN of an existing queue to use as the DLQ.
	// When set, Anvil wires the redrive policy to this queue without creating
	// a new one. If the parent queue is FIFO, this ARN must end with ".fifo".
	// When omitted, Anvil creates a managed DLQ as a child resource.
	Arn pulumi.StringInput `pulumi:"arn,optional"`

	// MaxReceiveCount is how many times a message can be received before
	// being moved to the DLQ. Default: 3.
	MaxReceiveCount int `pulumi:"maxReceiveCount,optional"`
}

// QueueLambdaConsumerArgs defines a Lambda function as an SQS consumer.
// Anvil creates the event source mapping (trigger) and grants the Lambda
// role the permissions needed to consume messages.
type QueueLambdaConsumerArgs struct {
	// Arn is the ARN of the Lambda function to trigger.
	// Use fn.arn from an Anvil Lambda component.
	Arn pulumi.StringInput `pulumi:"arn"`

	// LambdaRoleArn is the ARN of the Lambda's execution role.
	// Use fn.roleArn from an Anvil Lambda component.
	// Required so Anvil can attach the consume permissions policy.
	LambdaRoleArn pulumi.StringInput `pulumi:"lambdaRoleArn"`
}

// QueueConsumerArgs configures a consumer for this queue.
type QueueConsumerArgs struct {
	// Lambda wires an Anvil Lambda function as the SQS consumer.
	// Creates the event source mapping and grants consume permissions.
	Lambda *QueueLambdaConsumerArgs `pulumi:"lambda,optional"`
}

// QueueArgs defines the inputs for an Anvil-managed SQS queue.
type QueueArgs struct {
	// Fifo creates a FIFO queue when true. FIFO queues guarantee message
	// ordering and exactly-once processing but have lower throughput
	// (~3,000 msg/s vs unlimited for standard). Use for financial transactions,
	// inventory updates, or any workflow where ordering or deduplication matters.
	// Default: false (standard queue).
	Fifo bool `pulumi:"fifo,optional"`

	// Dlq configures the dead letter queue. Always provisioned — messages
	// that fail MaxReceiveCount times are moved here instead of being silently
	// dropped. Omit fields to use defaults (managed DLQ, maxReceiveCount: 3).
	// Set Arn to reuse an existing queue as the DLQ.
	Dlq *QueueDlqArgs `pulumi:"dlq,optional"`

	// Consumer wires a compute resource to consume messages from this queue.
	// Creates the event source mapping and grants the necessary IAM permissions.
	Consumer *QueueConsumerArgs `pulumi:"consumer,optional"`

	Hatch map[string]map[string]interface{} `pulumi:"hatch,optional"`
}

// Queue is the Anvil-managed SQS queue component resource.
type Queue struct {
	pulumi.ResourceState

	// Url is the URL of the SQS queue. Use this to send and receive messages.
	Url pulumi.StringOutput `pulumi:"url"`

	// Arn is the ARN of the SQS queue.
	Arn pulumi.StringOutput `pulumi:"arn"`

	// QueueName is the physical name of the SQS queue.
	QueueName pulumi.StringOutput `pulumi:"name"`

	// DlqUrl is the URL of the dead letter queue.
	DlqUrl pulumi.StringOutput `pulumi:"dlqUrl"`

	// DlqArn is the ARN of the dead letter queue.
	DlqArn pulumi.StringOutput `pulumi:"dlqArn"`

	name string
}

func (q *Queue) Name() string {
	return q.name
}

func (q *Queue) Annotate(a infer.Annotator) {
	a.SetToken("aws", "Queue")
	a.Describe(&q, "An Anvil-managed SQS queue. A dead letter queue is always provisioned to prevent silent message loss. SSE-SQS encryption is on by default at no cost.")
}

func resolveMaxReceiveCount(dlq *QueueDlqArgs) int {
	if dlq != nil && dlq.MaxReceiveCount > 0 {
		return dlq.MaxReceiveCount
	}
	return 3
}

// sqsURLFromARN derives the SQS queue URL from its ARN deterministically.
// ARN format: arn:aws:sqs:{region}:{accountId}:{queueName}
// URL format: https://sqs.{region}.amazonaws.com/{accountId}/{queueName}
func sqsURLFromARN(arn pulumi.StringOutput) pulumi.StringOutput {
	return arn.ApplyT(func(a string) string {
		parts := strings.Split(a, ":")
		if len(parts) != 6 {
			return ""
		}
		region := parts[3]
		accountId := parts[4]
		queueName := parts[5]
		return fmt.Sprintf("https://sqs.%s.amazonaws.com/%s/%s", region, accountId, queueName)
	}).(pulumi.StringOutput)
}

func NewQueue(ctx *pulumi.Context, name string, args QueueArgs, opts ...pulumi.ResourceOption) (*Queue, error) {
	q := &Queue{name: name}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, q, opts...); err != nil {
		return nil, err
	}

	// ── Validation ─────────────────────────────────────
	if args.Fifo && args.Dlq != nil && args.Dlq.Arn != nil {
		args.Dlq.Arn.ToStringOutput().ApplyT(func(arn string) (string, error) {
			if arn != "" && !strings.HasSuffix(arn, ".fifo") {
				return "", fmt.Errorf(
					"queue %q: fifo is true but dlq.arn %q does not end with \".fifo\" — "+
						"a FIFO queue requires a FIFO dead letter queue",
					name, arn,
				)
			}
			return arn, nil
		})
	}

	const visibilityTimeout = 30
	const messageRetention = 345600 // 4 days
	maxReceiveCount := resolveMaxReceiveCount(args.Dlq)

	// ── 1. Dead letter queue ───────────────────────────
	var dlqArn pulumi.StringOutput
	var dlqUrl pulumi.StringOutput

	byoDLQ := args.Dlq != nil && args.Dlq.Arn != nil

	if byoDLQ {
		dlqArn = args.Dlq.Arn.ToStringOutput()
		dlqUrl = sqsURLFromARN(dlqArn)
	} else {
		var dlqPhysicalName string
		if args.Fifo {
			dlqPhysicalName = provider.FifoQueueName(stage, name+"-dlq", stageId)
		} else {
			dlqPhysicalName = provider.PhysicalName(stage, name+"-dlq", "queue", stageId)
		}

		dlqProps := pulumi.Map{
			"name":                     pulumi.String(dlqPhysicalName),
			"visibilityTimeoutSeconds": pulumi.Int(visibilityTimeout),
			"messageRetentionSeconds":  pulumi.Int(messageRetention),
			"tags": pulumi.StringMap{
				"ManagedBy": pulumi.String("anvil"),
				"Role":      pulumi.String("dlq"),
			},
		}
		if args.Fifo {
			dlqProps["fifoQueue"] = pulumi.Bool(true)
		}

		dlqPropsMerged := hatch.Merge(args.Hatch["dlq"], dlqProps)
		dlqRes := &sqs.Queue{}
		if err := ctx.RegisterResource("aws:sqs/queue:Queue", name+"-dlq", dlqPropsMerged, dlqRes, pulumi.Parent(q)); err != nil {
			return nil, fmt.Errorf("failed to create DLQ for queue %q: %w", name, err)
		}
		dlqArn = dlqRes.Arn
		dlqUrl = dlqRes.Url
	}

	// ── 2. Main queue ──────────────────────────────────
	var physicalName string
	if args.Fifo {
		physicalName = provider.FifoQueueName(stage, name, stageId)
	} else {
		physicalName = provider.PhysicalName(stage, name, "queue", stageId)
	}

	redrivePolicy := pulumi.Sprintf(
		`{"deadLetterTargetArn":"%s","maxReceiveCount":%d}`,
		dlqArn,
		maxReceiveCount,
	)

	queueProps := pulumi.Map{
		"name":                     pulumi.String(physicalName),
		"visibilityTimeoutSeconds": pulumi.Int(visibilityTimeout),
		"messageRetentionSeconds":  pulumi.Int(messageRetention),
		"redrivePolicy":            redrivePolicy,
		"tags": pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}
	if args.Fifo {
		queueProps["fifoQueue"] = pulumi.Bool(true)
	}

	queuePropsMerged := hatch.Merge(args.Hatch["queue"], queueProps)
	queueRes := &sqs.Queue{}
	if err := ctx.RegisterResource("aws:sqs/queue:Queue", name, queuePropsMerged, queueRes, pulumi.Parent(q)); err != nil {
		return nil, fmt.Errorf("failed to create queue %q: %w", name, err)
	}

	// ── 3. Lambda consumer (optional) ─────────────────
	if args.Consumer != nil && args.Consumer.Lambda != nil {
		consumer := args.Consumer.Lambda

		// 3a. Event source mapping — wires the queue as a Lambda trigger.
		_, err := awslambda.NewEventSourceMapping(ctx, name+"-esm", &awslambda.EventSourceMappingArgs{
			EventSourceArn: queueRes.Arn,
			FunctionName:   consumer.Arn.ToStringOutput(),
			Enabled:        pulumi.Bool(true),
		}, pulumi.Parent(q))
		if err != nil {
			return nil, fmt.Errorf("failed to create event source mapping for queue %q: %w", name, err)
		}

		// 3b. IAM policy — grants the Lambda role permission to consume messages.
		policyJSON := queueRes.Arn.ApplyT(func(queueArn string) (string, error) {
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
						"sqs:ReceiveMessage",
						"sqs:DeleteMessage",
						"sqs:GetQueueAttributes",
					},
					Resource: []string{queueArn},
				}},
			})
			if err != nil {
				return "", fmt.Errorf("failed to marshal consumer policy: %w", err)
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

		_, err = iam.NewRolePolicy(ctx, name+"-consumer-policy", &iam.RolePolicyArgs{
			Role:   roleName,
			Policy: policyJSON,
		}, pulumi.Parent(q))
		if err != nil {
			return nil, fmt.Errorf("failed to attach consumer policy for queue %q: %w", name, err)
		}
	}

	// ── 4. Wire outputs ────────────────────────────────
	q.Url = queueRes.Url
	q.Arn = queueRes.Arn
	q.QueueName = queueRes.Name
	q.DlqUrl = dlqUrl
	q.DlqArn = dlqArn

	ctx.RegisterResourceOutputs(q, pulumi.Map{
		"url":    queueRes.Url,
		"arn":    queueRes.Arn,
		"name":   queueRes.Name,
		"dlqUrl": dlqUrl,
		"dlqArn": dlqArn,
	})

	return q, nil
}
