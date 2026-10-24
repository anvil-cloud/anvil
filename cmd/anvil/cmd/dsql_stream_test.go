package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dsql"
	dsqltypes "github.com/aws/aws-sdk-go-v2/service/dsql/types"
)

const (
	testCluster = "arn:aws:dsql:ap-southeast-2:123456789012:cluster/abc123"
	testKinesis = "arn:aws:kinesis:ap-southeast-2:123456789012:stream/events"
	testRole    = "arn:aws:iam::123456789012:role/dev-db-cdc-role"
)

func existingStream(kinesisArn, roleArn string, status dsqltypes.StreamStatus) *dsql.GetStreamOutput {
	return &dsql.GetStreamOutput{
		StreamIdentifier: aws.String("s1"),
		Status:           status,
		TargetDefinition: &dsqltypes.TargetDefinitionMemberKinesis{Value: dsqltypes.KinesisTargetDefinition{
			StreamArn: aws.String(kinesisArn),
			RoleArn:   aws.String(roleArn),
		}},
	}
}

func TestPlanStream(t *testing.T) {
	active := existingStream(testKinesis, testRole, dsqltypes.StreamStatusActive)
	tests := []struct {
		name     string
		kinesis  string
		role     string
		existing []*dsql.GetStreamOutput
		want     streamAction
	}{
		{"no stream wanted or present", "", "", nil, streamNone},
		{"stream removed", "", "", []*dsql.GetStreamOutput{active}, streamDelete},
		{"new stream", testKinesis, testRole, nil, streamCreate},
		{"unchanged", testKinesis, testRole, []*dsql.GetStreamOutput{active}, streamKeep},
		{"impaired is kept and reported", testKinesis, testRole, []*dsql.GetStreamOutput{existingStream(testKinesis, testRole, dsqltypes.StreamStatusImpaired)}, streamKeep},
		{"new Kinesis target", testKinesis + "-2", testRole, []*dsql.GetStreamOutput{active}, streamReplace},
		{"new role", testKinesis, testRole + "-2", []*dsql.GetStreamOutput{active}, streamReplace},
		{"failed is replaced", testKinesis, testRole, []*dsql.GetStreamOutput{existingStream(testKinesis, testRole, dsqltypes.StreamStatusFailed)}, streamReplace},
		{"duplicates are replaced by one", testKinesis, testRole, []*dsql.GetStreamOutput{active, active}, streamReplace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planStream(tt.kinesis, tt.role, tt.existing); got != tt.want {
				t.Errorf("planStream = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestComponentsLosingStream(t *testing.T) {
	deployed := []dsqlStreamComponent{
		{Name: "orders", ClusterArn: testCluster, KinesisArn: testKinesis, RoleArn: testRole}, // still wanted
		{Name: "audit", ClusterArn: testCluster, KinesisArn: testKinesis, RoleArn: testRole},  // stream removed
		{Name: "gone", ClusterArn: testCluster, KinesisArn: testKinesis, RoleArn: testRole},   // component removed
		{Name: "plain", ClusterArn: testCluster},                                              // never had one
	}
	got := componentsLosingStream(deployed, map[string]bool{"orders": true, "audit": false, "plain": false})
	if len(got) != 2 || got[0].Name != "audit" || got[1].Name != "gone" {
		t.Fatalf("got %+v, want audit and gone", got)
	}
	for _, c := range got {
		if c.KinesisArn != "" {
			t.Errorf("%s: KinesisArn = %q, want it cleared so reconcile deletes", c.Name, c.KinesisArn)
		}
	}
}

func TestDSQLComponentsFromDeployment(t *testing.T) {
	deployment := []byte(`{"resources": [
		{"urn": "urn:pulumi:dev::app::pulumi:pulumi:Stack::app-dev", "type": "pulumi:pulumi:Stack"},
		{"urn": "urn:pulumi:dev::app::anvil:aws:DSQL::orders", "type": "anvil:aws:DSQL", "outputs": {
			"clusterArns": {"ap-southeast-2": "` + testCluster + `"},
			"streamKinesisArn": "` + testKinesis + `", "streamRoleArn": "` + testRole + `"}},
		{"urn": "urn:pulumi:dev::app::anvil:aws:DSQL::global", "type": "anvil:aws:DSQL", "outputs": {
			"clusterArns": {"us-east-1": "arn:aws:dsql:us-east-1:1:cluster/us", "ap-southeast-2": "arn:aws:dsql:ap-southeast-2:1:cluster/au"},
			"streamKinesisArn": "", "streamRoleArn": ""}},
		{"urn": "urn:pulumi:dev::app::anvil:aws:DSQL::older", "type": "anvil:aws:DSQL", "outputs": {
			"clusterArns": {"us-west-2": "arn:aws:dsql:us-west-2:1:cluster/only"}}}
	]}`)

	got, err := dsqlComponentsFromDeployment(deployment, "ap-southeast-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d components, want 3: %+v", len(got), got)
	}
	byName := map[string]dsqlStreamComponent{}
	for _, c := range got {
		byName[c.Name] = c
	}
	if c := byName["orders"]; c.ClusterArn != testCluster || c.KinesisArn != testKinesis || c.RoleArn != testRole {
		t.Errorf("orders = %+v", c)
	}
	if c := byName["global"]; c.ClusterArn != "arn:aws:dsql:ap-southeast-2:1:cluster/au" || c.KinesisArn != "" {
		t.Errorf("global must use the stage region's cluster and have no stream: %+v", c)
	}
	if c := byName["older"]; c.ClusterArn != "arn:aws:dsql:us-west-2:1:cluster/only" || c.KinesisArn != "" {
		t.Errorf("older (state from before streams existed) = %+v", c)
	}
}

func TestArnHelpers(t *testing.T) {
	if got := clusterIDFromArn(testCluster); got != "abc123" {
		t.Errorf("clusterIDFromArn = %q", got)
	}
	if got := arnPart(testKinesis, 3); got != "ap-southeast-2" {
		t.Errorf("region = %q", got)
	}
	if got := arnPart(testKinesis, 2); got != "kinesis" {
		t.Errorf("service = %q", got)
	}
}

func TestReadStreamManifest(t *testing.T) {
	t.Chdir(t.TempDir())

	if _, ok, err := readStreamManifest(); ok || err != nil {
		t.Errorf("missing manifest: ok=%v err=%v, want not ok and no error", ok, err)
	}

	if err := os.MkdirAll(".anvil", 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"components": [{"name": "orders", "stream": true}, {"name": "audit", "stream": false}]}`
	if err := os.WriteFile(filepath.Join(".anvil", "dsql-streams.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	wanted, ok, err := readStreamManifest()
	if err != nil || !ok || !wanted["orders"] || wanted["audit"] {
		t.Errorf("got %v ok=%v err=%v", wanted, ok, err)
	}
}
