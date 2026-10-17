package waf

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// Defaults. Researched baseline: AWS WAF managed rule group docs, AWS Security
// Services Best Practices (WAF), and CloudFront's one-click protection.
// See docs/design/waf.md.
const (
	ScopeCloudFront = "cloudfront"
	ScopeRegional   = "regional"

	ModeCount = "count"
	ModeBlock = "block"

	defaultRateLimit     = 2000
	defaultWindowSeconds = 300
	defaultRetentionDays = 30
)

// managedGroup describes an AWS managed rule group Anvil can enable.
type managedGroup struct {
	key        string // input name under managedRules
	awsName    string // AWS rule group name
	defaultOn  bool
	ipBased    bool // evaluates the connecting IP only (can't read a forwarded IP header)
	priority   int
	ruleNames  []string
	metricName string
}

// managedGroups is the ordered list of supported AWS managed rule groups.
// Rule names are from the AWS WAF developer guide; they're used to route
// ruleOverrides to the right group.
var managedGroups = []managedGroup{
	{
		key: "ipReputation", awsName: "AWSManagedRulesAmazonIpReputationList", defaultOn: true, ipBased: true, priority: 40,
		ruleNames: []string{"AWSManagedIPReputationList", "AWSManagedReconnaissanceList", "AWSManagedIPDDoSList"},
	},
	{
		key: "anonymousIp", awsName: "AWSManagedRulesAnonymousIpList", defaultOn: false, ipBased: true, priority: 50,
		ruleNames: []string{"AnonymousIPList", "HostingProviderIPList"},
	},
	{
		key: "knownBadInputs", awsName: "AWSManagedRulesKnownBadInputsRuleSet", defaultOn: true, priority: 60,
		ruleNames: []string{
			"JavaDeserializationRCE_HEADER", "JavaDeserializationRCE_BODY", "JavaDeserializationRCE_URIPATH",
			"JavaDeserializationRCE_QUERYSTRING", "Host_localhost_HEADER", "PROPFIND_METHOD", "ExploitablePaths_URIPATH",
			"Log4JRCE_HEADER", "Log4JRCE_QUERYSTRING", "Log4JRCE_BODY", "Log4JRCE_URIPATH", "ReactJSRCE_BODY",
		},
	},
	{
		key: "core", awsName: "AWSManagedRulesCommonRuleSet", defaultOn: true, priority: 70,
		ruleNames: []string{
			"NoUserAgent_HEADER", "UserAgent_BadBots_HEADER", "SizeRestrictions_QUERYSTRING", "SizeRestrictions_Cookie_HEADER",
			"SizeRestrictions_BODY", "SizeRestrictions_URIPATH", "EC2MetaDataSSRF_BODY", "EC2MetaDataSSRF_COOKIE",
			"EC2MetaDataSSRF_URIPATH", "EC2MetaDataSSRF_QUERYARGUMENTS", "GenericLFI_QUERYARGUMENTS", "GenericLFI_URIPATH",
			"GenericLFI_BODY", "RestrictedExtensions_URIPATH", "RestrictedExtensions_QUERYARGUMENTS",
			"GenericRFI_QUERYARGUMENTS", "GenericRFI_BODY", "GenericRFI_URIPATH", "CrossSiteScripting_COOKIE",
			"CrossSiteScripting_QUERYARGUMENTS", "CrossSiteScripting_BODY", "CrossSiteScripting_URIPATH",
		},
	},
	{
		key: "adminProtection", awsName: "AWSManagedRulesAdminProtectionRuleSet", defaultOn: false, priority: 80,
		ruleNames: []string{"AdminProtection_URIPATH"},
	},
	{
		key: "sqli", awsName: "AWSManagedRulesSQLiRuleSet", defaultOn: true, priority: 90,
		ruleNames: []string{
			"SQLi_QUERYARGUMENTS", "SQLiExtendedPatterns_QUERYARGUMENTS", "SQLi_BODY", "SQLiExtendedPatterns_BODY",
			"SQLiExtendedPatterns_HEADER", "SQLiExtendedPatterns_URIPATH", "SQLi_COOKIE", "SQLi_URIPATH",
		},
	},
}

// defaultRuleOverrides applies unless the user overrides the same rule.
// SizeRestrictions_BODY blocks any body over 8 KB, which breaks form posts,
// JSON APIs and uploads; Lambda already caps bodies at 6 MB.
var defaultRuleOverrides = map[string]string{"SizeRestrictions_BODY": ModeCount}

// enabledGroup is a managed group resolved for this WAF.
type enabledGroup struct {
	managedGroup
	overrides map[string]string // rule name → action
}

// config is the fully resolved, validated WAF configuration.
type config struct {
	scope          string
	mode           string
	rateLimit      int // 0 = disabled
	windowSeconds  int
	groups         []enabledGroup
	allowV4        []string
	allowV6        []string
	blockV4        []string
	blockV6        []string
	blockCountries []string
	clientIPHeader string
	logging        bool
	retentionDays  int
	includeAllowed bool
	notices        []string
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// resolveConfig applies defaults and validates the inputs. It has no Pulumi
// dependencies so it can be unit-tested.
func resolveConfig(args WafArgs) (config, error) {
	cfg := config{
		scope:          ScopeCloudFront,
		mode:           ModeCount,
		rateLimit:      defaultRateLimit,
		windowSeconds:  defaultWindowSeconds,
		clientIPHeader: strings.TrimSpace(args.ClientIpHeader),
		logging:        true,
		retentionDays:  defaultRetentionDays,
	}

	switch args.Scope {
	case "", ScopeCloudFront:
	case ScopeRegional:
		cfg.scope = ScopeRegional
	default:
		return cfg, fmt.Errorf("invalid scope %q: must be %q or %q", args.Scope, ScopeCloudFront, ScopeRegional)
	}

	switch args.Mode {
	case "", ModeCount:
	case ModeBlock:
		cfg.mode = ModeBlock
	default:
		return cfg, fmt.Errorf("invalid mode %q: must be %q or %q", args.Mode, ModeCount, ModeBlock)
	}

	// ── Rate limit ──
	if rl := args.RateLimit; rl != nil {
		if !boolOr(rl.Enabled, true) {
			cfg.rateLimit = 0
		} else {
			if rl.Limit != 0 {
				if rl.Limit < 10 || rl.Limit > 2_000_000_000 {
					return cfg, fmt.Errorf("invalid rateLimit.limit %d: must be between 10 and 2,000,000,000 requests per window", rl.Limit)
				}
				cfg.rateLimit = rl.Limit
			}
			if rl.WindowSeconds != 0 {
				switch rl.WindowSeconds {
				case 60, 120, 300, 600:
					cfg.windowSeconds = rl.WindowSeconds
				default:
					return cfg, fmt.Errorf("invalid rateLimit.windowSeconds %d: must be 60, 120, 300 or 600", rl.WindowSeconds)
				}
			}
		}
	}

	// ── Managed rule groups ──
	mr := args.ManagedRules
	if mr == nil {
		mr = &WafManagedRulesArgs{}
	}
	explicit := map[string]*bool{
		"ipReputation": mr.IpReputation, "anonymousIp": mr.AnonymousIp, "knownBadInputs": mr.KnownBadInputs,
		"core": mr.Core, "adminProtection": mr.AdminProtection, "sqli": mr.Sqli,
	}
	ruleToGroup := map[string]string{}
	for _, g := range managedGroups {
		for _, r := range g.ruleNames {
			ruleToGroup[r] = g.key
		}
	}

	overrides := map[string]string{}
	for k, v := range defaultRuleOverrides {
		overrides[k] = v
	}
	for rule, action := range args.RuleOverrides {
		switch action {
		case ModeCount, ModeBlock, "allow":
		default:
			return cfg, fmt.Errorf("invalid ruleOverrides[%q] %q: must be \"count\", \"block\" or \"allow\"", rule, action)
		}
		if _, ok := ruleToGroup[rule]; !ok {
			return cfg, fmt.Errorf("ruleOverrides: %q isn't a rule in any supported AWS managed rule group. "+
				"Use the exact AWS rule name (e.g. \"SizeRestrictions_BODY\"), or hatch.waf for anything else", rule)
		}
		overrides[rule] = action
	}

	usedGroups := map[string]bool{}
	for _, g := range managedGroups {
		on := boolOr(explicit[g.key], g.defaultOn)
		if g.ipBased && cfg.clientIPHeader != "" {
			switch {
			case explicit[g.key] == nil && on:
				on = false
				cfg.notices = append(cfg.notices, fmt.Sprintf(
					"managedRules.%s is off because clientIpHeader is set: the AWS %s rule group only checks the "+
						"connecting IP (your proxy), not the visitor IP in %s. Set managedRules.%s explicitly to change this.",
					g.key, g.awsName, cfg.clientIPHeader, g.key))
			case on:
				cfg.notices = append(cfg.notices, fmt.Sprintf(
					"managedRules.%s is on while clientIpHeader is set: the AWS %s rule group only checks the "+
						"connecting IP, so behind a proxy it evaluates the proxy's IPs, not visitors'.", g.key, g.awsName))
			}
		}
		if !on {
			continue
		}
		eg := enabledGroup{managedGroup: g, overrides: map[string]string{}}
		for rule, action := range overrides {
			if ruleToGroup[rule] == g.key {
				eg.overrides[rule] = action
			}
		}
		usedGroups[g.key] = true
		cfg.groups = append(cfg.groups, eg)
	}
	for rule := range args.RuleOverrides {
		if !usedGroups[ruleToGroup[rule]] {
			return cfg, fmt.Errorf("ruleOverrides: %q belongs to managedRules.%s, which is off", rule, ruleToGroup[rule])
		}
	}

	// ── IP lists ──
	var err error
	if cfg.allowV4, cfg.allowV6, err = splitCIDRs("ipAllowList", args.IpAllowList); err != nil {
		return cfg, err
	}
	if cfg.blockV4, cfg.blockV6, err = splitCIDRs("ipBlockList", args.IpBlockList); err != nil {
		return cfg, err
	}

	// ── Countries ──
	for _, c := range args.BlockCountries {
		c = strings.ToUpper(strings.TrimSpace(c))
		if len(c) != 2 || c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' {
			return cfg, fmt.Errorf("invalid blockCountries entry %q: use two-letter ISO 3166 country codes, e.g. \"KP\"", c)
		}
		cfg.blockCountries = append(cfg.blockCountries, c)
	}
	sort.Strings(cfg.blockCountries)

	// ── Logging ──
	if lg := args.Logging; lg != nil {
		cfg.logging = boolOr(lg.Enabled, true)
		cfg.includeAllowed = lg.IncludeAllowed
		if lg.RetentionDays != 0 {
			if !validRetention(lg.RetentionDays) {
				return cfg, fmt.Errorf("invalid logging.retentionDays %d: must be a CloudWatch Logs retention value "+
					"(1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288 or 3653)",
					lg.RetentionDays)
			}
			cfg.retentionDays = lg.RetentionDays
		}
	}

	return cfg, nil
}

// splitCIDRs validates CIDRs (or bare IPs) and splits them by address family —
// a WAF IP set holds one family only.
func splitCIDRs(field string, entries []string) (v4, v6 []string, err error) {
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return nil, nil, fmt.Errorf("invalid %s entry %q: must be an IP address or CIDR", field, e)
			}
			if ip.To4() != nil {
				e += "/32"
			} else {
				e += "/128"
			}
		}
		ip, ipNet, perr := net.ParseCIDR(e)
		if perr != nil {
			return nil, nil, fmt.Errorf("invalid %s entry %q: must be an IP address or CIDR", field, e)
		}
		if ip.To4() != nil {
			v4 = append(v4, ipNet.String())
		} else {
			v6 = append(v6, ipNet.String())
		}
	}
	sort.Strings(v4)
	sort.Strings(v6)
	return v4, v6, nil
}

func validRetention(days int) bool {
	switch days {
	case 1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653:
		return true
	}
	return false
}
