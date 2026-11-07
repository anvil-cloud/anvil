# Org — Identity Center

Part of the [Org design](README.md).

## Identity Center

### Setup is manual

The Identity Center instance is set up in the console, like the org:
turning it on, choosing its home region (can't be changed without deleting
the instance), and connecting the identity source. An external provider
(Okta, Entra ID, Google Workspace) involves SAML metadata and a SCIM sync
token, which must never be in code or state.

Preflight checks it and reports what it found:

```
✓ Identity Center found: ap-southeast-2, source: Okta (external)
```

```
✗ Identity Center isn't enabled. Turn it on in the management account console
  (Identity Center → Enable), choose its region and identity source, then deploy.
```

Anvil then manages permission sets and assignments (below).

### Delegated administration

The instance always lives in management. **Default: no delegated admin.**
Anvil manages permission sets and assignments from management as code, so
the Org pipeline is the admin and no human needs to edit Identity Center
(`anvil org access` flags anything made by hand). Docs explain this against
AWS's advice to delegate: delegation exists to keep humans out of
management, and with Anvil none do this work there.

**Opt-in** for orgs with human Identity Center admins (SRA uses Shared
Services):

```ts
identityCenter: { delegatedAdmin: 'shared-services', … },
```

```
+ identity center delegated admin   shared-services (333333333333)
    scp   deny identitystore:CreateGroupMembership   (external identity source: membership comes from Okta)
    scp   deny sso-directory:CreateBearerToken       (no new SCIM tokens from this account)
```

- Org applies AWS's own recommended SCPs to that account: deny
  `identitystore:CreateGroupMembership` when the identity source is external,
  and deny `sso-directory:CreateBearerToken` always (lifted temporarily in
  `run` with a reason for SCIM token rotation).
- Must be an account in the Infrastructure OU (Shared Services in `sra`);
  preview errors for Security OU accounts (SRA keeps identity out of
  Security), Log Archive, management and workload accounts.
- A delegated admin can't change permission sets provisioned to management;
  those stay management-only.
- Anvil still deploys Identity Center config from management; delegation
  is for human admin tasks.

**MFA**: Identity Center's MFA settings apply only to its own directory or
AD. Preflight warns when the source is the built-in directory and MFA
isn't required; with an external provider, docs point to the provider's MFA.

### Preset permission sets

Built on AWS's job-function managed policies; each opt-in.

```ts
identityCenter: {
  permissionSets: {
    admin:     { enable: true, session: '1h' }, // AdministratorAccess
    developer: { enable: true },                // PowerUserAccess (no IAM, no Organizations)
    readOnly:  { enable: true },                // ReadOnlyAccess (reads data)
    viewOnly:  { enable: true },                // ViewOnlyAccess (metadata only, no data)
    security:  { enable: true },                // SecurityAudit + ViewOnlyAccess + read Log Archive
    securityOperator: { enable: true },         // act on findings in Audit (see Security OU access)
    billing:   { enable: true },                // Billing
    orgAdmin:  { enable: true, session: '1h' }, // management only; deploys the org, closes Suspended accounts
  },
},
```

```
+ permission sets   admin (1h), orgAdmin (1h), developer, readOnly, viewOnly, security, securityOperator, billing (4h)
```

- `session` is the only field besides `enable`: how long a sign-in lasts.
  **Default 4h** for every set; AWS allows 1h–12h. Docs suggest shorter for
  `admin` and `orgAdmin`.
- Managed policies, inline policies and boundaries aren't preset fields;
  changing them makes it a custom set.
- Every permission set Org creates for management (`orgAdmin`, `billing`,
  deployer sets) carries `ProtectManagementSecurity` (see Accounts →
  Protecting management resources).
- `orgAdmin` is the only set with `organizations:CloseAccount`, limited to
  accounts tagged `anvil:lifecycle=suspended` (see Who can close accounts).

**`orgAdmin`** is the built-in local deployer: AdministratorAccess **in
management only** (named users, `1h` suggested), so a local deploy can do
anything CI can, including updating the CI roles and turning on new
services. Explicit denies keep what should never happen blocked:

```json
{ "Sid": "NeverAsOrgAdmin",
  "Effect": "Deny",
  "Action": ["organizations:LeaveOrganization", "organizations:DeleteOrganization",
             "organizations:DisablePolicyType"],
  "Resource": "*" },
{ "Sid": "CloseOnlySuspended",
  "Effect": "Deny",
  "Action": ["organizations:CloseAccount", "organizations:RemoveAccountFromOrganization"],
  "Resource": "*",
  "Condition": { "StringNotEquals": { "aws:ResourceTag/anvil:lifecycle": "suspended" } } }
```

Not a generated least-privilege policy: permission-set changes only apply
to new sign-ins, so a local deploy that needs new permissions would stall
mid-deploy, and local is the recovery path.

**Data access.** `readOnly` (AWS ReadOnlyAccess) **reads data**: S3
objects, DynamoDB items, secrets, queue messages. `viewOnly` (AWS
ViewOnlyAccess) sees what exists and how it's configured, never contents;
docs recommend it over `readOnly` for prod.

`security` is metadata only, so the security team sees every account's
configuration but no customer data. The one data read it has is the logs,
scoped by an inline statement Anvil adds:

```json
{ "Sid": "ReadLogArchive",
  "Effect": "Allow",
  "Action": ["s3:GetObject", "s3:ListBucket"],
  "Resource": ["arn:aws:s3:::anvil-org-logs-<id>", "arn:aws:s3:::anvil-org-logs-<id>/*"] },
{ "Sid": "DecryptLogArchive",
  "Effect": "Allow",
  "Action": "kms:Decrypt",
  "Resource": "arn:aws:kms:<region>:<audit>:key/<id>" }
```

(`DecryptLogArchive` only when `logArchive.kms` is on.) Reading workload
data during an incident is deliberate: a custom set assigned to the
account, or break glass.

### Custom permission sets

In the same map, in config. Preset names (`admin`, `developer`, `readOnly`,
`viewOnly`, `security`, `securityOperator`, `billing`, `orgAdmin`) are reserved; any other name is custom
and needs `managedPolicies` and/or `inlinePolicy`. Unlike SCPs, a permission
set needs no `attach` handles from `ctx` (assignments say where it applies),
so it fits in config.

```ts
// policies/data-engineer.ts
export const dataEngineerPolicy = anvil.aws.policyDocument({
  statements: [
    { effect: 'Allow', actions: ['glue:*', 'athena:*'], resources: ['*'] },
    { effect: 'Allow', actions: ['s3:GetObject', 's3:PutObject'], resources: ['arn:aws:s3:::acme-data-*/*'] },
  ],
});
```

```ts
// index.ts
identityCenter: {
  permissionSets: {
    admin:     { enable: true, session: '1h' },
    developer: { enable: true },
    dataEngineer: {
      enable: true,
      managedPolicies: ['ReadOnlyAccess'],
      inlinePolicy: dataEngineerPolicy,
      session: '8h',
    },
    vendorSupport: {
      enable: true,
      inlinePolicy: anvil.aws.policyFile('./policies/vendor-support.json'),
    },
  },
},
```

```
  + permission sets
      admin           AdministratorAccess                      1h
      developer       PowerUserAccess                          4h
      dataEngineer    ReadOnlyAccess + inline (2 statements)   8h   ✓ ValidatePolicy
      vendorSupport   inline (./policies/vendor-support.json)  4h   ✓ ValidatePolicy
```

```
✗ identityCenter.permissionSets.analyst: custom set needs managedPolicies or inlinePolicy
✗ identityCenter.permissionSets.developer: managedPolicies isn't allowed on a preset;
  use a different name for a custom set
```

`inlinePolicy` takes the same policy value as SCPs (`policyDocument()` or
`policyFile()`).

### Assignments

**Member accounts: by group**, in `identityCenter.groups`: each group lists
the permission sets it gets and where. Groups only, never individual users
(AWS best practice); groups come from the identity source and are looked up
by name at deploy. **The management account is the exception: named users
only** (see Management access).

```ts
identityCenter: {
  permissionSets: { /* … */ },
  groups: {
    Engineers: {
      developer:    { stages: ['dev'] },
      viewOnly:     { stages: ['prod'] },
    },
    'Data Team': {
      dataEngineer: { accounts: ['analytics-prod', 'analytics-dev'] },
    },
    Security: {
      security:     { all: true },
    },
    Platform: {
      admin:        { ou: ['Workloads', 'Infrastructure'] },
    },
  },
},
```

```
+ assignments   (12)
    Engineers    developer      website-dev, analytics-dev
                 viewOnly       website-prod, analytics-prod
    Data Team    dataEngineer   analytics-prod, analytics-dev
    Security     security       all 9 accounts
    Platform     admin          website-prod, website-dev, analytics-prod, analytics-dev, network
```

Targets, exactly one per entry:

| Target | Means |
|---|---|
| `accounts: [...]` | Named accounts |
| `stages: [...]` | Every workload account in those stages |
| `ou: [...]` | Every account under those OU paths, nested included |
| `all: true` | Every member account (not management) |

- Identity Center assigns to accounts only, so Anvil expands stages, OUs and
  `all` to accounts, and adds new accounts to matching assignments when
  they're created. Preview lists the expanded accounts.
- Reads as an access list ("who can touch prod?"), which is what access
  reviews and auditors ask for.

```
✗ identityCenter.groups.Engineers: group not found in Identity Center (source: Okta)
✗ identityCenter.groups.Engineers.developer: set exactly one of accounts, stages, ou, all
```

### Pinned IDs

Config uses names only; no IDs in code. On first deploy Anvil records the
Identity Center ID behind every group and user name **in state**, and every
preview checks them. This catches a group or user being deleted and
re-created with the same name in the identity provider (new ID, possibly
different members), which would otherwise silently inherit its access.

```
⚠ identityCenter.groups.Platform: ID changed since last deploy
    was  9067c3d2-…-a1f0 (deleted)
    now  4f21b8e9-…-77c3 (created 2026-10-31 by SCIM)
  The group was re-created in your identity provider; its members may differ.
  Its assignments move to the new ID on deploy.
```

A pinned ID that now has a different name (renamed in the identity
provider) gets a clear message instead of "not found":

```
✗ identityCenter.groups.Engineers: not found, but its pinned group
  9a12…-e4 is now named "Engineering". Update the name in config.
```

- Applies to groups and to management users.
- **Granted with the warning:** the deploy moves the assignments to the new
  ID and updates the pin. No hold and no extra command; re-creating a group
  or user is rare, and the warning shows in preview, deploy output and the
  CI log. Accepted risk: a re-created principal gets access on the next
  deploy without review.

### Management access

AWS: assign **users directly** to permission sets in the management account,
not groups, because anyone who can change a group's membership in the
identity provider could add themselves and gain management access. So
management access is a separate block of named users:

```ts
identityCenter: {
  management: {
    orgAdmin: { users: ['alice@acme.com', 'bob@acme.com'] },
    billing:  { users: ['finance-lead@acme.com'] },
  },
  groups: { /* member accounts only */ },
},
```

```
+ management access   (users, not groups, per AWS)
    orgAdmin   alice@acme.com, bob@acme.com
    billing    finance-lead@acme.com
```

- Only `orgAdmin`, `billing` and sets listed in `deployers.permissionSets`
  can be assigned in management.
- Changing who has management access is a code change through PR review,
  not an identity-provider group edit.
- Users are looked up by **user name** (emails aren't guaranteed unique under
  SCIM), and more than one match is an error; unknown users are a
  preview error.
- `anvil org access` lists management users first.

```
✗ identityCenter.management.orgAdmin: user carol@acme.com not found in Identity Center (source: Okta)
✗ identityCenter.management.admin: only orgAdmin and billing can be assigned in management
```

### Security OU access

By default only these sets can be assigned to Security OU accounts; anything
else is an error, including through `all: true` or `ou`, unless explicitly
allowed in `run` with a reason (below):

| Account | Allowed sets |
|---|---|
| Log Archive | `security`, `viewOnly`, `billing` |
| Audit | `security`, `viewOnly`, `billing`, `securityOperator` |
| Emergency | **None.** No Identity Center assignments at all: break glass must work when Identity Center doesn't |
| Custom Security OU accounts (e.g. `secops`) | Same as Audit |

```
✗ identityCenter.groups.Platform.admin: all: true includes log-archive and audit;
  admin isn't allowed in Security OU accounts. Use ou: ['Workloads', 'Sandbox']
  or accounts: [...] instead.
```

`all: true` only reaches Security OU accounts for `security`, `viewOnly` and
`billing`; for other sets it errors as above. `readOnly` isn't allowed there
because it would read every log in Log Archive.

**Explicit opt-in in `run`.** Widening Security OU access is treated like
weakening a guardrail: in `run`, with a required `reason`, named accounts
only (no `all` or `ou`). Covers small teams where one group runs both
platform and security.

```ts
run(ctx) {
  ctx.identityCenter.allowInSecurityOu({
    group: 'Platform',
    permissionSet: 'admin',
    accounts: ['audit'],
    reason: 'platform team runs security tooling until SecOps is hired (TICKET-88)',
  });
}
```

```
⚠ Security OU access widened in run()
    Platform   admin   audit
      "platform team runs security tooling until SecOps is hired (TICKET-88)"
```

- The assignment itself still goes in `groups` as normal; the `run` call
  only lifts the block for that group, set and account.
- The reason shows in preview and in `anvil org access`, next to the
  assignment.
- Works for presets and custom sets. Never for the management account
  (named users in `management` only) and never changes the break glass.

**`securityOperator` preset**: `security` is read-only, but someone in Audit
has to act on findings (archive false positives, suppression rules,
workflow status). `securityOperator` is **triage only**: SecurityAudit +
ViewOnlyAccess (same base as `security`, no data reads) plus an inline
allow-list. Not AWS's FullAccess policies: they include disable/delete,
Macie data reveal, and whatever AWS adds later.

| Service | Allowed |
|---|---|
| Security Hub | Update findings (workflow status, notes), insights, automation rules |
| GuardDuty | Archive/unarchive findings, suppression filters |
| Inspector | Suppression filters, findings reports |
| Macie | Findings filters (including archive rules) |
| Detective | Search, investigations |

Plus explicit denies, a second layer under the SCP:

```json
{ "Sid": "NeverDisableOrReveal",
  "Effect": "Deny",
  "Action": [
    "guardduty:DeleteDetector", "guardduty:DisassociateMembers", "guardduty:DeleteMembers",
    "guardduty:UpdateDetector", "guardduty:CreateIPSet", "guardduty:UpdateIPSet",
    "securityhub:DisableSecurityHub", "securityhub:DisassociateMembers", "securityhub:DeleteMembers",
    "securityhub:BatchDisableStandards", "securityhub:UpdateOrganizationConfiguration",
    "inspector2:Disable", "inspector2:DisassociateMember",
    "macie2:DisableMacie", "macie2:DisassociateMember", "macie2:DeleteMember",
    "macie2:GetSensitiveDataOccurrences", "macie2:UpdateRevealConfiguration",
    "detective:DeleteGraph", "detective:DisassociateMembership", "detective:DeleteMembers"],
  "Resource": "*" }
```

- **No GuardDuty trusted IP lists** (`CreateIPSet`): they stop GuardDuty
  looking at an IP at all. Suppression filters archive findings but keep
  them stored and searchable.
- **No Macie sample reveal**: operators see where sensitive data is (bucket,
  object, type, count), never the values.
- New AWS actions aren't allowed until a baseline release adds them; a
  missing triage permission is an `AccessDenied` (fails safe).
- The same deny list is part of `protectSecurityServices`, so the SCP covers
  every disable and disassociate action too.
- Worst case if a session is compromised: findings hidden from the inbox
  (archived, marked resolved), never detection turned off. Filter and
  finding changes are in CloudTrail and Security Hub history.

```ts
groups: {
  SecOps: {
    securityOperator: { accounts: ['audit'] }, // act on findings
    security:         { all: true },           // read everywhere
  },
},
```

Independent of all of this, the built-in break glass (see Break glass) always
gives a way in that can't be removed, for when Identity Center or the
identity provider is down or an incident needs more than the allowed sets.

### `anvil org access`

Read-only CLI command showing who can reach which account, from **live**
Identity Center (not config), so hand-made console assignments show up too.

```
anvil org access

Access   acme · Identity Center ap-southeast-2 · live, read 2026-10-28 14:02

                   Engineers  Payments  Data  Platform  Security  Finance  All Eng
management         orgAdmin: alice@acme.com, bob@acme.com (users) · billing: finance-lead@acme.com
log-archive            ·         ·        ·      ·      security  billing     ·
audit                  ·         ·        ·      ·      security  billing     ·
website-prod       viewOnly      ·        ·    admin    security  billing     ·
website-dev        developer     ·        ·    admin    security  billing     ·
payments-ledger        ·     developer    ·    admin    security  billing     ·
sandbox-alice          ·         ·        ·      ·      security  billing   admin

⚠ 1 assignment not managed by Anvil (created outside code):
    jsmith@acme.com (user)   admin   website-prod   created 2026-10-21 in console
```

| Flag | Shows |
|---|---|
| `--account <name>` | Every group and set reaching one account |
| `--group <name>` | Every account one group reaches |
| `--set <name>` | Who has that set, and where |
| `--users` | Expands groups into members, for access reviews |
| `--format csv \| json` | Export for auditors |
| `--diff` | Config vs live: what deploy would add/remove, what's unmanaged |

- Reads only (Identity Center and Identity Store read APIs), with
  management credentials or Identity Center's delegated admin.
- Subcommand, like `anvil compliance dashboard`; flags kebab-case.
- `anvil org dashboard` shows the same matrix as a page.
