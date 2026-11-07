# Org — App ↔ account linking

Part of the [Org design](README.md).

## App ↔ account linking

An App names what it belongs to with `org` in its config; Anvil checks the
deploy credentials are for the right account. Credentials still come from
the environment (most deploys run in CI with an OIDC role per account), so
Anvil doesn't sign in or pick profiles.

```ts
new anvil.App({
  org: { workload: 'payments' },   // stage prod → payments-prod, dev → payments-dev
  run(ctx) { /* … */ },
});
```

| `org` | Links to |
|---|---|
| `{ workload: 'payments' }` | `payments-<stage>`, per stage |
| `{ account: 'data-lake' }` | One free-form account, every stage |
| `{ sandbox: 'alice' }` | `sandbox-alice` |

Exactly one key. Without `org`, an App deploys wherever its credentials point
(as today), still refused in the management account.

**Stage must be explicit when `org` is set.** The stage belongs to each
deploy, not the App (one App deploys dev, then prod), so there's no `stage`
inside `org`. Today a missing stage falls back to the active stage, then
the OS username, then `"dev"`; with `org` set, only the first two count:

| Stage from | With `org` |
|---|---|
| `--stage prod` | ✅ (CI should always pass it) |
| Active stage in `anvil.yaml` (`anvil stage set dev`) | ✅ set on purpose |
| OS username or `"dev"` fallback | ✗ error |

```
✗ No stage set. This App is linked to workload payments (stages: prod, dev).
  Pass --stage <prod|dev>, or run `anvil stage set <stage>`.
✗ Stage damien isn't a stage of workload payments (stages: prod, dev).
```

| `org` | Stage does |
|---|---|
| `workload` | Picks the account (`prod` → `payments-prod`); must be one of the workload's stages |
| `account`, `sandbox` | Every stage goes to the one account; stage only names the stack |

Apps without `org` keep today's fallbacks.

```
✗ Stage prod of workload payments deploys to payments-prod (222222222222),
  but these credentials are for payments-dev (444444444444).
✗ Workload payments has no stage perf (stages: prod, dev). Add it in Org config.
```

**How the App knows.** Member accounts can't call the Organizations account
APIs, so Org writes a read-only SSM parameter, `/anvil/org/account`, into
each account (in every region the org allows):

```json
{
  "org": "acme",
  "orgId": "o-ab12cd34ef",
  "account": "payments-prod",
  "accountId": "222222222222",
  "workload": "payments",
  "stage": "prod",
  "ou": "Workloads/Prod",
  "guardrails": {
    "allowedRegions": ["ap-southeast-2"],
    "usEast1": { "acm": true, "waf": true, "lambdaEdge": false }
  }
}
```

- The App reads it from its own account and checks workload and stage, and
  that `accountId` matches `sts:GetCallerIdentity` and `orgId` matches
  `organizations:DescribeOrganization` (any member can call it). A
  hand-made parameter in another account, or another Anvil org with the
  same workload name, fails the check.
- The `guardrails` summary lets App preview catch components the SCPs would
  block, since preview never calls create APIs and members can't read SCPs:
  `✗ SvelteKitSite "web": protection edge-oac needs Lambda@Edge in us-east-1,
  which this org blocks`. Required tags come from `tag:ListRequiredTags`
  instead (see requireTags).
- The parameter is protected by SCP from change in members, and written in
  the same deploy as the policies it summarises.
- Custom SCPs from `run` aren't in the summary; Anvil can't simulate
  arbitrary policies.

*Not planned:* `anvil org login` to sign in through Identity Center and pick
profiles per stage. CI already picks credentials; revisit if local deploys
need it.
