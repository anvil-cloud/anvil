package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cloudtrailtypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var bootstrapRegion string
var bootstrapStage string
var bootstrapEnvironment string
var bootstrapS3DataEvents bool

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Set up state storage for Anvil",
	Long:  `Creates an S3 bucket for Anvil state storage and writes the backend configuration to anvil.yaml.`,
	RunE:  runBootstrap,
}

func init() {
	bootstrapCmd.Flags().StringVar(&bootstrapRegion, "region", "", "AWS region for the state bucket (defaults to AWS CLI default)")
	bootstrapCmd.Flags().StringVar(&bootstrapStage, "stage", "", "Stage name, used in bucket naming (defaults to the active stage)")
	bootstrapCmd.Flags().StringVar(&bootstrapEnvironment, "environment", "", "Environment type: prod or nonprod (default nonprod)")
	bootstrapCmd.Flags().BoolVar(&bootstrapS3DataEvents, "s3-data-events", false, "Record S3 object-level reads/writes in CloudTrail for every bucket in the account (billed per event)")
	rootCmd.AddCommand(bootstrapCmd)
}

// anvilConfig represents the anvil.yaml file with multi-stage support.
type anvilConfig struct {
	Project string                  `yaml:"project"`
	Active  string                  `yaml:"active,omitempty"`
	Stages  map[string]*stageConfig `yaml:"stages"`
}

type stageConfig struct {
	ID               string `yaml:"id"`
	Region           string `yaml:"region"`
	Environment      string `yaml:"environment"`
	LoggingBucket    string `yaml:"loggingBucket,omitempty"`
	CloudTrailArn    string `yaml:"cloudTrailArn,omitempty"`
	ExistingTrailArn string `yaml:"existingTrailArn,omitempty"`
	// S3DataEvents opts the Anvil-managed CloudTrail trail into S3 object-level
	// data events. Off by default: they're billed per event and every CloudFront
	// asset fetch from S3 produces one.
	S3DataEvents bool `yaml:"s3DataEvents,omitempty"`
}

// resolveBucketName reconstructs the bucket name from its parts.
//
// S3 bucket names must be 3-63 chars of lowercase letters, digits and hyphens,
// so project/stage names like "testAuth0" are lowercased and other characters
// replaced. Names that were already valid are returned unchanged, so existing
// stages keep resolving to their buckets.
func resolveBucketName(stage, project, id string) string {
	prefix := sanitizeBucketPart(fmt.Sprintf("%s-anvil-state-%s", stage, project))
	suffix := sanitizeBucketPart(id)

	// Keep the unique id; trim the prefix if the whole name would exceed 63.
	if max := 63 - len(suffix) - 1; len(prefix) > max {
		prefix = strings.TrimRight(prefix[:max], "-")
	}
	return prefix + "-" + suffix
}

func sanitizeBucketPart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func runBootstrap(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// An explicit --stage (or a stage set by ensureBootstrapped) wins;
	// otherwise resolve it the same way every other command does.
	bootstrapStage = resolveStage(bootstrapStage)

	printBanner()
	fmt.Printf("  Bootstrapping \"%s\"...\n\n", bootstrapStage)

	// ── Resolve environment (flag → default) ──
	if bootstrapEnvironment == "" {
		bootstrapEnvironment = defaultEnvironment
	}

	if bootstrapEnvironment != "prod" && bootstrapEnvironment != "nonprod" {
		return fmt.Errorf("Invalid environment %q. Must be \"prod\" or \"nonprod\".", bootstrapEnvironment)
	}

	// ── Load AWS config ──
	var cfgOpts []func(*awsconfig.LoadOptions) error
	if bootstrapRegion != "" {
		cfgOpts = append(cfgOpts, awsconfig.WithRegion(bootstrapRegion))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return fmt.Errorf("AWS credentials not found or expired.\n  Run `aws configure` or set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.")
	}

	stsClient := sts.NewFromConfig(cfg)
	stsOut, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("AWS credentials not found or expired.\n  Run `aws configure` or set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.")
	}
	accountId := aws.ToString(stsOut.Account)

	region := cfg.Region
	if region == "" {
		return fmt.Errorf("No AWS region configured.\n  Set a default with `aws configure` or pass --region.")
	}

	// ── Read project name from anvil.yaml ──
	projectName, err := readProjectName()
	if err != nil {
		return err
	}

	// ── Load existing config (or create new) ──
	config, err := loadAnvilConfig()
	if err != nil {
		config = &anvilConfig{
			Project: projectName,
			Stages:  make(map[string]*stageConfig),
		}
	}

	// ── Check if this stage is already bootstrapped ──
	if sc, ok := config.Stages[bootstrapStage]; ok && sc.ID != "" {
		bucketName := resolveBucketName(bootstrapStage, config.Project, sc.ID)
		return verifyExistingBootstrap(ctx, cfg, accountId, region, bootstrapEnvironment, bucketName, config.Project)
	}

	// ── Generate state bucket name ──
	suffix, err := randomSuffix(6)
	if err != nil {
		return fmt.Errorf("failed to generate random suffix: %w", err)
	}

	bucketName := resolveBucketName(bootstrapStage, projectName, suffix)

	// ── Create S3 state bucket ──
	s3Client := s3.NewFromConfig(cfg)

	createInput := &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	}
	if region != "us-east-1" {
		createInput.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}

	_, err = s3Client.CreateBucket(ctx, createInput)
	if err != nil {
		return fmt.Errorf("failed to create state bucket: %s", mapError(err.Error()))
	}

	if isTTY() {
		fmt.Printf("  %s Created state bucket\n", green("✔"))
	} else {
		fmt.Printf("  [ok] Created state bucket\n")
	}

	// ── Enable versioning ──
	_, err = s3Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucketName),
		VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusEnabled,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to enable versioning: %w", err)
	}
	printCheck("Versioning enabled")

	// ── State retention ──
	// `anvil deploy` reconciles this from the App's defaults.state each time;
	// here it uses what the last discovery saw, or the defaults.
	settings := defaultStateSettings()
	if root, err := findProjectRoot(); err == nil {
		if settings, err = readStateSettings(root); err != nil {
			return err
		}
	}
	if err := putStateLifecycle(ctx, s3Client, bucketName, settings); err != nil {
		return fmt.Errorf("failed to set state retention: %w", err)
	}
	printCheck(fmt.Sprintf("State retention set (%d deploys)", settings.RetainVersions))

	// ── Enable encryption ──
	_, err = s3Client.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: aws.String(bucketName),
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{
				{
					ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
						SSEAlgorithm: s3types.ServerSideEncryptionAes256,
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to enable encryption: %w", err)
	}
	printCheck("Encryption enabled")

	// ── Block public access ──
	_, err = s3Client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(bucketName),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to block public access: %w", err)
	}
	printCheck("Public access blocked")

	// ── TLS-only bucket policy ──
	stateBucketPolicy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Sid": "DenyNonTLS",
				"Effect": "Deny",
				"Principal": "*",
				"Action": "s3:*",
				"Resource": [
					"arn:aws:s3:::%s",
					"arn:aws:s3:::%s/*"
				],
				"Condition": {
					"Bool": {
						"aws:SecureTransport": "false"
					}
				}
			}
		]
	}`, bucketName, bucketName)

	_, err = s3Client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucketName),
		Policy: aws.String(stateBucketPolicy),
	})
	if err != nil {
		return fmt.Errorf("failed to apply TLS policy to state bucket: %w", err)
	}
	printCheck("TLS policy applied")

	// ── Tags ──
	if _, err := ensureBucketTags(ctx, s3Client, bucketName, stateBucketTags(projectName, bootstrapStage)); err != nil {
		return fmt.Errorf("failed to tag state bucket: %w", err)
	}
	printCheck("Tags applied")

	// ── Logging bucket + CloudTrail ──
	loggingBucket, trailArn, err := setupLoggingInfrastructure(ctx, cfg, accountId, region, bootstrapEnvironment)
	if err != nil {
		return fmt.Errorf("logging setup failed: %w", err)
	}

	// ── Write stage to anvil.yaml ──
	config.Project = projectName
	if config.Stages == nil {
		config.Stages = make(map[string]*stageConfig)
	}

	config.Stages[bootstrapStage] = &stageConfig{
		ID:            suffix,
		Region:        region,
		Environment:   bootstrapEnvironment,
		LoggingBucket: loggingBucket,
		CloudTrailArn: trailArn,
		S3DataEvents:  bootstrapS3DataEvents,
	}

	err = writeAnvilConfig(*config)
	if err != nil {
		return fmt.Errorf("failed to write anvil.yaml: %w", err)
	}
	printCheck("Config written to anvil.yaml")

	fmt.Println()
	if isTTY() {
		fmt.Println(dim("  Ready. Run `anvil deploy` to get started."))
	} else {
		fmt.Println("  Ready. Run `anvil deploy` to get started.")
	}

	return nil
}

// verifyExistingBootstrap checks that an already-bootstrapped bucket is correctly configured,
// and ensures logging infrastructure exists, creating it if missing.
func verifyExistingBootstrap(ctx context.Context, cfg aws.Config, accountId, region, environment, bucketName, projectName string) error {
	s3Client := s3.NewFromConfig(cfg)

	_, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return fmt.Errorf("State bucket not found.\n  It may have been deleted. Remove the stage from anvil.yaml and run `anvil bootstrap` again.")
	}

	if isTTY() {
		fmt.Printf("  %s State bucket exists\n", green("✔"))
	} else {
		fmt.Printf("  [ok] State bucket exists\n")
	}

	versioning, err := s3Client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{
		Bucket: aws.String(bucketName),
	})
	if err == nil && versioning.Status == s3types.BucketVersioningStatusEnabled {
		printCheck("Verified: versioning enabled")
	} else {
		printWarn("Versioning is not enabled")
	}

	pab, err := s3Client.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{
		Bucket: aws.String(bucketName),
	})
	if err == nil && pab.PublicAccessBlockConfiguration != nil &&
		aws.ToBool(pab.PublicAccessBlockConfiguration.BlockPublicAcls) &&
		aws.ToBool(pab.PublicAccessBlockConfiguration.BlockPublicPolicy) {
		printCheck("Verified: public access blocked")
	} else {
		printWarn("Public access block is not fully configured")
	}

	// Stages bootstrapped before tagging was added get their tags backfilled.
	if added, err := ensureBucketTags(ctx, s3Client, bucketName, stateBucketTags(projectName, bootstrapStage)); err != nil {
		printWarn(fmt.Sprintf("Could not tag state bucket: %v", err))
	} else if added {
		printCheck("Tags added to state bucket")
	} else {
		printCheck("Verified: tags")
	}

	// ── Ensure logging infrastructure exists ──
	loggingBucket, trailArn, err := setupLoggingInfrastructure(ctx, cfg, accountId, region, environment)
	if err != nil {
		printWarn(fmt.Sprintf("Logging setup incomplete: %v", err))
	} else {
		// Backfill anvil.yaml if logging fields are missing
		config, configErr := loadAnvilConfig()
		if configErr == nil {
			if sc, ok := config.Stages[bootstrapStage]; ok {
				if sc.LoggingBucket == "" || sc.CloudTrailArn == "" {
					sc.LoggingBucket = loggingBucket
					sc.CloudTrailArn = trailArn
					writeAnvilConfig(*config)
					printCheck("Config updated with logging info")
				}
				if bootstrapS3DataEvents && !sc.S3DataEvents {
					sc.S3DataEvents = true
					writeAnvilConfig(*config)
					printCheck("Config updated: s3DataEvents enabled")
				}
			}
		}
	}

	printCheck("Config up to date")

	fmt.Println()
	if isTTY() {
		fmt.Println(dim("  Nothing to do. Already bootstrapped."))
	} else {
		fmt.Println("  Nothing to do. Already bootstrapped.")
	}

	return nil
}

// setupLoggingInfrastructure ensures the central logging bucket and CloudTrail trail exist.
// Safe to call on every bootstrap — checks before creating.
func setupLoggingInfrastructure(ctx context.Context, cfg aws.Config, accountId, region, environment string) (loggingBucket string, trailArn string, err error) {
	s3Client := s3.NewFromConfig(cfg)
	ctClient := cloudtrail.NewFromConfig(cfg)

	// ── Logging bucket ──
	loggingBucket = fmt.Sprintf("anvil-logging-%s-%s", accountId, region)

	_, err = s3Client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(loggingBucket),
	})
	if err != nil {
		// Bucket doesn't exist — create it
		createInput := &s3.CreateBucketInput{
			Bucket: aws.String(loggingBucket),
		}
		if region != "us-east-1" {
			createInput.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
				LocationConstraint: s3types.BucketLocationConstraint(region),
			}
		}
		if _, err = s3Client.CreateBucket(ctx, createInput); err != nil {
			return "", "", fmt.Errorf("failed to create logging bucket: %w", err)
		}

		// Block public access
		if _, err = s3Client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
			Bucket: aws.String(loggingBucket),
			PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
				BlockPublicAcls:       aws.Bool(true),
				BlockPublicPolicy:     aws.Bool(true),
				IgnorePublicAcls:      aws.Bool(true),
				RestrictPublicBuckets: aws.Bool(true),
			},
		}); err != nil {
			return "", "", fmt.Errorf("failed to block public access on logging bucket: %w", err)
		}

		// SSE-S3 encryption
		if _, err = s3Client.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
			Bucket: aws.String(loggingBucket),
			ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
				Rules: []s3types.ServerSideEncryptionRule{
					{
						ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
							SSEAlgorithm: s3types.ServerSideEncryptionAes256,
						},
					},
				},
			},
		}); err != nil {
			return "", "", fmt.Errorf("failed to enable encryption on logging bucket: %w", err)
		}

		// Versioning
		if _, err = s3Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
			Bucket: aws.String(loggingBucket),
			VersioningConfiguration: &s3types.VersioningConfiguration{
				Status: s3types.BucketVersioningStatusEnabled,
			},
		}); err != nil {
			return "", "", fmt.Errorf("failed to enable versioning on logging bucket: %w", err)
		}

		// TLS-only + CloudTrail write policy
		tlsPolicy := fmt.Sprintf(`{
			"Version": "2012-10-17",
			"Statement": [
				{
					"Sid": "DenyNonTLS",
					"Effect": "Deny",
					"Principal": "*",
					"Action": "s3:*",
					"Resource": [
						"arn:aws:s3:::%s",
						"arn:aws:s3:::%s/*"
					],
					"Condition": {
						"Bool": {
							"aws:SecureTransport": "false"
						}
					}
				},
				{
					"Sid": "AllowCloudTrailWrite",
					"Effect": "Allow",
					"Principal": {
						"Service": "cloudtrail.amazonaws.com"
					},
					"Action": "s3:PutObject",
					"Resource": "arn:aws:s3:::%s/cloudtrail/AWSLogs/%s/*",
					"Condition": {
						"StringEquals": {
							"s3:x-amz-acl": "bucket-owner-full-control"
						}
					}
				},
				{
					"Sid": "AllowCloudTrailACLCheck",
					"Effect": "Allow",
					"Principal": {
						"Service": "cloudtrail.amazonaws.com"
					},
					"Action": "s3:GetBucketAcl",
					"Resource": "arn:aws:s3:::%s"
				}
			]
		}`, loggingBucket, loggingBucket, loggingBucket, accountId, loggingBucket)

		if _, err = s3Client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
			Bucket: aws.String(loggingBucket),
			Policy: aws.String(tlsPolicy),
		}); err != nil {
			return "", "", fmt.Errorf("failed to apply bucket policy on logging bucket: %w", err)
		}

		// Lifecycle rules based on environment
		var lifecycleRules []s3types.LifecycleRule
		if environment == "prod" {
			lifecycleRules = []s3types.LifecycleRule{
				{
					ID:     aws.String("prod-audit-retention"),
					Status: s3types.ExpirationStatusEnabled,
					Filter: &s3types.LifecycleRuleFilter{
						Prefix: aws.String(""),
					},
					Transitions: []s3types.Transition{
						{Days: aws.Int32(90), StorageClass: s3types.TransitionStorageClassStandardIa},
						{Days: aws.Int32(365), StorageClass: s3types.TransitionStorageClassGlacier},
					},
					Expiration: &s3types.LifecycleExpiration{
						Days: aws.Int32(2555), // 7 years
					},
				},
			}
		} else {
			lifecycleRules = []s3types.LifecycleRule{
				{
					ID:     aws.String("nonprod-audit-retention"),
					Status: s3types.ExpirationStatusEnabled,
					Filter: &s3types.LifecycleRuleFilter{
						Prefix: aws.String(""),
					},
					Expiration: &s3types.LifecycleExpiration{
						Days: aws.Int32(30),
					},
				},
			}
		}

		if _, err = s3Client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
			Bucket: aws.String(loggingBucket),
			LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{
				Rules: lifecycleRules,
			},
		}); err != nil {
			return "", "", fmt.Errorf("failed to set lifecycle rules on logging bucket: %w", err)
		}

		printCheck("Logging bucket created")
	} else {
		printCheck("Logging bucket exists")
	}

	// Shared by every stage in the account and region, so no project/stage tags.
	if _, err := ensureBucketTags(ctx, s3Client, loggingBucket, loggingBucketTags); err != nil {
		printWarn(fmt.Sprintf("Could not tag logging bucket: %v", err))
	}

	// ── CloudTrail ──

	s3DataEvents := bootstrapS3DataEvents

	// Honour user-specified existing trail in anvil.yaml
	anvilCfg, configErr := loadAnvilConfig()
	if configErr == nil {
		if sc, ok := anvilCfg.Stages[bootstrapStage]; ok {
			if sc.ExistingTrailArn != "" {
				printCheck(fmt.Sprintf("Using existing CloudTrail trail (from anvil.yaml): %s", sc.ExistingTrailArn))
				return loggingBucket, sc.ExistingTrailArn, nil
			}
			s3DataEvents = s3DataEvents || sc.S3DataEvents
		}
	}

	// Check for any existing trail — CloudTrail is free for one trail per region.
	describeOut, err := ctClient.DescribeTrails(ctx, &cloudtrail.DescribeTrailsInput{
		IncludeShadowTrails: aws.Bool(false),
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to describe CloudTrail trails: %w", err)
	}

	for _, trail := range describeOut.TrailList {
		if trail.TrailARN != nil {
			trailArn = *trail.TrailARN
			printCheck(fmt.Sprintf("Using existing CloudTrail trail: %s", aws.ToString(trail.Name)))

			// Only trails Anvil created are reconciled; anyone else's trail is left as-is.
			if strings.HasPrefix(aws.ToString(trail.Name), "anvil-trail-") {
				if err := reconcileS3DataEvents(ctx, ctClient, trailArn, s3DataEvents); err != nil {
					printWarn(fmt.Sprintf("Could not update CloudTrail S3 data events: %v", err))
				}
				if err := tagTrail(ctx, ctClient, trailArn); err != nil {
					printWarn(fmt.Sprintf("Could not tag CloudTrail trail: %v", err))
				}
			}
			return loggingBucket, trailArn, nil
		}
	}

	// No existing trail — create one
	trailName := fmt.Sprintf("anvil-trail-%s-%s", accountId, region)
	createTrailOut, err := ctClient.CreateTrail(ctx, &cloudtrail.CreateTrailInput{
		Name:                    aws.String(trailName),
		S3BucketName:            aws.String(loggingBucket),
		S3KeyPrefix:             aws.String("cloudtrail"),
		IsMultiRegionTrail:      aws.Bool(true),
		EnableLogFileValidation: aws.Bool(true),
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to create CloudTrail trail: %w", err)
	}
	trailArn = aws.ToString(createTrailOut.TrailARN)

	// Tag the trail separately
	if err := tagTrail(ctx, ctClient, trailArn); err != nil {
		printWarn(fmt.Sprintf("Could not tag CloudTrail trail: %v", err))
	}

	// Management events, plus S3 data events only when opted in.
	if _, err = ctClient.PutEventSelectors(ctx, &cloudtrail.PutEventSelectorsInput{
		TrailName:      aws.String(trailArn),
		EventSelectors: trailEventSelectors(s3DataEvents),
	}); err != nil {
		return "", "", fmt.Errorf("failed to configure CloudTrail event selectors: %w", err)
	}

	// Start logging
	if _, err = ctClient.StartLogging(ctx, &cloudtrail.StartLoggingInput{
		Name: aws.String(trailArn),
	}); err != nil {
		return "", "", fmt.Errorf("failed to start CloudTrail logging: %w", err)
	}

	printCheck("CloudTrail trail created")
	return loggingBucket, trailArn, nil
}

// ── Tags ──

// stateBucketTags match the default tags App puts on a stage's resources, so
// the state bucket is in scope for that app's compliance scans.
func stateBucketTags(project, stage string) map[string]string {
	return map[string]string{
		"ManagedBy": "anvil",
		"project":   project,
		"stage":     stage,
		"Component": "StateBucket",
	}
}

// The logging bucket and trail are shared across the account and region, so
// they carry no project/stage and stay out of app-scoped scans.
var (
	loggingBucketTags = map[string]string{"ManagedBy": "anvil", "Component": "LoggingBucket"}
	trailTags         = map[string]string{"ManagedBy": "anvil", "Component": "CloudTrail"}
)

// ensureBucketTags adds want to the bucket's tags and reports whether anything
// changed. PutBucketTagging replaces the whole set, so existing tags (including
// any the user added) are read first and kept.
func ensureBucketTags(ctx context.Context, s3Client *s3.Client, bucket string, want map[string]string) (bool, error) {
	var existing []s3types.Tag
	out, err := s3Client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)})
	if err == nil {
		existing = out.TagSet
	} else {
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NoSuchTagSet" {
			return false, err
		}
	}

	merged, changed := mergeTags(existing, want)
	if !changed {
		return false, nil
	}
	_, err = s3Client.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket:  aws.String(bucket),
		Tagging: &s3types.Tagging{TagSet: merged},
	})
	return err == nil, err
}

// mergeTags overlays want onto existing, sorted by key for a stable order.
func mergeTags(existing []s3types.Tag, want map[string]string) ([]s3types.Tag, bool) {
	tags := make(map[string]string, len(existing)+len(want))
	for _, t := range existing {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	changed := false
	for k, v := range want {
		if cur, ok := tags[k]; !ok || cur != v {
			tags[k] = v
			changed = true
		}
	}

	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	merged := make([]s3types.Tag, 0, len(keys))
	for _, k := range keys {
		merged = append(merged, s3types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	return merged, changed
}

// tagTrail adds Anvil's tags to a trail. AddTags only sets the given keys, so
// other tags on the trail are untouched.
func tagTrail(ctx context.Context, ctClient *cloudtrail.Client, trailArn string) error {
	keys := make([]string, 0, len(trailTags))
	for k := range trailTags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tagsList := make([]cloudtrailtypes.Tag, 0, len(keys))
	for _, k := range keys {
		tagsList = append(tagsList, cloudtrailtypes.Tag{Key: aws.String(k), Value: aws.String(trailTags[k])})
	}
	_, err := ctClient.AddTags(ctx, &cloudtrail.AddTagsInput{
		ResourceId: aws.String(trailArn),
		TagsList:   tagsList,
	})
	return err
}

// ── Helpers ──

func printCheck(msg string) {
	if isTTY() {
		fmt.Printf("  %s %s\n", green("✔"), msg)
	} else {
		fmt.Printf("  [ok] %s\n", msg)
	}
}

func printWarn(msg string) {
	if isTTY() {
		fmt.Printf("  %s %s\n", yellow("⚠"), msg)
	} else {
		fmt.Printf("  [warn] %s\n", msg)
	}
}

func randomSuffix(length int) (string, error) {
	bytes := make([]byte, length)
	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes)[:length], nil
}

func readProjectName() (string, error) {
	config, err := loadAnvilConfig()
	if err != nil {
		return "", fmt.Errorf("Could not find anvil.yaml in this directory.\n  Run `anvil init` to create a project, or cd into an existing one.")
	}

	if config.Project == "" {
		return "", fmt.Errorf("No project name found in anvil.yaml.\n  Add a `project:` field to anvil.yaml.")
	}

	return strings.ReplaceAll(config.Project, " ", "-"), nil
}

func loadAnvilConfig() (*anvilConfig, error) {
	data, err := os.ReadFile("anvil.yaml")
	if err != nil {
		return nil, err
	}

	var config anvilConfig
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}

func writeAnvilConfig(config anvilConfig) error {
	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile("anvil.yaml", data, 0644)
}

// trailEventSelectors returns the event selectors for the Anvil-managed trail:
// all management events, plus S3 object-level data events when opted in.
func trailEventSelectors(s3DataEvents bool) []cloudtrailtypes.EventSelector {
	selector := cloudtrailtypes.EventSelector{
		ReadWriteType:           cloudtrailtypes.ReadWriteTypeAll,
		IncludeManagementEvents: aws.Bool(true),
	}
	if s3DataEvents {
		selector.DataResources = []cloudtrailtypes.DataResource{
			{
				Type:   aws.String("AWS::S3::Object"),
				Values: []string{"arn:aws:s3"},
			},
		}
	}
	return []cloudtrailtypes.EventSelector{selector}
}

// reconcileS3DataEvents brings an existing Anvil-managed trail in line with the
// s3DataEvents setting. The trail is shared by every project in the account and
// region, so the change applies account-wide.
func reconcileS3DataEvents(ctx context.Context, ctClient *cloudtrail.Client, trailArn string, s3DataEvents bool) error {
	current, err := ctClient.GetEventSelectors(ctx, &cloudtrail.GetEventSelectorsInput{
		TrailName: aws.String(trailArn),
	})
	if err != nil {
		return err
	}

	enabled := false
	for _, sel := range current.EventSelectors {
		for _, dr := range sel.DataResources {
			if aws.ToString(dr.Type) == "AWS::S3::Object" {
				enabled = true
			}
		}
	}
	// Advanced selectors are configured outside Anvil — leave them alone.
	if len(current.AdvancedEventSelectors) > 0 || enabled == s3DataEvents {
		return nil
	}

	if _, err := ctClient.PutEventSelectors(ctx, &cloudtrail.PutEventSelectorsInput{
		TrailName:      aws.String(trailArn),
		EventSelectors: trailEventSelectors(s3DataEvents),
	}); err != nil {
		return err
	}

	if s3DataEvents {
		printCheck("CloudTrail S3 data events enabled")
	} else {
		printCheck("CloudTrail S3 data events disabled (opt in with s3DataEvents: true)")
	}
	return nil
}
