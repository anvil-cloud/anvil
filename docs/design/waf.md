# WAF — `anvil:aws:Waf`

**Status:** Implemented (`provider/aws/waf`), with `waf: { arn }` attach points
on `SvelteKitSite` (CloudFront scope) and `CognitoUserPool` (regional scope).
Config resolution is unit-tested; **not yet deployed to AWS**.

## What it is

A standalone AWS WAF WebACL with researched defaults. Resources attach it; the
WAF itself doesn't know what it protects:

```ts
const waf = new anvil.aws.Waf('edge', {});                          // CloudFront scope, count mode
new anvil.aws.SvelteKitSite('web', { path: 'web', waf: { arn: waf.arn } });

const authWaf = new anvil.aws.Waf('auth', { scope: 'regional', mode: 'block' });
new anvil.aws.CognitoUserPool('users', { waf: { arn: authWaf.arn } });
```

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Shape | **Standalone component**, attached by the resource (`waf: { arn }`) | A WAF is its own thing; one WebACL can be shared by several sites (each WebACL costs ~$5/month) |
| Scope | **`cloudfront` (default) and `regional`** | Sites need CloudFront scope (us-east-1); Cognito user pools need regional scope. AWS recommends a WAF for every public user pool |
| Default mode | **`count`** | Matches are logged, not blocked, until the user switches to `block` — AWS's recommended rollout, so a new WAF can't break live traffic with false positives |
| Proxies | **`clientIpHeader`** (e.g. `CF-Connecting-IP`) | Behind a proxy the WAF sees the proxy's IPs. Rate limit, IP lists and geo rules read the header instead |
| Logging | **On by default, blocked/counted only** | A WAF can't be tuned without logs; dropping allowed requests keeps cost low |
| Origin protection | **Not in the WAF** | It lives on the site as a CloudFront Function (`originProtection: true`) — see [site-protection.md](site-protection.md) |
| HttpApi | **Not supported** | AWS WAF can't attach to API Gateway v2 HTTP APIs |

## Defaults (`new Waf('x', {})`)

Sources: AWS WAF managed rule group docs, AWS Security Services Best Practices
(WAF), CloudFront one-click protection.

| Priority | Rule | Default | Action |
|---|---|---|---|
| 0 | `ipAllowList` | — (only if set) | Allow (skips all later rules) |
| 10 | `ipBlockList` | — (only if set) | Block (always, regardless of mode) |
| 20 | `blockCountries` | — (only if set) | Follows `mode` |
| 30 | Rate limit, per client IP | **2000 / 5 min** | Follows `mode` |
| 40 | `AWSManagedRulesAmazonIpReputationList` (25 WCU) | **on** (off with `clientIpHeader`) | Follows `mode` |
| 50 | `AWSManagedRulesAnonymousIpList` (50 WCU) | off | Follows `mode` |
| 60 | `AWSManagedRulesKnownBadInputsRuleSet` (200 WCU) | **on** | Follows `mode` |
| 70 | `AWSManagedRulesCommonRuleSet` (700 WCU) | **on**, `SizeRestrictions_BODY` → count | Follows `mode` |
| 80 | `AWSManagedRulesAdminProtectionRuleSet` (100 WCU) | off | Follows `mode` |
| 90 | `AWSManagedRulesSQLiRuleSet` (200 WCU) | **on** | Follows `mode` |

Default action: **allow**. Default WCU ≈ 1,130 — under the 1,500 WCU threshold
above which AWS charges extra per request. With every group on ≈ 1,280.

- **`SizeRestrictions_BODY` → count by default.** It blocks any body over 8 KB,
  breaking forms, JSON APIs and uploads; Lambda already caps bodies at 6 MB.
  Override with `ruleOverrides: { SizeRestrictions_BODY: 'block' }`.
- **Rule order:** cheap IP checks first, expensive body inspection last.
- **Not included:** Linux/POSIX/Windows/PHP/WordPress groups (irrelevant to
  Node on Lambda), Bot Control and Anti-DDoS (extra subscription cost).

## Inputs

| Input | Default | Notes |
|---|---|---|
| `scope` | `cloudfront` | `cloudfront` (us-east-1) or `regional` (stack region) |
| `mode` | `count` | `count` or `block` |
| `rateLimit` | `{ limit: 2000, windowSeconds: 300, enabled: true }` | limit 10–2,000,000,000; window 60/120/300/600 |
| `managedRules` | core, knownBadInputs, ipReputation, sqli on | booleans per group; explicit always wins |
| `ruleOverrides` | `{ SizeRestrictions_BODY: 'count' }` | exact AWS rule name → `count` / `block` / `allow`; merged over the default |
| `ipAllowList`, `ipBlockList` | — | IPs or CIDRs, IPv4 and IPv6 (split into one IP set per family) |
| `blockCountries` | — | ISO 3166 two-letter codes |
| `clientIpHeader` | — | header with the visitor IP behind a proxy |
| `logging` | `{ enabled: true, retentionDays: 30, includeAllowed: false }` | |
| `hatch.waf` | — | typed overrides on the WebACL (escape hatch, e.g. custom rules) |

Outputs: `arn`, `webAclId`, `name`, `scope`, `logGroupName`.

## Resources created

| Resource | When |
|---|---|
| AWS provider pinned to `us-east-1` | `scope: cloudfront` |
| `wafv2.WebAcl` | always |
| `wafv2.IpSet` (per list × address family) | `ipAllowList` / `ipBlockList` set |
| CloudWatch log group `aws-waf-logs-<name>` + `WebAclLoggingConfiguration` | logging on (the `aws-waf-logs-` prefix is required by WAF) |
| `wafv2.WebAclAssociation` | created by `CognitoUserPool` when its `waf` is set (sites attach via the distribution's `webAclId`) |

Logging redacts `authorization`, `cookie` and `x-origin-secret`, and by default
keeps only `BLOCK`, `COUNT` and `EXCLUDED_AS_COUNT` records.

## `clientIpHeader` and IP-based rule groups

With `clientIpHeader` set, the rate limit (`FORWARDED_IP`), IP sets
(`ipSetForwardedIpConfig`, first IP in the header) and geo match
(`forwardedIpConfig`) read the visitor IP from the header; a missing header
doesn't match. AWS's `ipReputation` and `anonymousIp` groups only evaluate the
connecting IP, so behind a proxy they'd judge the proxy. They therefore default
to **off** when `clientIpHeader` is set (info message); setting them explicitly
keeps them on (info message that they evaluate the proxy's IPs).

**Only set `clientIpHeader` when every request comes through the proxy**
(pair it with the site's `originProtection: true`) — otherwise a client can
send the header itself and choose its own IP.

## Messages Anvil emits

| Level | When | Message (source) |
|---|---|---|
| **Error** | invalid `scope` / `mode` | `invalid scope "<x>": must be "cloudfront" or "regional"` / `invalid mode "<x>": …` (`config.go`) |
| **Error** | rate limit out of range / bad window | `invalid rateLimit.limit <n>: …` / `invalid rateLimit.windowSeconds <n>: must be 60, 120, 300 or 600` |
| **Error** | unknown rule in `ruleOverrides` | `ruleOverrides: "<rule>" isn't a rule in any supported AWS managed rule group. Use the exact AWS rule name (e.g. "SizeRestrictions_BODY"), or hatch.waf for anything else` |
| **Error** | override for a disabled group | `ruleOverrides: "<rule>" belongs to managedRules.<group>, which is off` |
| **Error** | bad action in `ruleOverrides` | `invalid ruleOverrides["<rule>"] "<x>": must be "count", "block" or "allow"` |
| **Error** | bad IP/CIDR, country, retention | `invalid ipAllowList entry …` / `invalid blockCountries entry …` / `invalid logging.retentionDays …` |
| **Info** | `clientIpHeader` set, IP group defaulted off | `managedRules.<group> is off because clientIpHeader is set: …` |
| **Info** | `clientIpHeader` set, IP group explicitly on | `managedRules.<group> is on while clientIpHeader is set: …` |
| **Error** (site) | site `waf.arn` isn't CloudFront scope | `site "<n>": waf.arn "<arn>" is not a CloudFront-scope WebACL — create the Waf with scope "cloudfront" (the default)` |
| **Error** (Cognito) | pool `waf.arn` isn't regional scope | `user pool "<n>": waf.arn "<arn>" is not a regional-scope WebACL — create the Waf with scope "regional" in the same region as the pool` |

## For the user docs

### ⚠️ Warnings

1. **A new WAF doesn't block anything yet.** It starts in `count` mode: watch
   the logs / metrics for false positives, then set `mode: 'block'`.
2. **Attaching a WAF to a site locks the site's server** (`protection` defaults
   to `edge-oac`) — see the Hosting docs.
3. **`clientIpHeader` is trusted blindly.** Only use it when the proxy is the
   only way in (`originProtection: true` on the site), or clients can spoof
   their IP to dodge rate limits and blocks.
4. **Behind a proxy, IP reputation and anonymous-IP groups are off by default**
   — they can't see visitor IPs. Rely on the proxy's own protection for those.
5. **Scope must match:** sites need `scope: 'cloudfront'` (the default),
   Cognito pools need `scope: 'regional'` in the same region as the pool. A
   mismatch fails the deploy.
6. **HttpApi can't take a WAF** (an API Gateway v2 limit). Verify webhook
   signatures and use authorizers there instead.
7. **WAF changes don't redeploy the site.** Updating rules or switching `mode`
   updates the WebACL in place, so it's safe to do on live traffic.
8. **Logs exclude allowed requests by default.** Set
   `logging: { includeAllowed: true }` to debug — at the cost of logging every
   request.

### 💡 Good to know

- **Cost:** ~$5/month per WebACL + $1/month per rule (≈ $10/month with defaults)
  + $0.60 per million requests + CloudWatch Logs ingestion (~$0.50/GB, blocked
  and counted requests only by default).
- **One WAF can protect several sites** — pass the same `waf.arn` to each.
- **Behind Cloudflare you may not need it** — Cloudflare includes a WAF on every
  plan. Use `originProtection: true` on the site to stop CloudFront being
  reached around Cloudflare.
- **Custom rules** go through `hatch.waf` (typed WebACL overrides).

## Future work

- Bot Control and Anti-DDoS as opt-in groups (paid).
- Body inspection size (`associationConfig`) beyond the 16 KB default.
- Per-path scope-down (e.g. stricter rate limits on `/login`).
- Rule-name list refresh from `wafv2.getManagedRuleGroup` instead of a static
  list, so new AWS rules can be overridden without an Anvil release.

## Sources

- [AWS WAF baseline rule groups](https://docs.aws.amazon.com/waf/latest/developerguide/aws-managed-rule-groups-baseline.html)
- [AWS WAF IP reputation rule groups](https://docs.aws.amazon.com/waf/latest/developerguide/aws-managed-rule-groups-ip-rep.html)
- [AWS WAF use-case rule groups (SQLi)](https://docs.aws.amazon.com/waf/latest/developerguide/aws-managed-rule-groups-use-case.html)
- [AWS Security Services Best Practices — WAF managed rules](https://aws.github.io/aws-security-services-best-practices/guides/waf/aws-managed-rules/docs/)
- [Amazon Cognito: AWS WAF with user pools](https://docs.aws.amazon.com/cognito/latest/developerguide/user-pool-waf.html)
- [AWS WAF pricing](https://aws.amazon.com/waf/pricing/)
