# Org — OU layout

Part of the [Org design](README.md).

## OU layout

**Org creates exactly the OUs written in `ou`, nothing more.** There are no
built-in OUs: `anvil org init` writes the AWS-recommended tree into
`anvil.config.ts` as a starting point, one entry per OU with a comment saying
what it's for, and from then on the file is the whole truth. Users add,
remove and nest OUs by editing it.

**OUs nest the way they sit in AWS.** Each OU is keyed by its name, and its
own `ou` holds the OUs inside it, so the file reads like the tree:

```ts
new Org({
  ou: {
    Security: { enable: true },
    Workloads: {
      enable: true,
      ou: {
        Prod:    { enable: true },
        NonProd: { enable: true },
        Data:    { enable: true }, // added by the user
      },
    },
    Partners:  { enable: true }, // new top-level OU
    Suspended: { enable: true },
  },
});
```

Everywhere else (an account's `ou`, guardrail attachments, `ctx.ou`) an OU is
named by its **path**: `'Workloads/Prod'`, `'Workloads/Data'`. The path is
also its name in state, so moving an OU block under a different parent is a
replace (preview shows it), while reformatting the file changes nothing.

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

`anvil org init` offers this tree in [tiers](05-accounts.md#starting-layouts-anvil-org-init):
Infrastructure and Deployments are only written for tiers that put accounts
in them. Workloads/Prod, Workloads/NonProd and Sandbox start empty, because
they're where the user's own accounts go. Suspended is empty until an account
is removed.

Nested OUs inherit their parent's policies: anything on Workloads applies to
Prod and NonProd, which add their own stricter ones. AWS allows 5 levels
below Root.

| Tier (init only) | OUs written |
|---|---|
| `minimum` | Security, Workloads (with Prod and NonProd inside), Suspended |
| `network` | + Infrastructure |
| `sra` | + Infrastructure, Deployments |
| every tier | Sandbox (asked, default yes), PolicyStaging (asked, default no) |

- **Every OU is just an entry in config.** Removing its block removes the
  OU and everything nested in it (AWS refuses while any of them still holds
  accounts, so preview fails first, naming them). There is no `disable`:
  delete the block.
- **Keys are names, not paths.** `'Workloads/Prod': {...}` as a key fails
  preview, showing the nested form to write instead.
- **Security** holds the core accounts and **Suspended** is where removed
  accounts go, so the features that need them (account creation, removal)
  fail preview with a clear message if they're missing. Nothing else
  depends on a particular OU.

Example: a `minimum` org with no sandboxes and an extra `Data` OU. Init
wrote the tree with Sandbox off; the user added `Workloads/Data`.

```ts
new Org({
  ou: {
    Security: { enable: true },
    Workloads: {
      enable: true,
      ou: {
        Prod:    { enable: true },
        NonProd: { enable: true },
        Data:    { enable: true },
      },
    },
    Suspended: { enable: true },
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

Adding a Network account later means adding `Infrastructure` and the
account to config; there's no tier to change.

### Custom OUs

Users can create their own OUs **anywhere**: at the top level or under any
other listed OU. It's their org.

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

### Custom OU and account syntax

Custom OUs are written **nested, the same way as the recommended ones**
(there's no difference once they're written), and **every account is
written inside its OU** (see [Writing accounts](05-accounts.md#writing-accounts)).

```ts
new Org({
  email: 'aws+{account}@acme.com',

  ou: {
    Security: {
      enable: true,
      accounts: {
        'log-archive': { enable: true },
        audit: { enable: true },
        emergency: { enable: true },
      },
    },
    Workloads: {
      enable: true,
      ou: {
        Prod: {
          enable: true,
          accounts: { 'payments-prod': { enable: true } },
          ou: {
            Data: {
              enable: true,
              accounts: { 'data-lake': { enable: true, tags: { team: 'data' } } },
            },
          },
        },
        NonProd: {
          enable: true,
          accounts: { 'payments-dev': { enable: true } },
        },
      },
    },
    Suspended: { enable: true },
    Partners: {
      enable: true,
      tags: { kind: 'partner' },
      ou: {
        Acme: {
          enable: true,
          accounts: { 'acme-integration': { enable: true, tags: { partner: 'acme' } } },
        },
      },
    },
    Exceptions: {
      enable: true,
      accounts: { 'legacy-app': { enable: true, tags: { owner: 'ops' } } },
    },
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

- Each OU value is an object (`enable`, `tags`, `ou`, `accounts`,
  `renamedFrom`), so adding fields is never a breaking change. Policies
  attach from `run` (`attach`).
- An account has no `ou` field: it lives where it's written. Moving its
  block moves it.
- Account names are unique across the whole org (`payments-prod` can't be
  written in two OUs).
- Removing an account follows the removal rule (moved to Suspended,
  released from state). Removing an OU block also removes the accounts
  written in it, so preview shows each of them as moved to Suspended
  before the OU goes.
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
