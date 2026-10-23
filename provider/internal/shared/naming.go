package provider

import (
	"fmt"
	"regexp"
	"strings"
)

// ── Resource Name Limits ───────────────────────────────────
// Keyed by the resource type label used in the naming pattern.
// If a resource type isn't listed, defaults to 255.

var NameLimits = map[string]int{
	"bucket":          63,  // S3: lowercase, numbers, hyphens only
	"lambda":          64,  // Lambda function name
	"function":        64,  // GCP Cloud Function
	"role":            64,  // IAM Role
	"policy":          128, // IAM Policy
	"queue":           80,  // SQS (FIFO .fifo suffix eats 5 chars)
	"topic":           256, // SNS
	"stream":          128, // Kinesis Data Stream
	"table":           255, // DynamoDB
	"site":            63,  // S3-backed site bucket
	"vpc":             256,
	"subnet":          256,
	"igw":             256,
	"rt":              256,
	"security-group":  256,
	"vpc-endpoint":    256,
	"flow-log":        256,
	"eip":             255, // Elastic IP allocation name tag
	"nat-gw":          255, // NAT Gateway name tag
	"nat-sg":          255, // NAT instance security group
	"nat-instance":    255, // fck-nat EC2 instance name tag
	"bastion":         255,
	"bastion-sg":      255,
	"profile":         128, // IAM instance profile — 128 char limit
	"log-group":       512, // CloudWatch Log Group
	"flow-log-role":   64,  // IAM role for VPC Flow Logs service
	"flow-log-bucket": 63,  // S3 bucket for flow logs (S3 lowercase limit)
}

// ── Character Sanitization ─────────────────────────────────
// S3 is the most restrictive: lowercase, numbers, hyphens only.
// Everything else allows letters, numbers, hyphens, underscores.

var s3SafePattern = regexp.MustCompile(`[^a-z0-9-]`)
var generalPattern = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ── PhysicalName ───────────────────────────────────────────
//
// Builds a consistent physical resource name following the pattern:
//
//	${stage}-${logicalName}-${resourceType}-${stageId}
//
// If the result exceeds the resource's name limit, it is truncated
// from the right to fit. Trailing hyphens/underscores are stripped
// after truncation.
//
// Examples:
//
//	PhysicalName("damienpace", "uploads", "bucket", "3c1234")
//	→ "damienpace-uploads-bucket-3c1234"
//
//	PhysicalName("prod", "api", "lambda", "a1b2c3")
//	→ "prod-api-lambda-a1b2c3"
//
//	PhysicalName("damienpace", "very-long-name-exceeding-limit", "bucket", "3c1234")
//	→ truncated to 63 chars
func PhysicalName(stage, logicalName, resourceType, stageId string) string {
	raw := fmt.Sprintf("%s-%s-%s-%s", stage, logicalName, resourceType, stageId)

	name := strings.ToLower(raw)

	// Sanitize based on resource type
	if resourceType == "bucket" || resourceType == "site" {
		name = s3SafePattern.ReplaceAllString(name, "-")
	} else {
		name = generalPattern.ReplaceAllString(raw, "-")
	}

	// Collapse consecutive hyphens
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}

	// Truncate if over limit
	limit := getNameLimit(resourceType)
	if len(name) > limit {
		name = name[:limit]
	}

	// Clean up trailing hyphens/underscores from truncation
	name = strings.TrimRight(name, "-_")

	return name
}

// ── FifoQueueName ──────────────────────────────────────────
//
// Same as PhysicalName but reserves 5 chars for the ".fifo" suffix.
// The suffix is appended automatically.
//
//	FifoQueueName("dev", "orders", "queue", "3c1234")
//	→ "dev-orders-queue-3c1234.fifo"
func FifoQueueName(stage, logicalName, stageId string) string {
	// Reserve 5 chars for ".fifo"
	raw := fmt.Sprintf("%s-%s-%s-%s", stage, logicalName, "queue", stageId)
	name := generalPattern.ReplaceAllString(raw, "-")

	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}

	fifoLimit := getNameLimit("queue") - 5 // 80 - 5 = 75
	if len(name) > fifoLimit {
		name = name[:fifoLimit]
	}

	name = strings.TrimRight(name, "-_")

	return name + ".fifo"
}

// ── IAMRoleName ────────────────────────────────────────────
//
// Builds a role name for a compute resource's execution role.
// Pattern: ${stage}-${logicalName}-role-${stageId}
// Limit: 64 chars.
//
//	IAMRoleName("damienpace", "api", "3c1234")
//	→ "damienpace-api-role-3c1234"
func IAMRoleName(stage, logicalName, stageId string) string {
	return PhysicalName(stage, logicalName, "role", stageId)
}

// ── IAMPolicyName ──────────────────────────────────────────
//
// Builds a policy name for grant policies.
// Pattern: ${stage}-${logicalName}-${grantSuffix}-policy-${stageId}
// Limit: 128 chars (plenty of room).
//
//	IAMPolicyName("damienpace", "uploads-api-read", "3c1234")
//	→ "damienpace-uploads-api-read-policy-3c1234"
func IAMPolicyName(stage, policyDesc, stageId string) string {
	return PhysicalName(stage, policyDesc, "policy", stageId)
}

// ── getNameLimit ───────────────────────────────────────────

func getNameLimit(resourceType string) int {
	if limit, ok := NameLimits[resourceType]; ok {
		return limit
	}
	return 255
}
