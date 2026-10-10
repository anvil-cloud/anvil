package vpc

import (
	"fmt"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/s3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// S3FlowLogLifecycle controls the storage tiering policy for flow log S3 delivery.
type S3FlowLogLifecycle string

const (
	// S3FlowLogLifecycleStandard auto-tiers: Standard (0-30d) → Standard-IA (30-90d) → Glacier Instant Retrieval (90d+).
	S3FlowLogLifecycleStandard S3FlowLogLifecycle = "standard"
)

// CloudWatchFlowLogArgs configures the CloudWatch destination for VPC Flow Logs.
type CloudWatchFlowLogArgs struct {
	// Retention is the number of days to retain flow log data in CloudWatch.
	// Common values: 7, 14, 30, 90. Required.
	Retention int `pulumi:"retention"`
}

// S3FlowLogArgs configures the S3 destination for VPC Flow Logs.
type S3FlowLogArgs struct {
	// Lifecycle controls the storage tiering policy.
	// "standard" — auto-tiered: Standard (0-30d) → Standard-IA (30-90d) → Glacier Instant Retrieval (90d+).
	Lifecycle S3FlowLogLifecycle `pulumi:"lifecycle"`
}

// FlowLogsArgs configures VPC Flow Log destinations.
// Either or both destinations can be enabled simultaneously.
// CloudWatch for active debugging, S3 for long-term compliance retention.
type FlowLogsArgs struct {
	// CloudWatch enables flow log delivery to a CloudWatch Log Group.
	// Use for fast querying with CloudWatch Logs Insights and active debugging.
	CloudWatch *CloudWatchFlowLogArgs `pulumi:"cloudwatch,optional"`

	// S3 enables flow log delivery to a dedicated S3 bucket with lifecycle tiering.
	// Use for compliance retention and audit evidence at minimal long-term cost.
	S3 *S3FlowLogArgs `pulumi:"s3,optional"`
}

// createFlowLogs provisions VPC Flow Log resources for the specified destinations.
// CloudWatch destination: Log Group + IAM role + FlowLog resource.
// S3 destination: dedicated bucket + encryption + public access block + lifecycle + bucket policy + FlowLog resource.
// Both can be active simultaneously.
func createFlowLogs(
	ctx *pulumi.Context,
	name, stage, stageId string,
	args *FlowLogsArgs,
	vpcResource *ec2.Vpc,
	parent pulumi.Resource,
) error {
	// ── CloudWatch destination ────────────────────────────────────────────────
	if args.CloudWatch != nil {
		logGroupName := "/" + provider.PhysicalName(stage, name, "flow-logs", stageId)

		logGroup := &cloudwatch.LogGroup{}
		if err := ctx.RegisterResource("aws:cloudwatch/logGroup:LogGroup", name+"-flow-log-group", pulumi.Map{
			"name":            pulumi.String(logGroupName),
			"retentionInDays": pulumi.Int(args.CloudWatch.Retention),
			"tags": pulumi.StringMap{
				"Name":      pulumi.String(logGroupName),
				"ManagedBy": pulumi.String("anvil"),
			},
		}, logGroup, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to create flow log CloudWatch log group: %w", err)
		}

		// IAM role — least privilege for the VPC Flow Logs service principal.
		// Only the actions required to write logs to CloudWatch.
		assumeRolePolicy := `{
			"Version": "2012-10-17",
			"Statement": [{
				"Effect": "Allow",
				"Principal": { "Service": "vpc-flow-logs.amazonaws.com" },
				"Action": "sts:AssumeRole"
			}]
		}`

		cwRole := &iam.Role{}
		if err := ctx.RegisterResource("aws:iam/role:Role", name+"-flow-log-role", pulumi.Map{
			"name":             pulumi.String(provider.PhysicalName(stage, name, "flow-log-role", stageId)),
			"assumeRolePolicy": pulumi.String(assumeRolePolicy),
			"tags": pulumi.StringMap{
				"ManagedBy": pulumi.String("anvil"),
			},
		}, cwRole, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to create flow log IAM role: %w", err)
		}

		// Inline policy scoped to the specific log group ARN.
		// DescribeLogGroups/Streams required for idempotent delivery.
		cwPolicy := logGroup.Arn.ApplyT(func(arn string) string {
			return fmt.Sprintf(`{
				"Version": "2012-10-17",
				"Statement": [{
					"Effect": "Allow",
					"Action": [
						"logs:CreateLogGroup",
						"logs:CreateLogStream",
						"logs:PutLogEvents",
						"logs:DescribeLogGroups",
						"logs:DescribeLogStreams"
					],
					"Resource": ["%s", "%s:*"]
				}]
			}`, arn, arn)
		}).(pulumi.StringOutput)

		if err := ctx.RegisterResource("aws:iam/rolePolicy:RolePolicy", name+"-flow-log-role-policy", pulumi.Map{
			"role":   cwRole.Name,
			"policy": cwPolicy,
		}, &iam.RolePolicy{}, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to attach flow log IAM policy: %w", err)
		}

		if err := ctx.RegisterResource("aws:ec2/flowLog:FlowLog", name+"-flow-log-cw", pulumi.Map{
			"vpcId":              vpcResource.ID(),
			"trafficType":        pulumi.String("ALL"),
			"iamRoleArn":         cwRole.Arn,
			"logDestination":     logGroup.Arn,
			"logDestinationType": pulumi.String("cloud-watch-logs"),
			"tags": pulumi.StringMap{
				"Name":      pulumi.String(provider.PhysicalName(stage, name, "flow-log", stageId) + "-cw"),
				"ManagedBy": pulumi.String("anvil"),
			},
		}, &ec2.FlowLog{}, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to create CloudWatch flow log: %w", err)
		}
	}

	// ── S3 destination ────────────────────────────────────────────────────────
	if args.S3 != nil {
		flowLogBucket, err := s3.NewBucketV2(ctx, name+"-flow-log-bucket", &s3.BucketV2Args{
			ForceDestroy: pulumi.Bool(false),
			Tags: pulumi.StringMap{
				"Name":      pulumi.String(provider.PhysicalName(stage, name, "flow-log-bucket", stageId)),
				"ManagedBy": pulumi.String("anvil"),
			},
		}, pulumi.Parent(parent))
		if err != nil {
			return fmt.Errorf("failed to create flow log S3 bucket: %w", err)
		}

		// Server-side encryption — AES256 is free and sufficient for flow logs.
		if _, err := s3.NewBucketServerSideEncryptionConfiguration(ctx, name+"-flow-log-bucket-sse", &s3.BucketServerSideEncryptionConfigurationArgs{
			Bucket: flowLogBucket.Bucket,
			Rules: s3.BucketServerSideEncryptionConfigurationRuleArray{
				&s3.BucketServerSideEncryptionConfigurationRuleArgs{
					ApplyServerSideEncryptionByDefault: &s3.BucketServerSideEncryptionConfigurationRuleApplyServerSideEncryptionByDefaultArgs{
						SseAlgorithm: pulumi.String("AES256"),
					},
				},
			},
		}, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to configure flow log bucket encryption: %w", err)
		}

		// Block all public access — flow logs must never be public.
		if _, err := s3.NewBucketPublicAccessBlock(ctx, name+"-flow-log-bucket-pab", &s3.BucketPublicAccessBlockArgs{
			Bucket:                flowLogBucket.Bucket,
			BlockPublicAcls:       pulumi.Bool(true),
			BlockPublicPolicy:     pulumi.Bool(true),
			IgnorePublicAcls:      pulumi.Bool(true),
			RestrictPublicBuckets: pulumi.Bool(true),
		}, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to configure flow log bucket public access block: %w", err)
		}

		// Lifecycle: Standard (0-30d) → Standard-IA (30-90d) → Glacier Instant Retrieval (90d+).
		// Only "standard" tier is supported today. The shape is forward-compatible
		// with LifecyclePolicy.resolve() when that utility is built.
		if args.S3.Lifecycle == S3FlowLogLifecycleStandard {
			if _, err := s3.NewBucketLifecycleConfiguration(ctx, name+"-flow-log-bucket-lifecycle", &s3.BucketLifecycleConfigurationArgs{
				Bucket: flowLogBucket.Bucket,
				Rules: s3.BucketLifecycleConfigurationRuleArray{
					&s3.BucketLifecycleConfigurationRuleArgs{
						Id:     pulumi.String("flow-log-tiering"),
						Status: pulumi.String("Enabled"),
						Transitions: s3.BucketLifecycleConfigurationRuleTransitionArray{
							&s3.BucketLifecycleConfigurationRuleTransitionArgs{
								Days:         pulumi.Int(30),
								StorageClass: pulumi.String("STANDARD_IA"),
							},
							&s3.BucketLifecycleConfigurationRuleTransitionArgs{
								Days:         pulumi.Int(90),
								StorageClass: pulumi.String("GLACIER_IR"),
							},
						},
					},
				},
			}, pulumi.Parent(parent)); err != nil {
				return fmt.Errorf("failed to configure flow log bucket lifecycle: %w", err)
			}
		}

		// Bucket policy — allows the VPC Flow Logs service to deliver logs.
		// delivery.logs.amazonaws.com is the service principal for VPC flow logs to S3.
		bucketPolicy := pulumi.All(flowLogBucket.Arn, flowLogBucket.Bucket).ApplyT(func(vals []interface{}) string {
			bucketArn := vals[0].(string)
			return fmt.Sprintf(`{
				"Version": "2012-10-17",
				"Statement": [{
					"Sid": "AWSLogDeliveryWrite",
					"Effect": "Allow",
					"Principal": { "Service": "delivery.logs.amazonaws.com" },
					"Action": "s3:PutObject",
					"Resource": "%s/AWSLogs/*",
					"Condition": {
						"StringEquals": { "s3:x-amz-acl": "bucket-owner-full-control" }
					}
				}, {
					"Sid": "AWSLogDeliveryAclCheck",
					"Effect": "Allow",
					"Principal": { "Service": "delivery.logs.amazonaws.com" },
					"Action": "s3:GetBucketAcl",
					"Resource": "%s"
				}]
			}`, bucketArn, bucketArn)
		}).(pulumi.StringOutput)

		if _, err := s3.NewBucketPolicy(ctx, name+"-flow-log-bucket-policy", &s3.BucketPolicyArgs{
			Bucket: flowLogBucket.Bucket,
			Policy: bucketPolicy,
		}, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to attach flow log bucket policy: %w", err)
		}

		if err := ctx.RegisterResource("aws:ec2/flowLog:FlowLog", name+"-flow-log-s3", pulumi.Map{
			"vpcId":              vpcResource.ID(),
			"trafficType":        pulumi.String("ALL"),
			"logDestination":     flowLogBucket.Arn,
			"logDestinationType": pulumi.String("s3"),
			"tags": pulumi.StringMap{
				"Name":      pulumi.String(provider.PhysicalName(stage, name, "flow-log", stageId) + "-s3"),
				"ManagedBy": pulumi.String("anvil"),
			},
		}, &ec2.FlowLog{}, pulumi.Parent(parent)); err != nil {
			return fmt.Errorf("failed to create S3 flow log: %w", err)
		}
	}

	return nil
}
