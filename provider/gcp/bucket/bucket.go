package storagebucket

import (
	"github.com/anvil-cloud/anvil/provider/internal/transform"
	"github.com/pulumi/pulumi-gcp/sdk/v8/go/gcp/storage"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type StorageBucketArgs struct {
	DataClassification string                            `pulumi:"dataClassification"`
	Location           string                            `pulumi:"location"`
	Transform          map[string]map[string]interface{} `pulumi:"transform,optional"`
}

type StorageBucket struct {
	pulumi.ResourceState

	BucketName pulumi.StringOutput `pulumi:"bucketName"`
	Url        pulumi.StringOutput `pulumi:"url"`
}

func (sb *StorageBucket) Annotate(a infer.Annotator) {
	a.Describe(&sb, "An Anvil-managed GCP Storage Bucket with sensible security defaults.")
}

func NewBucket(ctx *pulumi.Context, name string, args StorageBucketArgs, opts ...pulumi.ResourceOption) (*StorageBucket, error) {
	sb := &StorageBucket{}
	err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, sb, opts...)
	if err != nil {
		return nil, err
	}

	isSensitive := args.DataClassification == "sensitive" || args.DataClassification == "restricted"

	// Storage Bucket
	bucketProps := transform.MergeTransform(args.Transform["storageBucket"], pulumi.Map{
		"location":                 pulumi.String(args.Location),
		"forceDestroy":             pulumi.Bool(true),
		"uniformBucketLevelAccess": pulumi.Bool(true),
		"publicAccessPrevention":   pulumi.String("enforced"),
		"labels": pulumi.StringMap{
			"managed-by":          pulumi.String("anvil"),
			"data-classification": pulumi.String(args.DataClassification),
		},
	})

	// Enable versioning for sensitive data
	if isSensitive {
		bucketProps["versioning"] = pulumi.Map{
			"enabled": pulumi.Bool(true),
		}
	}

	res := &storage.Bucket{}
	err = ctx.RegisterResource("gcp:storage/bucket:Bucket", name, bucketProps, res, pulumi.Parent(sb))
	if err != nil {
		return nil, err
	}

	sb.BucketName = res.Name
	sb.Url = res.Url

	ctx.RegisterResourceOutputs(sb, pulumi.Map{
		"bucketName": res.Name,
		"url":        res.Url,
	})

	return sb, nil
}
