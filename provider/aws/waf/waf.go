package waf

import (
	"fmt"
	"regexp"

	"github.com/anvil-cloud/anvil/provider/internal/hatch"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/wafv2"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// WafRateLimitArgs configures the per-client rate limit.
type WafRateLimitArgs struct {
	// Limit is the maximum requests per client in each window. Default: 2000.
	Limit int `pulumi:"limit,optional"`

	// WindowSeconds is the evaluation window: 60, 120, 300 or 600. Default: 300.
	WindowSeconds int `pulumi:"windowSeconds,optional"`

	// Enabled turns the rate limit on or off. Default: true.
	Enabled *bool `pulumi:"enabled,optional"`
}

// WafManagedRulesArgs turns AWS managed rule groups on or off.
type WafManagedRulesArgs struct {
	// Core is the Core Rule Set (OWASP Top 10: XSS, LFI, RFI, SSRF). Default: true.
	Core *bool `pulumi:"core,optional"`

	// KnownBadInputs blocks known exploit patterns (Log4Shell, Java
	// deserialisation, React RCE). Default: true.
	KnownBadInputs *bool `pulumi:"knownBadInputs,optional"`

	// IpReputation blocks IPs on Amazon's threat-intelligence list. Default:
	// true, or false when clientIpHeader is set.
	IpReputation *bool `pulumi:"ipReputation,optional"`

	// Sqli blocks SQL injection patterns. Default: true.
	Sqli *bool `pulumi:"sqli,optional"`

	// AnonymousIp blocks VPNs, Tor and hosting providers. Suits B2B apps.
	// Default: false.
	AnonymousIp *bool `pulumi:"anonymousIp,optional"`

	// AdminProtection blocks requests to admin-looking paths. Default: false.
	AdminProtection *bool `pulumi:"adminProtection,optional"`
}

// WafLoggingArgs configures WAF logging to CloudWatch Logs.
type WafLoggingArgs struct {
	// Enabled turns logging on or off. Default: true.
	Enabled *bool `pulumi:"enabled,optional"`

	// RetentionDays is how long logs are kept. Default: 30.
	RetentionDays int `pulumi:"retentionDays,optional"`

	// IncludeAllowed also logs allowed requests. Default: false — only blocked
	// and counted requests are logged, which keeps cost low.
	IncludeAllowed bool `pulumi:"includeAllowed,optional"`
}

// WafArgs defines the inputs for an Anvil-managed WAF.
type WafArgs struct {
	Scope          string               `pulumi:"scope,optional"`
	Mode           string               `pulumi:"mode,optional"`
	RateLimit      *WafRateLimitArgs    `pulumi:"rateLimit,optional"`
	ManagedRules   *WafManagedRulesArgs `pulumi:"managedRules,optional"`
	RuleOverrides  map[string]string    `pulumi:"ruleOverrides,optional"`
	IpAllowList    []string             `pulumi:"ipAllowList,optional"`
	IpBlockList    []string             `pulumi:"ipBlockList,optional"`
	BlockCountries []string             `pulumi:"blockCountries,optional"`
	ClientIpHeader string               `pulumi:"clientIpHeader,optional"`
	Logging        *WafLoggingArgs      `pulumi:"logging,optional"`

	Hatch map[string]map[string]interface{} `pulumi:"hatch,optional"`
}

// Waf is the Anvil-managed WAF component resource.
type Waf struct {
	pulumi.ResourceState

	// Arn is the WebACL ARN. Pass it to a site's waf.arn or a user pool's waf.arn.
	Arn pulumi.StringOutput `pulumi:"arn"`

	// WebAclId is the WebACL ID.
	WebAclId pulumi.StringOutput `pulumi:"webAclId"`

	// WebAclName is the physical WebACL name.
	WebAclName pulumi.StringOutput `pulumi:"name"`

	// Scope is "cloudfront" or "regional".
	Scope pulumi.StringOutput `pulumi:"scope"`

	// LogGroupName is the CloudWatch log group receiving WAF logs. Empty when
	// logging is off.
	LogGroupName pulumi.StringOutput `pulumi:"logGroupName"`
}

func (w *Waf) Annotate(a infer.Annotator) {
	a.SetToken("aws", "Waf")
	a.Describe(&w, "An Anvil-managed AWS WAF WebACL with researched defaults: a per-client rate limit and AWS managed rule groups for IP reputation, known bad inputs, the OWASP core rule set and SQL injection.")
}

var nonMetricChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// WAF descriptions only allow word characters, whitespace and + = : # @ / - , .
var nonDescriptionChars = regexp.MustCompile(`[^\w\s+=:#@/\-,.]`)

// description returns a WebACL description WAF accepts.
func description(s string) string {
	s = nonDescriptionChars.ReplaceAllString(s, "-")
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}

// metricName returns a CloudWatch metric name WAF accepts.
func metricName(s string) string {
	s = nonMetricChars.ReplaceAllString(s, "-")
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

func actionFor(action string) pulumi.Map {
	return pulumi.Map{action: pulumi.Map{}}
}

func visibility(metric string) pulumi.Map {
	return pulumi.Map{
		"cloudwatchMetricsEnabled": pulumi.Bool(true),
		"metricName":               pulumi.String(metricName(metric)),
		"sampledRequestsEnabled":   pulumi.Bool(true),
	}
}

func NewWaf(ctx *pulumi.Context, name string, args WafArgs, opts ...pulumi.ResourceOption) (*Waf, error) {
	w := &Waf{}

	provider.NewContext(ctx)
	cfgAnvil := c.New(ctx, "anvil")
	stage := cfgAnvil.Require("stage")
	stageId := cfgAnvil.Require("stageId")

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, w, opts...); err != nil {
		return nil, err
	}

	cfg, err := resolveConfig(args)
	if err != nil {
		return nil, fmt.Errorf("waf %q: %w", name, err)
	}
	for _, n := range cfg.notices {
		ctx.Log.Info(n, &pulumi.LogArgs{Resource: w})
	}

	physicalName := provider.PhysicalName(stage, name, "waf", stageId)
	wafScope := "CLOUDFRONT"
	if cfg.scope == ScopeRegional {
		wafScope = "REGIONAL"
	}

	// CloudFront-scoped WAF resources (and their log group) must live in us-east-1.
	// The region is set per resource rather than through a separate provider, so
	// they keep the app provider's default tags and credentials.
	resOpts := []pulumi.ResourceOption{pulumi.Parent(w)}
	inRegion := func(props pulumi.Map) pulumi.Map {
		if cfg.scope == ScopeCloudFront {
			props["region"] = pulumi.String("us-east-1")
		}
		return props
	}

	tags := pulumi.StringMap{"ManagedBy": pulumi.String("anvil")}

	// ── IP sets (one per address family) ──
	newIPSet := func(suffix, version string, addresses []string) (*wafv2.IpSet, error) {
		if len(addresses) == 0 {
			return nil, nil
		}
		set := &wafv2.IpSet{}
		err := ctx.RegisterResource("aws:wafv2/ipSet:IpSet", name+"-"+suffix, inRegion(pulumi.Map{
			"name":             pulumi.String(physicalName + "-" + suffix),
			"scope":            pulumi.String(wafScope),
			"ipAddressVersion": pulumi.String(version),
			"addresses":        pulumi.ToStringArray(addresses),
			"tags":             tags,
		}), set, resOpts...)
		if err != nil {
			return nil, fmt.Errorf("waf %q: failed to create IP set %s: %w", name, suffix, err)
		}
		return set, nil
	}

	ipSetStatement := func(sets ...*wafv2.IpSet) pulumi.Map {
		var stmts pulumi.Array
		for _, s := range sets {
			if s == nil {
				continue
			}
			ref := pulumi.Map{"arn": s.Arn}
			if cfg.clientIPHeader != "" {
				ref["ipSetForwardedIpConfig"] = pulumi.Map{
					"headerName":       pulumi.String(cfg.clientIPHeader),
					"fallbackBehavior": pulumi.String("NO_MATCH"),
					"position":         pulumi.String("FIRST"),
				}
			}
			stmts = append(stmts, pulumi.Map{"ipSetReferenceStatement": ref})
		}
		if len(stmts) == 1 {
			return stmts[0].(pulumi.Map)
		}
		return pulumi.Map{"orStatement": pulumi.Map{"statements": stmts}}
	}

	var rules pulumi.Array

	// ── 0: allow list — always allow, skipping every later rule ──
	if len(cfg.allowV4)+len(cfg.allowV6) > 0 {
		v4, err := newIPSet("allow-v4", "IPV4", cfg.allowV4)
		if err != nil {
			return nil, err
		}
		v6, err := newIPSet("allow-v6", "IPV6", cfg.allowV6)
		if err != nil {
			return nil, err
		}
		rules = append(rules, pulumi.Map{
			"name": pulumi.String("ip-allow-list"), "priority": pulumi.Int(0),
			"action": actionFor("allow"), "statement": ipSetStatement(v4, v6),
			"visibilityConfig": visibility(name + "-ip-allow-list"),
		})
	}

	// ── 10: block list — always block ──
	if len(cfg.blockV4)+len(cfg.blockV6) > 0 {
		v4, err := newIPSet("block-v4", "IPV4", cfg.blockV4)
		if err != nil {
			return nil, err
		}
		v6, err := newIPSet("block-v6", "IPV6", cfg.blockV6)
		if err != nil {
			return nil, err
		}
		rules = append(rules, pulumi.Map{
			"name": pulumi.String("ip-block-list"), "priority": pulumi.Int(10),
			"action": actionFor(ModeBlock), "statement": ipSetStatement(v4, v6),
			"visibilityConfig": visibility(name + "-ip-block-list"),
		})
	}

	forwarded := func() pulumi.Map {
		return pulumi.Map{"headerName": pulumi.String(cfg.clientIPHeader), "fallbackBehavior": pulumi.String("NO_MATCH")}
	}

	// ── 20: blocked countries ──
	if len(cfg.blockCountries) > 0 {
		geo := pulumi.Map{"countryCodes": pulumi.ToStringArray(cfg.blockCountries)}
		if cfg.clientIPHeader != "" {
			geo["forwardedIpConfig"] = forwarded()
		}
		rules = append(rules, pulumi.Map{
			"name": pulumi.String("blocked-countries"), "priority": pulumi.Int(20),
			"action": actionFor(cfg.mode), "statement": pulumi.Map{"geoMatchStatement": geo},
			"visibilityConfig": visibility(name + "-blocked-countries"),
		})
	}

	// ── 30: per-client rate limit ──
	if cfg.rateLimit > 0 {
		rate := pulumi.Map{
			"limit":               pulumi.Int(cfg.rateLimit),
			"evaluationWindowSec": pulumi.Int(cfg.windowSeconds),
			"aggregateKeyType":    pulumi.String("IP"),
		}
		if cfg.clientIPHeader != "" {
			rate["aggregateKeyType"] = pulumi.String("FORWARDED_IP")
			rate["forwardedIpConfig"] = forwarded()
		}
		rules = append(rules, pulumi.Map{
			"name": pulumi.String("rate-limit"), "priority": pulumi.Int(30),
			"action": actionFor(cfg.mode), "statement": pulumi.Map{"rateBasedStatement": rate},
			"visibilityConfig": visibility(name + "-rate-limit"),
		})
	}

	// ── 40+: AWS managed rule groups ──
	groupOverride := "none" // use each rule's own action
	if cfg.mode == ModeCount {
		groupOverride = ModeCount
	}
	for _, g := range cfg.groups {
		stmt := pulumi.Map{"vendorName": pulumi.String("AWS"), "name": pulumi.String(g.awsName)}
		if len(g.overrides) > 0 {
			var overrides pulumi.Array
			for _, rule := range g.ruleNames { // stable order
				if action, ok := g.overrides[rule]; ok {
					overrides = append(overrides, pulumi.Map{"name": pulumi.String(rule), "actionToUse": actionFor(action)})
				}
			}
			stmt["ruleActionOverrides"] = overrides
		}
		rules = append(rules, pulumi.Map{
			"name": pulumi.String(g.awsName), "priority": pulumi.Int(g.priority),
			"overrideAction":   actionFor(groupOverride),
			"statement":        pulumi.Map{"managedRuleGroupStatement": stmt},
			"visibilityConfig": visibility(name + "-" + g.key),
		})
	}

	// ── WebACL ──
	aclProps := hatch.Merge(args.Hatch["waf"], inRegion(pulumi.Map{
		"name":             pulumi.String(physicalName),
		"scope":            pulumi.String(wafScope),
		"description":      pulumi.String(description(fmt.Sprintf("Anvil WAF %s, stage %s", name, stage))),
		"defaultAction":    actionFor("allow"),
		"rules":            rules,
		"visibilityConfig": visibility(physicalName),
		"tags":             tags,
	}))
	acl := &wafv2.WebAcl{}
	if err := ctx.RegisterResource("aws:wafv2/webAcl:WebAcl", name, aclProps, acl, resOpts...); err != nil {
		return nil, fmt.Errorf("waf %q: failed to create WebACL: %w", name, err)
	}

	// ── Logging ──
	logGroupName := pulumi.String("").ToStringOutput()
	if cfg.logging {
		logGroup := &cloudwatch.LogGroup{}
		// WAF only delivers to log groups whose name starts with aws-waf-logs-.
		err := ctx.RegisterResource("aws:cloudwatch/logGroup:LogGroup", name+"-logs", inRegion(pulumi.Map{
			"name":            pulumi.String("aws-waf-logs-" + physicalName),
			"retentionInDays": pulumi.Int(cfg.retentionDays),
			"tags":            tags,
		}), logGroup, resOpts...)
		if err != nil {
			return nil, fmt.Errorf("waf %q: failed to create log group: %w", name, err)
		}

		logProps := pulumi.Map{
			"resourceArn":           acl.Arn,
			"logDestinationConfigs": pulumi.StringArray{logGroup.Arn},
			// Never write credentials or the origin-protection secret to logs.
			"redactedFields": pulumi.Array{
				pulumi.Map{"singleHeader": pulumi.Map{"name": pulumi.String("authorization")}},
				pulumi.Map{"singleHeader": pulumi.Map{"name": pulumi.String("cookie")}},
				pulumi.Map{"singleHeader": pulumi.Map{"name": pulumi.String("x-origin-secret")}},
			},
		}
		if !cfg.includeAllowed {
			var conditions pulumi.Array
			for _, a := range []string{"BLOCK", "COUNT", "EXCLUDED_AS_COUNT"} {
				conditions = append(conditions, pulumi.Map{"actionCondition": pulumi.Map{"action": pulumi.String(a)}})
			}
			logProps["loggingFilter"] = pulumi.Map{
				"defaultBehavior": pulumi.String("DROP"),
				"filters": pulumi.Array{pulumi.Map{
					"behavior":    pulumi.String("KEEP"),
					"requirement": pulumi.String("MEETS_ANY"),
					"conditions":  conditions,
				}},
			}
		}
		err = ctx.RegisterResource("aws:wafv2/webAclLoggingConfiguration:WebAclLoggingConfiguration", name+"-logging",
			inRegion(logProps), &wafv2.WebAclLoggingConfiguration{}, resOpts...)
		if err != nil {
			return nil, fmt.Errorf("waf %q: failed to configure logging: %w", name, err)
		}
		logGroupName = logGroup.Name
	}

	// ── Outputs ──
	w.Arn = acl.Arn
	w.WebAclId = acl.ID().ToStringOutput()
	w.WebAclName = acl.Name
	w.Scope = pulumi.String(cfg.scope).ToStringOutput()
	w.LogGroupName = logGroupName

	ctx.RegisterResourceOutputs(w, pulumi.Map{
		"arn":          acl.Arn,
		"webAclId":     acl.ID(),
		"name":         acl.Name,
		"scope":        pulumi.String(cfg.scope),
		"logGroupName": logGroupName,
	})

	return w, nil
}
