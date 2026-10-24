package kinesis

import (
	"strings"
	"testing"

	"github.com/anvil-cloud/anvil/provider/internal/hatch"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestResolveConfig(t *testing.T) {
	tests := []struct {
		name    string
		args    KinesisStreamArgs
		want    streamConfig
		wantErr string
	}{
		{"defaults are dsql cdc ready", KinesisStreamArgs{}, streamConfig{modeOnDemand, 0, 24, 2048}, ""},
		{"smaller record size", KinesisStreamArgs{MaxRecordSizeKib: 1024}, streamConfig{modeOnDemand, 0, 24, 1024}, ""},
		{"provisioned with one shard", KinesisStreamArgs{Provisioned: &KinesisProvisionedArgs{ShardCount: 1}}, streamConfig{modeProvisioned, 1, 24, 2048}, ""},
		{"provisioned without a shard count", KinesisStreamArgs{Provisioned: &KinesisProvisionedArgs{}}, streamConfig{}, "provisioned.shardCount must be at least 1, got 0"},
		{"provisioned with shards", KinesisStreamArgs{Provisioned: &KinesisProvisionedArgs{ShardCount: 4}, RetentionHours: 168}, streamConfig{modeProvisioned, 4, 168, 2048}, ""},
		{"negative shards", KinesisStreamArgs{Provisioned: &KinesisProvisionedArgs{ShardCount: -1}}, streamConfig{}, "provisioned.shardCount must be at least 1"},
		{"retention too short", KinesisStreamArgs{RetentionHours: 12}, streamConfig{}, "retentionHours must be between 24 and 8760"},
		{"retention too long", KinesisStreamArgs{RetentionHours: 9000}, streamConfig{}, "retentionHours must be between 24 and 8760"},
		{"record size too small", KinesisStreamArgs{MaxRecordSizeKib: 512}, streamConfig{}, "maxRecordSizeKib must be between 1024 and 10240"},
		{"record size too large", KinesisStreamArgs{MaxRecordSizeKib: 20480}, streamConfig{}, "maxRecordSizeKib must be between 1024 and 10240"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveConfig("events", tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("config = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestStreamPropsSecureDefaults(t *testing.T) {
	props := streamProps("dev-events-stream-abc", streamConfig{modeOnDemand, 0, 24, 2048})

	if props["encryptionType"] != pulumi.String("KMS") || props["kmsKeyId"] != pulumi.String("alias/aws/kinesis") {
		t.Errorf("encryption = %v/%v, want KMS with alias/aws/kinesis", props["encryptionType"], props["kmsKeyId"])
	}
	mode := props["streamModeDetails"].(pulumi.Map)["streamMode"]
	if mode != pulumi.String("ON_DEMAND") {
		t.Errorf("streamMode = %v, want ON_DEMAND", mode)
	}
	if _, ok := props["shardCount"]; ok {
		t.Error("on-demand stream must not set shardCount")
	}
}

func TestStreamPropsProvisioned(t *testing.T) {
	props := streamProps("dev-events-stream-abc", streamConfig{modeProvisioned, 3, 48, 2048})

	if mode := props["streamModeDetails"].(pulumi.Map)["streamMode"]; mode != pulumi.String("PROVISIONED") {
		t.Errorf("streamMode = %v, want PROVISIONED", mode)
	}
	if props["shardCount"] != pulumi.Int(3) || props["retentionPeriod"] != pulumi.Int(48) || props["maxRecordSizeInKib"] != pulumi.Int(2048) {
		t.Errorf("shardCount/retention/maxRecordSize = %v/%v/%v", props["shardCount"], props["retentionPeriod"], props["maxRecordSizeInKib"])
	}
}

func TestStreamPropsHatchCustomerManagedKey(t *testing.T) {
	props := streamProps("dev-events-stream-abc", streamConfig{modeOnDemand, 0, 24, 1024})
	merged := hatch.Merge(map[string]interface{}{
		"kmsKeyId": "arn:aws:kms:us-east-1:123456789012:key/abc",
	}, props)

	// hatch.Merge wraps overrides in pulumi.Any, so check the default is gone.
	if merged["kmsKeyId"] == pulumi.String("alias/aws/kinesis") {
		t.Error("kmsKeyId still the AWS-managed key, want the hatch override")
	}
	if merged["encryptionType"] != pulumi.String("KMS") {
		t.Errorf("encryptionType = %v, want KMS kept", merged["encryptionType"])
	}
}

func TestResolveConsumers(t *testing.T) {
	fn := &KinesisLambdaConsumerArgs{Arn: pulumi.String("arn:fn"), LambdaRoleArn: pulumi.String("arn:role")}

	disabled := false
	resolved, warnings, err := resolveConsumers("events", []KinesisConsumerArgs{
		{ConsumerName: "indexer", Lambda: fn, GrantRead: true},
		{ConsumerName: "audit-log", Lambda: fn, GrantRead: true, BatchSize: 500, Enabled: &disabled, StartingPosition: "LATEST"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 || resolved[0].batchSize != 100 || resolved[1].batchSize != 500 {
		t.Errorf("resolved = %+v, want batch sizes 100 and 500", resolved)
	}
	if !resolved[0].enabled || resolved[1].enabled {
		t.Errorf("enabled = %v/%v, want default true and explicit false", resolved[0].enabled, resolved[1].enabled)
	}
	if resolved[0].startingPosition != "TRIM_HORIZON" || resolved[1].startingPosition != "LATEST" {
		t.Errorf("startingPosition = %q/%q, want default TRIM_HORIZON and explicit LATEST", resolved[0].startingPosition, resolved[1].startingPosition)
	}
	if resolved[0].filterPatterns != nil {
		t.Errorf("no filter must deliver every record, got patterns %v", resolved[0].filterPatterns)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings %v", warnings)
	}

	tests := []struct {
		name      string
		consumers []KinesisConsumerArgs
		wantErr   string
	}{
		{"missing name", []KinesisConsumerArgs{{Lambda: fn, GrantRead: true}}, "consumers[0].consumerName is required"},
		{"name with spaces", []KinesisConsumerArgs{{ConsumerName: "my indexer", Lambda: fn, GrantRead: true}}, "must start with a letter"},
		{"name starting with a digit", []KinesisConsumerArgs{{ConsumerName: "1indexer", Lambda: fn, GrantRead: true}}, "must start with a letter"},
		{"name too long", []KinesisConsumerArgs{{ConsumerName: strings.Repeat("a", 33), Lambda: fn, GrantRead: true}}, "at most 32 characters"},
		{"duplicate name", []KinesisConsumerArgs{{ConsumerName: "indexer", Lambda: fn, GrantRead: true}, {ConsumerName: "indexer", Lambda: fn, GrantRead: true}}, `consumers[1].consumerName "indexer" is already used by consumers[0]`},
		{"missing lambda", []KinesisConsumerArgs{{ConsumerName: "indexer", GrantRead: true}}, `consumer "indexer": lambda is required`},
		{"batch size too large", []KinesisConsumerArgs{{ConsumerName: "indexer", Lambda: fn, GrantRead: true, BatchSize: 20000}}, `consumer "indexer": batchSize must be between 1 and 10000`},
		{"unknown starting position", []KinesisConsumerArgs{{ConsumerName: "indexer", Lambda: fn, GrantRead: true, StartingPosition: "latest"}}, `consumer "indexer": startingPosition must be "TRIM_HORIZON" or "LATEST", got "latest"`},
		{"AT_TIMESTAMP not supported", []KinesisConsumerArgs{{ConsumerName: "indexer", Lambda: fn, GrantRead: true, StartingPosition: "AT_TIMESTAMP"}}, `startingPosition must be "TRIM_HORIZON" or "LATEST"`},
		{"bad filter", []KinesisConsumerArgs{{ConsumerName: "indexer", Lambda: fn, GrantRead: true, Filter: &KinesisConsumerFilterArgs{}}}, `consumer "indexer": filter must set tables or patterns`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := resolveConsumers("events", tt.consumers); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveConsumersGrantReadFalseWarnsWithoutFailing(t *testing.T) {
	fn := &KinesisLambdaConsumerArgs{Arn: pulumi.String("arn:fn"), LambdaRoleArn: pulumi.String("arn:role")}

	resolved, warnings, err := resolveConsumers("events", []KinesisConsumerArgs{
		{ConsumerName: "indexer", Lambda: fn, GrantRead: true},
		{ConsumerName: "audit", Lambda: fn},
	})
	if err != nil {
		t.Fatalf("grantRead: false must not fail the deploy: %v", err)
	}
	if len(resolved) != 2 {
		t.Errorf("resolved = %+v, want both consumers wired", resolved)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `consumer "audit" has grantRead: false`) {
		t.Errorf("warnings = %v, want one for audit", warnings)
	}
}

func TestConsumerPolicy(t *testing.T) {
	policy, err := consumerPolicy("arn:aws:kinesis:us-east-1:123:stream/s", "arn:aws:sqs:us-east-1:123:dlq", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range append(append([]string{}, readActions...), "sqs:SendMessage", "stream/s", ":dlq") {
		if !strings.Contains(policy, want) {
			t.Errorf("policy missing %q: %s", want, policy)
		}
	}
	if strings.Contains(policy, "*") {
		t.Errorf("policy must not use wildcards: %s", policy)
	}
}

func TestConsumerPolicyWithoutGrantRead(t *testing.T) {
	policy, err := consumerPolicy("arn:aws:kinesis:us-east-1:123:stream/s", "arn:aws:sqs:us-east-1:123:dlq", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(policy, "kinesis:") || strings.Contains(policy, "stream/s") {
		t.Errorf("policy must not grant stream access: %s", policy)
	}
	if !strings.Contains(policy, "sqs:SendMessage") {
		t.Errorf("policy must still allow sending to the DLQ: %s", policy)
	}
}

func TestRoleNameFromArn(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:iam::123:role/dev-api-role":              "dev-api-role",
		"arn:aws:iam::123:role/service-role/dev-api-role": "dev-api-role",
	} {
		if got := roleNameFromArn(arn); got != want {
			t.Errorf("roleNameFromArn(%q) = %q, want %q", arn, got, want)
		}
	}
}

func TestFilterPatterns(t *testing.T) {
	tests := []struct {
		name    string
		filter  *KinesisConsumerFilterArgs
		want    []string
		wantErr string
	}{
		{"no filter", nil, nil, ""},
		{"unqualified tables share one pattern", &KinesisConsumerFilterArgs{Tables: []string{"orders", "customers"}},
			[]string{`{"data":{"source":{"table":["orders","customers"]}}}`}, ""},
		{"qualified table matches its schema", &KinesisConsumerFilterArgs{Tables: []string{"app.orders"}},
			[]string{`{"data":{"source":{"schema":["app"],"table":["orders"]}}}`}, ""},
		{"mixed tables and raw patterns", &KinesisConsumerFilterArgs{Tables: []string{"app.orders", "audit"}, Patterns: []string{`{"data":{"op":["d"]}}`}},
			[]string{`{"data":{"source":{"schema":["app"],"table":["orders"]}}}`, `{"data":{"source":{"table":["audit"]}}}`, `{"data":{"op":["d"]}}`}, ""},
		{"empty filter", &KinesisConsumerFilterArgs{}, nil, "filter must set tables or patterns"},
		{"bad table", &KinesisConsumerFilterArgs{Tables: []string{"a.b.c"}}, nil, `"a.b.c" must be "table" or "schema.table"`},
		{"empty schema", &KinesisConsumerFilterArgs{Tables: []string{".orders"}}, nil, `must be "table" or "schema.table"`},
		{"pattern not json", &KinesisConsumerFilterArgs{Patterns: []string{"orders"}}, nil, "is not a JSON object"},
		{"too many patterns", &KinesisConsumerFilterArgs{Tables: []string{"a.t1", "a.t2", "a.t3", "a.t4", "a.t5", "a.t6"}}, nil, "filter produces 6 patterns, Lambda allows 5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := filterPatterns(tt.filter)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("patterns = %v, want %v", got, tt.want)
			}
		})
	}
}
