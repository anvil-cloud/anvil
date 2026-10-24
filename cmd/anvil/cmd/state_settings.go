package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// appSettingsFile holds App settings the CLI needs before Pulumi runs. The SDK
// App writes it during discovery (ANVIL_BUILD_MODE) to the path in
// ANVIL_APP_SETTINGS_PATH. It is separate from the build manifest because the
// provider's manifest writers rewrite that file from fixed structs.
const appSettingsFile = "app-settings.json"

const (
	defaultRetainVersions = 3
	minRetainVersions     = 2
	maxRetainVersions     = 101
)

// stateSettings is the App's defaults.state, with defaults applied.
type stateSettings struct {
	// RetainVersions is how many deploys' state is kept, counting the latest:
	// 3 keeps the latest deploy's state and the two deploys before it.
	RetainVersions int
	CompressState  bool
}

func defaultStateSettings() stateSettings {
	return stateSettings{RetainVersions: defaultRetainVersions, CompressState: true}
}

func appSettingsPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".anvil", appSettingsFile)
}

// readStateSettings reads the settings the last discovery run wrote. No file
// (no discovery yet, or a program that never constructed an App) means defaults.
func readStateSettings(projectRoot string) (stateSettings, error) {
	data, err := os.ReadFile(appSettingsPath(projectRoot))
	if os.IsNotExist(err) {
		return defaultStateSettings(), nil
	}
	if err != nil {
		return stateSettings{}, fmt.Errorf("failed to read app settings: %w", err)
	}
	return parseStateSettings(data)
}

func parseStateSettings(data []byte) (stateSettings, error) {
	var raw struct {
		State *struct {
			RetainVersions *int  `json:"retainVersions"`
			CompressState  *bool `json:"compressState"`
		} `json:"state"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return stateSettings{}, fmt.Errorf("invalid app settings: %w", err)
	}
	s := defaultStateSettings()
	if raw.State != nil {
		if raw.State.RetainVersions != nil {
			s.RetainVersions = *raw.State.RetainVersions
		}
		if raw.State.CompressState != nil {
			s.CompressState = *raw.State.CompressState
		}
	}
	if s.RetainVersions < minRetainVersions || s.RetainVersions > maxRetainVersions {
		return stateSettings{}, fmt.Errorf("defaults.state.retainVersions must be from %d to %d (got %d); 1 would keep no previous state to recover from",
			minRetainVersions, maxRetainVersions, s.RetainVersions)
	}
	return s, nil
}

// pulumiEnv is the environment every Pulumi run gets.
//
// Checkpoint backups (.pulumi/backups/) are off: the state bucket is versioned,
// and stateLifecycleRules keeps the previous versions instead. Pulumi reads
// plain and gzipped state whichever way compression is set, and rewrites the
// file in the configured format on its next save.
func pulumiEnv(backendURL string, s stateSettings) map[string]string {
	execPath, _ := os.Executable()
	pluginDir := filepath.Dir(execPath)
	return map[string]string{
		"PULUMI_BACKEND_URL":                            backendURL,
		"PULUMI_CONFIG_PASSPHRASE":                      "",
		"PULUMI_DIY_BACKEND_GZIP":                       strconv.FormatBool(s.CompressState),
		"PULUMI_DIY_BACKEND_DISABLE_CHECKPOINT_BACKUPS": "true",
		"PATH": pluginDir + ":" + os.Getenv("PATH"),
	}
}

// stateLifecycleRules is the state bucket's whole lifecycle configuration.
// PutBucketLifecycleConfiguration replaces every rule, so this is the full set.
// Retention is counted in deploys, not saves: Pulumi saves the state after
// every resource operation, so one deploy leaves hundreds of S3 versions of
// the state file. Each deploy's final state is the checkpoint copy Pulumi
// writes to .pulumi/history/, which pruneHistoryCheckpoints trims to
// retainVersions after every deploy.
//
//   - .pulumi/stacks/: old versions of the state file (and its .bak) are the
//     saves made during deploys. Delete them a day after they're replaced, S3's
//     minimum. The current state file is never expired.
//   - .pulumi/history/: delete the old versions left behind when
//     pruneHistoryCheckpoints deletes a checkpoint copy. Current history files
//     are never expired, and the .history.json summaries are never deleted.
//   - .pulumi/backups/: checkpoint copies from before backups were turned off.
//     Delete them, and their old versions.
//   - Whole bucket: remove delete markers with no versions behind them (lock
//     files leave one per update) and abandoned multipart uploads.
func stateLifecycleRules(s stateSettings) ([]s3types.LifecycleRule, error) {
	if s.RetainVersions < minRetainVersions || s.RetainVersions > maxRetainVersions {
		return nil, fmt.Errorf("retainVersions must be from %d to %d (got %d)", minRetainVersions, maxRetainVersions, s.RetainVersions)
	}
	return []s3types.LifecycleRule{
		{
			ID:                          aws.String("anvil-state-saves-cleanup"),
			Status:                      s3types.ExpirationStatusEnabled,
			Filter:                      &s3types.LifecycleRuleFilter{Prefix: aws.String(".pulumi/stacks/")},
			NoncurrentVersionExpiration: &s3types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(1)},
		},
		{
			ID:                          aws.String("anvil-pruned-history-cleanup"),
			Status:                      s3types.ExpirationStatusEnabled,
			Filter:                      &s3types.LifecycleRuleFilter{Prefix: aws.String(".pulumi/history/")},
			NoncurrentVersionExpiration: &s3types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(1)},
		},
		{
			ID:                          aws.String("anvil-checkpoint-backups-cleanup"),
			Status:                      s3types.ExpirationStatusEnabled,
			Filter:                      &s3types.LifecycleRuleFilter{Prefix: aws.String(".pulumi/backups/")},
			Expiration:                  &s3types.LifecycleExpiration{Days: aws.Int32(1)},
			NoncurrentVersionExpiration: &s3types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(1)},
		},
		{
			ID:                             aws.String("anvil-bucket-cleanup"),
			Status:                         s3types.ExpirationStatusEnabled,
			Filter:                         &s3types.LifecycleRuleFilter{Prefix: aws.String("")},
			Expiration:                     &s3types.LifecycleExpiration{ExpiredObjectDeleteMarker: aws.Bool(true)},
			AbortIncompleteMultipartUpload: &s3types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(1)},
		},
	}, nil
}

// putStateLifecycle writes the full lifecycle configuration to the state bucket.
func putStateLifecycle(ctx context.Context, s3Client *s3.Client, bucket string, s stateSettings) error {
	rules, err := stateLifecycleRules(s)
	if err != nil {
		return err
	}
	_, err = s3Client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket:                 aws.String(bucket),
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: rules},
	})
	return err
}

// stateBucketFromBackend returns the bucket in an s3:// backend URL, or "" for
// any other backend (an ANVIL_BACKEND_URL override, or a stage not bootstrapped).
func stateBucketFromBackend(backendURL string) string {
	rest, ok := strings.CutPrefix(backendURL, "s3://")
	if !ok {
		return ""
	}
	bucket, _, _ := strings.Cut(rest, "?")
	bucket, _, _ = strings.Cut(bucket, "/")
	return bucket
}

// stateBucketClient returns the stage's state bucket and an S3 client for it,
// or "" when the stage's state isn't in an Anvil-bootstrapped bucket.
func stateBucketClient(ctx context.Context, stage string) (string, *s3.Client, error) {
	bucket := stateBucketFromBackend(resolveBackendForStage(stage))
	if bucket == "" || os.Getenv("ANVIL_BACKEND_URL") != "" {
		return "", nil, nil
	}
	cfg, err := awsConfigFor(ctx, resolveRegionForStage(stage))
	if err != nil {
		return "", nil, err
	}
	return bucket, s3.NewFromConfig(cfg), nil
}

// reconcileStateLifecycle applies the lifecycle rules to an existing stage's
// state bucket on every deploy, so stages bootstrapped before retention existed
// pick them up.
func reconcileStateLifecycle(ctx context.Context, stage string, s stateSettings) error {
	bucket, client, err := stateBucketClient(ctx, stage)
	if bucket == "" || err != nil {
		return err
	}
	return putStateLifecycle(ctx, client, bucket, s)
}

const historyPrefix = ".pulumi/history/"

// historyCheckpointsToDelete returns the checkpoint copies under
// .pulumi/history/ older than the newest keep in each stack's directory.
// Pulumi writes one per update (deploy, refresh or destroy), named
// <stack>-<unix nanoseconds>.checkpoint.json, plus .gz or .zst when state is
// compressed. The .history.json summaries beside them are never returned, nor
// is any file whose name doesn't parse.
func historyCheckpointsToDelete(keys []string, keep int) []string {
	type checkpoint struct {
		key  string
		nano int64
	}
	byDir := map[string][]checkpoint{}
	for _, key := range keys {
		base := path.Base(key)
		name, _, ok := strings.Cut(base, ".checkpoint.json")
		if !ok || !strings.HasPrefix(key, historyPrefix) {
			continue
		}
		dash := strings.LastIndex(name, "-")
		nano, err := strconv.ParseInt(name[dash+1:], 10, 64)
		if dash < 0 || err != nil {
			continue
		}
		dir := path.Dir(key)
		byDir[dir] = append(byDir[dir], checkpoint{key, nano})
	}

	var del []string
	for _, cps := range byDir {
		sort.Slice(cps, func(i, j int) bool { return cps[i].nano > cps[j].nano })
		for _, cp := range cps[min(keep, len(cps)):] {
			del = append(del, cp.key)
		}
	}
	sort.Strings(del)
	return del
}

// pruneHistoryCheckpoints keeps the newest retainVersions deploy checkpoints
// in the stage's state bucket and deletes the rest. The bucket is versioned,
// so each delete leaves an old version, which anvil-pruned-history-cleanup
// removes a day later.
func pruneHistoryCheckpoints(ctx context.Context, stage string, s stateSettings) error {
	bucket, client, err := stateBucketClient(ctx, stage)
	if bucket == "" || err != nil {
		return err
	}

	var keys []string
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(historyPrefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("failed to list state history: %w", err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
	}

	del := historyCheckpointsToDelete(keys, s.RetainVersions)
	for start := 0; start < len(del); start += 1000 { // DeleteObjects takes at most 1000 keys
		batch := del[start:min(start+1000, len(del))]
		objects := make([]s3types.ObjectIdentifier, len(batch))
		for i, key := range batch {
			objects[i] = s3types.ObjectIdentifier{Key: aws.String(key)}
		}
		out, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &s3types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("failed to delete old state checkpoints: %w", err)
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("failed to delete %d old state checkpoint(s), e.g. %s: %s",
				len(out.Errors), aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Message))
		}
	}
	return nil
}
