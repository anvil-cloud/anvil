# Org — Org dashboard

Part of the [Org design](README.md).

## Org dashboard

### Hosting

**v1: local**, served by the CLI exactly like `anvil compliance dashboard`:
a static page embedded in the binary, bound to `127.0.0.1` on a random port
with a random token in the URL. The CLI reads AWS with the user's
credentials and serves the data on local `/api/*`; the browser never sees
credentials. Protections (as the compliance dashboard already does):

- Host-header check (blocks DNS rebinding);
- the token is sent in an `X-Anvil-Token` header and compared in constant
  time;
- strict CSP and `Referrer-Policy: no-referrer`;
- **every write** (e.g. adding a notification listener) is a `POST` to
  `/api/*` with the header token and an `Origin` check; the CLI makes the
  AWS call, never the browser. It shows only what those credentials can read (management:
the whole org; a member account: that account).

```
anvil org dashboard

  Org dashboard   http://127.0.0.1:52817/?t=9f3c…   (Ctrl+C to stop)
  Reading as: damien (management) · acme · 11 accounts
```

**Later: hosted** (*planned, well after v1*). The same page deployed into
Audit, so a team shares one URL:

- Always behind Identity Center sign-in; never anonymous.
- An option to take it off the public internet entirely, reachable only over
  a VPN or private network.
- Needs its own design: the backend read role (Organizations and Identity
  Center data lives in management, which conflicts with keeping management
  empty), session handling, and audit logging of who viewed what.
- *Direction (2026-11-01, to design when building it):* SSO sign-in through
  Identity Center; access scoped per group in config; a backend (e.g. API
  Gateway + functions) acting with its own narrowly scoped roles per area;
  **no user AWS credentials** in the browser or dashboard. This is also
  where listener management and chat channel permissions get designed
  (audit finding A10).

The page is kept portable (static bundle, all data through `/api/*`) so
hosting it later doesn't mean a rewrite.

### Shape

A three-level drill-down:

1. **Org**: a tile per account, grouped by OU: cost this month and trend,
   compliance %, apps and resources, findings and exceptions.
2. **Account**: cost by service and by app, compliance (Security Hub CSPM if
   on; Prowler once org-wide compliance exists), guardrails and exceptions,
   access, tagged/untagged resources, apps.
3. **App**: cost by component, resources by component (from the App's
   Pulumi state), findings for its resources, deploy history.

| Data | Source |
|---|---|
| Accounts, OUs, guardrails, exceptions | Org state, Organizations |
| Access | Identity Center (`anvil org access`) |
| Cost per account / service | Cost Explorer from management (~$0.01 per call, cached daily, ~24h lag) |
| Cost per app / component | Cost Explorer grouped by Anvil's default tags (below) |
| Compliance | Security Hub CSPM; Prowler later |
| Resources per app | Each App's Pulumi state |
| Untagged resources | Resource Explorer or the tagging API |
| Shared costs (data transfer, support, tax) | Shown as "not in an app" |

### Cost allocation tags

**Org always activates Anvil's default tags as cost allocation tags**: the
`project` and `stage` tags every App puts on every resource. Not a setting;
it's part of having an Anvil org, so per-app cost works from day one.
`requireTags` keys and other user tags aren't activated; users activate
those in the Billing console if they want them.

```
+ cost allocation tags   project, stage   (management)
    backfill             12 months requested (Apps have tagged since their first deploy)
```

- Activation is a management-only billing setting, done by Org from
  management.
- **Backfill:** on first activation Org requests a backfill of up to 12
  months (`StartCostAllocationTagBackfill`), so an adopted org whose Apps
  already tagged resources gets historical per-app cost. AWS allows one
  backfill per 24 hours and only fills periods where the tag was actually on
  the resource.
- A tag key can only be activated once it appears in billing data (up to
  ~24h after first use); until then preview shows it pending and the next
  deploy retries.
- Cost Explorer refreshes once a day, so new splits show up the next day.
