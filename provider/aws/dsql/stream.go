package dsql

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	awsiam "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// StreamManifestPath is where build mode records which DSQL components want a
// CDC stream, so `anvil deploy` can delete a stream before Pulumi removes its
// role or cluster. It's separate from the build manifest, which other writers
// rewrite from their own structs. Must match cmd/anvil/cmd/dsql_stream.go.
const StreamManifestPath = ".anvil/dsql-streams.json"

type streamManifest struct {
	Components []streamManifestEntry `json:"components"`
}

type streamManifestEntry struct {
	Name   string `json:"name"`
	Stream bool   `json:"stream"`
}

func validateStream(s *DSQLStreamArgs) error {
	if s == nil {
		return nil
	}
	if s.KinesisArn == nil {
		return errors.New("stream.kinesisArn is required")
	}
	if s.RoleArn != nil && s.KmsKeyArn != nil {
		return errors.New("stream.kmsKeyArn only applies to the role Anvil creates; with stream.roleArn, grant kms:GenerateDataKey on your role instead")
	}
	return nil
}

// streamTrustPolicy lets DSQL assume the role, but only on behalf of this
// cluster in this account (AWS's examples use cluster/*, which lets any
// cluster in the account write with it). DSQL presents the CDC stream's ARN,
// <cluster ARN>/stream/<id>, as aws:SourceArn, not the cluster's, so the
// condition matches the cluster and its streams.
func streamTrustPolicy(clusterArn string) (string, error) {
	parts := strings.Split(clusterArn, ":")
	if len(parts) < 6 || parts[4] == "" {
		return "", fmt.Errorf("unexpected DSQL cluster ARN %q", clusterArn)
	}
	doc := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]string{"Service": "dsql.amazonaws.com"},
			"Action":    "sts:AssumeRole",
			"Condition": map[string]any{
				"StringEquals": map[string]string{"aws:SourceAccount": parts[4]},
				"ArnLike":      map[string][]string{"aws:SourceArn": {clusterArn, clusterArn + "/stream/*"}},
			},
		}},
	}
	b, err := json.Marshal(doc)
	return string(b), err
}

// streamRolePolicy lets the role write to one Kinesis stream, and use its
// customer-managed key when there is one.
func streamRolePolicy(kinesisArn, kmsKeyArn string) (string, error) {
	statements := []map[string]any{{
		"Effect":   "Allow",
		"Action":   []string{"kinesis:PutRecord", "kinesis:PutRecords", "kinesis:DescribeStreamSummary", "kinesis:ListShards"},
		"Resource": kinesisArn,
	}}
	if kmsKeyArn != "" {
		statements = append(statements, map[string]any{
			"Effect":   "Allow",
			"Action":   []string{"kms:GenerateDataKey"},
			"Resource": kmsKeyArn,
		})
	}
	b, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": statements})
	return string(b), err
}

// newStreamRole creates the role DSQL assumes to write CDC events to Kinesis
// and returns its ARN.
func newStreamRole(ctx *pulumi.Context, d *DSQL, name, stage, stageId string, clusterArn pulumi.StringOutput, s *DSQLStreamArgs) (pulumi.StringOutput, error) {
	trust := clusterArn.ApplyT(streamTrustPolicy).(pulumi.StringOutput)

	role := &awsiam.Role{}
	if err := ctx.RegisterResource("aws:iam/role:Role",
		fmt.Sprintf("%s-cdc-role", name),
		pulumi.Map{
			"name":             pulumi.String(provider.IAMRoleName(stage, name+"-cdc", stageId)),
			"assumeRolePolicy": trust,
			"tags": pulumi.StringMap{
				"ManagedBy": pulumi.String("anvil"),
			},
		},
		role,
		pulumi.Parent(d),
	); err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("creating CDC stream IAM role: %w", err)
	}

	kms := pulumi.String("").ToStringOutput()
	if s.KmsKeyArn != nil {
		kms = s.KmsKeyArn.ToStringOutput()
	}
	policy := pulumi.All(s.KinesisArn.ToStringOutput(), kms).ApplyT(func(v []any) (string, error) {
		return streamRolePolicy(v[0].(string), v[1].(string))
	}).(pulumi.StringOutput)

	rolePolicy := &awsiam.RolePolicy{}
	if err := ctx.RegisterResource("aws:iam/rolePolicy:RolePolicy",
		fmt.Sprintf("%s-cdc-kinesis", name),
		pulumi.Map{
			"role":   role.Name,
			"policy": policy,
		},
		rolePolicy,
		pulumi.Parent(d),
	); err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("attaching CDC stream Kinesis policy: %w", err)
	}

	// Hand out the ARN only once the policy is attached, so DSQL never gets a
	// role that can't write yet.
	return pulumi.All(role.Arn, rolePolicy.ID()).ApplyT(func(v []any) string {
		return v[0].(string)
	}).(pulumi.StringOutput), nil
}

// appendStreamToManifest records, during build mode, whether this component
// wants a CDC stream.
func appendStreamToManifest(name string, wanted bool) error {
	provider.ManifestMu.Lock()
	defer provider.ManifestMu.Unlock()

	if err := os.MkdirAll(".anvil", 0755); err != nil {
		return err
	}

	m := streamManifest{}
	if data, err := os.ReadFile(StreamManifestPath); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	m.Components = append(m.Components, streamManifestEntry{Name: name, Stream: wanted})

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(StreamManifestPath, data, 0644)
}
