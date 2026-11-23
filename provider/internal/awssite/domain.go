package awssite

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/anvil-cloud/anvil/provider/internal/cfrule"
	"github.com/anvil-cloud/anvil/provider/sites"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/acm"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudfront"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/route53"
	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// DNS providers a site's records can be created in.
const (
	DNSRoute53    = "route53"
	DNSCloudflare = "cloudflare"
	DNSManual     = "manual"
)

// cloudFrontZoneID is the fixed Route53 hosted zone ID for every CloudFront
// distribution, used as the alias target zone.
const cloudFrontZoneID = "Z2FDTNDATAQYW2"

// DomainInputs describes a site's custom domain.
type DomainInputs struct {
	// Domain is the production hostname the user configured.
	Domain string
	// Dns is the optional dns block from the component's args.
	Dns *sites.SiteDnsArgs
	// WwwRedirect is the component's wwwRedirect input (nil = default on).
	WwwRedirect *bool
	// Stage and Environment come from the anvil config ("prod" | "nonprod").
	Stage       string
	Environment string
	// OriginProtection and WafAttached come from the hosting layer; they
	// decide the proxy warnings and whether Anvil adds the Cloudflare rule.
	OriginProtection bool
	WafAttached      bool
}

// SiteDomain is a site's resolved custom domain: the hostnames for this
// stage, the validated ACM certificate, and where the DNS records live.
type SiteDomain struct {
	// Hostname is the canonical name CloudFront serves for this stage.
	Hostname string
	// RedirectFrom is the other half of the apex/www pair, redirected to
	// Hostname. Empty when there's no pairing.
	RedirectFrom string
	// Provider is the resolved DNS provider: route53, cloudflare or manual.
	Provider string

	// CertARN is the validated ACM certificate ARN, ready for CloudFront.
	CertARN pulumi.StringOutput
	// Validation is the CertificateValidation resource. CloudFront must depend
	// on it so the certificate is issued before the distribution uses it.
	Validation pulumi.Resource

	ctx              *pulumi.Context
	parent           pulumi.Resource
	name             string
	zone             string
	zoneID           string
	proxied          bool
	originProtection bool
	route53Provider  pulumi.ProviderResource // set when dns.roleArn is used
	certInfo         pulumi.StringOutput
}

var invalidLabelChars = regexp.MustCompile(`[^a-z0-9-]+`)

// StageLabel turns a stage name into a DNS label: lowercase letters, digits
// and single hyphens, at most 63 characters.
func StageLabel(stage string) (string, error) {
	label := invalidLabelChars.ReplaceAllString(strings.ToLower(stage), "-")
	for strings.Contains(label, "--") {
		label = strings.ReplaceAll(label, "--", "-")
	}
	label = strings.Trim(label, "-")
	if len(label) > 63 {
		label = strings.Trim(label[:63], "-")
	}
	if label == "" {
		return "", fmt.Errorf("stage %q can't be used in a hostname; use letters, digits and hyphens", stage)
	}
	return label, nil
}

// ResolveHostname returns the hostname a stage serves and its zone. Prod
// stages use domain as-is. Other stages insert the stage label just above the
// zone: www.example.com → www.<stage>.example.com, example.com →
// <stage>.example.com.
func ResolveHostname(domain, zone, stage, environment string) (hostname, resolvedZone string, err error) {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
	if zone == "" {
		zone = strings.TrimSuffix(ExtractZoneName(domain), ".")
	}
	if domain != zone && !strings.HasSuffix(domain, "."+zone) {
		return "", "", fmt.Errorf("domain %q is not in zone %q", domain, zone)
	}
	if environment == "prod" {
		return domain, zone, nil
	}
	label, err := StageLabel(stage)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSuffix(domain, zone) + label + "." + zone, zone, nil
}

// PairedName returns the other half of the apex/www pair for a prod
// hostname: www.<zone> for the apex, the apex for www.<zone>. Empty for
// non-prod stages, other subdomains, or when the pairing is turned off.
func PairedName(hostname, zone, environment string, enabled bool) string {
	if environment != "prod" || !enabled {
		return ""
	}
	switch hostname {
	case zone:
		return "www." + zone
	case "www." + zone:
		return zone
	}
	return ""
}

// NewSiteDomain resolves the stage's hostnames and DNS provider, issues an
// ACM certificate in us-east-1, and, unless the provider is manual, creates
// the validation records so validation completes on its own. Call PointAt
// once the CloudFront distribution exists.
func NewSiteDomain(ctx *pulumi.Context, parent pulumi.Resource, name string, in DomainInputs) (*SiteDomain, error) {
	dns := in.Dns
	if dns == nil {
		dns = &sites.SiteDnsArgs{}
	}

	hostname, zone, err := ResolveHostname(in.Domain, dns.Zone, in.Stage, in.Environment)
	if err != nil {
		return nil, err
	}
	wwwRedirect := in.WwwRedirect == nil || *in.WwwRedirect

	d := &SiteDomain{
		Hostname:         hostname,
		RedirectFrom:     PairedName(hostname, zone, in.Environment, wwwRedirect),
		ctx:              ctx,
		parent:           parent,
		name:             name,
		zone:             zone,
		proxied:          dns.Proxied == nil || *dns.Proxied,
		originProtection: in.OriginProtection,
	}

	if err := d.resolveProvider(dns); err != nil {
		return nil, err
	}
	if err := d.checkOptions(in, dns); err != nil {
		return nil, err
	}

	names := d.names()
	certArgs := pulumi.Map{
		"domainName":       pulumi.String(hostname),
		"validationMethod": pulumi.String("DNS"),
		"region":           pulumi.String(usEast1),
		"tags":             pulumi.StringMap{"ManagedBy": pulumi.String("anvil")},
	}
	if d.RedirectFrom != "" {
		certArgs["subjectAlternativeNames"] = pulumi.StringArray{pulumi.String(d.RedirectFrom)}
	}
	cert := &acm.Certificate{}
	if err := ctx.RegisterResource("aws:acm/certificate:Certificate", name+"-cert", certArgs, cert, pulumi.Parent(parent)); err != nil {
		return nil, err
	}

	validationArgs := pulumi.Map{
		"certificateArn": cert.Arn,
		"region":         pulumi.String(usEast1),
	}
	validationOpts := []pulumi.ResourceOption{pulumi.Parent(parent)}
	var fqdns pulumi.StringArray
	var certLines []pulumi.StringOutput

	for i, host := range names {
		suffix := ""
		if i > 0 {
			suffix = "-redirect"
		}
		opt := validationOption(cert, host)
		recName, recValue, recType := opt.name, opt.value, opt.recordType
		certLines = append(certLines, pulumi.Sprintf("  %s  Name: %s  Value: %s", recType, recName, recValue))

		switch d.Provider {
		case DNSRoute53:
			rec := &route53.Record{}
			err := ctx.RegisterResource("aws:route53/record:Record", name+"-cert-validation-record"+suffix, pulumi.Map{
				"zoneId":         pulumi.String(d.zoneID),
				"name":           recName,
				"type":           recType,
				"records":        pulumi.StringArray{recValue},
				"ttl":            pulumi.Int(60),
				"allowOverwrite": pulumi.Bool(true), // ACM validation names are unique to the certificate
			}, rec, d.route53Opts()...)
			if err != nil {
				return nil, err
			}
			fqdns = append(fqdns, rec.Fqdn)
		case DNSCloudflare:
			rec, err := cloudflare.NewDnsRecord(ctx, name+"-cert-validation-record"+suffix, &cloudflare.DnsRecordArgs{
				ZoneId:  pulumi.String(d.zoneID),
				Name:    trimDot(recName),
				Type:    pulumi.String("CNAME"),
				Content: trimDot(recValue),
				Ttl:     pulumi.Float64(60),
				Proxied: pulumi.Bool(false), // ACM must see the record itself
				Comment: pulumi.String(d.comment("ACM certificate validation")),
			}, pulumi.Parent(parent))
			if err != nil {
				return nil, err
			}
			validationOpts = append(validationOpts, pulumi.DependsOn([]pulumi.Resource{rec}))
		}
	}
	if len(fqdns) > 0 {
		validationArgs["validationRecordFqdns"] = fqdns
	}

	certValidation := &acm.CertificateValidation{}
	err = ctx.RegisterResource("aws:acm/certificateValidation:CertificateValidation", name+"-cert-validation",
		validationArgs, certValidation, validationOpts...)
	if err != nil {
		return nil, err
	}

	d.CertARN = certValidation.CertificateArn
	d.Validation = certValidation
	d.certInfo = d.validationInstructions(cert, certLines)
	return d, nil
}

// names is the canonical hostname followed by the redirect name, if any.
func (d *SiteDomain) names() []string {
	if d.RedirectFrom == "" {
		return []string{d.Hostname}
	}
	return []string{d.Hostname, d.RedirectFrom}
}

// checkOptions rejects contradictory settings and logs the warnings that
// explain what a setting does or doesn't protect.
func (d *SiteDomain) checkOptions(in DomainInputs, dns *sites.SiteDnsArgs) error {
	ctx, log := d.ctx, &pulumi.LogArgs{Resource: d.parent}

	if in.WwwRedirect != nil && *in.WwwRedirect && in.Environment == "prod" && d.RedirectFrom == "" {
		ctx.Log.Warn(fmt.Sprintf("wwwRedirect only pairs a zone's apex with its www name; %s is neither, so there's nothing to redirect.", d.Hostname), log)
	}
	if dns.RoleArn != "" && d.Provider != DNSRoute53 {
		ctx.Log.Warn(fmt.Sprintf("dns.roleArn only applies to Route53; ignoring it for %s.", d.Provider), log)
	}

	if d.Provider != DNSCloudflare {
		if dns.Proxied != nil {
			ctx.Log.Warn(fmt.Sprintf("dns.proxied only applies to Cloudflare; ignoring it for %s.", d.Provider), log)
		}
		if in.OriginProtection {
			ctx.Log.Info("Origin protection enabled: CloudFront only accepts requests carrying the "+
				OriginSecretHeader+" header. Configure your CDN/proxy to send it on every request, "+
				"with the originSecret output as the value — until then the site returns 403.", log)
		}
		return nil
	}

	// Cloudflare from here on.
	if !d.proxied {
		if in.OriginProtection {
			return fmt.Errorf("originProtection needs Cloudflare's proxy to add the %s header, but dns.proxied is false: "+
				"visitors would reach CloudFront without it and get 403. Remove one of the two settings", OriginSecretHeader)
		}
		return nil
	}

	ctx.Log.Info(fmt.Sprintf("%s is proxied through Cloudflare. The zone's SSL/TLS mode must be \"Full (strict)\"; "+
		"\"Flexible\" makes CloudFront redirect Cloudflare back to HTTPS forever.", d.Hostname), log)

	for _, host := range d.names() {
		if strings.Contains(strings.TrimSuffix(host, "."+d.zone), ".") {
			ctx.Log.Warn(fmt.Sprintf("%s is more than one level below %s. Cloudflare's free certificate doesn't cover it while proxied, "+
				"so browsers get a certificate error unless the zone has Advanced Certificate Manager with Total TLS. "+
				"Set dns.proxied to false to serve it straight from CloudFront.", host, d.zone), log)
		}
	}
	if in.OriginProtection {
		ctx.Log.Info(fmt.Sprintf("Origin protection enabled: Anvil adds a Cloudflare Transform Rule sending the %s header for %s. "+
			"It uses one of the zone's Transform Rules (Free 10, Pro 25, Business 50, Enterprise 300), "+
			"and the API token needs Zone > Transform Rules > Edit.", OriginSecretHeader, strings.Join(d.names(), " and ")), log)
	} else {
		ctx.Log.Warn(fmt.Sprintf("%s is proxied through Cloudflare, but CloudFront still accepts requests sent to it directly, "+
			"bypassing Cloudflare's WAF, bot protection and rate limits. Set originProtection to true to accept only traffic from Cloudflare.", d.Hostname), log)
	}
	if in.WafAttached {
		ctx.Log.Warn("An AWS WAF is attached and the site is proxied through Cloudflare, so the WAF sees Cloudflare's IP addresses "+
			"instead of visitors'. Rate-based, IP and geo rules then apply to Cloudflare as a whole and can block real users. "+
			"Set clientIpHeader: 'CF-Connecting-IP' on the Waf, together with originProtection so the header can't be forged.", log)
	}
	return nil
}

// resolveProvider picks the DNS provider and looks up its zone. An explicit
// provider must work or the deploy fails; the default falls back to manual
// when no matching Route53 zone is found.
func (d *SiteDomain) resolveProvider(dns *sites.SiteDnsArgs) error {
	if dns.RoleArn != "" && (dns.Provider == "" || dns.Provider == DNSRoute53) {
		p, err := aws.NewProvider(d.ctx, d.name+"-dns", &aws.ProviderArgs{
			Region: pulumi.String(usEast1),
			AssumeRoles: aws.ProviderAssumeRoleArray{aws.ProviderAssumeRoleArgs{
				RoleArn:     pulumi.String(dns.RoleArn),
				SessionName: pulumi.String("anvil-dns"),
			}},
		}, pulumi.Parent(d.parent))
		if err != nil {
			return err
		}
		d.route53Provider = p
	}

	switch dns.Provider {
	case "":
		if id, ok := d.lookupRoute53Zone(); ok {
			d.Provider, d.zoneID = DNSRoute53, id
			return nil
		}
		where := "this account"
		if dns.RoleArn != "" {
			where = "the account of dns.roleArn"
		}
		d.ctx.Log.Warn(fmt.Sprintf("No Route53 hosted zone for %s in %s, so Anvil isn't creating DNS records. "+
			"Add the records in the dnsRecords output yourself, or set dns.provider.", d.zone, where), &pulumi.LogArgs{Resource: d.parent})
		d.Provider = DNSManual
		return nil
	case DNSRoute53:
		id, ok := d.lookupRoute53Zone()
		if !ok {
			if dns.RoleArn != "" {
				return fmt.Errorf("dns.provider is \"route53\" but no public hosted zone for %s was found through dns.roleArn %s; "+
					"check the role can be assumed and allows route53:ListHostedZonesByName", d.zone, dns.RoleArn)
			}
			return fmt.Errorf("dns.provider is \"route53\" but this account has no public hosted zone for %s; "+
				"create one, set dns.roleArn to a role in the account that has it, set dns.zone, or set dns.provider to \"manual\"", d.zone)
		}
		d.Provider, d.zoneID = DNSRoute53, id
		return nil
	case DNSCloudflare:
		if os.Getenv("CLOUDFLARE_API_TOKEN") == "" {
			return fmt.Errorf("dns.provider is \"cloudflare\" but CLOUDFLARE_API_TOKEN isn't set; set it to a token with " +
				"Zone > Zone > Read and Zone > DNS > Edit on the zone, or set dns.provider to \"manual\"")
		}
		zone, err := cloudflare.LookupZone(d.ctx, &cloudflare.LookupZoneArgs{
			Filter: &cloudflare.GetZoneFilter{Name: pulumi.StringRef(d.zone), Match: "all"},
		})
		if err != nil || zone == nil || zone.Id == "" {
			return fmt.Errorf("couldn't find Cloudflare zone %s with this API token (it needs Zone > Zone > Read; "+
				"set dns.zone if the zone is named differently): %v", d.zone, err)
		}
		d.Provider, d.zoneID = DNSCloudflare, zone.Id
		return nil
	case DNSManual:
		d.Provider = DNSManual
		return nil
	default:
		return fmt.Errorf("dns.provider must be \"route53\", \"cloudflare\" or \"manual\", got %q", dns.Provider)
	}
}

func (d *SiteDomain) route53Opts() []pulumi.ResourceOption {
	opts := []pulumi.ResourceOption{pulumi.Parent(d.parent)}
	if d.route53Provider != nil {
		opts = append(opts, pulumi.Provider(d.route53Provider))
	}
	return opts
}

func (d *SiteDomain) lookupRoute53Zone() (string, bool) {
	var opts []pulumi.InvokeOption
	if d.route53Provider != nil {
		opts = append(opts, pulumi.Provider(d.route53Provider))
	}
	zone, err := route53.LookupZone(d.ctx, &route53.LookupZoneArgs{
		Name:        pulumi.StringRef(d.zone + "."),
		PrivateZone: pulumi.BoolRef(false),
	}, opts...)
	if err != nil || zone == nil {
		return "", false
	}
	return zone.ZoneId, true
}

// cloudflareOriginRule is the internal Anvil resource keeping the site's
// Cloudflare Transform Rule (see internal/cfrule).
type cloudflareOriginRule struct {
	pulumi.CustomResourceState
}

// PointAt creates the records sending the hostnames to the distribution (and,
// for Cloudflare with origin protection, the header rule first), and returns
// the dnsRecords output: the records Anvil manages, or, for manual, the
// records to add.
func (d *SiteDomain) PointAt(distribution *cloudfront.Distribution, originSecret pulumi.StringOutput) (pulumi.StringOutput, error) {
	ctx, parent, name := d.ctx, d.parent, d.name
	names := d.names()

	switch d.Provider {
	case DNSRoute53:
		for i, host := range names {
			prefix := name + "-domain"
			if i > 0 {
				prefix = name + "-redirect"
			}
			for _, recordType := range []string{"A", "AAAA"} {
				// No allowOverwrite: an existing record for the name fails the
				// deploy instead of being taken over.
				err := ctx.RegisterResource("aws:route53/record:Record", fmt.Sprintf("%s-%s", prefix, strings.ToLower(recordType)), pulumi.Map{
					"zoneId": pulumi.String(d.zoneID),
					"name":   pulumi.String(host),
					"type":   pulumi.String(recordType),
					"aliases": pulumi.Array{pulumi.Map{
						"name":                 distribution.DomainName,
						"zoneId":               pulumi.String(cloudFrontZoneID),
						"evaluateTargetHealth": pulumi.Bool(false),
					}},
				}, &route53.Record{}, d.route53Opts()...)
				if err != nil {
					return pulumi.StringOutput{}, err
				}
			}
		}
	case DNSCloudflare:
		recordOpts := []pulumi.ResourceOption{pulumi.Parent(parent)}
		if d.proxied && d.originProtection {
			// The header rule goes in before the records, so the site never
			// answers 403 to visitors coming through Cloudflare.
			rule := &cloudflareOriginRule{}
			err := ctx.RegisterResource(cfrule.Token, name+"-origin-rule", pulumi.Map{
				"zoneId":      pulumi.String(d.zoneID),
				"zoneName":    pulumi.String(d.zone),
				"hostnames":   pulumi.ToStringArray(names),
				"header":      pulumi.String(OriginSecretHeader),
				"secret":      originSecret,
				"description": pulumi.String(d.comment("origin protection")),
			}, rule, pulumi.Parent(parent))
			if err != nil {
				return pulumi.StringOutput{}, err
			}
			recordOpts = append(recordOpts, pulumi.DependsOn([]pulumi.Resource{rule}))
		}
		ttl := 300.0
		if d.proxied {
			ttl = 1 // "automatic": required for proxied records
		}
		for i, host := range names {
			resName := name + "-domain-cname"
			if i > 0 {
				resName = name + "-redirect-cname"
			}
			// Cloudflare rejects a CNAME where any record for the name already
			// exists, so an existing record fails the deploy, never overwritten.
			_, err := cloudflare.NewDnsRecord(ctx, resName, &cloudflare.DnsRecordArgs{
				ZoneId:  pulumi.String(d.zoneID),
				Name:    pulumi.String(host),
				Type:    pulumi.String("CNAME"),
				Content: distribution.DomainName,
				Ttl:     pulumi.Float64(ttl),
				Proxied: pulumi.Bool(d.proxied),
				Comment: pulumi.String(d.comment("site")),
			}, recordOpts...)
			if err != nil {
				return pulumi.StringOutput{}, err
			}
		}
	}

	provider := d.Provider
	return pulumi.All(d.certInfo, distribution.DomainName).ApplyT(func(args []interface{}) string {
		certLines, cfDomain := args[0].(string), args[1].(string)
		lines := []string{}
		if certLines != "" {
			lines = append(lines, certLines)
		}
		for _, host := range names {
			lines = append(lines, fmt.Sprintf("  CNAME  Name: %s  Value: %s", host, cfDomain))
		}
		header := fmt.Sprintf("DNS records managed by Anvil in %s for %s:", provider, names[0])
		if provider == DNSManual {
			header = fmt.Sprintf("DNS records to add for %s:", names[0])
		}
		return header + "\n" + strings.Join(lines, "\n")
	}).(pulumi.StringOutput), nil
}

type validationRecord struct {
	name, value, recordType pulumi.StringOutput
}

// validationOption picks the certificate's validation record for one domain.
// ACM doesn't keep the options in request order, so match by domain name.
func validationOption(cert *acm.Certificate, domain string) validationRecord {
	pick := func(field func(acm.CertificateDomainValidationOption) *string) pulumi.StringOutput {
		return cert.DomainValidationOptions.ApplyT(func(opts []acm.CertificateDomainValidationOption) string {
			for _, o := range opts {
				if o.DomainName != nil && *o.DomainName == domain {
					if v := field(o); v != nil {
						return *v
					}
				}
			}
			return ""
		}).(pulumi.StringOutput)
	}
	return validationRecord{
		name:       pick(func(o acm.CertificateDomainValidationOption) *string { return o.ResourceRecordName }),
		value:      pick(func(o acm.CertificateDomainValidationOption) *string { return o.ResourceRecordValue }),
		recordType: pick(func(o acm.CertificateDomainValidationOption) *string { return o.ResourceRecordType }),
	}
}

// validationInstructions joins the certificate's validation records and, in
// manual mode, warns until the certificate is issued.
func (d *SiteDomain) validationInstructions(cert *acm.Certificate, lines []pulumi.StringOutput) pulumi.StringOutput {
	ctx, host, manual := d.ctx, d.Hostname, d.Provider == DNSManual
	inputs := []interface{}{cert.Status, cert.Arn}
	for _, l := range lines {
		inputs = append(inputs, l)
	}
	return pulumi.All(inputs...).ApplyT(func(args []interface{}) string {
		status, arn := args[0].(string), args[1].(string)
		var out []string
		for _, a := range args[2:] {
			if s := a.(string); !strings.Contains(s, "Name:  ") {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return ""
		}
		// cert.Status is only recorded at creation time (always
		// PENDING_VALIDATION), so confirm against ACM before nagging.
		if manual && status != "ISSUED" && !certIssued(ctx, host, arn) {
			ctx.Log.Warn(fmt.Sprintf("\n⚠ ⏳ Add these DNS records to validate your certificate for %s:\n%s\n"+
				"   Deploy will continue automatically once the records are detected.\n", host, strings.Join(out, "\n")), nil)
		}
		return strings.Join(out, "\n")
	}).(pulumi.StringOutput)
}

func (d *SiteDomain) comment(what string) string {
	return fmt.Sprintf("Managed by Anvil (%s %s, %s)", d.ctx.Project(), d.name, what)
}

func trimDot(s pulumi.StringOutput) pulumi.StringOutput {
	return s.ApplyT(func(v string) string { return strings.TrimSuffix(v, ".") }).(pulumi.StringOutput)
}

// certIssued reports whether ACM currently has the certificate with the given
// ARN in the ISSUED state. Any lookup failure is treated as not issued.
func certIssued(ctx *pulumi.Context, domain, arn string) bool {
	if arn == "" {
		return false
	}
	found, err := acm.LookupCertificate(ctx, &acm.LookupCertificateArgs{
		Domain:     pulumi.StringRef(domain),
		Statuses:   []string{"ISSUED"},
		MostRecent: pulumi.BoolRef(true),
		Region:     pulumi.StringRef(usEast1),
	})
	return err == nil && found != nil && found.Arn == arn
}
