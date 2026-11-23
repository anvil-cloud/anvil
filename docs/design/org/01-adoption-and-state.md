# Org — Adoption and state

Part of the [Org design](README.md).

## Adopting the org

**Adopt only.** Anvil never creates the organization. The user creates it in
the console with **all features** enabled. Creating an org and closing
accounts are hard to undo, so they stay manual.

### Flow

1. User creates the organization in the console (all features).
2. `anvil org init` scaffolds a project with `new Org({...})` and an
   `anvil.org.yaml`.
3. `anvil org deploy` with management-account credentials. Preflight fails fast if:
   - the caller isn't the management account
     ("Org must deploy from the management account (123…)");
   - the org's feature set isn't `ALL` (link to the console upgrade page);
   - the org is managed by Control Tower (the two would fight over OUs, SCPs
     and the trail):
     ```
     ✗ This org is managed by Control Tower. Org doesn't support Control Tower yet;
       Anvil Apps work in its accounts as normal.
     ```
     Control Tower isn't required by AWS or by compliance frameworks (they
     require controls, not a product), so standalone ships first. A
     `controlTower: true` mode for adopting existing Control Tower orgs is
     deferred (see Open topics).
4. Org-level settings are enabled: policy types (SCP, RCP, declarative,
   tag, backup when used, AI opt-out) and trusted access for the security
   services that are on.

### The organization is never in state

Pulumi's `aws.organizations.Organization` represents the whole org; deleting
it deletes the organization. Anvil does not use it. Instead a custom
`OrgSettings` resource in the Anvil provider enables policy types and trusted
access, and **its delete is a no-op**. The org is only ever looked up. Anvil
should be structurally unable to delete the org, not just configured not to.

## CLI

**Org projects use their own commands and no stages.**

| Command | Does |
|---|---|
| `anvil org init` | Ask for a starting layout, explaining each OU, then scaffold an Org project (TypeScript): `anvil.config.ts` with every OU written out, `anvil.org.yaml`, `package.json`, `tsconfig.json`, `.gitignore`. `--tier minimum\|network\|sra`, `--sandbox=false` and `--policy-staging` skip the questions; `--region` sets the home region, `--sdk-path` links a local SDK checkout. Creates nothing in AWS |
| `anvil org preview` | Preflight + plan, changes nothing (creates the state bucket on first use) |
| `anvil org deploy` | Preflight + deploy |
| `anvil org destroy` | Remove Anvil from the org, keeping the security minimum (see Destroy) |
| `anvil org access`, `anvil org dashboard`, `anvil org upgrade` | Org-only extras |

- **Project type comes from the program.** The discovery run (the same
  `ANVIL_BUILD_MODE` run that finds Lambdas for Apps, no AWS calls) reports
  whether the entry point exports `new Org` or `new App`. Plain `anvil
  deploy` / `preview` / `destroy` refuse an Org project, `anvil org …`
  refuses an App:

  ```
  ✗ This project defines an Org. Use anvil org deploy.
  ✗ This project defines an App. Use anvil deploy --stage <stage>.
  ```

- **`--stage` is an error** in an Org project: one Org per organization, no
  stages.
- **`anvil.org.yaml`** sits next to the code and holds what the CLI needs
  before running the program. Required for an Org project (discovery says
  Org and it's missing → error pointing to `anvil org init`); an
  `anvil.org.yaml` next to an App is an error too.

  ```yaml
  # anvil.org.yaml (written by anvil org init and the first deploy; commit it)
  project: acme-org
  region: ap-southeast-2               # home region
  state:
    bucket: anvil-org-state-o-ab12cd34  # recorded on first deploy
  baseline: 1.0.0                      # accepted Org baseline (anvil org upgrade)
  ```

- **Home region is whatever the user sets**, like an App stage's region:
  `--region` on `anvil org init` or the first deploy, else the AWS
  config's region, else `ap-southeast-2`; stored in `anvil.org.yaml`. It's
  where the state bucket, Audit event bus, notification topic and other
  regional home resources live. AWS (Control Tower, LZA) recommends your
  main operating region, not us-east-1 by default. Docs: choose carefully;
  moving it later means moving the state bucket and home resources.
- **The state bucket is created on first use**, by the first `anvil org
  preview` or `deploy`, after preflight confirms the caller is the
  management account, and recorded in `anvil.org.yaml`.

## State

There are no stages: one `Org` per organization, with **one state bucket in
the management account**. App state buckets are unchanged (one per stage, in
the app's own account); org state is never mixed with app state.

The management account was chosen over Log Archive or a dedicated tooling
account because Org creates those accounts, so the first deploy would have
nowhere to write state.

### Hardening

The Org bucket gets everything an App bucket gets (versioning, lifecycle
cleanup, public access block, TLS-only policy), plus:

| # | Hardening | Decision |
|---|---|---|
| 1 | Bucket policy denies `s3:DeleteObjectVersion` and `s3:DeleteBucket` | Default |
| 2 | `retainVersions` default 10 (App: 3) | Default |
| 3 | Customer-managed KMS key, with S3 Bucket Keys | Opt-in |
| 4 | CloudTrail data events on the state bucket | Default for Org |
| 5 | Replicate state to Log Archive | Opt-in, not in v1 |
| 6 | Object Lock | Not now |
| 7 | Restrict to named principals | Default (once `ci` and `orgAdmin` exist) |

Notes:

- **(1) doesn't block cleanup.** Lifecycle rules aren't subject to bucket
  policies, and `pruneHistoryCheckpoints` deletes by key (delete markers), not
  by version. The gap: an admin could set a lifecycle rule that expires
  everything. That's prevented by (7) once the bucket is locked, and detected
  by watched changes (`buckets`) before that.
- **(3) is reversible.** Changing default encryption only affects new
  objects; the state file is rewritten every deploy, older versions age out,
  and S3 reads mixed encryption transparently. Configured as
  `defaults.state.encryption: 'sse-s3' | 'kms'`, reconciled on deploy like
  `retainVersions`. Order matters: create the key and its policy before
  switching the bucket. The key policy delegates to IAM in the management
  account so CI roles work with ordinary IAM grants. Switching back requires
  keeping the key until KMS-encrypted versions have aged out; Anvil checks
  before allowing key deletion.
- **(7) only Org's principals touch state.** A bucket policy limits reads
  and writes, so nobody else in management can edit state and have the next
  CI deploy act on it:

  | Who | Why | Access |
  |---|---|---|
  | `anvil-org-deploy` | CI deploys | read + write |
  | `anvil-org-preview` | CI previews | read |
  | `orgAdmin` users (Identity Center) | Local deploys | read + write |
  | Roles in `deployers` | Local deploys | read + write |

  Break glass has no state access: it lives in the emergency account and
  only reaches members. Recovering management (including this bucket) is
  the management root user's job.

  ```json
  { "Sid": "OnlyOrgPrincipalsWrite",
    "Effect": "Deny",
    "Principal": "*",
    "Action": ["s3:PutObject", "s3:DeleteObject",
               "s3:PutBucketPolicy", "s3:DeleteBucketPolicy", "s3:PutLifecycleConfiguration",
               "s3:PutBucketVersioning", "s3:PutEncryptionConfiguration",
               "s3:PutReplicationConfiguration", "s3:PutBucketOwnershipControls"],
    "Resource": ["arn:aws:s3:::anvil-org-state-<id>", "arn:aws:s3:::anvil-org-state-<id>/*"],
    "Condition": { "ArnNotLike": { "aws:PrincipalArn": [
      "arn:aws:iam::<mgmt>:role/anvil-org-deploy",
      "arn:aws:iam::<mgmt>:role/aws-reserved/sso.amazonaws.com/ap-southeast-2/AWSReservedSSO_orgAdmin_a1b2c3d4",
      "arn:aws:iam::<mgmt>:role/PlatformAdmin" ] } } }
  ```

  Deployers (the CI role, `orgAdmin`, everything in `deployers`) are listed
  by **exact ARN**, pinned at deploy as for `anvil-org-automation`'s trust. A second
  statement does the same for `s3:GetObject` and `s3:GetObjectVersion`
  (old state versions too), plus `anvil-org-preview`. Deleting the policy is
  denied like changing it, so the lock can't be removed in one call; the
  attempt alerts through watched changes (`buckets`). Covering `PutLifecycleConfiguration` also closes the
  lifecycle gap in (1).

  - **Switches on once the principals exist** (`ci` set and
    `identityCenter.management.orgAdmin` has users), so the first deploy
    works with whatever admin credentials the user has:

    ```
    ⚠ state bucket is open to every admin in management.
      It locks to Org's principals once both are set:
        ci.github                                (CI deploy and preview roles)
        identityCenter.management.orgAdmin.users (people who can deploy locally)
    ```

  - **Lock-out is an error, and break glass doesn't count as a way back
    in.** Break glass is for emergencies only, so preflight checks the
    caller and the remaining non-break-glass principals, never suggesting
    break glass:

    ```
    ✗ This deploy would lock you out of the Org state bucket.
      You're deploying as  arn:aws:iam::111111111111:role/AdminRole
      After this deploy only these can access state:
        anvil-org-deploy (CI), orgAdmin users: alice@acme.com, bob@acme.com
      To fix, do one of:
        1. Sign in through Identity Center as alice@acme.com or bob@acme.com
           (orgAdmin in management) and run anvil org deploy again.
        2. Add yourself to identityCenter.management.orgAdmin.users, deploy
           that change first from CI, then run this deploy as orgAdmin.
    ```

    Removing `ci.github` or the last `orgAdmin` user re-opens the bucket
    (it's locked only while both exist), so preview warns rather than
    errors:

    ```
    ⚠ This deploy removes ci.github, so the Org state bucket opens to every
      admin in management again.
      To keep it locked, keep ci.github (and identityCenter.management.orgAdmin.users).
    ```

  - Docs only, not a preflight path: the management root user can always
    delete a bucket policy that locks everyone out (AWS guarantee).
  - Ordinary management admins can't read state; they use `anvil org
    access`, the dashboard, or sign in as `orgAdmin`.
- **(4) records every caller** (Anvil CLI, CI, console, raw Pulumi):
  principal and session name, source IP, user agent, action, key and time.
  It doesn't record what changed; versions cover that. CI docs should tell
  pipelines to set the role session name to repo and run ID. Cost: a few
  hundred events per deploy, fractions of a cent.

### Bootstrap

`anvil bootstrap` for an Org skips the per-stage CloudTrail trail when the org
trail replaces it (on by default; see Security services → CloudTrail).

## What Anvil manages

| Scope | Treatment |
|---|---|
| Resources Org creates (OUs, accounts, policies, attachments, delegated admins) | Managed |
| Policy attachments on **Root** | Managed |
| Root itself (tags, settings) | Read only |
| Management account | Read only, exposed as `org.management` |
| Management account root user | Never touched |
| Emergency account (`emergency`) | Created by Org with its lockdown; its root and `break-glass-*` IAM users are made by hand and never touched by Org; at least one required (preflight) |
| `anvil-org-automation` in members | Created by `CreateAccount`; trust narrowed to the Org deploy role; protected by SCP |
| `OrganizationAccountAccessRole` in members | Created by Org (break glass), trusts only the emergency account's `break-glass-*` users with MFA; protected by SCP; never used by automation |

- **Pulumi state is the source of truth** for what Anvil manages.
- Everything Anvil creates is tagged `anvil:managed=true` and
  `anvil:org=<project>`. Tags are a hint for humans and the dashboard only.
  Prune decides from state, never tags.
- Org-wide guardrails attach at Root (rather than each top-level OU) so
  accounts that end up at Root by accident, such as a newly invited one, are
  still covered.
- Anvil never moves the management account. SCPs and RCPs don't apply to it,
  so placing it in an OU would wrongly suggest that OU's guardrails cover it.

### Preflight warnings

Read-only checks, never changes:

- management root user has no MFA;
- management root user has access keys;
- management account has no alternate contacts (billing, operations, security);
- any member account has no security contact.

## Existing resources and collisions

**v1 manages only what is declared and created by Anvil.** Nothing existing is
adopted. Matching is used only to **detect collisions in preview**, before
anything changes, by listing what the org already has.

| Resource | Unique key |
|---|---|
| OU | name + parent |
| Account | email (globally unique in AWS) |
| SCP / RCP / AI opt-out / tag policy | name + policy type |
| Policy attachment | policy + target |
| Delegated admin | service |

```
✗ OU "Workloads" already exists under Root (ou-ab12-xyz) and isn't managed by Anvil.
  Rename it in your config, or remove the existing OU.
```

With the create-in-console flow, most orgs have only Root and the management
account on first deploy, so collisions should be rare. Adopting existing
resources (import by these keys) is future work.

## Prune

Removing something from code deletes it the normal Pulumi way. **Prune** is
the separate, fully opt-in step of removing things that exist in the org but
were never declared.

- Off by default; enabled with `prune: true`.
- Every deploy that would prune lists exactly what will be removed and asks
  for confirmation. In CI it also needs `--yes-prune`, so a config change
  alone never deletes in a pipeline.
- **Never** touches accounts (no closing, no moving).
- Removes only **empty** unmanaged OUs; non-empty ones are reported.
- **Never** detaches the AWS-managed `FullAWSAccess` SCP.
- **Never** touches Root or the management account.

What it removes, by value:

1. Unmanaged policy attachments on Anvil-managed OUs or accounts.
2. Unmanaged SCPs and RCPs (detach, then delete).
3. Empty unmanaged OUs.

*Open:* one `prune: true` switch versus per-type scoping.
