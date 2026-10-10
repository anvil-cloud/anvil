package aws

import (
	"testing"

	"github.com/anvil-cloud/anvil/provider/sites"
)

// Every site component must expose the shared hosting inputs so every
// framework gets the same protection. New site components copy this test.
func TestExposesHostingInputs(t *testing.T) {
	if missing := sites.MissingHostingInputs(SvelteKitSiteArgs{}); len(missing) > 0 {
		t.Errorf("SvelteKitSiteArgs is missing shared hosting inputs %v (see sites.HostingInputNames)", missing)
	}
}
