# Org — Accounts

Part of the [Org design](README.md).

## Accounts

### Tiers

AWS treats Log Archive and Audit as required everywhere (Control Tower and
LZA force them), and Well-Architected recommends a dedicated emergency
access account; the rest depend on need. Org exposes this as tiers so
users start small and step up when ready:

```ts
new Org({ tier: 'minimum' }) // Log Archive, Audit, Emergency
new Org({ tier: 'network' }) // + Network
new Org({ tier: 'sra' })     // + Shared Services, Deployments, Backup
```

| Account | OU | Tier | Purpose |
|---|---|---|---|
| Log Archive | Security | minimum | Org CloudTrail, Config history, immutable logs |
| Audit (Security Tooling) | Security | minimum | Delegated admin for every security service, CloudTrail included; break-glass event bus; notification topic |
| Emergency | Security | minimum | Break-glass IAM users only (see Break glass); root kept, nothing else runs there |
| Network | Infrastructure | network | Transit Gateway, egress, shared VPCs, DNS |
| Shared Services | Infrastructure | sra | Directory, shared tooling, golden AMIs |
| Deployments | Deployments | sra | CI/CD into workload accounts |
| Backup | Infrastructure | sra | Central AWS Backup vault |

Tiers create both OUs and accounts: `minimum` creates 3 accounts, `network`
4, `sra` 7. Org also fills its own accounts (the log bucket in Log Archive,
security services in Audit, shared networking in Network).

### Workload accounts

Workload accounts (one per workload per environment, as AWS recommends) and
sandboxes are **declared by the user**. Tiers never create them. They arrive
empty apart from org-wide guardrails and security services; what runs inside
them comes from an `App`.

**Stages** (environments) and the OU each one lives in are **defined once at
the org level**; workloads pick from that list. Placement decides which
guardrails apply, so it is never inferred. Accounts are added rarely, so the
extra edit for a new stage is acceptable.

The name `stages` matches App's stage concept, so App linking later reads
naturally: the `payments` App's `prod` stage deploys to `payments-prod`. Org
itself still has no stages of its own (one Org per organization).

```ts
new Org({
  tier: 'minimum',
  email: 'aws+{account}@acme.com',

  stages: {
    prod:    { ou: 'Prod' },
    perf:    { ou: 'Prod' },
    staging: { ou: 'NonProd' },
    dev:     { ou: 'NonProd' },
  },

  workloads: {
    payments:  { stages: ['prod', 'perf', 'staging', 'dev'] },
    analytics: { stages: ['prod', 'dev'] },
  },

  sandboxes: ['alice'],
});
```

- Account names are `<workload>-<stage>` (`payments-prod`) and
  `sandbox-<name>`. Emails come from the org-wide `email` pattern.
- A stage that isn't defined is a type error in TypeScript and a
  preview error in Python and Go, so nothing is created until someone decides
  its OU.
- Removing a stage or workload follows the downgrade rule: its
  accounts move to Suspended for closing in the console.
- Accounts that aren't workloads or sandboxes (e.g. a partner integration)
  go in `accounts` (see Custom OU and free-form account syntax).
- *Future:* preset tiers for common shapes, e.g. a single SaaS product tier
  that adds the recommended accounts plus `prod` and `dev` workload accounts.

### Grouping workloads

Follows AWS guidance (*Organizing Your AWS Environment Using Multiple AWS
Accounts*): **OUs group accounts by the controls they need, not by ownership
or org structure; everything else is tags.**

1. **Workloads → Prod / NonProd** is the primary split.
2. **Tags** for tidiness (owner, team, cost centre). Workloads accept `tags`,
   and every workload account gets `anvil:workload=<name>` and
   `anvil:stage=<stage>` automatically. The dashboard groups and filters by
   these.

   ```ts
   workloads: {
     payments:  { stages: ['prod', 'dev'], tags: { team: 'billing' } },
     analytics: { stages: ['prod', 'dev'], tags: { team: 'data' } },
   }
   ```

3. **Group OUs under Prod and NonProd** (e.g. `Prod/Data`, `NonProd/Data`)
   are what AWS recommends when a set of accounts needs its own guardrails.

`workloads` always places accounts directly in the stage's OU (Prod or
NonProd). Anything else, including group OUs or an OU per workload, is built
with [custom OUs](04-ou-layout.md#custom-ous) and free-form `accounts`. That is allowed but
not recommended; the docs carry AWS's guidance rather than Anvil enforcing it
(an OU per workload spreads prod across many OUs and repeats prod guardrails
per workload).

### Account emails

Every account needs an email unique across all of AWS. It belongs to the
account's root user, so whoever reads that inbox could recover root if root
credentials came back. `centralRoot` deletes them and `denyMemberRoot`
blocks root, but the inbox is still the account's recovery point, so use a
shared mailbox.

- **Org-wide pattern**, `email: 'aws+{account}@acme.com'`. `{account}` is
  required and replaced by the account name.
- **Overrides:** a free-form account can set its own `email`; a workload can
  set its own pattern (`email: 'payments+{account}@acme.com'`). Covers mail
  servers without plus addressing and inboxes owned elsewhere (e.g. a
  partner).
- **Preview validates every address** before anything is created: AWS's
  64-character limit, valid format, and uniqueness within the config.
- **Docs recommend a shared group mailbox** (e.g. `aws@acme.com`), never one
  person's inbox: if they leave, nobody can recover the account's root user.

### Renames

Accounts, workloads, stages and custom OUs are keyed by name, so renaming a
key would otherwise look like a removal (account suspended) plus a new
account. Renames are explicit with `renamedFrom`, mapped to Pulumi aliases so
state moves and nothing is recreated:

```ts
accounts: {
  'analytics-lake': { ou: 'Workloads/Prod/Data', renamedFrom: 'data-lake' },
},
```

```
~ data-lake → analytics-lake   (222222222222)   name updated, same account
    email unchanged: aws+data-lake@acme.com (set at creation; change it in the console)
    ℹ renamedFrom can be removed after this deploy
```

| Rename | Where `renamedFrom` goes | Effect |
|---|---|---|
| Free-form account | the account | Account name updated (`account:PutAccountName`) |
| Workload | the workload | All its accounts renamed (`payments-prod` → `billing-prod`) |
| Stage | the stage (`uat: { ou: 'NonProd', renamedFrom: 'staging' }`) | That stage's account renamed in every workload |
| Custom OU | the OU path | OU renamed (`UpdateOrganizationalUnit`); accounts stay |

- **Email is fixed at creation.** Changing a root email needs a code sent to
  the new address, so it's a console task; the email pattern only applies to
  new accounts.
- Moving an account to another OU (changing its `ou`) is a move, not a
  rename: `MoveAccount`, and preview shows the guardrail change.
- **Missing `renamedFrom` is caught**: a removal and an addition of the same
  kind in the same OU in one deploy is a preview error:

  ```
  ✗ data-lake removed and analytics-lake added in the same OU.
    If this is a rename, add renamedFrom: 'data-lake' to analytics-lake.
    If you really mean to suspend data-lake and create a new account, pass --allow-replace.
  ```

### Confirming account creation

Accounts are permanent, so creating one is a **hard stop**:

- **Interactive:** preview lists every new account and the user must type
  `yes` (not just `y`):

  ```
  + payments-perf      Workloads/Prod
  + analytics-qa       Workloads/NonProd

  2 new accounts are permanent: their emails can never be reused,
  and closing an account takes 90 days.
  Type "yes" to create them: _
  ```

- **Non-interactive (CI):** the deploy stops unless `--yes-create-accounts`
  is passed. Plain `--yes` approves everything else but **not** account
  creation, so a config change alone never creates accounts in a pipeline.

Approval gates in CI are the user's choice. This is stricter than AFT or
Terraform, which rely on PR review plus a pipeline approval gate and have no
special account flag. The docs recommend a gate and give an example:

```yaml
# .github/workflows/org.yml
on:
  pull_request:
  push:
    branches: [main]

permissions:
  id-token: write      # OIDC: no stored AWS keys
  contents: read

jobs:
  preview:
    runs-on: ubuntu-latest
    outputs:
      new_accounts: ${{ steps.p.outputs.new_accounts }}   # e.g. "reporting-prod, reporting-dev"
    steps:
      - uses: actions/checkout@<sha>                      # v4.2.2
      - uses: aws-actions/configure-aws-credentials@<sha> # v4.1.0
        with:
          role-to-assume: arn:aws:iam::111111111111:role/anvil-org-preview   # read-only
          role-session-name: ${{ github.repository_id }}-${{ github.run_id }}
          aws-region: ap-southeast-2
      - id: p
        run: anvil org preview --github-output                # writes new_accounts

  deploy:                                                 # no new accounts
    if: github.ref == 'refs/heads/main' && needs.preview.outputs.new_accounts == ''
    needs: preview
    runs-on: ubuntu-latest
    environment: org                                      # required reviewers = the approval gate
    steps:
      - uses: actions/checkout@<sha>                      # v4.2.2
      - uses: aws-actions/configure-aws-credentials@<sha> # v4.1.0
        with:
          role-to-assume: arn:aws:iam::111111111111:role/anvil-org-deploy
          role-session-name: ${{ github.repository_id }}-${{ github.run_id }}
          aws-region: ap-southeast-2
      - run: anvil org deploy --yes

  deploy-with-accounts:                                   # creates accounts: its own approval
    if: github.ref == 'refs/heads/main' && needs.preview.outputs.new_accounts != ''
    needs: preview
    runs-on: ubuntu-latest
    environment: org-accounts                             # required reviewers who may approve accounts
    steps:
      - uses: actions/checkout@<sha>                      # v4.2.2
      - uses: aws-actions/configure-aws-credentials@<sha> # v4.1.0
        with:
          role-to-assume: arn:aws:iam::111111111111:role/anvil-org-deploy
          role-session-name: ${{ github.repository_id }}-${{ github.run_id }}
          aws-region: ap-southeast-2
      - run: anvil org deploy --yes --yes-create-accounts
```

How it plays out:

```
guardrail change      preview: no new accounts → deploy (environment org) → done
new workload          preview: reporting-prod, reporting-dev → deploy-with-accounts paused
                      "Approve 2 new accounts" → org-accounts reviewer approves → created
typo creating one     same pause; reviewer sees an unexpected account name and rejects; nothing created
```

- Account creation is always its own approval, in the same run; a push
  never creates accounts on the normal approval alone.
- `anvil org preview --github-output` exposes `new_accounts` as a job output
  (and in the job summary), so the approval page names the accounts.
- Docs explain setting up both environments; `org-accounts` can have the
  same or stricter reviewers.
- How detection works: `anvil org preview` finds account resources with a
  create/replace step in the Pulumi plan (the same list behind the local
  typed `yes`) and appends `new_accounts=…` to `$GITHUB_OUTPUT`. Safety net:
  `anvil org deploy` recomputes the plan and still hard-stops without
  `--yes-create-accounts`, so a moved `main` can't slip an account through.
- *Open, to revisit:* Damien isn't keen on routing by a preview output;
  discuss another way to get a separate approval for account creation
  before this example is final.

Docs guidance for CI:

- Equivalents: GitLab protected environments with deployment approvals,
  CodePipeline manual approval, Buildkite `block` steps, Azure DevOps
  environment approvals.
- Org creates both roles itself (see CI roles below); don't hand-make them.
- Pin every action to a full commit SHA (with the version as a comment), so
  a moved tag can't swap in different code.
- Restrict approvers to a small group and prevent self-review.
- Approvers read the deploy job's own preview, since `main` may have moved
  since the PR. `anvil org deploy` writes a job summary listing new accounts.

### CI roles

**Management account only**, for the Org project's own pipeline. Org
creates the GitHub OIDC provider and two roles:

```ts
new Org({
  ci: {
    github: { repo: 'acme/org-infra', environment: 'org', accountsEnvironment: 'org-accounts' },
  },
});
```

`accountsEnvironment` (default `<environment>-accounts`) is the GitHub
environment for deploys that create accounts (see Confirming account
creation). Both are exact matches, never wildcards, so a new environment in
the repo (with no reviewers) can't assume the deploy role.

```
+ ci   oidc provider  token.actions.githubusercontent.com (aud sts.amazonaws.com)
       anvil-org-preview  read-only  · repo:acme/org-infra:pull_request, ref:refs/heads/main
       anvil-org-deploy   generated  · repo:acme/org-infra:environment:org only (42 actions, 9 services)
```

| Role | Trusted for (`sub`) | Permissions |
|---|---|---|
| `anvil-org-preview` | `repo:<repo>:pull_request`, `repo:<repo>:ref:refs/heads/main` | Generated read actions only; read the state bucket |
| `anvil-org-deploy` | `repo:<repo>:environment:<environment>` and `repo:<repo>:environment:<accountsEnvironment>` only | Generated, strict least privilege (below) |

Both trusts also pin `aud` to `sts.amazonaws.com`. Exact `sub` matches only,
no wildcards.

**Generated least privilege.** The deploy role's policy is computed from
the config, not from everything Org can do:

1. The provider holds a map of each Org resource type to the exact actions
   it needs to create, update, read and delete (versioned with the
   baseline in `provider/aws/org/baseline.json`).
2. Org collects the resource types the config actually uses: Macie off
   means no `macie2:*`, no compliance means no `codebuild:*`.
3. Resources are scoped wherever the service allows: `sts:AssumeRole` only
   on `role/anvil-org-automation`, S3 only on the state bucket,
   Organizations writes only on this org's ARNs, Identity Center only on its
   instance ARN, `iam:CreateServiceLinkedRole` only with
   `iam:AWSServiceName` for the services in use.
4. Checked with IAM Access Analyzer `ValidatePolicy` in preview.

Always on top, as explicit denies:

```json
{ "Sid": "NeverFromCi",
  "Effect": "Deny",
  "Action": ["organizations:CloseAccount", "organizations:LeaveOrganization",
             "organizations:DeleteOrganization", "organizations:RemoveAccountFromOrganization",
             "organizations:DisablePolicyType"],
  "Resource": "*" },
{ "Sid": "CantChangeItself",
  "Effect": "Deny",
  "Action": "iam:*",
  "Resource": ["arn:aws:iam::<mgmt>:role/anvil-org-deploy",
               "arn:aws:iam::<mgmt>:role/anvil-org-preview",
               "arn:aws:iam::<mgmt>:oidc-provider/token.actions.githubusercontent.com",
               "arn:aws:iam::<mgmt>:role/anvil-break-glass-forwarder"] }
```

**The deploy role can't change itself**, so a compromised workflow can't
widen it. The cost: a config change that needs new actions (turning on
Macie, say) can't deploy from CI until the role is updated by a local
deploy with a person's own credentials. Preview catches it before anything
runs:

```
✗ anvil-org-deploy is missing permissions for this change
    + macie2:EnableMacie, macie2:EnableOrganizationAdminAccount, … (6)
  CI can't widen its own role. Run anvil org deploy locally with your own
  credentials to update it, then re-run the pipeline.
```

Removing features shrinks the role on the next local deploy; CI leaves it
wider until then (preview notes it).

- The first deploy is always local (the roles don't exist yet).
- GitHub only at first; others (GitLab, Buildkite) later on the same
  pattern, or bring your own role.
- *Benched:* per-workload CI roles for Apps (an OIDC role in each workload
  account, trusted for that workload's repo and environments).

**Opt-in RCP for GitHub OIDC.** No role anywhere in the org can be assumed
from GitHub repos outside the named GitHub owners (AWS sample RCP), however
the role was created. Packed into the baseline RCP, no extra slot:

```ts
guardrails: { githubOidc: { owners: ['acme'] } }
```

```json
{ "Sid": "githubOidc",
  "Effect": "Deny",
  "Principal": "*",
  "Action": "sts:AssumeRoleWithWebIdentity",
  "Resource": "*",
  "Condition": {
    "StringNotLikeIfExists": {
      "token.actions.githubusercontent.com:sub": ["repo:acme/*"] },
    "Null": { "token.actions.githubusercontent.com:sub": "false" } } }
```

### Account quota

New organizations start with a quota of 10 accounts (AWS: new orgs can start
lower). The management account, pending invitations, and closed accounts in
their 90-day post-closure period all count. Preflight reads the quota
(Service Quotas, "Maximum number of accounts") and counts before creating
anything:

```
Accounts   in org 8 (incl. management, 1 suspended, 0 invitations) + 4 new = 12
           quota 10
✗ This deploy needs 12 accounts but the org's quota is 10.
  Request an increase to at least 12 in the management account, then deploy:
  Service Quotas → AWS Organizations → Maximum number of accounts (us-east-1)
```

```
⚠ Accounts 9 of 10 after this deploy. Request a quota increase before adding more.
```

- **Tell only.** Anvil never requests the increase; it's a manual request in
  the console. Small increases are often approved quickly; larger ones can
  take days through AWS Support.
- Concurrent creation (5 at a time) is handled by the readiness queue.

### New-account readiness

`CreateAccount` reports success before an account is usable: the access role
takes time to become assumable, and services activate per region. Org never
touches a new account before it's ready.

1. **Create**: at most 5 in flight (AWS limit); the rest queue.
2. **Move to its OU immediately**, so its guardrails apply before anything
   else (new accounts start at Root, covered by the Root baseline only).
3. **Readiness check**: assume `anvil-org-automation` with retry and
   backoff, then one cheap call per allowed region; retries on
   `AccessDenied`, `OptInRequired`, `SubscriptionRequiredException`,
   `AuthFailure`.
4. **Configure** once ready: narrow `anvil-org-automation` trust, create
   `OrganizationAccountAccessRole` (break glass), S3 account Block Public
   Access, EBS encryption by default, scan role, SSM `/anvil/org/account`,
   Identity Center assignments, tags.

Implemented as an `AccountReady` resource in the Anvil provider; every
per-account resource depends on it.

### Automation role

Org never uses the break-glass role for automation. Accounts are created
with `CreateAccount(RoleName: "anvil-org-automation")`, so AWS creates
Anvil's own role in place of `OrganizationAccountAccessRole`:

```
CreateAccount(RoleName) ─▶ anvil-org-automation (admin, trusts management)
Org deploy role ─▶ anvil-org-automation
   1. narrow its trust to Org's deployers (exact ARNs, below)
   2. create OrganizationAccountAccessRole (break glass, emergency account's break-glass-* users only)
   3. rest of setup
```

**Who it trusts: Org's deployers, by exact ARN.** CI is strongly
recommended but not required; local deploys by a person are allowed.

```json
{ "Effect": "Allow",
  "Principal": { "AWS": [
    "arn:aws:iam::<mgmt>:role/anvil-org-deploy",
    "arn:aws:iam::<mgmt>:role/aws-reserved/sso.amazonaws.com/ap-southeast-2/AWSReservedSSO_orgAdmin_a1b2c3d4" ] },
  "Action": "sts:AssumeRole" }
```

- **CI** through `anvil-org-deploy` (when `ci` is set); **local deploys**
  through `orgAdmin` (the named users in
  `identityCenter.management.orgAdmin.users`). No CI-only restriction.
- **Exact ARNs, no patterns.** IAM stores trusted roles as unique IDs, so a
  deleted and re-created role doesn't inherit the trust, and a look-alike
  permission set can't match. Org assigns `orgAdmin` first, reads its role
  ARN, then writes the trust, in the same deploy.
- **Before either exists** (first deploy with whatever admin credentials):
  Org pins the caller's exact ARN as a temporary bootstrap principal, and
  warns until a permanent deployer replaces it:

  ```
  ⚠ anvil-org-automation in 3 accounts trusts your current credentials
    (arn:aws:iam::111111111111:role/AdminRole) until a permanent deployer exists.
    To fix: add identityCenter.management.orgAdmin.users (or ci.github) and deploy again;
            the bootstrap trust is then removed.
  ```

- **Lock-out check:** a deploy that would leave no deployer able to reach
  member accounts (other than break glass) is a preflight error with the
  fix steps, like the state bucket.

- No CloudFormation/StackSets; everything stays in Pulumi.
- Every break-glass alert is a real use; deploys never fire one.
- Nothing in management can reach or create break glass: it trusts only
  users in the emergency account, whose lockdown allows no other
  principals. A compromised pipeline or `orgAdmin` session is limited to
  what `anvil-org-automation` (SCP-protected) allows.
- For seconds after creation the role trusts the whole management account,
  until step 1; the same exposure as AWS's default role.
- If the role is deleted or changed, the SCP blocks it; on drift Org errors
  in preview (it can't get back in without break glass).

**Accounts Org didn't create** (future work, with account adoption) lack
the role. Org bootstraps it once through `OrganizationAccountAccessRole`
with session name `anvil-bootstrap-<accountId>`. The alert is **labelled,
not suppressed** (session names are caller-chosen, and filtering on the
deploy role would silence a compromised pipeline):

```
~ legacy-billing    bootstrapping automation role via OrganizationAccountAccessRole
                    ⚠ expect 1 break-glass alert (session anvil-bootstrap-111122223333)
```

Considered: a service-managed StackSet creating the role in every OU
(no break-glass use even for adopted accounts, but a second deploy system,
slower readiness, role vanishes when an account leaves the OUs). Revisit if
account adoption becomes common.

```
+ payments-perf      created (444455556666) · moved to Workloads/Prod
                     waiting for account to be ready… role ✓ · ap-southeast-2 ✓ · us-east-1 ✓  (2m14s)
                     configured: break-glass role, ssm parameter, scan role, 3 assignments
```

**Not ready in time (15 min): the account is left pending and the deploy
finishes.** The account is already created and guarded by its OU; its setup
completes on the next deploy (idempotent).

```
⚠ payments-perf (444455556666) created and moved to Workloads/Prod, but not ready after 15m
    (us-east-1: OptInRequired). Its setup will finish on the next deploy.
    Pending: break-glass role, ssm parameter, scan role, 3 assignments
✓ deploy finished with 1 account pending
```

- `--strict` turns pending accounts into a failed deploy, for CI that wants it.
- Docs: set the CI deploy role's session to 2h; preview warns when one deploy
  creates more than ~10 accounts (5 at a time can outlast a 1h session).

### Deployers

**Anyone with the right permissions in management can deploy locally, as
long as their role is named as a deployer in code.** Having some admin role
in management isn't enough on its own; becoming able to deploy the org is
always a reviewed change.

`orgAdmin` and the CI deploy role are always deployers. `deployers` adds
more:

```ts
deployers: {
  roles: ['PlatformAdmin'],            // IAM roles that already exist in management
  permissionSets: ['platformAdmin'],   // Identity Center sets defined in this config
},
```

- **`roles`**: an existing IAM role in management (made by hand or other
  tooling). Org never creates or changes it, or decides who can use it; it
  only names it.
- **No chaining into management from other accounts.** Pipelines and
  people never reach management through a role in another account (e.g. a
  Deployments-account pipeline assuming a management role). That would
  make another account a path to org admin. Preflight reads each
  `deployers.roles` trust policy and errors if it trusts any principal
  outside management:

  ```
  ✗ deployers.roles: OrgDeployer trusts arn:aws:iam::333333333333:role/pipeline
    (another account). Org deployers must be reached from management only.
    To fix: remove that principal from OrgDeployer's trust, or use ci.github
    (OIDC straight into management).
  ```

  App pipelines stay in their own accounts and create their roles there;
  they never touch management.
- **`permissionSets`**: a custom set from `identityCenter.permissionSets`,
  assigned to named users in `identityCenter.management` (deployer sets can
  be assigned there, alongside `orgAdmin` and `billing`). Identity Center
  creates the role (`AWSReservedSSO_platformAdmin_…`) when it's assigned.
- Org pins each deployer's **exact ARN** in `anvil-org-automation`'s trust
  and the state bucket policy. A deleted and re-created role doesn't keep
  access. Names only, no PII.

```
~ deployers   + role/PlatformAdmin (AROA…X7Q, created 2025-03-02)
              + permission set platformAdmin (AWSReservedSSO_platformAdmin_9f8e…)
```

**Preflight checks both things, and says which is missing:**

1. **Is the caller a deployer?** If not:

   ```
   ✗ You're deploying as arn:aws:iam::111111111111:role/PowerUser, which isn't
     a deployer, so it can't reach member accounts or Org state.
     To fix, do one of:
       1. Sign in through Identity Center as an orgAdmin user (alice@acme.com, bob@acme.com).
       2. Add PowerUser to deployers.roles, deploy that change as orgAdmin or from CI,
          then run this deploy again.
   ```

2. **Can the caller's own permissions do this deploy in management?**
   Being a deployer only gets a role *into* member accounts and state;
   everything in management runs with the role's own permissions. Preflight
   simulates only the actions this deploy needs
   (`iam:SimulatePrincipalPolicy`):

   ```
   ✗ role/PowerUser is a deployer but can't run this deploy in management.
     Missing:
       organizations:CreatePolicy, organizations:AttachPolicy, organizations:MoveAccount, … (14)
       iam:UpdateAssumeRolePolicy (anvil-org-deploy), … (3)
     To fix, do one of:
       1. Deploy as orgAdmin.
       2. Give role/PowerUser the missing permissions in management.
   ```

   A deployer with partial permissions can still deploy changes that don't
   need the missing ones.

### Alternate contacts

Security, operations and billing contacts are **not managed in code**. AWS's
`PutAlternateContact` requires name, title, email **and phone**, and phone
numbers don't belong in a repo or in state.

- Preflight **warns** about any account missing a security contact (AWS sends
  abuse and compromise notices there; without one they go only to the root
  email).
- Docs explain how to set them in the console and recommend team details
  (group email, on-call number) over personal ones.
- *Future:* the dashboard shows missing contacts per account and could set
  them directly through the API, so they never pass through code or state.

### Moving down a tier

Accounts are permanent, so a downgrade never closes anything. Instead the
accounts leaving the tier are:

1. cleaned of Anvil's per-account access first, while Anvil can still
   reach them: the scan role and the SSM parameter are deleted, and
   `anvil-org-automation` is **disabled** (trust denies everyone, inline
   policies deleted, managed policies detached last; it can't delete itself,
   see Destroy). Break glass, `OrganizationAccountAccessRole`, stays;
2. moved to **Suspended** (deny-all, so workloads in them stop; only
   break glass can get in, and any use there alerts `critical`:
   *"break-glass-1 used OrganizationAccountAccessRole in suspended account
   analytics-old"*. A suspended account should have no use at all);
3. tagged `anvil:lifecycle=suspended`;
4. released from Anvil's state.

The preview lists the affected accounts and asks for confirmation. The user
then closes them in the console.

**Closing a Suspended account** (docs must walk through it):

1. **Want to reuse the email?** Change the account's root email first: a
   closed account's email can never be used for another AWS account. Same
   for an account alias (delete it first).
2. **Don't want it counting against the org's account quota?** Closed
   accounts stay in the org and count for the 90-day post-closure period.
   Removing it from the org first avoids that, but accounts created by Org
   can't be removed straight away: AWS needs the standalone sign-up details
   (payment method, verified contact, support plan) entered in the account
   itself, the account must be at least 4 days old, and it can't be a
   delegated admin. The path: move it out of Suspended, recover root,
   complete sign-up, then remove it, and delete
   `OrganizationAccountAccessRole` afterwards (AWS leaves it, still trusting
   the old management account). It then becomes a standalone account the
   user closes as root.
3. **Close it** from management with `orgAdmin` (only accounts tagged
   `anvil:lifecycle=suspended`).
4. **Closure quota:** AWS limits how many member accounts can be closed per
   rolling 30 days. AWS's own pages disagree (10% vs 20% of member accounts,
   with a 250 minimum on the Organizations quota page); docs link the live
   quota page instead of quoting a number. Exceeding it gives
   `CLOSE_ACCOUNT_QUOTA_EXCEEDED`.
5. Within 90 days, AWS Support can reopen a closed account.

### Who can close accounts

Only the management account can close member accounts, and SCPs don't apply
there, so the restriction lives in **Identity Center permission sets**:

- only the `orgAdmin` preset (assigned to named users in `management`)
  gets `organizations:CloseAccount` and `RemoveAccountFromOrganization`;
- even then, only for accounts tagged `anvil:lifecycle=suspended`
  (`aws:ResourceTag` condition);
- every permission set except `orgAdmin` (which deploys and writes the tag)
  is denied changing `anvil:*` tags, so no other set can forge the tag to
  close an arbitrary account. `orgAdmin` itself could tag and then close;
  accepted for now (rare), so the tag rule guards against mistakes rather
  than a compromised `orgAdmin` (audit 2 → A3).

Members can't close themselves either: centralized root access (guardrails
topic) removes member root credentials.

"Management account is read only" means its **account settings** (tags,
contacts, placement). Anvil still creates resources in management (see
Management account stays empty for the list).

### Management account stays empty

AWS: use the management account only for tasks that require it. SCPs don't
apply there, so nothing deployed there is guarded.

**Apps are hard-refused in management**, with no override. Any account can
call `organizations:DescribeOrganization`, so an App knows when its
credentials are the management account's, with or without an Org project:

```
✗ This is the organization's management account (123456789012). Apps can't
  deploy here: SCPs don't apply to it, so nothing would be guarded.
  Deploy to a member account instead.
```

What Anvil itself puts in management, all required there:

- the Org state bucket;
- Identity Center permission sets and assignments;
- the org trail (or the minimal org trail when `cloudtrail` is disabled);
- Organizations policies;
- the CI roles and GitHub OIDC provider (when `ci` is set);
- alert rules (`anvil-break-glass-*`, every enabled region: management
  root activity, `AssumeRole` on `OrganizationAccountAccessRole`, people
  assuming `anvil-org-automation`) and their forwarder role. The
  break-glass users themselves live in the emergency account, not here;
- watched-changes rules for management (guardrails, CloudTrail, Identity
  Center, the state bucket);
- the `anvil-management-alerts` topic and the `FailedInvocations` alarms
  (second alert path, see below);
- cost allocation tag activation.

No workloads, and nothing an App deploys.

#### Protecting management resources

**SCPs never apply in management**, so nothing above is protected by an
SCP, including the org trail (AWS always creates org trails in management).
Two layers instead:

1. **Explicit denies on every Org identity** in management: the CI deploy
   role, `orgAdmin`, `billing` and every deployer permission set Org
   creates. Only actions that are never part of a deploy are denied, on
   resources that are never removed (not even by `anvil org destroy`):

   ```json
   { "Sid": "ProtectManagementSecurity",
     "Effect": "Deny",
     "Action": ["cloudtrail:StopLogging", "cloudtrail:DeleteTrail",
                "events:DeleteRule", "events:DisableRule", "events:RemoveTargets",
                "events:PutTargets"],
     "Resource": ["arn:aws:cloudtrail:*:<mgmt>:trail/anvil-org-trail",
                  "arn:aws:cloudtrail:*:<mgmt>:trail/anvil-minimal-trail",
                  "arn:aws:events:*:<mgmt>:rule/anvil-break-glass-*",
                  "arn:aws:events:*:<mgmt>:rule/anvil-watch-*"] },
   { "Sid": "ProtectForwarder",
     "Effect": "Deny",
     "Action": ["iam:DeleteRole", "iam:UpdateAssumeRolePolicy", "iam:PutRolePolicy",
                "iam:DeleteRolePolicy", "iam:AttachRolePolicy", "iam:DetachRolePolicy",
                "iam:PutRolePermissionsBoundary"],
     "Resource": "arn:aws:iam::<mgmt>:role/anvil-break-glass-forwarder" }
   ```

   Every alert from management reaches Audit through
   `anvil-break-glass-forwarder` (cross-account bus delivery needs a role),
   so it's protected like the rules. `PutRule` stays allowed for deploys, but
   a rule pattern changed outside an Anvil deploy is `critical`. Creating
   targets is part of a deploy, so `PutTargets` is lifted only for the
   deployer's own deploy, like watch opt-outs.

   Updates (`PutRule`, `UpdateTrail`, event selectors) stay allowed, so
   deploys and new regions work locally and from CI.

2. **A second alert path that doesn't use the forwarder:** a small SNS
   topic in management (`anvil-management-alerts`) receives alerts about the
   forwarder role, rule targets and patterns directly, with no cross-account
   hop, plus a CloudWatch alarm on the rules' `FailedInvocations`, so a
   broken delivery chain is noticed even when nothing was "changed".
   Listeners subscribe to it like the Audit topic.

3. **A paired watcher** for principals Org can't put a deny on (hand-made
   admins, root): in every enabled region, `anvil-watch-<region>` alerts on
   changes to that region's break-glass rule and to the trail, and the
   break-glass rule alerts on changes to the watcher. Deleting either fires
   `critical` from the other; silencing a region takes two deletions, and
   the first is already reported.

```
stolen orgAdmin → cloudtrail stop-logging anvil-org-trail          ✗ denied (ProtectManagementSecurity)
LegacyAdmin     → events delete-rule anvil-break-glass-eu-north-1  ✓ succeeds
                  → critical from anvil-watch-eu-north-1: "break-glass rule deleted by role/LegacyAdmin"
deploy          → update-trail (add state-bucket data events)      ✓ allowed
```

- Turning off a watched area (`watch.<area>.disable`) deletes its rules, so
  it's a **local** deploy: the deployer removes the deny for those rules,
  like CI role changes. Preview says so.

Adopted orgs that already have workloads in management get a preflight
warning; Anvil never moves or deletes them:

```
⚠ management (123456789012) has workloads: 3 Lambda functions, 1 RDS instance,
  2 S3 buckets outside Anvil. AWS recommends moving workloads to member accounts.
```

**Management-only jobs, and where they go:**

| Job | Where |
|---|---|
| Billing exports, budgets, cost anomaly detection, schedules | Console for now; Org `billing` is parked (see [Later: billing](later-billing.md)) |
| Savings Plans / Reserved Instance purchases | Console (`denyCommitmentPurchases` blocks them in members) |
| Support plan | Console |
| Organizations, Identity Center, org trail | Org (already) |
