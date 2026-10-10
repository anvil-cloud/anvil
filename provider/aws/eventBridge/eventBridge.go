package eventBridge

import (
	"encoding/json"
	"fmt"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/anvil-cloud/anvil/provider/internal/transform"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	awslambda "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lambda"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// EventBusRulePattern defines the EventBridge event pattern for a rule.
// All fields are AND-ed — an event must match every specified field.
type EventBusRulePattern struct {
	// Source filters on the event source field.
	// e.g. ["anvil.api", "anvil.worker"]
	Source []string `pulumi:"source,optional"`

	// DetailType filters on the detail-type field.
	// e.g. ["order.created", "order.cancelled"]
	DetailType []string `pulumi:"detailType,optional"`

	// Detail matches against fields inside the event detail payload.
	// Structure depends entirely on the event producer.
	// e.g. { "orderId": [{ "exists": true }], "status": ["pending"] }
	Detail interface{} `pulumi:"detail,optional"`
}

// EventBusLambdaTargetArgs defines a Lambda function as a rule target.
type EventBusLambdaTargetArgs struct {
	// Arn is the ARN of the Lambda function to invoke.
	// Use fn.arn from an Anvil Lambda component.
	Arn pulumi.StringInput `pulumi:"arn"`

	// LambdaRoleArn is the ARN of the Lambda's execution role.
	// Use fn.roleArn from an Anvil Lambda component.
	LambdaRoleArn pulumi.StringInput `pulumi:"lambdaRoleArn"`
}

// EventBusRuleTarget defines what receives matching events.
type EventBusRuleTarget struct {
	// Lambda invokes an Anvil Lambda function when events match the rule pattern.
	// Anvil creates the EventBridge target and grants EventBridge permission
	// to invoke the function via a resource-based policy.
	Lambda *EventBusLambdaTargetArgs `pulumi:"lambda,optional"`
}

// EventBusRule defines an EventBridge rule on this bus.
type EventBusRule struct {
	// Name is the logical name for this rule.
	Name string `pulumi:"name"`

	// Pattern defines which events this rule matches.
	Pattern EventBusRulePattern `pulumi:"pattern"`

	// Target defines what receives matching events.
	Target EventBusRuleTarget `pulumi:"target"`
}

// EventBusArgs defines the inputs for an Anvil-managed EventBridge event bus.
type EventBusArgs struct {
	// Rules defines EventBridge rules on this bus.
	// Each rule matches events by pattern and routes them to a target.
	Rules []EventBusRule `pulumi:"rules,optional"`

	Transform map[string]map[string]interface{} `pulumi:"transform,optional"`
}

// EventBus is the Anvil-managed EventBridge event bus component resource.
type EventBus struct {
	pulumi.ResourceState

	// BusName is the name of the EventBridge event bus.
	// Pass to HttpApi consumer: { eventBridge: { name: bus.name } }
	BusName pulumi.StringOutput `pulumi:"name"`

	// Arn is the ARN of the EventBridge event bus.
	Arn pulumi.StringOutput `pulumi:"arn"`

	name string
}

func (e *EventBus) Name() string {
	return e.name
}

func (e *EventBus) Annotate(a infer.Annotator) {
	a.SetToken("aws", "EventBus")
	a.Describe(&e, "An Anvil-managed EventBridge event bus. Archives events for 7 days by default for replay and debugging. Rules route matching events to Lambda targets.")
}

// buildPatternJSON serialises an EventBusRulePattern to an EventBridge
// content-based filtering JSON string.
func buildPatternJSON(pattern EventBusRulePattern) (string, error) {
	doc := map[string]interface{}{}

	if len(pattern.Source) > 0 {
		doc["source"] = pattern.Source
	}
	if len(pattern.DetailType) > 0 {
		doc["detail-type"] = pattern.DetailType
	}
	if pattern.Detail != nil {
		doc["detail"] = pattern.Detail
	}

	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("failed to marshal event pattern: %w", err)
	}
	return string(b), nil
}

func NewEventBus(ctx *pulumi.Context, name string, args EventBusArgs, opts ...pulumi.ResourceOption) (*EventBus, error) {
	e := &EventBus{name: name}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, e, opts...); err != nil {
		return nil, err
	}

	physicalName := provider.PhysicalName(stage, name, "bus", stageId)

	// ── 1. Event bus ───────────────────────────────────
	busProps := transform.MergeTransform(args.Transform["bus"], pulumi.Map{
		"name": pulumi.String(physicalName),
		"tags": pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	})

	busRes := &cloudwatch.EventBus{}
	if err := ctx.RegisterResource("aws:cloudwatch/eventBus:EventBus", name, busProps, busRes, pulumi.Parent(e)); err != nil {
		return nil, fmt.Errorf("failed to create event bus %q: %w", name, err)
	}

	// ── 2. Archive ─────────────────────────────────────
	// Default 7 day retention. Override via transform["archive"].
	archiveProps := transform.MergeTransform(args.Transform["archive"], pulumi.Map{
		"name":           pulumi.Sprintf("%s-archive", physicalName),
		"eventSourceArn": busRes.Arn,
		"retentionDays":  pulumi.Int(7),
	})

	if err := ctx.RegisterResource("aws:cloudwatch/eventArchive:EventArchive", name+"-archive", archiveProps, &cloudwatch.EventArchive{}, pulumi.Parent(e)); err != nil {
		return nil, fmt.Errorf("failed to create event archive for bus %q: %w", name, err)
	}

	// ── 3. Rules ───────────────────────────────────────
	for _, rule := range args.Rules {
		rule := rule

		patternJSON, err := buildPatternJSON(rule.Pattern)
		if err != nil {
			return nil, fmt.Errorf("bus %q rule %q: %w", name, rule.Name, err)
		}

		ruleName := provider.PhysicalName(stage, name+"-"+rule.Name, "rule", stageId)

		ruleRes := &cloudwatch.EventRule{}
		if err := ctx.RegisterResource("aws:cloudwatch/eventRule:EventRule", name+"-rule-"+rule.Name, pulumi.Map{
			"name":         pulumi.String(ruleName),
			"eventBusName": busRes.Name,
			"eventPattern": pulumi.String(patternJSON),
			"state":        pulumi.String("ENABLED"),
			"tags": pulumi.StringMap{
				"ManagedBy": pulumi.String("anvil"),
			},
		}, ruleRes, pulumi.Parent(e)); err != nil {
			return nil, fmt.Errorf("failed to create rule %q on bus %q: %w", rule.Name, name, err)
		}

		// ── Lambda target ──────────────────────────────
		if rule.Target.Lambda != nil {
			target := rule.Target.Lambda

			// Wire the EventBridge target pointing at the Lambda
			if err := ctx.RegisterResource("aws:cloudwatch/eventTarget:EventTarget", name+"-target-"+rule.Name, pulumi.Map{
				"rule":         ruleRes.Name,
				"eventBusName": busRes.Name,
				"arn":          target.Arn.ToStringOutput(),
			}, &cloudwatch.EventTarget{}, pulumi.Parent(e)); err != nil {
				return nil, fmt.Errorf("failed to create target for rule %q on bus %q: %w", rule.Name, name, err)
			}

			// Grant EventBridge permission to invoke the Lambda via resource-based policy.
			// This is NOT an IAM grant on the Lambda role — it's a permission on the
			// Lambda function itself allowing events.amazonaws.com to invoke it.
			if _, err := awslambda.NewPermission(ctx, name+"-perm-"+rule.Name, &awslambda.PermissionArgs{
				Action:    pulumi.String("lambda:InvokeFunction"),
				Function:  target.Arn.ToStringOutput(),
				Principal: pulumi.String("events.amazonaws.com"),
				SourceArn: ruleRes.Arn,
			}, pulumi.Parent(e)); err != nil {
				return nil, fmt.Errorf("failed to grant EventBridge invoke permission for rule %q on bus %q: %w", rule.Name, name, err)
			}
		}
	}

	// ── 4. Wire outputs ────────────────────────────────
	e.BusName = busRes.Name
	e.Arn = busRes.Arn

	ctx.RegisterResourceOutputs(e, pulumi.Map{
		"name": busRes.Name,
		"arn":  busRes.Arn,
	})

	return e, nil
}
