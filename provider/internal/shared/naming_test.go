package provider

import (
	"strings"
	"testing"
)

func TestPhysicalName_Basic(t *testing.T) {
	name := PhysicalName("damienpace", "uploads", "bucket", "3c1234")
	expected := "damienpace-uploads-bucket-3c1234"
	if name != expected {
		t.Errorf("got %q, want %q", name, expected)
	}
}

func TestPhysicalName_Lambda(t *testing.T) {
	name := PhysicalName("damienpace", "api", "lambda", "3c1234")
	expected := "damienpace-api-lambda-3c1234"
	if name != expected {
		t.Errorf("got %q, want %q", name, expected)
	}
}

func TestPhysicalName_BucketTruncation(t *testing.T) {
	// S3 limit is 63
	name := PhysicalName("damienpace", "very-long-logical-name-that-will-exceed", "bucket", "3c1234")
	if len(name) > 63 {
		t.Errorf("bucket name exceeds 63 chars: %d chars → %q", len(name), name)
	}
	if strings.HasSuffix(name, "-") {
		t.Errorf("name should not end with hyphen: %q", name)
	}
}

func TestPhysicalName_LambdaTruncation(t *testing.T) {
	// Lambda limit is 64
	name := PhysicalName("damienpace", "process-very-long-order-handler-name", "lambda", "3c1234")
	if len(name) > 64 {
		t.Errorf("lambda name exceeds 64 chars: %d chars → %q", len(name), name)
	}
	if strings.HasSuffix(name, "-") {
		t.Errorf("name should not end with hyphen: %q", name)
	}
}

func TestPhysicalName_QueueLimit(t *testing.T) {
	// SQS limit is 80
	name := PhysicalName("damienpace", "very-long-queue-name-for-processing-orders", "queue", "3c1234")
	if len(name) > 80 {
		t.Errorf("queue name exceeds 80 chars: %d chars → %q", len(name), name)
	}
}

func TestPhysicalName_BucketLowercase(t *testing.T) {
	name := PhysicalName("DamienPace", "MyUploads", "bucket", "3C1234")
	if name != strings.ToLower(name) {
		t.Errorf("bucket name should be lowercase: %q", name)
	}
}

func TestPhysicalName_SanitizesSpecialChars(t *testing.T) {
	name := PhysicalName("dev", "my.uploads_v2", "bucket", "abc123")
	if strings.Contains(name, ".") {
		t.Errorf("bucket name should not contain dots: %q", name)
	}
	if strings.Contains(name, "_") {
		t.Errorf("bucket name should not contain underscores: %q", name)
	}
}

func TestPhysicalName_LambdaAllowsUnderscores(t *testing.T) {
	name := PhysicalName("dev", "my_handler", "lambda", "abc123")
	expected := "dev-my_handler-lambda-abc123"
	if name != expected {
		t.Errorf("got %q, want %q", name, expected)
	}
}

func TestPhysicalName_NoDoubleHyphens(t *testing.T) {
	name := PhysicalName("dev", "my--uploads", "bucket", "abc123")
	if strings.Contains(name, "--") {
		t.Errorf("name should not contain double hyphens: %q", name)
	}
}

func TestPhysicalName_ShortStage(t *testing.T) {
	name := PhysicalName("dev", "api", "lambda", "a1b2c3")
	expected := "dev-api-lambda-a1b2c3"
	if name != expected {
		t.Errorf("got %q, want %q", name, expected)
	}
}

func TestPhysicalName_ProdStage(t *testing.T) {
	name := PhysicalName("prod", "uploads", "bucket", "f9e8d7")
	expected := "prod-uploads-bucket-f9e8d7"
	if name != expected {
		t.Errorf("got %q, want %q", name, expected)
	}
}

func TestFifoQueueName(t *testing.T) {
	name := FifoQueueName("dev", "orders", "3c1234")
	expected := "dev-orders-queue-3c1234.fifo"
	if name != expected {
		t.Errorf("got %q, want %q", name, expected)
	}
	if len(name) > 80 {
		t.Errorf("fifo queue name exceeds 80 chars: %d chars → %q", len(name), name)
	}
}

func TestFifoQueueName_Truncation(t *testing.T) {
	name := FifoQueueName("damienpace", "very-long-queue-name-for-processing-orders-quickly", "3c1234")
	if len(name) > 80 {
		t.Errorf("fifo queue name exceeds 80 chars: %d chars → %q", len(name), name)
	}
	if !strings.HasSuffix(name, ".fifo") {
		t.Errorf("fifo queue name should end with .fifo: %q", name)
	}
}

func TestIAMRoleName(t *testing.T) {
	name := IAMRoleName("damienpace", "api", "3c1234")
	expected := "damienpace-api-role-3c1234"
	if name != expected {
		t.Errorf("got %q, want %q", name, expected)
	}
}

func TestIAMRoleName_Truncation(t *testing.T) {
	name := IAMRoleName("damienpace", "very-long-lambda-handler-name-for-processing", "3c1234")
	if len(name) > 64 {
		t.Errorf("IAM role name exceeds 64 chars: %d chars → %q", len(name), name)
	}
}

func TestIAMPolicyName(t *testing.T) {
	name := IAMPolicyName("damienpace", "uploads-api-read", "3c1234")
	expected := "damienpace-uploads-api-read-policy-3c1234"
	if name != expected {
		t.Errorf("got %q, want %q", name, expected)
	}
}

// ── Table test for all resource types at limit ─────────────

func TestAllResourceTypes_RespectLimits(t *testing.T) {
	longName := "this-is-a-very-very-long-logical-name-that-should-definitely-exceed-any-reasonable-limit-for-any-aws-resource"

	for resourceType, limit := range NameLimits {
		if resourceType == "queue" {
			// Test standard queue
			name := PhysicalName("damienpace", longName, resourceType, "3c1234")
			if len(name) > limit {
				t.Errorf("%s: name exceeds %d chars: %d chars → %q", resourceType, limit, len(name), name)
			}
			// Test FIFO queue
			fifo := FifoQueueName("damienpace", longName, "3c1234")
			if len(fifo) > limit {
				t.Errorf("fifo %s: name exceeds %d chars: %d chars → %q", resourceType, limit, len(fifo), fifo)
			}
			continue
		}
		name := PhysicalName("damienpace", longName, resourceType, "3c1234")
		if len(name) > limit {
			t.Errorf("%s: name exceeds %d chars: %d chars → %q", resourceType, limit, len(name), name)
		}
		if strings.HasSuffix(name, "-") || strings.HasSuffix(name, "_") {
			t.Errorf("%s: name ends with trailing separator: %q", resourceType, name)
		}
	}
}

// OAC names are unique per AWS account, so two stages of one site must differ.
func TestOACNamesDifferPerStage(t *testing.T) {
	prod := PhysicalName("prod", "web", "oac", "b6381e")
	staging := PhysicalName("staging", "web", "oac", "dd763e")
	if prod == staging {
		t.Fatalf("both stages got %q", prod)
	}
	if prod != "prod-web-oac-b6381e" {
		t.Errorf("prod = %q", prod)
	}
	long := PhysicalName("a-very-long-personal-stage-name", "a-very-long-site-component-name", "lambda-oac", "b6381e")
	if len(long) > 64 {
		t.Errorf("len = %d, want <= 64", len(long))
	}
}
