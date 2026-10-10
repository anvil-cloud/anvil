package oauthauthorizer

import (
	"github.com/anvil-cloud/anvil/provider/internal/apigateway"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// ── Args ───────────────────────────────────────────────────────────────────

// OAuthAuthorizerArgs defines the inputs for an Anvil-managed OAuth JWT authorizer.
type OAuthAuthorizerArgs struct {
	// Issuer is the OIDC issuer URL of your identity provider.
	// API Gateway fetches public keys from {issuer}/.well-known/jwks.json to verify tokens.
	// Examples:
	//   Auth0:   "https://your-tenant.auth0.com/"
	//   Clerk:   "https://your-instance.clerk.accounts.dev"
	//   Google:  "https://accounts.google.com"
	Issuer string `pulumi:"issuer"`

	// Audience is the list of intended recipients for the JWT.
	// API Gateway rejects tokens whose 'aud' claim does not match one of these values.
	// Auth0: the API identifier (e.g. "https://api.myapp.com"), not the client ID.
	Audience []string `pulumi:"audience"`
}

// ── Component ──────────────────────────────────────────────────────────────

// OAuthAuthorizer is the Anvil-managed JWT authorizer component resource.
// Works with any OIDC-compliant identity provider — Auth0, Clerk, Google, Okta, etc.
// Pass authorizerId to HttpApi.defaultAuthorizerId to protect your API routes.
type OAuthAuthorizer struct {
	pulumi.ResourceState

	// AuthorizerId is a reference to this authorizer's JWT configuration.
	// Pass this to HttpApi defaultAuthorizerId — HttpApi creates the API Gateway
	// authorizer on its own API (authorizers can't be shared across APIs).
	AuthorizerId pulumi.StringOutput `pulumi:"authorizerId"`
}

func (o *OAuthAuthorizer) Annotate(a infer.Annotator) {
	a.SetToken("aws", "OAuthAuthorizer")
	a.Describe(&o, "An Anvil-managed JWT authorizer for HTTP API Gateway. Works with any OIDC-compliant identity provider (Auth0, Clerk, Google, Okta, Cognito). API Gateway verifies the JWT signature, issuer, audience, and expiry on every request — your Lambda only runs if the token is valid. Pass authorizerId to HttpApi defaultAuthorizerId.")
}

// NewOAuthAuthorizer creates a new Anvil-managed JWT authorizer.
// The authorizer verifies JWTs issued by the given OIDC provider on every inbound request.
// No Lambda or custom code required — verification is handled natively by API Gateway.
// The API Gateway authorizer resource is created by the HttpApi it is attached to.
func NewOAuthAuthorizer(ctx *pulumi.Context, name string, args OAuthAuthorizerArgs, opts ...pulumi.ResourceOption) (*OAuthAuthorizer, error) {
	o := &OAuthAuthorizer{}

	provider.NewContext(ctx)

	opts = provider.WithDefault(opts, false)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, o, opts...); err != nil {
		return nil, err
	}

	// API Gateway authorizers belong to a single API, so the authorizer itself
	// is created by HttpApi. authorizerId carries the issuer + audience there.
	o.AuthorizerId = apigateway.EncodeJwtAuthorizerRef(pulumi.String(args.Issuer), args.Audience)

	ctx.RegisterResourceOutputs(o, pulumi.Map{
		"authorizerId": o.AuthorizerId,
	})

	return o, nil
}
