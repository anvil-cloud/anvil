# Org — OU layout

Part of the [Org design](README.md).

## OU layout

Org creates the AWS-recommended OU tree, and users add their own OUs on top,
at the top level or under any recommended OU:

```ts
new Org({
  ou: {
    'Workloads/Data': { enable: true }, // adds Workloads/Data
    'Partners':       { enable: true }, // new top-level OU
  },
});
```

Recommended tree (AWS SRA / *Organizing Your AWS Environment*):

```
Root
├── Security          Log Archive, Audit, Emergency
├── Infrastructure    Network, Shared Services, Backup
├── Deployments       CI/CD
├── Workloads
│   ├── Prod
│   └── NonProd
├── Sandbox
└── Suspended         deny-all; accounts awaiting closure
```

OUs **grow with the [tier](05-accounts.md#tiers)**: Infrastructure and Deployments appear
only once a tier puts accounts in them. Workloads/Prod, Workloads/NonProd and
Sandbox come with `minimum` but start empty, because they're where the user's
own accounts go. Suspended is empty until a downgrade.

Nested OUs inherit their parent's policies: anything on Workloads applies to
Prod and NonProd, which add their own stricter ones. AWS allows 5 levels
below Root.

| Tier | OUs added |
|---|---|
| `minimum` | Security, Workloads/Prod, Workloads/NonProd, Sandbox, Suspended |
| `network` | + Infrastructure |
| `sra` | + Deployments |

- **Sandbox is the only recommended OU users can turn off**
  (`ou: { Sandbox: { disable: true } }`), since not every org gives developers their own
  accounts. All others stay once their tier creates them.
- **Security** holds the required accounts; **Suspended** is where tier
  downgrades move accounts.
- Users can always add their own OUs, at the top level or under any
  recommended OU.

Example: a `minimum` org with no sandboxes and an extra `Data` OU.

```ts
new Org({
  tier: 'minimum',
  ou: {
    Sandbox: { disable: true },
    'Workloads/Data': { enable: true },
  },
});
```

```
Root
├── Security       Log Archive, Audit, Emergency
├── Workloads
│   ├── Prod
│   ├── NonProd
│   └── Data
└── Suspended
```

Moving to `tier: 'network'` then adds Infrastructure with the Network account.

### Custom OUs

Users can create their own OUs **anywhere**: at the top level or under any
recommended OU. It's their org.

AWS's guidance is soft (nothing in AWS, Control Tower or LZA enforces it), so
Anvil doesn't block anything. The docs repeat it next to the feature:

> Base OUs on function or a shared set of controls, not on your reporting
> structure. Org charts change more often than security needs, so OUs built
> around teams keep needing reshuffling, and moving an account changes which
> guardrails apply to it. Use tags for grouping by owner, team or cost
> centre.

Costs of ignoring it are soft: more places to misattach or forget a
guardrail, the 5-level depth limit used up faster, and reorganisations that
move accounts between guardrails. Preview may hint when a custom OU has no
policies of its own; it never blocks.

### Custom OU and free-form account syntax

Custom OUs are **flat, keyed by full path**, in the same `ou` map that turns
Sandbox off. Accounts that aren't workloads or sandboxes go in `accounts`,
naming their OU by path.

```ts
new Org({
  tier: 'minimum',
  email: 'aws+{account}@acme.com',
  stages: { prod: { ou: 'Prod' }, dev: { ou: 'NonProd' } },
  workloads: { payments: { stages: ['prod', 'dev'] } },

  ou: {
    Sandbox: { disable: true },
    'Partners':            { enable: true, tags: { kind: 'partner' } },
    'Partners/Acme':       { enable: true },
    'Workloads/Prod/Data': { enable: true },
    'Exceptions':          { enable: true },
  },

  accounts: {
    'data-lake':        { ou: 'Workloads/Prod/Data', tags: { team: 'data' } },
    'acme-integration': { ou: 'Partners/Acme',       tags: { partner: 'acme' } },
    'legacy-app':       { ou: 'Exceptions',          tags: { owner: 'ops' } },
  },
});
```

```
Root
├── (management account, at Root; read only)
├── Security
│   ├── log-archive
│   ├── audit
│   └── emergency
├── Workloads
│   ├── Prod
│   │   ├── payments-prod
│   │   └── Data
│   │       └── data-lake
│   └── NonProd
│       └── payments-dev
├── Suspended
├── Partners
│   └── Acme
│       └── acme-integration
└── Exceptions
    └── legacy-app
```

- Each OU value is an object (`enable`, `tags`, `renamedFrom`), so adding
  fields is never a breaking change. Policies attach from `run` (`attach`).
- Every path segment's parent must exist: a recommended OU (in the current
  tier) or another declared custom OU. Preview fails otherwise, with a
  "did you mean" suggestion. In TypeScript, `accounts[].ou` is typed from the
  `ou` keys plus the built-in OUs, so typos are editor errors.
- Free-form accounts keep their exact name (no `-<stage>` suffix), get the
  org email pattern, and get no `anvil:workload` / `anvil:stage` tags.
- Account names are unique across workloads, sandboxes and `accounts`
  (`payments-prod` can't also be declared in `accounts`).
- Removing a free-form account follows the downgrade rule (moved to
  Suspended, released from state). Removing a custom OU that still holds
  accounts is a preview error.
- Security is open like every other OU (see below).

### The Security OU

Users can add custom child OUs and accounts to Security (e.g. forensics, a
SIEM). The three core accounts stay protected because their strict
guardrails attach **to the accounts themselves**, not to the OU:

```
Root            org-wide guardrails     → every account
└── Security    Security OU baseline    → every account in Security, user's included
    ├── log-archive   + Log Archive lockdown   → this account only
    ├── audit         + Audit lockdown         → this account only
    ├── emergency     + protectBreakGlass      → this account only (see Break glass)
    └── siem          (OU baseline only)
```

| Policy | Attached to | log-archive | audit | emergency | user account |
|---|---|---|---|---|---|
| Deny leaving the org, protect CloudTrail/GuardDuty | Root | ✅ | ✅ | ✅ | ✅ |
| Security baseline (no public resources; plus `allowedRegions` when set, at Root) | Security OU | ✅ | ✅ | ✅ | ✅ |
| Log Archive lockdown (logs can't be deleted or changed; humans read-only) | log-archive | ✅ | | | |
| Audit lockdown (security service config only changeable by `anvil-org-automation`) | audit | | ✅ | | |
| `protectBreakGlass` (only `break-glass-*` users, no workloads; root kept; see Break glass) | emergency | | | ✅ | |

- User accounts in Security get the **baseline only**, not the lockdowns, so
  tools like a SIEM keep working.
- Stricter rules for a user's own account are opt-in, attached to that
  account (shape decided with guardrails).
- Cost: account-level policies are less visible than OU-level ones, and use
  part of the 10-SCPs-per-target limit on Log Archive and Audit.
- Policy contents above are illustrative; the guardrails topic defines them.
- **User docs must explain this split**: what everything in Security
  inherits, what only Log Archive and Audit get, and how to make a custom
  security account stricter.
