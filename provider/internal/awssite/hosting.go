package awssite

import (
	"fmt"
	"strings"

	"github.com/anvil-cloud/anvil/provider/sites"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudfront"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lambda"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// HostingInputs are the shared hosting inputs every site component exposes
// (see sites.HostingInputNames).
type HostingInputs struct {
	Protection       string
	Waf              *sites.SiteWafArgs
	OriginProtection bool
	SecurityHeaders  *sites.SiteSecurityHeadersArgs

	// Transform is the component's transform map; Hosting reads its
	// "responseHeadersPolicy" entry.
	Transform map[string]map[string]interface{}
}

// Hosting is the shared hosting layer for every site component: server
// protection (none / oac / edge-oac), WAF attachment, origin protection and
// security headers. A framework component creates it once, then:
//
//	h, _ := awssite.NewHosting(ctx, site, name, inputs)   // before the server Lambda
//	url, _ := h.CreateFunctionURL(fn)                      // after the Lambda
//	h.ApplyTo(&cfArgs)                                     // before the distribution
//	h.GrantAccess(fn, distribution.Arn)                    // after the distribution
//
// so every framework gets identical protection without re-implementing it.
type Hosting struct {
	ctx    *pulumi.Context
	parent pulumi.Resource
	name   string

	// Mode is the resolved protection mode.
	Mode string

	// OriginSecret is the origin-protection secret (empty when off).
	OriginSecret pulumi.StringOutput

	lambdaOAC       *cloudfront.OriginAccessControl
	webACLArn       pulumi.StringOutput
	originGuardArn  pulumi.StringOutput
	edgeSignerArn   pulumi.StringOutput
	headersPolicyID pulumi.StringOutput
}

// NewHosting resolves the hosting inputs, reports any defaults applied, and
// creates the resources that don't depend on the server Lambda.
func NewHosting(ctx *pulumi.Context, parent pulumi.Resource, name string, in HostingInputs) (*Hosting, error) {
	h := &Hosting{ctx: ctx, parent: parent, name: name, OriginSecret: pulumi.String("").ToStringOutput()}
	logArgs := &pulumi.LogArgs{Resource: parent}

	// ── Protection mode ──
	resolved, err := ResolveProtection(in.Protection, in.Waf != nil, in.OriginProtection)
	if err != nil {
		return nil, err
	}
	h.Mode = resolved.Mode
	if resolved.Notice != "" {
		ctx.Log.Info(resolved.Notice, logArgs)
	}
	if resolved.Warning != "" {
		ctx.Log.Warn(resolved.Warning, logArgs)
	}

	// ── Security headers ──
	headers, err := ResolveSecurityHeaders(in.SecurityHeaders)
	if err != nil {
		return nil, err
	}
	if headers.Enabled {
		h.headersPolicyID, err = CreateSecurityHeadersPolicy(ctx, parent, name, headers, in.Transform["responseHeadersPolicy"])
		if err != nil {
			return nil, err
		}
	}

	// ── Lambda OAC: CloudFront signs requests to the AWS_IAM Function URL ──
	if IsIAMProtected(h.Mode) {
		h.lambdaOAC = &cloudfront.OriginAccessControl{}
		err = ctx.RegisterResource("aws:cloudfront/originAccessControl:OriginAccessControl", name+"-lambda-oac", pulumi.Map{
			"name":                          pulumi.Sprintf("%s-lambda-oac", name),
			"originAccessControlOriginType": pulumi.String("lambda"),
			"signingBehavior":               pulumi.String("always"),
			"signingProtocol":               pulumi.String("sigv4"),
		}, h.lambdaOAC, pulumi.Parent(parent))
		if err != nil {
			return nil, err
		}
	}

	// ── WAF: CloudFront only accepts CLOUDFRONT-scope WebACLs (".../global/webacl/...") ──
	if in.Waf != nil {
		h.webACLArn = in.Waf.Arn.ToStringOutput().ApplyT(func(arn string) (string, error) {
			if !strings.Contains(arn, ":global/webacl/") {
				return "", fmt.Errorf("site %q: waf.arn %q is not a CloudFront-scope WebACL — "+
					"create the Waf with scope \"cloudfront\" (the default)", name, arn)
			}
			return arn, nil
		}).(pulumi.StringOutput)
	}

	// ── Edge signer (edge-oac) ──
	if UsesEdgeSigner(h.Mode) {
		h.edgeSignerArn, err = SetupEdgeSigner(ctx, parent, name)
		if err != nil {
			return nil, err
		}
	}

	// ── Origin protection ──
	if in.OriginProtection {
		pr, err := SetupOriginProtection(ctx, parent, name)
		if err != nil {
			return nil, fmt.Errorf("origin protection setup failed: %w", err)
		}
		h.originGuardArn = pr.FunctionArn
		h.OriginSecret = pr.OriginSecret
		ctx.Log.Info("Origin protection enabled: CloudFront only accepts requests carrying the "+
			OriginSecretHeader+" header. Configure your CDN/proxy to send it on every request, "+
			"with the originSecret output as the value — until then the site returns 403.", logArgs)
	}

	return h, nil
}

// CreateFunctionURL creates the server Lambda's Function URL with the auth type
// for the resolved protection mode.
func (h *Hosting) CreateFunctionURL(fn *lambda.Function) (*lambda.FunctionUrl, error) {
	return CreateSiteFunctionURL(h.ctx, h.parent, h.name, fn, h.Mode)
}

// ApplyTo sets the hosting fields on the distribution's CloudFront args.
func (h *Hosting) ApplyTo(cf *CloudFrontArgs) {
	cf.LambdaOAC = h.lambdaOAC
	cf.WebACLArn = h.webACLArn
	cf.ViewerRequestFunctionArn = h.originGuardArn
	cf.EdgeSignerArn = h.edgeSignerArn
	cf.ResponseHeadersPolicyID = h.headersPolicyID
}

// GrantAccess adds the Lambda permissions for the Function URL once the
// distribution exists.
func (h *Hosting) GrantAccess(fn *lambda.Function, distributionArn pulumi.StringOutput) error {
	return GrantSiteFunctionURLAccess(h.ctx, h.parent, h.name, fn, h.Mode, distributionArn)
}
