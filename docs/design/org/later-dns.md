# Org — Later: DNS roles for site domains

Part of the [Org design](README.md). **Parked:** not in v1. Single-account
setups work today; this note captures how Org would make the multi-account
case automatic.

## Problem

Site components (`SvelteKitSite`) create their own DNS records: the ACM
validation record and the alias records for the site's hostname. On Route53
they look up the domain's public hosted zone **in the account they deploy
to**.

In an org, one domain has one hosted zone, usually in the Network account
("Transit Gateway, egress, shared VPCs, DNS" in [Accounts](05-accounts.md)),
while each stage deploys to its own workload account. A `payments-dev`
deploy of `damien.youlosei.win` can't see the `youlosei.win` zone in
Network. Delegating a subdomain doesn't help: non-prod hostnames sit directly
under the apex (`<stage>.youlosei.win`, `www.<stage>.youlosei.win`), so
there's no shared label to delegate.

## What exists without Org

See [Site domains and DNS](../site-domains.md) for the full behaviour.

`dns.route53.roleArn` on the site component: the deploy assumes that role (in the
account that owns the zone) for the zone lookup and record changes only. The
certificate and CloudFront distribution stay in the workload account.
Users create the role themselves.

```ts
dns: { route53: { roleArn: 'arn:aws:iam::111111111111:role/anvil-dns' } }
```

## What Org would add

1. **Zones declared in Org config**, with the account that owns each:

   ```ts
   new Org({
     tier: 'network',
     dns: { zones: { 'youlosei.win': { account: 'network' } } },
   });
   ```

2. **One role per environment per zone** in the owning account, e.g.
   `anvil-dns-prod` and `anvil-dns-nonprod`. Trusted by the workload
   accounts of that environment only (by account ID, plus
   `aws:PrincipalOrgID`).

3. **Record-name limits per environment** using Route53's IAM condition
   keys (`route53:ChangeResourceRecordSetsNormalizedRecordNames`,
   `…RecordTypes`, `…Actions`):

   | Role | May change |
   |---|---|
   | `anvil-dns-prod` | The apex, `www`, the prod hostnames and their `_…` ACM validation records |
   | `anvil-dns-nonprod` | `*.youlosei.win` except the prod names; A, AAAA, CNAME only |

   Neither role may touch MX, TXT, NS or CAA, so a compromised pipeline
   can't redirect mail, prove domain ownership to a third party, or get a
   certificate issued another way. A compromised dev pipeline can't touch
   prod's names at all.

4. **Discovery through the account parameter.** Org already writes
   `/anvil/org/account` into every account (see
   [App ↔ account linking](11-app-linking.md)). It would gain the zones the
   account may use and the role for each:

   ```json
   "dns": { "youlosei.win": "arn:aws:iam::111111111111:role/anvil-dns-nonprod" }
   ```

   A site component with no `dns.route53.roleArn` reads the parameter, finds the
   zone, and assumes the role. `roleArn` set explicitly always wins.

5. **Guardrails.** The `denyRoute53Domains` SCP stays as is; the DNS roles
   only need `route53:ChangeResourceRecordSets`, `ListResourceRecordSets`,
   `ListHostedZonesByName`, `GetHostedZone` and `GetChange`.

## Open

- Whether Org creates the hosted zone too, or only adopts an existing one
  (registrar nameservers must point at it either way).
- Cloudflare zones: Org could hold the API token in the Network account
  behind the same per-environment split (a Lambda that checks record names,
  since Cloudflare tokens can't be limited by name). See
  [Site domains and DNS](../site-domains.md).
- Exact prod-name list for the condition keys: derived from declared
  workloads, or listed in the zone config.
