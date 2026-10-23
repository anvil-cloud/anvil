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
		{"empty provisioned defaults to one shard", KinesisStreamArgs{Provisioned: &KinesisProvisionedArgs{}}, streamConfig{modeProvisioned, 1, 24, 2048}, ""},
		{"provisioned with shards", KinesisStreamArgs{Provisioned: &KinesisProvisionedArgs{ShardCount: 4}, RetentionHours: 168}, streamConfig{modeProvisioned, 4, 168, 2048}, ""},
		{"negative shards", KinesisStreamArgs{Provisioned: &KinesisProvisionedArgs{ShardCount: -1}}, streamConfig{}, "provisioned.shardCount must be positive"},
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
