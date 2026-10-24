package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var stageListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all stages",
	Long:  `Lists all stages found in the S3 backend and marks the active one.`,
	RunE:  runStageList,
}

func init() {
	stageCmd.AddCommand(stageListCmd)
}

func runStageList(cmd *cobra.Command, args []string) error {
	config, err := loadAnvilConfig()
	if err != nil {
		fmt.Println()
		fmt.Println("  No anvil.yaml found. Run `anvil deploy` to get started.")
		fmt.Println()
		return nil
	}

	if len(config.Stages) == 0 {
		fmt.Println()
		fmt.Println("  No stages found. Run `anvil deploy` to create your first stage.")
		fmt.Println()
		return nil
	}

	// Discover stacks from S3 backends, merged with anvil.yaml entries.
	stacks := discoverStacks(config)

	// Collect and sort for stable output.
	names := make([]string, 0, len(stacks))
	for name := range stacks {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Println()
	printBanner()

	if isTTY() {
		fmt.Printf("  %s Stages %s\n\n", green("⚒"), dim(fmt.Sprintf("(%s)", config.Project)))
	} else {
		fmt.Printf("  Stages (%s)\n\n", config.Project)
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"", "Stage", "Region", "Env"})
	table.SetBorder(false)
	table.SetColumnSeparator("")
	table.SetHeaderLine(false)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetTablePadding("   ")
	table.SetNoWhiteSpace(true)

	for _, name := range names {
		indicator := " "
		if name == config.Active {
			indicator = "*"
		}

		region := ""
		environment := ""
		if sc, ok := config.Stages[name]; ok {
			if sc.Region != "" {
				region = sc.Region
			}
			if sc.Environment != "" {
				environment = sc.Environment
			}
		}

		table.Append([]string{indicator, name, region, environment})
	}

	table.Render()

	fmt.Println()
	if isTTY() {
		fmt.Printf("  %s = active stage\n", green("*"))
	}
	fmt.Println()

	return nil
}

// discoverStacks queries each bootstrapped S3 backend for .pulumi/stacks/*.json files
// and returns a deduplicated set of stack names found.
func discoverStacks(config *anvilConfig) map[string]bool {
	found := make(map[string]bool)

	if config.Project == "" {
		return found
	}

	type bucketInfo struct {
		name   string
		region string
	}
	seen := make(map[string]bool)
	var buckets []bucketInfo

	for stage, sc := range config.Stages {
		if sc == nil || sc.ID == "" {
			continue
		}
		bucketName := resolveBucketName(stage, config.Project, sc.ID)
		if seen[bucketName] {
			continue
		}
		seen[bucketName] = true
		buckets = append(buckets, bucketInfo{name: bucketName, region: sc.Region})
	}

	ctx := context.Background()

	for _, b := range buckets {
		stackNames, err := listStacksInBucket(ctx, b.name, b.region)
		if err != nil {
			continue
		}
		for _, name := range stackNames {
			if strings.Contains(name, "/") {
				continue
			}
			found[name] = true
		}
	}

	for name := range config.Stages {
		found[name] = true
	}

	return found
}

// listStacksInBucket lists Pulumi stack files in an S3 bucket.
func listStacksInBucket(ctx context.Context, bucketName, region string) ([]string, error) {
	var cfgOpts []func(*awsconfig.LoadOptions) error
	if region != "" {
		cfgOpts = append(cfgOpts, awsconfig.WithRegion(region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg)

	prefix := ".pulumi/stacks/"
	output, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: &bucketName,
		Prefix: &prefix,
	})
	if err != nil {
		return nil, err
	}

	var names []string
	for _, obj := range output.Contents {
		if obj.Key == nil {
			continue
		}
		key := *obj.Key
		if name, ok := stackNameFromKey(strings.TrimPrefix(key, prefix)); ok {
			names = append(names, name)
		}
	}

	return names, nil
}

// stackNameFromKey returns the stack a state file key belongs to. State is
// written gzipped (PULUMI_DIY_BACKEND_GZIP), but stacks last saved before that
// still hold plain .json until their next update. A stack mid-migration has
// both, which callers dedupe.
func stackNameFromKey(key string) (string, bool) {
	for _, ext := range []string{".json", ".json.gz", ".json.zst"} {
		if name, ok := strings.CutSuffix(key, ext); ok && name != "" {
			return name, true
		}
	}
	return "", false
}
