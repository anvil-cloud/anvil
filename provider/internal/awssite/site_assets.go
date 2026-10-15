package awssite

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/s3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// UploadSiteAssets walks staticDir and uploads every file to the given S3 bucket.
// Cache headers are set per file type:
//   - Immutable assets (/_app/immutable/*, /_astro/*, etc): max-age=31536000, immutable
//   - Everything else: s-maxage=86400 with stale-while-revalidate
//
// The immutablePrefixes param lets each framework declare which path prefixes
// contain content-hashed assets eligible for long-term caching.
//
// Each object's resource name comes from its key, not its content, so a changed
// file (a new robots.txt) is updated in place. Naming by content made every edit a
// create-then-delete of two resources pointing at the same key, and the delete
// removed the object the create had just uploaded.
func UploadSiteAssets(ctx *pulumi.Context, parent pulumi.Resource, name string, bucket *s3.Bucket, staticDir string, immutablePrefixes []string) error {
	return filepath.Walk(staticDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, _ := filepath.Rel(staticDir, path)
		key := filepath.ToSlash(relPath)

		contentType := mime.TypeByExtension(filepath.Ext(path))
		if contentType == "" {
			contentType = "application/octet-stream"
		}

		cacheControl := "public, max-age=0, s-maxage=86400, stale-while-revalidate=8640"
		for _, prefix := range immutablePrefixes {
			if strings.HasPrefix(key, prefix) {
				cacheControl = "public, max-age=31536000, immutable"
				break
			}
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		resourceName, legacyName := assetResourceNames(name, key, content)

		return ctx.RegisterResource("aws:s3/bucketObjectv2:BucketObjectv2", resourceName, pulumi.Map{
			"bucket":       bucket.Bucket,
			"key":          pulumi.String(key),
			"source":       pulumi.NewFileAsset(path),
			"contentType":  pulumi.String(contentType),
			"cacheControl": pulumi.String(cacheControl),
		}, &s3.BucketObjectv2{}, pulumi.Parent(parent),
			// Stacks deployed before key-based names hold this object under its content-based
			// name. The alias adopts it instead of creating a new resource and deleting the old
			// one (which would remove the object). It matches when the file is unchanged since
			// that deploy; a file that also changed in the same deploy needs a refresh + deploy.
			pulumi.Aliases([]pulumi.Alias{{Name: pulumi.String(legacyName)}}))
	})
}

// assetResourceNames returns the resource name for the object at key, and the
// content-based name earlier versions used for it. The current name hashes the key
// (stable across edits, and unique even when two keys sanitise to the same text).
func assetResourceNames(name, key string, content []byte) (current, legacy string) {
	return assetName(name, key, []byte(key)), assetName(name, key, content)
}

func assetName(name, key string, hashed []byte) string {
	sum := sha256.Sum256(hashed)
	return fmt.Sprintf("%s-asset-%s-%s", name, SanitiseResourceName(key), hex.EncodeToString(sum[:8]))
}
