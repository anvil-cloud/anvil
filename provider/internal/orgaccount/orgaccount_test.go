package orgaccount

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTrustPolicy_ExactPrincipalsSorted(t *testing.T) {
	got, err := TrustPolicy([]string{
		"arn:aws:iam::111111111111:role/b",
		"arn:aws:iam::111111111111:role/a",
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Statement []struct {
			Effect    string
			Action    string
			Principal struct{ AWS []string }
		}
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	st := doc.Statement[0]
	if st.Effect != "Allow" || st.Action != "sts:AssumeRole" {
		t.Errorf("unexpected statement %+v", st)
	}
	if want := []string{"arn:aws:iam::111111111111:role/a", "arn:aws:iam::111111111111:role/b"}; strings.Join(st.Principal.AWS, ",") != strings.Join(want, ",") {
		t.Errorf("principals %v, want %v", st.Principal.AWS, want)
	}
	if strings.Contains(got, "*") || strings.Contains(got, ":root") {
		t.Errorf("trust must name exact principals, got %s", got)
	}
}
