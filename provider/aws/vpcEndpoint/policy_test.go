package vpcendpoint

import (
	"encoding/json"
	"testing"
)

// Calls through an endpoint are signed by the caller's role session, not by a
// service principal, so the policy must use Principal "*" and limit callers
// with aws:PrincipalAccount instead.
func TestDefaultEndpointPolicyIsAccountScoped(t *testing.T) {
	raw, err := buildDefaultEndpointPolicy("123456789012")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Statement []struct {
			Effect    string                       `json:"Effect"`
			Principal interface{}                  `json:"Principal"`
			Action    []string                     `json:"Action"`
			Condition map[string]map[string]string `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("policy is not valid JSON: %v", err)
	}
	if len(doc.Statement) != 1 {
		t.Fatalf("got %d statements, want 1", len(doc.Statement))
	}
	st := doc.Statement[0]
	if st.Principal != "*" {
		t.Errorf("Principal = %v, want \"*\"", st.Principal)
	}
	if got := st.Condition["StringEquals"]["aws:PrincipalAccount"]; got != "123456789012" {
		t.Errorf("aws:PrincipalAccount = %q, want 123456789012", got)
	}
}
