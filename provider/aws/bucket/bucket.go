package bucket

import (
	"github.com/anvil-cloud/anvil/provider/internal/hatch"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/s3"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// BucketArgs defines the inputs for an Anvil-managed S3 bucket.
//
// Tier 1 controls (free, universally required by CIS, ISO 27017, SOC 2,
// NIST 800-53, ISO 27001) are always enforced with no configuration needed:
//   - Block all public access
//   - SSE-S3 encryption at rest
//   - TLS-only bucket policy
//   - Versioning in production
//   - Incomplete multipart upload cleanup after 7 days
//   - Intelligent-Tiering for objects >= 128KB (disabled if custom lifecycle provided)
//
// Tier 2 controls incur cost and must be explicitly opted into.
type BucketArgs struct {
	// Logging enables S3 server access logging to the centralised logging
	// bucket created during `anvil bootstrap`. Required by most compliance
	// frameworks for audit trails. Incurs storage cost on the logging bucket.
	// Default: false
	Logging bool `pulumi:"logging,optional"`

	// Encryption upgrades at-rest encryption from SSE-S3 (free, tier 1
	// default) to SSE-KMS. Required by HIPAA, PCI-DSS, FedRAMP, and IRAP
	// for key management audit trails. Incurs KMS API call cost.
	// Valid values: "kms" — omit to use the tier 1 SSE-S3 default.
	Encryption string `pulumi:"encryption,optional"`

	// Versioning enables versioning in non-production stages. Versioning is
	// always on in production (tier 1). Enable this if you need version
	// history in dev/staging. Incurs additional storage cost per version.
	// Default: false (production always on regardless)
	Versioning bool `pulumi:"versioning,optional"`

	// VersionRetention sets how long non-current object versions are kept
	// before automatic expiry. Strongly recommended when versioning is enabled
	// to prevent unbounded storage growth.
	// Presets: "7d" | "30d" | "90d" | "1y" | "3y" | "6y" | "7y"
	// Default: "" (keep versions forever)
	VersionRetention string `pulumi:"versionRetention,optional"`

	// StorageTransition controls when objects move from Standard storage to
	// cheaper storage classes. Only applies when no custom lifecycle is
	// provided via hatch["lifecycleRules"] — custom lifecycles replace
	// this setting entirely.
	// Presets: "30d" | "90d" | "1y"
	//   "30d" → IA at 30d, Glacier at 90d
	//   "90d" → IA at 90d, Glacier at 365d
	//   "1y"  → IA at 365d, Glacier at 730d
	// Default: "" (Intelligent-Tiering handles transitions automatically)
	StorageTransition string `pulumi:"storageTransition,optional"`

	// Retention enables Object Lock with a retention period in days (COMPLIANCE mode).
	// Objects cannot be deleted or overwritten until the period expires.
	// Required by PCI-DSS (365), FedRAMP (1095), HIPAA (2190).
	// Must be set at bucket creation — changing requires bucket replacement.
	// Incurs storage cost. Default: 0 (disabled)
	Retention int `pulumi:"retention,optional"`

	Hatch map[string]map[string]interface{} `pulumi:"hatch,optional"`
}

type Bucket struct {
	pulumi.ResourceState

	BucketName pulumi.StringOutput `pulumi:"bucketName"`
	Arn        pulumi.StringOutput `pulumi:"arn"`
	name       string
}

func (b *Bucket) Annotate(a infer.Annotator) {
	a.SetToken("aws", "Bucket")
	a.Describe(&b, "An Anvil-managed S3 bucket. Tier 1 security controls (public access block, SSE-S3 encryption, TLS policy, production versioning, multipart cleanup, intelligent tiering) are enforced automatically at no cost, satisfying the technical baseline of CIS Benchmarks, ISO 27017, SOC 2, NIST 800-53, and ISO 27001.")
}

// resolveDays converts a retention preset string to an integer day count.
// Returns 0 if the preset is unrecognised or empty.
func resolveDays(preset string) int {
	switch preset {
	case "7d":
		return 7
	case "30d":
		return 30
	case "90d":
		return 90
	case "1y":
		return 365
	case "3y":
		return 1095
	case "6y":
		return 2190
	case "7y":
		return 2555
	default:
		return 0
	}
}

func NewBucket(ctx *pulumi.Context, name string, args BucketArgs, opts ...pulumi.ResourceOption) (*Bucket, error) {
	b := &Bucket{name: name}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")
	environment := cfg.Require("environment")
	loggingBucket := cfg.Get("loggingBucket")

	isProduction := environment == "prod"
	useKMS := args.Encryption == "kms"
	needsVersioning := isProduction || args.Versioning
	versionRetentionDays := resolveDays(args.VersionRetention)
	_, hasCustomLifecycle := args.Hatch["lifecycleRules"]

	physicalName := provider.PhysicalName(stage, name, "bucket", stageId)

	opts = provider.WithDefault(opts, true)

	err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, b, opts...)
	if err != nil {
		return nil, err
	}

	// ── 1. Base Bucket ─────────────────────────────────
	// Tier 1: always created.
	bucketProps := hatch.Merge(args.Hatch["bucket"], pulumi.Map{
		"bucket":       pulumi.String(physicalName),
		"forceDestroy": pulumi.Bool(true),
		"tags": pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	})

	res := &s3.Bucket{}
	err = ctx.RegisterResource("aws:s3/bucket:Bucket", name, bucketProps, res, pulumi.Parent(b))
	if err != nil {
		return nil, err
	}

	// ── 2. Public Access Block ─────────────────────────
	// Tier 1: always on. Required by CIS, ISO 27017, SOC 2, NIST 800-53,
	// ISO 27001. Free.
	pabProps := hatch.Merge(args.Hatch["publicAccessBlock"], pulumi.Map{
		"bucket":                res.ID(),
		"blockPublicAcls":       pulumi.Bool(true),
		"blockPublicPolicy":     pulumi.Bool(true),
		"ignorePublicAcls":      pulumi.Bool(true),
		"restrictPublicBuckets": pulumi.Bool(true),
	})
	err = ctx.RegisterResource("aws:s3/bucketPublicAccessBlock:BucketPublicAccessBlock", name+"-pab", pabProps, &s3.BucketPublicAccessBlock{}, pulumi.Parent(b))
	if err != nil {
		return nil, err
	}

	// ── 3. Encryption ──────────────────────────────────
	// Tier 1: SSE-S3 (AES256) always on. Free.
	// Tier 2: SSE-KMS when encryption: "kms". Incurs KMS API call cost.
	sseAlgorithm := "AES256"
	if useKMS {
		sseAlgorithm = "aws:kms"
	}

	encProps := hatch.Merge(args.Hatch["serverSideEncryptionConfiguration"], pulumi.Map{
		"bucket": res.ID(),
		"rules": pulumi.Array{
			pulumi.Map{
				"applyServerSideEncryptionByDefault": pulumi.Map{
					"sseAlgorithm": pulumi.String(sseAlgorithm),
				},
				"bucketKeyEnabled": pulumi.Bool(true),
			},
		},
	})

	err = ctx.RegisterResource("aws:s3/bucketServerSideEncryptionConfigurationV2:BucketServerSideEncryptionConfigurationV2", name+"-enc", encProps, &s3.BucketServerSideEncryptionConfigurationV2{}, pulumi.Parent(b))
	if err != nil {
		return nil, err
	}

	// ── 4. TLS-only bucket policy ──────────────────────
	// Tier 1: always on. Denies any request not using HTTPS.
	// Required by all 5 tier 1 frameworks. Free.
	bucketArn := res.Arn
	tlsPolicy := pulumi.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Sid": "DenyNonTLS",
			"Effect": "Deny",
			"Principal": "*",
			"Action": "s3:*",
			"Resource": ["%s", "%s/*"],
			"Condition": {
				"Bool": { "aws:SecureTransport": "false" }
			}
		}]
	}`, bucketArn, bucketArn)

	policyProps := hatch.Merge(args.Hatch["policy"], pulumi.Map{
		"bucket": res.ID(),
		"policy": tlsPolicy,
	})
	err = ctx.RegisterResource("aws:s3/bucketPolicy:BucketPolicy", name+"-policy", policyProps, &s3.BucketPolicy{}, pulumi.Parent(b))
	if err != nil {
		return nil, err
	}

	// ── 5. Versioning ──────────────────────────────────
	// Tier 1: always on in production. Near-free (storage overhead only).
	// Tier 2: opt-in for non-production via versioning: true.
	if needsVersioning {
		vProps := hatch.Merge(args.Hatch["versioning"], pulumi.Map{
			"bucket": res.ID(),
			"versioningConfiguration": pulumi.Map{
				"status": pulumi.String("Enabled"),
			},
		})
		err = ctx.RegisterResource("aws:s3/bucketVersioningV2:BucketVersioningV2", name+"-ver", vProps, &s3.BucketVersioningV2{}, pulumi.Parent(b))
		if err != nil {
			return nil, err
		}
	}

	// ── 6. Lifecycle rules ─────────────────────────────
	// Builds lifecycle rules from three independent concerns:
	//   a) Multipart upload cleanup — always on, pure hygiene, free saving
	//   b) Intelligent-Tiering OR StorageTransition presets — skipped if
	//      user provides hatch["lifecycleRules"] (their rules replace)
	//   c) Version expiry — when versionRetention is set and versioning is on

	lifecycleRules := pulumi.Array{
		// ── a) Multipart upload cleanup ────────────────
		// Tier 1: always on. Cleans up abandoned multipart uploads after
		// 7 days. Prevents silent storage cost accumulation. Free.
		pulumi.Map{
			"id":     pulumi.String("anvil-cleanup-incomplete-uploads"),
			"status": pulumi.String("Enabled"),
			"abortIncompleteMultipartUpload": pulumi.Map{
				"daysAfterInitiation": pulumi.Int(7),
			},
		},
	}

	if !hasCustomLifecycle {
		if args.StorageTransition != "" {
			// ── b1) StorageTransition preset ───────────────
			// User has chosen explicit transition points.
			// Standard → IA → Glacier based on preset.
			var iaDays, glacierDays int
			switch args.StorageTransition {
			case "30d":
				iaDays, glacierDays = 30, 90
			case "90d":
				iaDays, glacierDays = 90, 365
			case "1y":
				iaDays, glacierDays = 365, 730
			}

			lifecycleRules = append(lifecycleRules, pulumi.Map{
				"id":     pulumi.String("anvil-storage-transition"),
				"status": pulumi.String("Enabled"),
				"transitions": pulumi.Array{
					pulumi.Map{
						"days":         pulumi.Int(iaDays),
						"storageClass": pulumi.String("STANDARD_IA"),
					},
					pulumi.Map{
						"days":         pulumi.Int(glacierDays),
						"storageClass": pulumi.String("GLACIER"),
					},
				},
			})
		} else {
			// ── b2) Intelligent-Tiering default ────────────
			// Applied when no custom lifecycle and no StorageTransition preset.
			// Automatically moves objects >= 128KB between frequent and
			// infrequent access tiers based on actual access patterns.
			// Objects < 128KB excluded — monitoring fee without benefit.
			lifecycleRules = append(lifecycleRules, pulumi.Map{
				"id":     pulumi.String("anvil-intelligent-tiering"),
				"status": pulumi.String("Enabled"),
				"filter": pulumi.Map{
					"objectSizeGreaterThan": pulumi.Int(131072), // 128KB
				},
				"transitions": pulumi.Array{
					pulumi.Map{
						"days":         pulumi.Int(0),
						"storageClass": pulumi.String("INTELLIGENT_TIERING"),
					},
				},
			})
		}
	}

	// ── c) Version expiry ──────────────────────────────
	// Applied whenever versionRetention is set and versioning is active.
	// Prevents unbounded storage growth from accumulated old versions.
	// Also cleans up expired delete markers.
	// Independent of custom lifecycle — always appended if set.
	if versionRetentionDays > 0 && needsVersioning {
		lifecycleRules = append(lifecycleRules, pulumi.Map{
			"id":     pulumi.String("anvil-version-expiry"),
			"status": pulumi.String("Enabled"),
			"noncurrentVersionExpiration": pulumi.Map{
				"noncurrentDays": pulumi.Int(versionRetentionDays),
			},
			"expiredObjectDeleteMarker": pulumi.Bool(true),
		})
	}

	lifecycleProps := hatch.Merge(args.Hatch["lifecycleRules"], pulumi.Map{
		"bucket": res.ID(),
		"rules":  lifecycleRules,
	})
	err = ctx.RegisterResource("aws:s3/bucketLifecycleConfigurationV2:BucketLifecycleConfigurationV2", name+"-lifecycle", lifecycleProps, &s3.BucketLifecycleConfigurationV2{}, pulumi.Parent(b))
	if err != nil {
		return nil, err
	}

	// ── 7. Server access logging ───────────────────────
	// Tier 2: opt-in via logging: true. Logs to the centralised logging
	// bucket created during `anvil bootstrap`. Falls back to self if
	// loggingBucket is not set. Incurs storage cost on the logging bucket.
	if args.Logging {
		var targetBucket pulumi.StringInput
		if loggingBucket != "" {
			targetBucket = pulumi.String(loggingBucket)
		} else {
			targetBucket = res.ID().ToStringOutput()
		}

		logProps := hatch.Merge(args.Hatch["logging"], pulumi.Map{
			"bucket":       res.ID(),
			"targetBucket": targetBucket,
			"targetPrefix": pulumi.Sprintf("s3-access-logs/%s/", physicalName),
		})
		err = ctx.RegisterResource("aws:s3/bucketLoggingV2:BucketLoggingV2", name+"-log", logProps, &s3.BucketLoggingV2{}, pulumi.Parent(b))
		if err != nil {
			return nil, err
		}
	}

	// ── 8. Object Lock / Retention ─────────────────────
	// Tier 2: opt-in via retention: N (days). Enables Object Lock in
	// COMPLIANCE mode — objects cannot be deleted or overwritten until
	// the retention period expires. Required by PCI-DSS (365), FedRAMP
	// (1095), HIPAA (2190). Incurs storage cost.
	// Note: Object Lock must be enabled at bucket creation. Changing this
	// value after initial deploy requires bucket replacement.
	if args.Retention > 0 {
		lockProps := hatch.Merge(args.Hatch["objectLockConfiguration"], pulumi.Map{
			"bucket":            res.ID(),
			"objectLockEnabled": pulumi.String("Enabled"),
			"rule": pulumi.Map{
				"defaultRetention": pulumi.Map{
					"mode": pulumi.String("COMPLIANCE"),
					"days": pulumi.Int(args.Retention),
				},
			},
		})
		err = ctx.RegisterResource("aws:s3/bucketObjectLockConfigurationV2:BucketObjectLockConfigurationV2", name+"-lock", lockProps, &s3.BucketObjectLockConfigurationV2{}, pulumi.Parent(b))
		if err != nil {
			return nil, err
		}
	}

	b.BucketName = res.Bucket
	b.Arn = res.Arn

	ctx.RegisterResourceOutputs(b, pulumi.Map{
		"bucketName": res.Bucket,
		"arn":        res.Arn,
	})

	return b, nil
}
