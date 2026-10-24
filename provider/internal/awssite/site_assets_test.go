package awssite

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/s3"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestAssetResourceNamesStayStableAcrossEdits(t *testing.T) {
	before, _ := assetResourceNames("web", "robots.txt", []byte("User-agent: *\n"))
	after, _ := assetResourceNames("web", "robots.txt", []byte("User-agent: *\nDisallow: /private\n"))
	if before != after {
		t.Fatalf("editing a file changed its resource name (%q → %q); it would be replaced, deleting the object", before, after)
	}
}

func TestAssetResourceNamesLegacyMatchesContentBasedName(t *testing.T) {
	content := []byte("User-agent: *\n")
	_, legacy := assetResourceNames("web", "robots.txt", content)

	// The name earlier versions registered: content hash, so existing stacks can be adopted by alias.
	sum := sha256.Sum256(content)
	want := "web-asset-robots-txt-" + hex.EncodeToString(sum[:8])
	if legacy != want {
		t.Fatalf("legacy name = %q, want %q", legacy, want)
	}
}

func TestAssetResourceNamesUniquePerKey(t *testing.T) {
	// Both keys sanitise to "a-b-txt"; the key hash keeps their resource names apart.
	a, _ := assetResourceNames("web", "a_b.txt", []byte("x"))
	b, _ := assetResourceNames("web", "a-b.txt", []byte("x"))
	if a == b {
		t.Fatalf("different keys share resource name %q", a)
	}
}

type keyRecorder struct {
	mu   sync.Mutex
	keys []string
}

func (r *keyRecorder) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	if args.TypeToken == "aws:s3/bucketObjectv2:BucketObjectv2" {
		r.mu.Lock()
		r.keys = append(r.keys, args.Inputs["key"].StringValue())
		r.mu.Unlock()
	}
	return args.Name + "_id", args.Inputs, nil
}

func (r *keyRecorder) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func TestUploadSiteAssetsSkipsPrecompressedCopiesWithSource(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{
		"robots.txt",
		"_app/immutable/app.js", "_app/immutable/app.js.br", "_app/immutable/app.js.gz",
		"index.html", "index.html.gz",
		"data.json.br", // no data.json beside it: kept
		"archive.gz",   // no archive beside it: kept
	} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rec := &keyRecorder{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		bucket, err := s3.NewBucket(ctx, "assets", nil)
		if err != nil {
			return err
		}
		return UploadSiteAssets(ctx, bucket, "web", bucket, dir, []string{"_app/immutable/"})
	}, pulumi.WithMocks("project", "stack", rec))
	if err != nil {
		t.Fatal(err)
	}

	sort.Strings(rec.keys)
	want := []string{"_app/immutable/app.js", "archive.gz", "data.json.br", "index.html", "robots.txt"}
	if !slices.Equal(rec.keys, want) {
		t.Fatalf("uploaded keys = %v, want %v", rec.keys, want)
	}
}
