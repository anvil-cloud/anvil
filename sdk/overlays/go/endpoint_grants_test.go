package anvil

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type grantMocks struct {
	mu        sync.Mutex
	resources []pulumi.MockResourceArgs
}

func (m *grantMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, args)
	m.mu.Unlock()
	return args.Name + "_id", args.Inputs, nil
}

func (m *grantMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func (m *grantMocks) byType(token string) []pulumi.MockResourceArgs {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []pulumi.MockResourceArgs
	for _, r := range m.resources {
		if r.TypeToken == token {
			out = append(out, r)
		}
	}
	return out
}

// fakeTarget is a VPC compute target with fixed outputs. Anvil's own
// components are remote components, whose outputs don't resolve under Go
// mocks, so the tests use plain AWS resources and this target instead.
type fakeTarget struct{ name, sg string }

func (f fakeTarget) GrantName() string { return f.name }
func (f fakeTarget) GrantRoleArn() pulumi.StringOutput {
	return pulumi.String("arn:aws:iam::123456789012:role/" + f.name).ToStringOutput()
}
func (f fakeTarget) GrantSecurityGroupId() pulumi.StringOutput {
	return pulumi.String(f.sg).ToStringOutput()
}

// fakeEndpoint returns a GrantEndpoint with fixed outputs, plus a plain SG to
// act as the granting resource.
func fakeEndpoint(ctx *pulumi.Context, name, service string) (*GrantEndpoint, pulumi.Resource, error) {
	parent, err := ec2.NewSecurityGroup(ctx, name, &ec2.SecurityGroupArgs{})
	if err != nil {
		return nil, nil, err
	}
	return &GrantEndpoint{
		Name:            name,
		EndpointId:      pulumi.String("vpce-" + name).ToStringOutput(),
		SecurityGroupId: pulumi.String("sg-" + name).ToStringOutput(),
		Service:         pulumi.String(service).ToStringOutput(),
	}, parent, nil
}

const (
	ingressToken = "aws:vpc/securityGroupIngressRule:SecurityGroupIngressRule"
	egressToken  = "aws:vpc/securityGroupEgressRule:SecurityGroupEgressRule"
	policyToken  = "aws:iam/rolePolicy:RolePolicy"
)

// Two grants from one Lambda through one endpoint: one ingress + one egress
// rule (deduped), both SG-to-SG on 443, and every IAM statement limited to
// the endpoint with aws:SourceVpce.
func TestEndpointGrantWiresOneWayRulesAndSourceVpce(t *testing.T) {
	mocks := &grantMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		ep, granting, err := fakeEndpoint(ctx, "sqs1", "sqs")
		if err != nil {
			return err
		}
		target := fakeTarget{"proc1", "sg-proc1"}
		opts := &GrantOptions{Endpoint: ep}
		arn := pulumi.String("arn:aws:sqs:ap-southeast-2:123456789012:orders")
		if err := CreateGrant(ctx, granting, "orders-proc1-send", target, []string{"sqs:SendMessage"}, []pulumi.StringInput{arn}, opts); err != nil {
			return err
		}
		return CreateGrant(ctx, granting, "retry-proc1-send", target, []string{"sqs:SendMessage"}, []pulumi.StringInput{arn}, opts)
	}, pulumi.WithMocks("project", "stack", mocks))
	if err != nil {
		t.Fatal(err)
	}

	ingress, egress := mocks.byType(ingressToken), mocks.byType(egressToken)
	if len(ingress) != 1 || len(egress) != 1 {
		t.Fatalf("got %d ingress / %d egress rules, want 1 / 1", len(ingress), len(egress))
	}
	checkRule := func(kind string, in resource.PropertyMap, sg, ref string) {
		if got := in["securityGroupId"].StringValue(); got != sg {
			t.Errorf("%s securityGroupId = %q, want %q", kind, got, sg)
		}
		if got := in["referencedSecurityGroupId"].StringValue(); got != ref {
			t.Errorf("%s referencedSecurityGroupId = %q, want %q", kind, got, ref)
		}
		if in["fromPort"].NumberValue() != 443 || in["toPort"].NumberValue() != 443 {
			t.Errorf("%s ports = %v-%v, want 443", kind, in["fromPort"], in["toPort"])
		}
		if _, ok := in["cidrIpv4"]; ok {
			t.Errorf("%s has cidrIpv4; must be SG-to-SG", kind)
		}
	}
	checkRule("ingress", ingress[0].Inputs, "sg-sqs1", "sg-proc1")
	checkRule("egress", egress[0].Inputs, "sg-proc1", "sg-sqs1")

	policies := mocks.byType(policyToken)
	if len(policies) != 2 {
		t.Fatalf("got %d role policies, want 2", len(policies))
	}
	for _, p := range policies {
		var doc struct {
			Statement []struct {
				Condition map[string]map[string]string `json:"Condition"`
			} `json:"Statement"`
		}
		if err := json.Unmarshal([]byte(p.Inputs["policy"].StringValue()), &doc); err != nil {
			t.Fatal(err)
		}
		if got := doc.Statement[0].Condition["StringEquals"]["aws:SourceVpce"]; got != "vpce-sqs1" {
			t.Errorf("%s aws:SourceVpce = %q, want vpce-sqs1", p.Name, got)
		}
	}
}

// Without an endpoint, grants are unchanged: no SG rules, no Condition.
func TestGrantWithoutEndpointHasNoNetworkOrCondition(t *testing.T) {
	mocks := &grantMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		parent, err := ec2.NewSecurityGroup(ctx, "q2", &ec2.SecurityGroupArgs{})
		if err != nil {
			return err
		}
		return CreateGrant(ctx, parent, "q-proc2-send", fakeTarget{"proc2", "sg-proc2"}, []string{"sqs:SendMessage"},
			[]pulumi.StringInput{pulumi.String("arn:aws:sqs:ap-southeast-2:123456789012:q")}, nil)
	}, pulumi.WithMocks("project", "stack", mocks))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(mocks.byType(ingressToken)) + len(mocks.byType(egressToken)); n != 0 {
		t.Errorf("got %d SG rules, want 0", n)
	}
	policies := mocks.byType(policyToken)
	if len(policies) != 1 || strings.Contains(policies[0].Inputs["policy"].StringValue(), "Condition") {
		t.Errorf("want one policy without a Condition, got %v", policies)
	}
}

// An endpoint for another service is rejected.
func TestEndpointServiceMismatchFails(t *testing.T) {
	mocks := &grantMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		ep, granting, err := fakeEndpoint(ctx, "kms3", "kms")
		if err != nil {
			return err
		}
		return CreateGrant(ctx, granting, "q-proc3-send", fakeTarget{"proc3", "sg-proc3"}, []string{"sqs:SendMessage"},
			[]pulumi.StringInput{pulumi.String("arn:aws:sqs:ap-southeast-2:123456789012:q")},
			&GrantOptions{Endpoint: ep})
	}, pulumi.WithMocks("project", "stack", mocks))
	if err == nil || !strings.Contains(err.Error(), "routes \"kms\"") {
		t.Fatalf("want service mismatch error, got %v", err)
	}
}

func TestEndpointServiceForActions(t *testing.T) {
	cases := map[string]struct {
		actions []string
		want    string
		wantErr bool
	}{
		"sqs":        {[]string{"sqs:SendMessage", "sqs:ReceiveMessage"}, "sqs", false},
		"kinesis":    {[]string{"kinesis:GetRecords"}, "kinesis-streams", false},
		"mixed":      {[]string{"sqs:SendMessage", "kms:Decrypt"}, "", true},
		"no prefix":  {[]string{"SendMessage"}, "", true},
		"no actions": {nil, "", true},
	}
	for name, tc := range cases {
		got, err := endpointServiceForActions(tc.actions)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q, err=%v", name, got, err, tc.want, tc.wantErr)
		}
	}
}

// Network only: the rule pair, no policy.
func TestGrantEndpointAccessNetworkOnly(t *testing.T) {
	mocks := &grantMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		ep, _, err := fakeEndpoint(ctx, "ecr4", "ecr.api")
		if err != nil {
			return err
		}
		return GrantEndpointAccess(ctx, ep, fakeTarget{"proc4", "sg-proc4"}, nil)
	}, pulumi.WithMocks("project", "stack", mocks))
	if err != nil {
		t.Fatal(err)
	}
	if len(mocks.byType(ingressToken)) != 1 || len(mocks.byType(egressToken)) != 1 {
		t.Fatalf("want 1 ingress + 1 egress rule")
	}
	if n := len(mocks.byType(policyToken)); n != 0 {
		t.Errorf("got %d policies, want 0", n)
	}
}

// With actions: a SourceVpce-limited policy, and the rule pair shared with a
// resource grant through the same endpoint.
func TestGrantEndpointAccessWithActions(t *testing.T) {
	mocks := &grantMocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		ep, _, err := fakeEndpoint(ctx, "secrets5", "secretsmanager")
		if err != nil {
			return err
		}
		target := fakeTarget{"proc5", "sg-proc5"}
		if err := GrantEndpointAccess(ctx, ep, target, nil); err != nil {
			return err
		}
		return GrantEndpointAccess(ctx, ep, target, &EndpointAccessOptions{
			Actions:   []string{"secretsmanager:GetSecretValue"},
			Resources: []pulumi.StringInput{pulumi.String("arn:aws:secretsmanager:ap-southeast-2:123456789012:secret:db")},
		})
	}, pulumi.WithMocks("project", "stack", mocks))
	if err != nil {
		t.Fatal(err)
	}
	if len(mocks.byType(ingressToken)) != 1 || len(mocks.byType(egressToken)) != 1 {
		t.Fatalf("rule pair not deduped")
	}
	policies := mocks.byType(policyToken)
	if len(policies) != 1 {
		t.Fatalf("got %d policies, want 1", len(policies))
	}
	doc := policies[0].Inputs["policy"].StringValue()
	if !strings.Contains(doc, `"aws:SourceVpce":"vpce-secrets5"`) || !strings.Contains(doc, "secretsmanager:GetSecretValue") {
		t.Errorf("unexpected policy %s", doc)
	}
}

// Actions from another service, or the same actions twice, are rejected.
func TestGrantEndpointAccessRejects(t *testing.T) {
	run := func(f func(ctx *pulumi.Context) error) error {
		return pulumi.RunErr(f, pulumi.WithMocks("project", "stack", &grantMocks{}))
	}
	res := []pulumi.StringInput{pulumi.String("*")}
	err := run(func(ctx *pulumi.Context) error {
		ep, _, err := fakeEndpoint(ctx, "secrets6", "secretsmanager")
		if err != nil {
			return err
		}
		return GrantEndpointAccess(ctx, ep, fakeTarget{"proc6", "sg-proc6"}, &EndpointAccessOptions{Actions: []string{"kms:Decrypt"}, Resources: res})
	})
	if err == nil || !strings.Contains(err.Error(), "cannot carry kms:Decrypt") {
		t.Errorf("want service error, got %v", err)
	}
	err = run(func(ctx *pulumi.Context) error {
		ep, _, err := fakeEndpoint(ctx, "kms7", "kms")
		if err != nil {
			return err
		}
		o := &EndpointAccessOptions{Actions: []string{"kms:Decrypt"}, Resources: res}
		if err := GrantEndpointAccess(ctx, ep, fakeTarget{"proc7", "sg-proc7"}, o); err != nil {
			return err
		}
		return GrantEndpointAccess(ctx, ep, fakeTarget{"proc7", "sg-proc7"}, o)
	})
	if err == nil || !strings.Contains(err.Error(), "already granted") {
		t.Errorf("want duplicate error, got %v", err)
	}
	err = run(func(ctx *pulumi.Context) error {
		ep, _, err := fakeEndpoint(ctx, "kms8", "kms")
		if err != nil {
			return err
		}
		return GrantEndpointAccess(ctx, ep, fakeTarget{"proc8", "sg-proc8"}, &EndpointAccessOptions{Actions: []string{"kms:Decrypt"}})
	})
	if err == nil || !strings.Contains(err.Error(), "both Actions and Resources") {
		t.Errorf("want actions/resources error, got %v", err)
	}
}

func TestShortHashMatchesOtherRuntimes(t *testing.T) {
	// FNV-1a 32 of "secretsmanager:GetSecretValue", as computed by the TS and Python runtimes.
	if got := shortHash("secretsmanager:GetSecretValue"); got != "df5c357c" {
		t.Fatalf("shortHash = %q, want df5c357c (TS/Python value)", got)
	}
}
