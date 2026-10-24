package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dsql"
	dsqltypes "github.com/aws/aws-sdk-go-v2/service/dsql/types"
	"github.com/aws/aws-sdk-go-v2/service/kinesis"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

// DSQL change data capture (CDC) streams have no Pulumi or CloudFormation
// resource, so the CLI manages them with the DSQL API around `pulumi up`:
//
//   - The DSQL component creates the IAM role and publishes streamKinesisArn
//     and streamRoleArn as outputs, and in build mode records whether each
//     component wants a stream in dsqlStreamManifestPath.
//   - Before `up`, streams no longer wanted (stream removed, or the component
//     gone) are deleted, so Pulumi can then remove their role and cluster.
//   - After `up`, each wanted stream is created, or replaced if its target
//     changed (DSQL has no update), and checked for IMPAIRED/FAILED.
//   - `destroy` deletes them first.
//
// AWS is the record: a stream is Anvil's when it carries tagManagedBy=anvil and
// tagComponent=<component>. Streams created any other way are never touched.

const (
	dsqlStreamManifestPath = ".anvil/dsql-streams.json" // must match provider/aws/dsql/stream.go

	tagManagedBy = "ManagedBy"
	tagComponent = "anvil:component"
	tagProject   = "anvil:project"
	tagStage     = "anvil:stage"

	minCDCRecordSizeKiB = 2048 // DSQL rows can be up to 2 MiB
	streamPollInterval  = 10 * time.Second
	streamWaitTimeout   = 20 * time.Minute
)

// dsqlStreamComponent is one DSQL component as the deployed state records it.
type dsqlStreamComponent struct {
	Name       string
	ClusterArn string // the provider region's cluster, where the stream lives
	KinesisArn string // "" when the component has no stream
	RoleArn    string
}

// readStreamManifest returns which DSQL components want a stream, from the
// last discovery run. ok is false when discovery didn't record any, in which
// case nothing is deleted before `up`.
func readStreamManifest() (wanted map[string]bool, ok bool, err error) {
	data, err := os.ReadFile(dsqlStreamManifestPath)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to read %s: %w", dsqlStreamManifestPath, err)
	}
	var m struct {
		Components []struct {
			Name   string `json:"name"`
			Stream bool   `json:"stream"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("invalid %s: %w", dsqlStreamManifestPath, err)
	}
	wanted = map[string]bool{}
	for _, c := range m.Components {
		wanted[c.Name] = c.Stream
	}
	return wanted, true, nil
}

// dsqlComponentsFromDeployment reads DSQL components out of an exported
// Pulumi deployment. region picks the cluster for multi-region components.
func dsqlComponentsFromDeployment(deployment json.RawMessage, region string) ([]dsqlStreamComponent, error) {
	var d struct {
		Resources []struct {
			URN     string         `json:"urn"`
			Type    string         `json:"type"`
			Outputs map[string]any `json:"outputs"`
		} `json:"resources"`
	}
	if len(deployment) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(deployment, &d); err != nil {
		return nil, fmt.Errorf("failed to read stack state: %w", err)
	}

	var out []dsqlStreamComponent
	for _, r := range d.Resources {
		if r.Type != "anvil:aws:DSQL" {
			continue
		}
		c := dsqlStreamComponent{Name: r.URN[strings.LastIndex(r.URN, "::")+2:]}
		if arns, ok := r.Outputs["clusterArns"].(map[string]any); ok {
			if arn, ok := arns[region].(string); ok {
				c.ClusterArn = arn
			} else if len(arns) == 1 {
				for _, v := range arns {
					c.ClusterArn, _ = v.(string)
				}
			}
		}
		c.KinesisArn, _ = r.Outputs["streamKinesisArn"].(string)
		c.RoleArn, _ = r.Outputs["streamRoleArn"].(string)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func stackDSQLComponents(ctx context.Context, s auto.Stack, stage string) ([]dsqlStreamComponent, error) {
	dep, err := s.Export(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read stack state: %w", err)
	}
	return dsqlComponentsFromDeployment(dep.Deployment, resolveRegionForStage(stage))
}

// arnPart returns the colon-separated field i of an ARN, or "".
func arnPart(arn string, i int) string {
	parts := strings.SplitN(arn, ":", 6)
	if i < len(parts) {
		return parts[i]
	}
	return ""
}

func clusterIDFromArn(arn string) string {
	return arn[strings.LastIndex(arn, "/")+1:]
}

func dsqlClientFor(ctx context.Context, clusterArn string) (*dsql.Client, error) {
	cfg, err := awsConfigFor(ctx, arnPart(clusterArn, 3))
	if err != nil {
		return nil, err
	}
	return dsql.NewFromConfig(cfg), nil
}

// anvilStreams returns the CDC streams on the cluster that Anvil created for
// this component, skipping deleted ones.
func anvilStreams(ctx context.Context, client *dsql.Client, clusterID, component string) ([]*dsql.GetStreamOutput, error) {
	var out []*dsql.GetStreamOutput
	pages := dsql.NewListStreamsPaginator(client, &dsql.ListStreamsInput{ClusterIdentifier: aws.String(clusterID)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list CDC streams: %w", err)
		}
		for _, sum := range page.Streams {
			if sum.Status == dsqltypes.StreamStatusDeleted || sum.Status == dsqltypes.StreamStatusDeleting {
				continue
			}
			got, err := client.GetStream(ctx, &dsql.GetStreamInput{ClusterIdentifier: aws.String(clusterID), StreamIdentifier: sum.StreamIdentifier})
			if err != nil {
				return nil, fmt.Errorf("failed to read CDC stream %s: %w", aws.ToString(sum.StreamIdentifier), err)
			}
			if got.Tags[tagManagedBy] == "anvil" && got.Tags[tagComponent] == component {
				out = append(out, got)
			}
		}
	}
	return out, nil
}

// streamTarget returns the Kinesis stream and role a CDC stream writes with.
func streamTarget(s *dsql.GetStreamOutput) (kinesisArn, roleArn string) {
	if k, ok := s.TargetDefinition.(*dsqltypes.TargetDefinitionMemberKinesis); ok {
		return aws.ToString(k.Value.StreamArn), aws.ToString(k.Value.RoleArn)
	}
	return "", ""
}

// streamAction is what reconciling one component's stream will do.
type streamAction string

const (
	streamKeep    streamAction = "keep"
	streamCreate  streamAction = "create"
	streamReplace streamAction = "replace"
	streamDelete  streamAction = "delete"
	streamNone    streamAction = "none"
)

// planStream decides what to do given the stream config wanted (kinesisArn ""
// for none) and Anvil's existing streams. A FAILED stream is replaced; DSQL
// can't recover it.
func planStream(kinesisArn, roleArn string, existing []*dsql.GetStreamOutput) streamAction {
	if kinesisArn == "" {
		if len(existing) > 0 {
			return streamDelete
		}
		return streamNone
	}
	if len(existing) == 0 {
		return streamCreate
	}
	if len(existing) > 1 {
		return streamReplace
	}
	k, r := streamTarget(existing[0])
	if k != kinesisArn || r != roleArn || existing[0].Status == dsqltypes.StreamStatusFailed {
		return streamReplace
	}
	return streamKeep
}

// checkKinesisTarget fails early on a target DSQL can't deliver to.
func checkKinesisTarget(ctx context.Context, clusterArn, kinesisArn string) error {
	if arnPart(kinesisArn, 2) != "kinesis" {
		return fmt.Errorf("stream.kinesisArn %q isn't a Kinesis stream ARN", kinesisArn)
	}
	if cr, kr := arnPart(clusterArn, 3), arnPart(kinesisArn, 3); cr != kr {
		return fmt.Errorf("the Kinesis stream is in %s but the DSQL cluster is in %s; CDC only delivers within one region", kr, cr)
	}
	cfg, err := awsConfigFor(ctx, arnPart(kinesisArn, 3))
	if err != nil {
		return err
	}
	sum, err := kinesis.NewFromConfig(cfg).DescribeStreamSummary(ctx, &kinesis.DescribeStreamSummaryInput{StreamARN: aws.String(kinesisArn)})
	if err != nil {
		return fmt.Errorf("can't read Kinesis stream %s: %w", kinesisArn, err)
	}
	if size := sum.StreamDescriptionSummary.MaxRecordSizeInKiB; size != nil && *size < minCDCRecordSizeKiB {
		return fmt.Errorf("Kinesis stream %s accepts records up to %d KiB; DSQL CDC needs at least %d (rows can be up to 2 MiB). Set maxRecordSizeKib: %d", kinesisArn, *size, minCDCRecordSizeKiB, minCDCRecordSizeKiB)
	}
	return nil
}

func waitForStream(ctx context.Context, client *dsql.Client, clusterID, streamID string, want dsqltypes.StreamStatus) error {
	deadline := time.Now().Add(streamWaitTimeout)
	for {
		got, err := client.GetStream(ctx, &dsql.GetStreamInput{ClusterIdentifier: aws.String(clusterID), StreamIdentifier: aws.String(streamID)})
		var notFound *dsqltypes.ResourceNotFoundException
		if want == dsqltypes.StreamStatusDeleted && errors.As(err, &notFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to check CDC stream %s: %w", streamID, err)
		}
		if got.Status == want {
			return nil
		}
		if got.Status == dsqltypes.StreamStatusFailed && want != dsqltypes.StreamStatusDeleted {
			return fmt.Errorf("CDC stream %s failed%s", streamID, statusReason(got))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("CDC stream %s is still %s after %s", streamID, got.Status, streamWaitTimeout)
		}
		time.Sleep(streamPollInterval)
	}
}

func statusReason(s *dsql.GetStreamOutput) string {
	if s.StatusReason != nil && s.StatusReason.Error != "" {
		return fmt.Sprintf(": %s", s.StatusReason.Error)
	}
	return ""
}

func deleteStreams(ctx context.Context, client *dsql.Client, clusterID string, streams []*dsql.GetStreamOutput) error {
	for _, st := range streams {
		id := aws.ToString(st.StreamIdentifier)
		if _, err := client.DeleteStream(ctx, &dsql.DeleteStreamInput{ClusterIdentifier: aws.String(clusterID), StreamIdentifier: aws.String(id)}); err != nil {
			var notFound *dsqltypes.ResourceNotFoundException
			if errors.As(err, &notFound) {
				continue
			}
			return fmt.Errorf("failed to delete CDC stream %s: %w", id, err)
		}
		if err := waitForStream(ctx, client, clusterID, id, dsqltypes.StreamStatusDeleted); err != nil {
			return err
		}
	}
	return nil
}

// reconcileStream makes the component's CDC stream match its config.
func reconcileStream(ctx context.Context, c dsqlStreamComponent, project, stage string) error {
	if c.ClusterArn == "" {
		return nil
	}
	client, err := dsqlClientFor(ctx, c.ClusterArn)
	if err != nil {
		return err
	}
	clusterID := clusterIDFromArn(c.ClusterArn)
	existing, err := anvilStreams(ctx, client, clusterID, c.Name)
	if err != nil {
		return err
	}

	switch planStream(c.KinesisArn, c.RoleArn, existing) {
	case streamNone:
		return nil
	case streamDelete:
		fmt.Printf("  Deleting CDC stream for %s...\n", c.Name)
		if err := deleteStreams(ctx, client, clusterID, existing); err != nil {
			return err
		}
		printCheck(fmt.Sprintf("CDC stream for %s deleted", c.Name))
		return nil
	case streamKeep:
		st := existing[0]
		switch st.Status {
		case dsqltypes.StreamStatusCreating:
			if err := waitForStream(ctx, client, clusterID, aws.ToString(st.StreamIdentifier), dsqltypes.StreamStatusActive); err != nil {
				return err
			}
		case dsqltypes.StreamStatusImpaired:
			printWarn(fmt.Sprintf("CDC stream for %s is IMPAIRED%s: DSQL can't deliver to Kinesis. Check the role can write to the stream.", c.Name, statusReason(st)))
			return nil
		}
		printCheck(fmt.Sprintf("CDC stream for %s active", c.Name))
		return nil
	case streamReplace:
		fmt.Printf("  Replacing CDC stream for %s (DSQL can't update a stream; changes made until the new one is active aren't captured)...\n", c.Name)
		if err := deleteStreams(ctx, client, clusterID, existing); err != nil {
			return err
		}
	case streamCreate:
		fmt.Printf("  Creating CDC stream for %s (this takes a few minutes)...\n", c.Name)
	}

	if err := checkKinesisTarget(ctx, c.ClusterArn, c.KinesisArn); err != nil {
		return err
	}
	created, err := client.CreateStream(ctx, &dsql.CreateStreamInput{
		ClusterIdentifier: aws.String(clusterID),
		Format:            dsqltypes.StreamFormatJson,
		Ordering:          dsqltypes.StreamOrderingUnordered,
		TargetDefinition: &dsqltypes.TargetDefinitionMemberKinesis{Value: dsqltypes.KinesisTargetDefinition{
			StreamArn: aws.String(c.KinesisArn),
			RoleArn:   aws.String(c.RoleArn),
		}},
		Tags: map[string]string{
			tagManagedBy: "anvil",
			tagComponent: c.Name,
			tagProject:   project,
			tagStage:     stage,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create CDC stream for %s: %w", c.Name, err)
	}
	if err := waitForStream(ctx, client, clusterID, aws.ToString(created.StreamIdentifier), dsqltypes.StreamStatusActive); err != nil {
		return err
	}
	printCheck(fmt.Sprintf("CDC stream for %s active: %s", c.Name, aws.ToString(created.Arn)))
	return nil
}

// componentsLosingStream returns the deployed components whose stream must go
// before `up`: their stream was removed, or the component itself was.
func componentsLosingStream(deployed []dsqlStreamComponent, wanted map[string]bool) []dsqlStreamComponent {
	var out []dsqlStreamComponent
	for _, c := range deployed {
		if c.KinesisArn != "" && !wanted[c.Name] {
			c.KinesisArn = ""
			out = append(out, c)
		}
	}
	return out
}

// removeUnwantedStreams runs before `up`.
func removeUnwantedStreams(ctx context.Context, s auto.Stack, project, stage string) error {
	wanted, ok, err := readStreamManifest()
	if err != nil || !ok {
		return err
	}
	deployed, err := stackDSQLComponents(ctx, s, stage)
	if err != nil {
		return err
	}
	for _, c := range componentsLosingStream(deployed, wanted) {
		if err := reconcileStream(ctx, c, project, stage); err != nil {
			return err
		}
	}
	return nil
}

// reconcileStreams runs after a successful `up`.
func reconcileStreams(ctx context.Context, s auto.Stack, project, stage string) error {
	deployed, err := stackDSQLComponents(ctx, s, stage)
	if err != nil {
		return err
	}
	for _, c := range deployed {
		if err := reconcileStream(ctx, c, project, stage); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	return nil
}

// deleteAllStreams runs before `destroy`.
func deleteAllStreams(ctx context.Context, s auto.Stack, project, stage string) error {
	deployed, err := stackDSQLComponents(ctx, s, stage)
	if err != nil {
		return err
	}
	for _, c := range deployed {
		c.KinesisArn = ""
		if err := reconcileStream(ctx, c, project, stage); err != nil {
			return err
		}
	}
	return nil
}

// previewStreams prints what the next deploy will do to CDC streams. A
// component that wants a stream but isn't deployed yet shows as created.
func previewStreams(ctx context.Context, s auto.Stack, stage string) error {
	wanted, ok, err := readStreamManifest()
	if err != nil || !ok {
		return err
	}
	deployed, err := stackDSQLComponents(ctx, s, stage)
	if err != nil {
		return err
	}
	byName := map[string]dsqlStreamComponent{}
	for _, c := range deployed {
		byName[c.Name] = c
	}

	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	for _, c := range deployed {
		if _, ok := wanted[c.Name]; !ok {
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)

	var lines []string
	for _, name := range names {
		c, isDeployed := byName[name]
		var existing []*dsql.GetStreamOutput
		if isDeployed && c.ClusterArn != "" {
			client, err := dsqlClientFor(ctx, c.ClusterArn)
			if err != nil {
				return err
			}
			if existing, err = anvilStreams(ctx, client, clusterIDFromArn(c.ClusterArn), name); err != nil {
				return err
			}
		}
		switch {
		case wanted[name] && len(existing) == 0:
			lines = append(lines, fmt.Sprintf("  [+] CDC stream for %s will be created after deploy", name))
		case wanted[name]:
			lines = append(lines, fmt.Sprintf("      CDC stream for %s is %s (replaced on deploy if its Kinesis stream or role changes)", name, existing[0].Status))
		case len(existing) > 0:
			lines = append(lines, fmt.Sprintf("  [-] CDC stream for %s will be deleted before deploy", name))
		}
	}
	if len(lines) > 0 {
		fmt.Println()
		for _, l := range lines {
			fmt.Println(l)
		}
	}
	return nil
}
