package awssite

import (
	"fmt"

	"github.com/anvil-cloud/anvil/provider/internal/transform"
	"github.com/anvil-cloud/anvil/provider/sites"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudfront"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Security headers added to every response by a CloudFront response headers
// policy. Why each one, and what it changes for visitors and developers, is
// recorded in docs/design/security-headers.md.
//
// Every header uses override: false — a header the app sets itself always
// wins, so an app with its own policy is never overruled.

// Values for SiteSecurityHeadersArgs.FrameOptions.
const (
	FrameOptionsSameOrigin = "sameorigin"
	FrameOptionsDeny       = "deny"
	FrameOptionsNone       = "none" // no X-Frame-Options header: the site can be embedded anywhere
)

// Values for SiteSecurityHeadersArgs.CrossOriginOpenerPolicy.
const (
	COOPSameOriginAllowPopups = "same-origin-allow-popups"
	COOPSameOrigin            = "same-origin"
	COOPNone                  = "none" // no Cross-Origin-Opener-Policy header
)

// DefaultPermissionsPolicy turns off browser features most sites never use, so
// injected scripts or embedded third parties can't use them either. payment is
// deliberately not included: it would block the Payment Request API used by
// Apple Pay / Google Pay buttons (e.g. Stripe).
const DefaultPermissionsPolicy = "camera=(), microphone=(), geolocation=(), usb=(), browsing-topics=()"

// permissionsPolicyNone sends no Permissions-Policy header.
const permissionsPolicyNone = "none"

// hstsMaxAgeSeconds is one year — the common baseline, and the minimum for HSTS preload.
const hstsMaxAgeSeconds = 31536000

// maxCustomHeaderValue is CloudFront's limit for a custom header value.
const maxCustomHeaderValue = 1783

// SecurityHeadersConfig is the resolved security-headers setting for a site.
// Empty PermissionsPolicy / CrossOriginOpenerPolicy means the header isn't sent.
type SecurityHeadersConfig struct {
	Enabled                 bool
	FrameOptions            string
	HstsIncludeSubDomains   bool
	HstsPreload             bool
	PermissionsPolicy       string
	CrossOriginOpenerPolicy string
}

// ResolveSecurityHeaders applies defaults and validates the inputs. nil means
// all defaults.
func ResolveSecurityHeaders(args *sites.SiteSecurityHeadersArgs) (SecurityHeadersConfig, error) {
	if args == nil {
		args = &sites.SiteSecurityHeadersArgs{}
	}
	cfg := SecurityHeadersConfig{
		Enabled:                 args.Enabled == nil || *args.Enabled,
		FrameOptions:            FrameOptionsSameOrigin,
		PermissionsPolicy:       DefaultPermissionsPolicy,
		CrossOriginOpenerPolicy: COOPSameOriginAllowPopups,
	}
	if args.Hsts != nil {
		cfg.HstsIncludeSubDomains, cfg.HstsPreload = args.Hsts.IncludeSubDomains, args.Hsts.Preload
	}

	switch args.FrameOptions {
	case "", FrameOptionsSameOrigin:
	case FrameOptionsDeny, FrameOptionsNone:
		cfg.FrameOptions = args.FrameOptions
	default:
		return cfg, fmt.Errorf("invalid securityHeaders.frameOptions %q: must be %q, %q or %q",
			args.FrameOptions, FrameOptionsSameOrigin, FrameOptionsDeny, FrameOptionsNone)
	}

	if cfg.HstsPreload && !cfg.HstsIncludeSubDomains {
		return cfg, fmt.Errorf("securityHeaders.hsts.preload requires hsts.includeSubDomains: " +
			"browsers only accept HSTS preload for a whole domain including its subdomains")
	}

	switch args.PermissionsPolicy {
	case "":
	case permissionsPolicyNone:
		cfg.PermissionsPolicy = ""
	default:
		if err := validateHeaderValue("securityHeaders.permissionsPolicy", args.PermissionsPolicy); err != nil {
			return cfg, err
		}
		cfg.PermissionsPolicy = args.PermissionsPolicy
	}

	switch args.CrossOriginOpenerPolicy {
	case "", COOPSameOriginAllowPopups:
	case COOPSameOrigin:
		cfg.CrossOriginOpenerPolicy = COOPSameOrigin
	case COOPNone:
		cfg.CrossOriginOpenerPolicy = ""
	default:
		return cfg, fmt.Errorf("invalid securityHeaders.crossOriginOpenerPolicy %q: must be %q, %q or %q",
			args.CrossOriginOpenerPolicy, COOPSameOriginAllowPopups, COOPSameOrigin, COOPNone)
	}

	return cfg, nil
}

// validateHeaderValue rejects values CloudFront can't send or that could
// smuggle extra headers: only printable ASCII, no CR/LF, within CloudFront's limit.
func validateHeaderValue(field, v string) error {
	if len(v) > maxCustomHeaderValue {
		return fmt.Errorf("%s is %d characters; CloudFront allows at most %d", field, len(v), maxCustomHeaderValue)
	}
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("%s contains an invalid character %q: header values must be printable ASCII on one line", field, r)
		}
	}
	return nil
}

// CreateSecurityHeadersPolicy creates the CloudFront response headers policy
// and returns its ID for every cache behavior.
func CreateSecurityHeadersPolicy(ctx *pulumi.Context, parent pulumi.Resource, name string, cfg SecurityHeadersConfig, overrides map[string]interface{}) (pulumi.StringOutput, error) {
	security := pulumi.Map{
		// HSTS: browsers always use HTTPS for this domain after the first visit.
		"strictTransportSecurity": pulumi.Map{
			"accessControlMaxAgeSec": pulumi.Int(hstsMaxAgeSeconds),
			"includeSubdomains":      pulumi.Bool(cfg.HstsIncludeSubDomains),
			"preload":                pulumi.Bool(cfg.HstsPreload),
			"override":               pulumi.Bool(false),
		},
		// nosniff: browsers don't guess a file's type and run it as a script.
		"contentTypeOptions": pulumi.Map{"override": pulumi.Bool(false)},
		// Only the origin is sent to other sites, never full URLs.
		"referrerPolicy": pulumi.Map{
			"referrerPolicy": pulumi.String("strict-origin-when-cross-origin"),
			"override":       pulumi.Bool(false),
		},
	}
	// Clickjacking: other sites can't embed this one in a frame.
	if cfg.FrameOptions != FrameOptionsNone {
		frame := "SAMEORIGIN"
		if cfg.FrameOptions == FrameOptionsDeny {
			frame = "DENY"
		}
		security["frameOptions"] = pulumi.Map{"frameOption": pulumi.String(frame), "override": pulumi.Bool(false)}
	}

	// Headers CloudFront's securityHeadersConfig doesn't cover go in as custom headers.
	var custom pulumi.Array
	if cfg.PermissionsPolicy != "" {
		// Browser features the site and its embeds may use.
		custom = append(custom, pulumi.Map{
			"header": pulumi.String("Permissions-Policy"), "value": pulumi.String(cfg.PermissionsPolicy), "override": pulumi.Bool(false),
		})
	}
	if cfg.CrossOriginOpenerPolicy != "" {
		// Isolates the site's window from windows on other sites.
		custom = append(custom, pulumi.Map{
			"header": pulumi.String("Cross-Origin-Opener-Policy"), "value": pulumi.String(cfg.CrossOriginOpenerPolicy), "override": pulumi.Bool(false),
		})
	}

	props := pulumi.Map{
		"comment":               pulumi.Sprintf("Anvil security headers for site %s", name),
		"securityHeadersConfig": security,
	}
	if len(custom) > 0 {
		props["customHeadersConfig"] = pulumi.Map{"items": custom}
	}
	props = transform.MergeTransform(overrides, props)

	policy := &cloudfront.ResponseHeadersPolicy{}
	err := ctx.RegisterResource("aws:cloudfront/responseHeadersPolicy:ResponseHeadersPolicy", name+"-security-headers",
		props, policy, pulumi.Parent(parent))
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create security headers policy: %w", err)
	}
	return policy.ID().ToStringOutput(), nil
}
