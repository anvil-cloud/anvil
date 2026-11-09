package dsql

import (
	"strings"
	"testing"
)

// A VPC lives in one region, so VPC endpoints cannot be combined with
// multiRegion. With hasNat no endpoints are created, so that is allowed.
func TestValidateVpc(t *testing.T) {
	multi := &DSQLMultiRegionArgs{Regions: []string{"us-east-1", "us-east-2"}, WitnessRegion: "us-west-2"}

	cases := []struct {
		name    string
		args    DSQLArgs
		wantErr bool
	}{
		{"no vpc", DSQLArgs{}, false},
		{"vpc single region", DSQLArgs{Vpc: &DSQLVpcArgs{}}, false},
		{"multiRegion without vpc", DSQLArgs{MultiRegion: multi}, false},
		{"multiRegion with vpc and hasNat", DSQLArgs{MultiRegion: multi, Vpc: &DSQLVpcArgs{HasNat: true}}, false},
		{"multiRegion with vpc endpoints", DSQLArgs{MultiRegion: multi, Vpc: &DSQLVpcArgs{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVpc(tc.args)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "multiRegion") {
					t.Fatalf("want multiRegion error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
