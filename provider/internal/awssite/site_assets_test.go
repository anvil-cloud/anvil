package awssite

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
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
