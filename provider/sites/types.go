// Package sites defines the shared types used across all framework build pipelines.
// Framework-specific build logic lives in subdirectories (sveltekit/, astro/, etc).
// Provider-specific infra wiring lives in provider/internal/awssite, provider/internal/gcpsite, etc.
//
// Adding a framework? Read docs/design/adding-a-site-framework.md first: upload static files
// with awssite.UploadSiteAssets, and make sure the server package carries its dependencies.
package sites

import (
	"reflect"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// BuildResult is the structured output of a framework build.
// Provider-specific components consume this to deploy static assets
// to object storage (S3/Cloud Storage) and server code to compute (Lambda/Cloud Run).
type BuildResult struct {
	// StaticDir is the absolute path to the directory containing static/client assets.
	// These get uploaded to object storage and served via CDN.
	StaticDir string

	// ServerDir is the absolute path to the directory containing the Node.js server bundle.
	// This gets packaged and deployed to a compute service.
	ServerDir string

	// ServerEntry is the absolute path to the Node.js server entry point (e.g. index.js).
	// The compute service uses this as its handler.
	ServerEntry string

	// Framework identifies which framework produced this build result.
	// Used by the infra layer to make framework-specific decisions
	// (e.g. CloudFront cache path patterns).
	// Values: "sveltekit" | "astro" | "nuxt" | "remix"
	Framework string
}

// ── Shared hosting inputs ─────────────────────────────────────────────────
//
// Every site component (SvelteKitSite and future framework components) exposes
// the same hosting inputs — protection, waf, originProtection and
// securityHeaders — and wires them through internal/awssite's Hosting helper,
// so every framework gets the same protection.
//
// The fields are declared on each component's Args struct rather than embedded
// from a shared struct: Pulumi's component input decoding (ConstructInputs.CopyTo)
// only fills top-level fields by their pulumi tag, so inputs inside an embedded
// struct would silently never be set. HostingInputNames + MissingHostingInputs
// keep every component's Args in sync — each site component has a test calling it.

// HostingInputNames are the pulumi input names every site component must expose.
var HostingInputNames = []string{"protection", "waf", "originProtection", "securityHeaders"}

// MissingHostingInputs returns the hosting inputs args (a site component's Args
// struct) doesn't declare. Empty means the component exposes all of them.
func MissingHostingInputs(args any) []string {
	t := reflect.TypeOf(args)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	declared := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("pulumi"), ",")[0]
		declared[name] = true
	}
	var missing []string
	for _, n := range HostingInputNames {
		if !declared[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

// SiteWafArgs attaches a WAF to the site. Composes freely with
// originProtection. While a WAF is attached, protection defaults to
// "edge-oac" so the server Function URL can't be used to bypass the WAF.
type SiteWafArgs struct {
	// Arn is the ARN of a WAF WebACL with CLOUDFRONT scope (us-east-1).
	// Pass waf.arn from an Anvil Waf component.
	Arn pulumi.StringInput `pulumi:"arn" schema:"required"`
}

// SiteSecurityHeadersArgs configures the security headers CloudFront adds to
// every response. On by default: Strict-Transport-Security (1 year),
// X-Content-Type-Options: nosniff, X-Frame-Options: SAMEORIGIN,
// Referrer-Policy: strict-origin-when-cross-origin, a Permissions-Policy that
// turns off camera, microphone, geolocation, USB and Topics, and
// Cross-Origin-Opener-Policy: same-origin-allow-popups. A header your app sets
// itself always takes priority. Content-Security-Policy is not set — configure
// it in the framework (e.g. SvelteKit's kit.csp).
type SiteSecurityHeadersArgs struct {
	// Enabled turns the security headers on or off. Default: true.
	Enabled *bool `pulumi:"enabled,optional"`

	// FrameOptions controls whether other sites can embed this one in a frame.
	// "sameorigin" (default): only pages on this site. "deny": never.
	// "none": no X-Frame-Options header — the site can be embedded anywhere.
	FrameOptions string `pulumi:"frameOptions,optional"`

	// Hsts adds optional Strict-Transport-Security directives.
	Hsts *SiteHstsArgs `pulumi:"hsts,optional"`

	// PermissionsPolicy is the Permissions-Policy header value: which browser
	// features the site (and anything it embeds) may use. Default:
	// "camera=(), microphone=(), geolocation=(), usb=(), browsing-topics=()".
	// Set your own value to allow a feature, e.g. "camera=(self), microphone=(self)",
	// or "none" to send no Permissions-Policy header.
	PermissionsPolicy string `pulumi:"permissionsPolicy,optional"`

	// CrossOriginOpenerPolicy isolates the site's browser window from windows
	// on other sites. "same-origin-allow-popups" (default): isolated, but popups
	// it opens (OAuth sign-in, payments) still work. "same-origin": strict
	// isolation — breaks sign-in and payment popups that report back.
	// "none": no Cross-Origin-Opener-Policy header.
	CrossOriginOpenerPolicy string `pulumi:"crossOriginOpenerPolicy,optional"`
}

// SiteHstsArgs configures optional Strict-Transport-Security directives. Both
// are hard to undo — browsers remember them for the max-age (1 year).
type SiteHstsArgs struct {
	// IncludeSubDomains applies HSTS to every subdomain of the site's domain.
	// Default: false.
	IncludeSubDomains bool `pulumi:"includeSubDomains,optional"`

	// Preload marks the domain as eligible for browsers' built-in HSTS preload
	// list. Requires includeSubDomains. Default: false.
	Preload bool `pulumi:"preload,optional"`
}

// SiteDnsArgs controls where the site's DNS records are created. Only used
// when domain is set.
type SiteDnsArgs struct {
	// Provider is where Anvil creates the certificate validation and site
	// records. "route53": the public hosted zone for the domain, in this AWS
	// account or the account of roleArn; the deploy fails if there isn't one.
	// "cloudflare": the Cloudflare zone for the domain, using the API token in
	// the CLOUDFLARE_API_TOKEN environment variable (needed on destroy too);
	// the token needs Zone > Zone > Read and Zone > DNS > Edit on the zone,
	// plus Zone > Transform Rules > Edit when originProtection is on. "manual":
	// Anvil creates no DNS records and the dnsRecords output lists what to
	// add. Default: "route53" when a matching hosted zone is found, otherwise
	// "manual". An existing record for a name the site needs fails the deploy;
	// Anvil never overwrites it.
	Provider string `pulumi:"provider,optional"`

	// Zone is the DNS zone the domain belongs to, e.g. "example.com".
	// Default: the last two labels of domain. Set it for domains such as
	// "www.example.co.uk".
	Zone string `pulumi:"zone,optional"`

	// Proxied routes traffic through Cloudflare's proxy in front of
	// CloudFront. Cloudflare only. Default: true. The zone's SSL/TLS mode must
	// be "Full (strict)". Cloudflare's free certificate only covers one level
	// of subdomain, so proxied names like "www.<stage>.example.com" need
	// Advanced Certificate Manager with Total TLS; Anvil warns about them.
	// While proxied, CloudFront still accepts requests sent to it directly
	// unless originProtection is on.
	Proxied *bool `pulumi:"proxied,optional"`

	// RoleArn is an IAM role in the AWS account that owns the Route53 hosted
	// zone (e.g. a network account). Anvil assumes it for the zone lookup and
	// record changes only; the certificate and CloudFront stay in the deploy
	// account. Route53 only.
	RoleArn string `pulumi:"roleArn,optional"`
}
