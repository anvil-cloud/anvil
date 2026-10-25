package dsql

import (
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// recordingMocks records every resource registered under the mocked context.
type recordingMocks struct {
	mu        sync.Mutex
	resources []pulumi.MockResourceArgs
}

func (m *recordingMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resources = append(m.resources, args)
	return args.Name + "_id", args.Inputs, nil
}

func (m *recordingMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func (m *recordingMocks) byType(token string) []pulumi.MockResourceArgs {
	var out []pulumi.MockResourceArgs
	for _, r := range m.resources {
		if r.TypeToken == token {
			out = append(out, r)
		}
	}
	return out
}

// The endpoint SG must let its members both send and receive 5432. This is
// the only egress a VPC compute gets toward DSQL, for Go and SDK grantConnect
// alike, so a missing egress rule means no connectivity.
func TestWireEndpointSecurityGroupIsSelfReferencingOn5432(t *testing.T) {
	const sgID = "sg-0123456789abcdef0"
	mocks := &recordingMocks{}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return wireEndpointSecurityGroup(ctx, "db", "ap-southeast-2", pulumi.String(sgID).ToStringOutput(), nil)
	}, pulumi.WithMocks("project", "stack", mocks))
	if err != nil {
		t.Fatal(err)
	}

	for _, token := range []string{
		"aws:vpc/securityGroupIngressRule:SecurityGroupIngressRule",
		"aws:vpc/securityGroupEgressRule:SecurityGroupEgressRule",
	} {
		rules := mocks.byType(token)
		if len(rules) != 1 {
			t.Fatalf("%s: got %d rules, want 1", token, len(rules))
		}
		in := rules[0].Inputs
		if got := in["securityGroupId"].StringValue(); got != sgID {
			t.Errorf("%s: securityGroupId = %q, want %q", token, got, sgID)
		}
		if got := in["referencedSecurityGroupId"].StringValue(); got != sgID {
			t.Errorf("%s: referencedSecurityGroupId = %q, want self (%q)", token, got, sgID)
		}
		if in["fromPort"].NumberValue() != 5432 || in["toPort"].NumberValue() != 5432 {
			t.Errorf("%s: ports = %v-%v, want 5432", token, in["fromPort"], in["toPort"])
		}
		if got := in["ipProtocol"].StringValue(); got != "tcp" {
			t.Errorf("%s: ipProtocol = %q, want tcp", token, got)
		}
		if _, ok := in["cidrIpv4"]; ok {
			t.Errorf("%s: unexpected cidrIpv4 — rule must be SG-scoped", token)
		}
	}

	// Distinct names per region so multi-region clusters don't collide.
	names := map[string]bool{}
	for _, r := range mocks.resources {
		if names[r.Name] {
			t.Errorf("duplicate resource name %q", r.Name)
		}
		names[r.Name] = true
	}
}
