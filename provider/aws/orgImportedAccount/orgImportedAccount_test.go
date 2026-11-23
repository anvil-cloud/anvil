package orgImportedAccount

import (
	"context"
	"strings"
	"testing"
)

func TestBootstrap_DefaultsOn(t *testing.T) {
	off := false
	if !(Args{}).bootstrap() {
		t.Error("bootstrap should default to true")
	}
	if (Args{Bootstrap: &off}).bootstrap() {
		t.Error("bootstrap: false should turn it off")
	}
}

func TestReconcile_ValidatesBeforeAWS(t *testing.T) {
	r := &OrgImportedAccount{}
	cases := map[string]Args{
		"must be 12 digits":           {AccountID: "12345", OuID: "ou-x", OrgName: "o"},
		"needs at least one deployer": {AccountID: "123456789012", OuID: "ou-x", OrgName: "o"},
	}
	for want, in := range cases {
		if _, err := r.reconcile(context.Background(), in, nil, true); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: got %v, want error containing %q", in, err, want)
		}
	}
}
