package vpcsg

import (
	"sync"
	"testing"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type mocks struct {
	mu        sync.Mutex
	resources []pulumi.MockResourceArgs
}

func (m *mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resources = append(m.resources, args)
	return args.Name + "_id", args.Inputs, nil
}

func (m *mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

// allowAllOutbound must recreate exactly the rule AWS adds to a new SG:
// all protocols, all ports, to 0.0.0.0/0.
func TestAllowAllOutboundMatchesAwsDefaultRule(t *testing.T) {
	m := &mocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		sg, err := ec2.NewSecurityGroup(ctx, "fn-sg", &ec2.SecurityGroupArgs{})
		if err != nil {
			return err
		}
		return AllowAllOutbound(ctx, "fn", sg.ID().ToStringOutput(), sg)
	}, pulumi.WithMocks("project", "stack", m))
	if err != nil {
		t.Fatal(err)
	}

	var rules []pulumi.MockResourceArgs
	for _, r := range m.resources {
		if r.TypeToken == "aws:vpc/securityGroupEgressRule:SecurityGroupEgressRule" {
			rules = append(rules, r)
		}
	}
	if len(rules) != 1 {
		t.Fatalf("got %d egress rules, want 1", len(rules))
	}
	in := rules[0].Inputs
	if got := in["ipProtocol"].StringValue(); got != "-1" {
		t.Errorf("ipProtocol = %q, want -1 (all traffic)", got)
	}
	if got := in["cidrIpv4"].StringValue(); got != "0.0.0.0/0" {
		t.Errorf("cidrIpv4 = %q, want 0.0.0.0/0", got)
	}
	if _, ok := in["fromPort"]; ok {
		t.Error("fromPort must be unset for protocol -1")
	}
	if got := in["securityGroupId"].StringValue(); got != "fn-sg_id" {
		t.Errorf("securityGroupId = %q, want fn-sg_id", got)
	}
}
