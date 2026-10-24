package dsql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const testClusterArn = "arn:aws:dsql:ap-southeast-2:123456789012:cluster/abc123"

func TestValidateStream(t *testing.T) {
	if err := validateStream(nil); err != nil {
		t.Errorf("no stream: %v", err)
	}
	if err := validateStream(&DSQLStreamArgs{KinesisArn: pulumi.String("arn:k")}); err != nil {
		t.Errorf("kinesis only: %v", err)
	}
	if err := validateStream(&DSQLStreamArgs{}); err == nil || !strings.Contains(err.Error(), "kinesisArn is required") {
		t.Errorf("missing kinesisArn: %v", err)
	}
	both := &DSQLStreamArgs{KinesisArn: pulumi.String("arn:k"), RoleArn: pulumi.String("arn:r"), KmsKeyArn: pulumi.String("arn:kms")}
	if err := validateStream(both); err == nil || !strings.Contains(err.Error(), "kmsKeyArn only applies") {
		t.Errorf("roleArn + kmsKeyArn: %v", err)
	}
}

func TestStreamTrustPolicyIsScopedToTheCluster(t *testing.T) {
	policy, err := streamTrustPolicy(testClusterArn)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Statement []struct {
			Principal map[string]string
			Action    string
			Condition struct {
				StringEquals map[string]string
				ArnLike      map[string][]string
			}
		}
	}
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatal(err)
	}
	st := doc.Statement[0]
	if st.Principal["Service"] != "dsql.amazonaws.com" || st.Action != "sts:AssumeRole" {
		t.Errorf("principal/action = %v %v", st.Principal, st.Action)
	}
	// DSQL presents the stream's ARN (<cluster>/stream/<id>) as SourceArn.
	want := []string{testClusterArn, testClusterArn + "/stream/*"}
	if got := st.Condition.ArnLike["aws:SourceArn"]; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("SourceArn = %v, want %v", got, want)
	}
	if st.Condition.StringEquals["aws:SourceAccount"] != "123456789012" {
		t.Errorf("SourceAccount = %q", st.Condition.StringEquals["aws:SourceAccount"])
	}
	// The only wildcard is the stream id under this one cluster.
	if strings.Contains(policy, "cluster/*") || strings.Count(policy, "*") != 1 {
		t.Errorf("trust policy must be scoped to this cluster: %s", policy)
	}

	if _, err := streamTrustPolicy("not-an-arn"); err == nil {
		t.Error("bad ARN accepted")
	}
}

func TestStreamRolePolicy(t *testing.T) {
	kinesis := "arn:aws:kinesis:ap-southeast-2:123456789012:stream/events"

	plain, err := streamRolePolicy(kinesis, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kinesis:PutRecord", "kinesis:PutRecords", "kinesis:DescribeStreamSummary", "kinesis:ListShards", kinesis} {
		if !strings.Contains(plain, want) {
			t.Errorf("policy missing %q: %s", want, plain)
		}
	}
	if strings.Contains(plain, "kms:") || strings.Contains(plain, "*") {
		t.Errorf("AWS-managed key needs no KMS grant and no wildcards: %s", plain)
	}

	withKey, err := streamRolePolicy(kinesis, "arn:aws:kms:ap-southeast-2:123456789012:key/k1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withKey, "kms:GenerateDataKey") || !strings.Contains(withKey, "key/k1") {
		t.Errorf("customer key not granted: %s", withKey)
	}
}
