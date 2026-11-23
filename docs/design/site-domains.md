# Site domains and DNS

How site components (`SvelteKitSite`) name each stage, issue certificates and
create their DNS records in Route53 or Cloudflare without manual steps.

**Status:** implemented on `sg-endpoint-grants`; not yet deployed against a
real Route53 or Cloudflare zone. Multi-account automation through the Org
component is parked in [org/later-dns.md](org/later-dns.md).

## API

```ts
new anvil.aws.SvelteKitSite('web', {
  path: 'web',
  domain: 'youlosei.win',          // the prod hostname
  wwwRedirect: true,               // default; prod only
  originProtection: false,         // default; opt in
  dns: {
    provider: 'cloudflare',        // route53 | cloudflare | manual
    zone: 'youlosei.win',          // default: last two labels of domain
    proxied: true,                 // Cloudflare only; default true
    roleArn: 'arn:aws:iam::…',     // Route53 only: zone in another account
  },
});
```

Only `domain` is needed for the common case. Everything else has a default.

## Hostnames

The stage's environment decides the hostname. It's set when the stage is
bootstrapped (`anvil.yaml` → `stages.<name>.environment`); a stage that was
never bootstrapped is `nonprod`.

| Environment | `domain: 'youlosei.win'` | `domain: 'www.youlosei.win'` | `domain: 'app.youlosei.win'` |
|---|---|---|---|
| prod | `youlosei.win` | `www.youlosei.win` | `app.youlosei.win` |
| nonprod, stage `damien` | `damien.youlosei.win` | `www.damien.youlosei.win` | `app.damien.youlosei.win` |

- Non-prod stages insert the stage name just above the zone. On prod,
  `--stage` only names resources.
- Stage names are made DNS-safe: lowercase, anything but letters, digits and
  hyphens becomes a hyphen, at most 63 characters. `Damien_Pace` →
  `damien-pace`.
- There is no option to change the pattern. One rule keeps every component
  and stage predictable.

### apex ↔ www pairing

On prod, an apex or `www` domain also answers on the other name, which
301-redirects to the one written in `domain`:

| `domain` | Served | Redirects to it |
|---|---|---|
| `youlosei.win` | `youlosei.win` | `www.youlosei.win` |
| `www.youlosei.win` | `www.youlosei.win` | `youlosei.win` |
| `app.youlosei.win` | `app.youlosei.win` | — |

- Prod only; non-prod stages have no pair.
- The certificate covers both names; CloudFront serves both; a CloudFront
  Function sends the redirect, keeping the path and query string.
- `wwwRedirect: false` turns it off.
- An existing DNS record for the other name fails the deploy. Anvil never
  takes over a record it didn't create.

## DNS providers

| `dns.provider` | What Anvil creates | Default when |
|---|---|---|
| `route53` | ACM validation CNAMEs and A/AAAA aliases in the public hosted zone | a matching zone is found |
| `cloudflare` | ACM validation CNAMEs (never proxied) and site CNAMEs (proxied by default) | never; set it explicitly |
| `manual` | Nothing; the `dnsRecords` output lists what to add | no Route53 zone is found |

An explicit provider must work or the deploy fails with what to fix. The
default falls back to `manual` with a warning.

Every record the site needs is created by Anvil and deleted on destroy. An
existing record for one of those names fails the deploy instead of being
overwritten (Route53 records don't set `allowOverwrite`; Cloudflare rejects a
CNAME next to any other record). ACM validation records are the exception on
Route53: their names are unique to the certificate.

### Route53

1. Look up the public hosted zone named after `dns.zone`.
2. Request the certificate in us-east-1 (both names when paired).
3. Create the validation CNAMEs; wait until ACM issues the certificate
   (usually 1–3 minutes).
4. Create the distribution with the hostnames as aliases.
5. Create A and AAAA alias records for each hostname.

Requirements:

- The domain's nameservers point at the hosted zone. Domains bought through
  Route53's registrar get the zone automatically.
- The zone is in the deploy account, or in the account of `dns.roleArn`.

#### `dns.roleArn`: zone in another account

When the zone lives in a network account, Anvil assumes `roleArn` (session
name `anvil-dns`, us-east-1) for the zone lookup and record changes only. The
certificate and CloudFront stay in the deploy account. The deploy's own
credentials must be allowed to assume the role.

The role needs `route53:ListHostedZonesByName`, `route53:GetHostedZone`,
`route53:GetChange`, `route53:ListResourceRecordSets` and
`route53:ChangeResourceRecordSets` on the zone. Route53's IAM condition keys
can limit which names each environment may change, so a non-prod pipeline
can't touch prod's records:

```json
{
  "Effect": "Allow",
  "Action": "route53:ChangeResourceRecordSets",
  "Resource": "arn:aws:route53:::hostedzone/Z123",
  "Condition": {
    "ForAllValues:StringLike": {
      "route53:ChangeResourceRecordSetsNormalizedRecordNames": ["*.youlosei.win"]
    },
    "ForAllValues:StringEquals": {
      "route53:ChangeResourceRecordSetsRecordTypes": ["A", "AAAA", "CNAME"]
    }
  }
}
```

Adjust the patterns to exclude the prod names in the non-prod role. Org
creating these roles automatically is parked in
[org/later-dns.md](org/later-dns.md).

### Cloudflare

The token is read from `CLOUDFLARE_API_TOKEN` in the shell that runs
`anvil deploy` and `anvil destroy`. It never goes in `anvil.yaml` or code.

```bash
export CLOUDFLARE_API_TOKEN=...
anvil deploy --stage damien
```

Create it under My Profile → API Tokens → Custom token, limited to the one
zone:

| Permission | Needed for |
|---|---|
| Zone → Zone → Read | Looking up the zone ID |
| Zone → DNS → Edit | Validation and site records |
| Zone → Transform Rules → Edit | Only with `originProtection` |

Steps on deploy:

1. Look up the zone ID by name.
2. Request the certificate; create the validation CNAMEs (not proxied);
   wait for ACM.
3. Create the distribution.
4. With `originProtection`: add the site's Transform Rule (below).
5. Create a CNAME for each hostname to the distribution. Proxied records use
   TTL automatic; unproxied ones 300 seconds.

Every record carries the comment `Managed by Anvil (<project> <site>, …)`.

#### Proxy

`dns.proxied` defaults to true: someone choosing Cloudflare wants it in
front. Things to know while proxied:

- **SSL/TLS mode must be "Full (strict)"** for the zone. "Flexible" makes
  Cloudflare call CloudFront over HTTP, CloudFront redirects back to HTTPS,
  and the browser loops. Anvil doesn't change zone settings.
- **Certificates for deep names.** Cloudflare's free Universal SSL
  certificate covers the apex and one level of subdomain. Names two or more
  levels below the zone (`www.damien.youlosei.win`) need Advanced Certificate
  Manager (about $10/month per zone) with Total TLS, which issues a
  certificate for every proxied hostname at any depth. Without it browsers
  get a certificate error. `proxied: false` serves those names straight from
  CloudFront, which has its own ACM certificate.
- **CloudFront is still reachable directly** at its `*.cloudfront.net`
  name, bypassing Cloudflare's WAF, bot protection and rate limits, unless
  `originProtection` is on.
- **An AWS WAF sees Cloudflare's IP addresses**, not visitors'. Rate-based,
  IP and geo rules then apply to Cloudflare as a whole and can block real
  users. Set `clientIpHeader: 'CF-Connecting-IP'` on the `Waf`, together
  with `originProtection` so a client can't send the header itself.
- **Zone features that rewrite HTML** (Rocket Loader, Email Obfuscation,
  Zaraz) inject scripts, which breaks `kit.csp` hashes and nonces. They're
  zone settings and stay the user's call.
- **Two caches.** Cloudflare may keep serving an old copy of a static file
  after a deploy until its cache is purged.

To keep prod proxied and serve deep non-prod names from CloudFront:

```ts
dns: { provider: 'cloudflare', proxied: ctx.stage === 'prod' },
```

(In Go, `ctx.IsProduction`.)

#### Origin protection

`originProtection` stays opt-in. When it's on and the site is proxied
through Cloudflare, Anvil adds the header itself:

- One Transform Rule per site, in the zone's `http_request_late_transform`
  phase: `(http.host eq "<hostname>" [or http.host eq "<pair>"])` →
  set `x-origin-secret` to the site's secret.
- Anvil adds, updates and deletes only its own rule through Cloudflare's
  per-rule API, identified by `ref` `anvil-origin-<hostname>`. Rules created
  by hand are never touched. (Pulumi's `cloudflare.Ruleset` would own and
  rewrite the whole phase, so it isn't used.)
- The rule is created before the DNS records, so visitors never get a 403
  while the site comes up.
- The CloudFront Function checks the header before anything else and strips
  it, so the app never sees the secret.

The rule is an internal Anvil resource (`anvil:cloudflare:OriginSecretRule`,
[internal/cfrule](../../provider/internal/cfrule/cfrule.go)), registered with
the provider but not exposed in the SDKs.

Each protected site uses one Transform Rule. Cloudflare's limits per zone
([source](https://developers.cloudflare.com/rules/transform/)):

| Plan | Active Transform Rules |
|---|---|
| Free | 10 |
| Pro | 25 |
| Business | 50 |
| Enterprise | 300 |

The limit appears to be shared with other Transform Rules in the zone,
including ones added by hand. When Cloudflare refuses the rule, the deploy
error names the zone's plan and limit and suggests `originProtection: false`
for that stage, removing unused rules, or upgrading.

`originProtection` with `proxied: false` is an error: visitors would reach
CloudFront without the header and get 403.

With Route53 or `manual`, nothing adds the header for you; configure your
proxy to send `x-origin-secret` with the `originSecret` output.

## Warnings

| When | Level | Message says |
|---|---|---|
| No Route53 zone found and no provider set | warn | Falling back to manual; add the `dnsRecords` yourself or set `dns.provider` |
| Cloudflare, proxied | info | SSL/TLS mode must be Full (strict) |
| Cloudflare, proxied, name ≥ 2 levels deep | warn | Free certificate doesn't cover it; ACM + Total TLS or `proxied: false` |
| Cloudflare, proxied, no `originProtection` | warn | CloudFront is reachable directly; set `originProtection` |
| Cloudflare, proxied, WAF attached | warn | WAF sees Cloudflare IPs; set `clientIpHeader` on the Waf with `originProtection` |
| Cloudflare, `originProtection` | info | Uses one Transform Rule; plan limits; token permission |
| `originProtection` without Cloudflare | info | Configure the proxy with `originSecret`; 403 until then |
| `dns.proxied` set, not Cloudflare | warn | Ignored |
| `dns.roleArn` set, not Route53 | warn | Ignored |
| `wwwRedirect: true` on a domain that isn't apex or www (prod) | warn | Nothing to redirect |
| `dns`/`wwwRedirect` set without `domain` | warn | No effect |
| Manual, certificate not issued yet | warn | Lists the validation records to add |

Errors (the deploy stops): explicit `route53` with no zone; explicit
`cloudflare` with no token or zone not found; unknown provider; a domain
outside `dns.zone`; a stage name with no usable characters;
`originProtection` with `proxied: false`; Cloudflare refusing the origin rule.

## Account limits

Per AWS account (defaults; most can be raised in Service Quotas):

| Quota | Default | Used per site |
|---|---|---|
| Custom response headers policies | 20 | 1 (security headers) |
| Origin access controls | 100 | 1–2 |
| CloudFront Functions | 100 | 1 with origin protection or the www redirect |
| KeyValueStores | 200 | 1 with origin protection |
| Distributions | 500 | 1 |
| ACM certificates (us-east-1) | 2,500 live, 5,000 per rolling year | 1 |

The response headers policy quota caps one account at 20 sites unless it's
raised. Destroying a stage deletes its certificate; expired and revoked
certificates still count toward ACM's 2,500.

## Decisions

| Decision | Chosen | Rejected |
|---|---|---|
| Option layout | `domain` top-level, record settings in `dns` | Everything inside a `domain` object (breaks `domain: '…'`); everything inside `dns` |
| Non-prod naming | Stage inserted above the zone, by environment | Per-kind templates (`devStages`, `prStages`, `stageDomain`); `<stage>-www` single-level names |
| apex/www | Paired on prod, written name canonical | Rewriting `domain` to add `www` |
| Cloudflare proxy | On, warn on deep names | Off for deep names automatically; off by default |
| Zone in another account | `dns.roleArn` | Delegating subdomains (no shared label under the apex) |
| Origin protection | Opt-in; Anvil adds one Transform Rule per site | Proxy turns it on; one rule per environment; Cloudflare IP allow-list on the WAF |
| Cloudflare token | `CLOUDFLARE_API_TOKEN` in the deploy shell | OIDC (Cloudflare's API doesn't support it); delegating to Route53 |

Later: keeping the token out of CI with a Lambda broker that holds it in
Secrets Manager and checks record names; Org-managed DNS roles.
