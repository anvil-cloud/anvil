package cognitoauth

import (
	"fmt"

	"github.com/anvil-cloud/anvil/provider/internal/apigateway"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	awsconfig "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/config"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// ── Args ───────────────────────────────────────────────────────────────────

// CognitoAuthArgs defines the inputs for an Anvil-managed Cognito JWT authorizer.
type CognitoAuthArgs struct {
	// UserPoolId is the Cognito user pool ID.
	// Pass pool.userPoolId directly. Anvil derives the issuer URL automatically.
	// Example: "us-east-1_abc123"
	UserPoolId pulumi.StringInput `pulumi:"userPoolId"`

	// Audience is the list of Cognito app client IDs allowed to access this API.
	// API Gateway rejects tokens whose 'aud' claim does not match one of these values.
	// Pass your Cognito app client ID(s) here.
	Audience []string `pulumi:"audience"`
}

// ── Component ──────────────────────────────────────────────────────────────

// CognitoAuth is the Anvil-managed Cognito JWT authorizer component resource.
// Convenience wrapper for Cognito-backed APIs — derives the issuer URL automatically
// from the user pool ID so you don't have to construct the Cognito endpoint string.
// Under the hood creates the same JWT authorizer as OAuthAuthorizer.
// Pass authorizerId to HttpApi defaultAuthorizerId to protect your API routes.
type CognitoAuth struct {
	pulumi.ResourceState

	// AuthorizerId is a reference to this authorizer's JWT configuration.
	// Pass this to HttpApi defaultAuthorizerId — HttpApi creates the API Gateway
	// authorizer on its own API (authorizers can't be shared across APIs).
	AuthorizerId pulumi.StringOutput `pulumi:"authorizerId"`
}

func (c *CognitoAuth) Annotate(a infer.Annotator) {
	a.SetToken("aws", "CognitoAuth")
	a.Describe(&c, "An Anvil-managed JWT authorizer backed by a Cognito user pool. Derives the issuer URL automatically from the user pool ID — no manual endpoint construction required. Creates the same native API Gateway JWT authorizer as OAuthAuthorizer. Pass authorizerId to HttpApi defaultAuthorizerId to protect your API routes.")
}

// NewCognitoAuth creates a new Anvil-managed Cognito JWT authorizer.
// The issuer URL is derived automatically: https://cognito-idp.{region}.amazonaws.com/{userPoolId}
// This is the standard Cognito OIDC discovery endpoint — API Gateway fetches
// public keys from it to verify tokens without any Lambda or custom code.
func NewCognitoAuth(ctx *pulumi.Context, name string, args CognitoAuthArgs, opts ...pulumi.ResourceOption) (*CognitoAuth, error) {
	c := &CognitoAuth{}

	provider.NewContext(ctx)

	opts = provider.WithDefault(opts, false)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, c, opts...); err != nil {
		return nil, err
	}

	// Fetch the AWS region from the provider config.
	// Cognito issuer URLs are region-scoped — we need the region to construct
	// the correct OIDC discovery endpoint.
	region := awsconfig.GetRegion(ctx)
	if region == "" {
		return nil, fmt.Errorf("CognitoAuth %q: could not determine AWS region — ensure the AWS provider is configured with a region", name)
	}

	// Derive the Cognito issuer URL from the region and user pool ID.
	// Format: https://cognito-idp.{region}.amazonaws.com/{userPoolId}
	// This is the standard Cognito OIDC issuer endpoint. API Gateway fetches
	// JWKS from {issuer}/.well-known/jwks.json automatically.
	issuerUrl := args.UserPoolId.ToStringOutput().ApplyT(func(poolId string) string {
		return fmt.Sprintf("https://cognito-idp.%s.amazonaws.com/%s", region, poolId)
	}).(pulumi.StringOutput)

	// API Gateway authorizers belong to a single API, so the authorizer itself
	// is created by HttpApi. authorizerId carries the issuer + audience there.
	c.AuthorizerId = apigateway.EncodeJwtAuthorizerRef(issuerUrl, args.Audience)

	ctx.RegisterResourceOutputs(c, pulumi.Map{
		"authorizerId": c.AuthorizerId,
	})

	return c, nil
}
