# Org — Reviews and audits (history)

Part of the [Org design](README.md).

## Review against best practices (2026-10-28)

*To come back to.* Checked against AWS Organizations best practices, SRA,
the multi-account whitepaper, and the declarative policy and Identity Center
docs.

### Gaps to discuss

1. ~~**No policy testing path before Root.**~~ *Resolved: see Testing policies.* AWS strongly recommends testing
   SCPs before attaching to Root (move accounts into a test OU a few at a
   time). Org attaches the baseline at Root on first deploy; custom policies
   can target `ctx.ou.root`. Missing: a Policy Staging OU or staged rollout.
2. *Benched (no adopted orgs yet; revisit when one appears).* Draft: preflight detection per risky rule (Access Analyzer external access for `denyOutsideOrg`, IMDSv1 usage for `imdsV2`, IAM users with keys for `denyIamUsers`, plain-HTTP calls for `requireTls`) with a hard stop, fixed or excepted in `run` with a reason. **Baseline rules that can break adopted orgs.** `denyOutsideOrg` (RCP at
   Root) needs AWS service principal exemptions (`aws:PrincipalIsAWSService`,
   e.g. CloudFront OAC) and third-party access; AWS data-perimeter guidance
   rolls it out gradually. `imdsV2` required: AWS says audit IMDSv1 usage
   first. Needs a preflight check or monitor-first mode for adopted orgs.
3. *Resolved: see Log Archive and the org trail.* **Log Archive bucket and org trail not designed**: retention, Object Lock,
   KMS, log file validation, access logging, lifecycle (SRA covers these).
4. *Resolved: see Secure account defaults.* **Secure defaults protected but not turned on.** `denyPublicS3` and
   `protectEncryptionDefaults` stop disabling S3 Block Public Access and EBS
   default encryption, but nothing enables them per account. Unused cheap
   declarative policies: snapshot and AMI block public access, serial console
   off, VPC Block Public Access (carefully).
5. *Resolved: see Budgets.* **Sandbox "spend-capped" with no cap**: only `denyExpensiveResources`;
   budgets wait on `billing`.
6. *Resolved: see Delegated administration and Management access.* **Identity Center admin location**: AWS recommends delegating it to a
   member account; the spec manages it from management (undecided). No check
   of MFA settings for the built-in directory.
7. *Resolved: see GuardDuty protection plans.* **GuardDuty protection plans** (S3, EKS, runtime, malware, RDS, Lambda)
   unspecified.
8. *Benched until requested.* Draft: opt-in `backup` with org backup policies (tag-selected via `anvil:backup=<plan>`, settable from components), plans with schedule and retention attached per OU, copies to a central vault in the Backup account with opt-in Vault Lock; central copy needs `tier: 'sra'`. **`sra` tier Backup account** with no org backup policy.

### Intentional deviations (documented, no action)

Security services opt-in except CloudTrail (on by default); GuardDuty limited to allowed
regions; Control Tower not supported yet; Apps hard-refused in management.

### Stale or contradictory text

*Fixed 2026-10-30:* break-glass role in "What Anvil manages"; tiers comment
vs moving down; typed `yes` in the API example; management account shown at
Root, not as an OU; package name `@anvil-cloud/sdk`; full list of policy
types; Security OU baseline wording; leftover open/later notes; preset
preview; `CspmStandard` and `PolicyValue` defined; warning when a
replacement guardrail covers less than the rule it replaces.

## Research: common pain points vs Org (2026-10-28)

*To come back to.* What people struggle with when setting up Organizations,
LZA, Control Tower and AFT, from LZA GitHub issues, AWS docs and
practitioner blogs (anecdotal; versions vary).

| Pain point | Seen in | Org today |
|---|---|---|
| Slow pipelines; small changes trigger long full runs (hours) | LZA | ✅ Pulumi diff from the CLI, no pipeline in management |
| Opaque failures (generic CodeBuild/CloudFormation errors, hidden dependencies) | LZA | ✅ preview validates policies, limits, emails, names before deploy |
| Config learning curve (large YAML) | LZA, AFT | ✅ typed config in TS/Python/Go; tiers start small |
| First-run quota failures (CodeBuild/Lambda concurrency) | LZA | ✅ resolved: account quota preflight, tell only (see Account quota); 5 concurrent handled by the readiness queue |
| Upgrades break (version-dependent API changes) | LZA | ✅ resolved: semver baseline, accepted version in `anvil.yaml`, `anvil org upgrade --preview` (see Baseline versions and upgrades) |
| Renames/removals break (removed OU, renamed VPC) | LZA | ✅ resolved (`renamedFrom`, see Renames); was **gap**: renaming an account or OU key looks like delete + create; accounts are permanent, so renames need explicit semantics (e.g. `renamedFrom`) |
| Drift blocks operations | Control Tower | ⚠ partial: Pulumi refresh plus planned drift view; never blocks |
| SCP lockout at Root, policies not tested | Organizations | ✅ resolved: Policy Staging OU, `staged`, impact check, lock-out check (see Testing policies) |
| AccessDenied hard to trace to a policy | Organizations, CT | ✅ enough: AWS errors name the policy (Jan 2026), policy descriptions carry rule names and reasons, one Sid per rule; predictable denies caught in App preview. `anvil org explain` not planned |
| Removing `FullAWSAccess` causes an outage | Organizations | ✅ never detached |
| Region SCP blocks Organizations/global APIs | Organizations | ✅ global exemptions built in |
| Doing everything at once, then abandoning | Organizations | ✅ tiers; security services opt-in |
| Account provisioning slow or hangs; CI session expires mid-provision | CT (20–45 min) | ✅ `CreateAccount` directly (minutes); docs set a 2h role session; preview warns above ~10 new accounts |
| Automation races a new account (role not yet assumable, StackSets lag) | CT, AFT | ✅ resolved (see New-account readiness) |
| Identity Center assignments not added for new accounts | CT | ✅ stage/OU/all targets expand to new accounts |
| Permission sets fail where a customer-managed policy is missing; slow reprovisioning at scale | Identity Center | ✅ presets use AWS-managed + inline only; ⚠ show reprovisioning progress |
| Account emails unique forever; root mailboxes unmonitored | CT, Organizations | ✅ pattern + validation + shared-mailbox docs; change-email-before-closing in the closing steps |
| Closure quota (AWS docs say 10% vs 20% per rolling 30 days); closed accounts count against the org quota for 90 days unless removed first | Organizations | ✅ resolved: closing steps documented (see Moving down a tier) |
| Workloads in the management account | Organizations | ✅ Apps hard-refused |
| Other policy types never enabled | Organizations | ✅ Org enables them |
| Member root users without MFA | Organizations | ✅ centralized root access removes member root credentials |
| Execution role deleted, account unmanageable | CT | ✅ `protectAccessRole` + `protectAnvilRoles`, can't be weakened |

New gaps found: account quota preflight, rename semantics, new-account
readiness wait, upgrade diffs, closure-quota docs (all resolved).
`anvil org explain` considered and dropped.

## Security review (2026-10-30)

*All settled 2026-11-01* (each finding notes its decision). Problems and gaps found in this design, ranked.

### High

1. **Anvil's automation uses `OrganizationAccountAccessRole`.** Readiness and
   per-account setup assume the break-glass role, so (a) every deploy fires
   break-glass alerts, which trains people to ignore them, and (b) the role
   has AdministratorAccess and trusts the whole management account, so any
   management principal allowed `sts:AssumeRole` (the CI deploy role,
   `orgAdmin`, `billing` if mis-scoped) is admin in every account, including
   Log Archive and Audit, bypassing Security OU access rules. Fix: a
   dedicated automation role in each member account trusting only the Org
   deploy role; narrow `OrganizationAccountAccessRole` trust to named
   break-glass principals (`aws:PrincipalArn`); alert on its use in every
   account.
   *Settled 2026-11-01:* `CreateAccount(RoleName: "anvil-org-automation")`,
   trust narrowed to the deploy role; Org creates break glass trusting
   `break-glass-*` only; emergency principal now required (root can't assume
   roles). Adopted accounts (future) bootstrap once via break glass with a
   labelled alert. See Accounts → Automation role.
2. **SCP exemptions for Anvil's own automation aren't designed, and name-based
   exemptions can be spoofed.** `protectSecurityServices`, `protectAccessRole`,
   the account-default protections, the SSM parameter, scan role and alert
   rule all need an exempt principal so Anvil can manage them. Exempting by
   role name (`aws:PrincipalArn` like `role/anvil-*`) only holds if nobody
   else can create or change roles with that name. Fix: a baseline rule
   denying `iam:CreateRole`, `UpdateAssumeRolePolicy`, policy attach/put and
   `DeleteRole` on `role/anvil-*` except by the automation role itself.
   *Settled 2026-11-01:* only `anvil-org-automation` is exempt, by exact
   ARN, from `protect*` rules only; new `protectAnvilRoles` (can't be
   weakened). See Guardrails → Anvil's exemption.
3. **`denyOutsideOrg` exempts every AWS service principal**, which opens the
   confused deputy problem (a service acting for another customer's account).
   AWS's own sample RCP only lets service principals through when
   `aws:SourceOrgID` is this org (or `aws:SourceAccount` is a named third
   party), checked whenever `aws:SourceAccount` is present.
   *Settled 2026-11-01:* second statement in `denyOutsideOrg`; outside
   sources allowed explicitly via `except({ sourceAccounts, reason })`. Size
   handling (minify, spill, preview error) in Guardrails → Policy size and
   slots.
4. **The `security` preset reads all data.** SecurityAudit + ReadOnlyAccess
   assigned with `all: true` lets the security team read every S3 object,
   DynamoDB item and log in prod. ReadOnlyAccess includes `s3:GetObject` and
   other data reads; ViewOnlyAccess doesn't. Fix: `security` =
   SecurityAudit + ViewOnlyAccess; document that `readOnly` reads data, and
   consider a `viewOnly` preset for prod.
   *Settled 2026-11-01:* `security` = SecurityAudit + ViewOnlyAccess +
   inline read of Log Archive (bucket, and key when KMS is on); new
   `viewOnly` preset; `readOnly` documented as reading data and replaced by
   `viewOnly` in the Security OU allowed sets. See Identity Center → Preset
   permission sets.
5. **Identity Center groups and users are matched by name.** If a group or
   user is deleted and re-created with the same name (by an IdP admin), the
   new one silently inherits access, including `orgAdmin` in management.
   *Settled 2026-11-01:* IDs pinned in state (never in config); preview
   warns when the ID behind a name changes and the deploy grants the new ID
   (accepted risk, rare). See Identity Center → Pinned IDs.

### Medium

6. **The CI deploy role in management is the keys to the org** and isn't
   scoped. Fix: Anvil generates its policy (no `CloseAccount`,
   `LeaveOrganization`, `DeleteOrganization`); OIDC trust pinned to repo,
   branch/environment `sub` and `aud`; docs pin GitHub Actions to commit
   SHAs (the example uses `@v4` tags); offer AWS's sample RCP that limits
   `sts:AssumeRoleWithWebIdentity` from GitHub to the user's GitHub org.
   *Settled 2026-11-01:* Org creates the OIDC provider, preview and deploy
   roles in management only (`ci.github`); deploy policy generated as
   strict least privilege from the config, with explicit denies; it can't
   change itself (new actions need a local deploy, preview errors). Actions
   pinned to SHAs in docs; opt-in `guardrails.githubOidc` RCP. Per-workload
   CI roles benched. See Accounts → CI roles.
7. **Break-glass alerts in one region and only the Security OU.** AssumeRole
   events land where STS was called (global endpoint: us-east-1), and
   EventBridge rules only see their own region. Fix: rules in us-east-1 plus
   every allowed region, in every member account.
   *Settled 2026-11-01:* watched at the source instead: rules in
   management in every enabled region matching break-glass principals,
   management root and any `AssumeRole` on `OrganizationAccountAccessRole`,
   forwarded to an event bus in Audit. Protected from CI by the deploy
   role's deny. See Break glass.
8. **`securityOperator` uses AWS's FullAccess policies**, which include
   disable/delete actions and, for Macie, revealing sensitive data samples.
   Fix: a custom policy without disable/delete and without Macie sample
   reveal; make sure `protectSecurityServices` covers every disable action
   (including disassociating members).
   *Settled 2026-11-01:* SecurityAudit + ViewOnlyAccess + inline triage
   allow-list, with an explicit deny on disable/disassociate, trusted IP
   lists and Macie reveal; `protectSecurityServices` uses the same list.
   See Identity Center → Security OU access.
9. **The Org state bucket isn't limited to named principals** (hardening #7
   "No"). Anyone with S3 write in management could tamper with state, and
   the next CI deploy would act on it. Fix: bucket policy allowing writes
   only from the deploy role (and break glass).
   *Settled 2026-11-01:* bucket policy limits reads and writes (and bucket
   policy/lifecycle changes) to the deploy and preview roles, `orgAdmin`
   users and break glass; switches on once `ci` and `orgAdmin` exist.
   Lock-out is a preflight error with exact fix steps; break glass never
   counts as a way back in. See State → Hardening (7).

### Low

10. **With no security services on, there's no org audit trail.** All
    services are opt-in; preview should warn plainly when `cloudtrail` is
    off, not only for break-glass alerts.
    *Settled 2026-11-01:* CloudTrail on by default as a basic org trail
    (management events, all accounts and regions, Log Archive; ~$1–3/month
    small org). Disabling needs a reason and leaves a management-only trail
    that can't be removed, so break-glass alerts always work. See Security
    services → CloudTrail.
11. **destroyUntagged's delete role and function** should be protected by
    SCP like the other Anvil roles (covered by fix 2).
    *Settled 2026-11-01:* covered by `protectAnvilRoles`.

Sources: [AWS RCP examples](https://docs.aws.amazon.com/organizations/latest/userguide/orgs_manage_policies_rcps_examples.html),
[aws-samples/resource-control-policy-examples](https://github.com/aws-samples/resource-control-policy-examples),
[CloudTrail cross-account activity](https://aws.amazon.com/blogs/security/aws-cloudtrail-now-tracks-cross-account-activity-to-its-origin/),
[EventBridge CloudTrail rules](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-ct-api-tutorial.html),
[ViewOnlyAccess vs ReadOnlyAccess](https://dev.classmethod.jp/articles/iam-policy-readonly-vs-viewonly/).

## Audit (2026-11-01)

*All settled 2026-11-01* (A10 deferred to the hosted dashboard; A12's detection mechanism open). Two passes over the whole spec: (A) best practice,
loopholes, lockouts and security gaps; (B) text that contradicts itself.

### A. Best practice, loopholes and lockouts

**High**

1. *Settled 2026-11-01:* trusts the CI deploy role and `orgAdmin` by exact
   ARN; bootstrap trust pins the first caller until one exists; lock-out
   check. No CI-only restriction (local deploys allowed). Extra deployers
   are explicit in `deployers` (roles or permission sets, exact ARNs);
   preflight checks the caller is a deployer and simulates its management
   permissions. See Accounts → Automation role, Deployers.
   **Lockout: `anvil-org-automation` trusts only the CI deploy role.**
   Local deploys (the first deploy, CI role changes, new permissions, new
   break-glass regions) run as a person, so they can't reach any member
   account. With no `ci` set there's no deploy role at all, and the trust
   has nothing valid to point at. Needs: trust the deploy role **and** the
   local-deploy principal (`orgAdmin`), exact ARNs; define what it trusts
   when `ci` is off.
2. *Settled 2026-11-01:* AdministratorAccess in management with explicit
   denies (leave/delete org, disable policy types, close or remove
   non-suspended accounts, break glass). See Identity Center → Preset
   permission sets.
   **Lockout: `orgAdmin` can't deploy.** The spec makes `orgAdmin` the
   local-deploy principal (state bucket, CI roles, lock-out fixes), but its
   permission set is "Organizations (close Suspended only)". Needs: decide
   what `orgAdmin` is (deploy-capable admin in management, or a separate
   `orgDeployer` set) and keep account closing as a narrow extra.
3. *Settled 2026-11-01:* keep the `break-glass-*` pattern. Every identity
   Org controls in management (CI deploy role, `orgAdmin`, `billing`,
   deployer sets) has an explicit `iam:*` deny on `break-glass-*`, so it
   can't create look-alikes or change the real ones. Principals Org doesn't
   control (hand-made admins, root) can, but any creation or change of a
   `break-glass-*` principal fires a `critical` alert before it's used.
   Exact-ARN pinning considered and not taken. (The `orgAdmin` SSO pattern
   part is already covered by Deployers.)
   **Loophole: `break-glass-*` is a name pattern in management**, where no
   SCP applies. Any management IAM admin can create `break-glass-x` and be
   admin in every member account (and read/write Org state). Alerts fire on
   use, not on creation. Needs: pin the exact principal ARNs found at
   preflight (trust policies store unique IDs, so re-creation breaks trust),
   and alert on `CreateUser`/`CreateRole`/policy changes for `break-glass-*`
   names. Same for the state bucket's `AWSReservedSSO_orgAdmin_*` pattern
   (a permission set named `orgAdmin_x` would match).
4. *Settled 2026-11-01:* `services` is deny-only (allow list dropped as
   too strict; write a custom SCP in `run` for one). Protected services
   (incl. `ssm`) can't be denied, and `anvil-org-automation` skips the rule.
   See Guardrails → services.
   **Lockout: `services.allowed` core list misses what Anvil needs.** Core
   is sts, iam, organizations, kms, logs, cloudwatch, tag, support and the
   security services. Anvil also needs `ssm` (`/anvil/org/account`, read by
   every App preview), `ec2` (EBS encryption default, BPA), `s3`, `events`,
   `sns`, `lambda` (destroyUntagged, forwarder), `codebuild` (Prowler in
   Audit), `sso`, plus `inspector2`/`macie2`/`detective` when on. Needs: core
   list derived from what the config enables, like the CI role.
5. *Settled 2026-11-01:* preview lock-out check on every custom deny:
   error for `anvil-org-automation` and Log Archive delivery, warning for
   break glass (bound by SCPs, recoverable from management). See
   Guardrails → Testing policies. Break glass stays bound by SCPs in member
   accounts (see Break glass).
   **Lockout: custom SCPs/RCPs aren't checked against Anvil or break
   glass.** A custom deny at Root (the spec's own `approvedRegions` example
   has no Anvil exemptions) can block `anvil-org-automation` or
   `OrganizationAccountAccessRole`, or an RCP can block CloudTrail/Config
   delivery to Log Archive. Needs: preview checks custom denies against the
   actions Anvil and break glass need, and errors with the fix.

**Medium**

6. *Settled 2026-11-01:* `denyPublicS3` and `protectEncryptionDefaults`
   moved to Root. Exceptions explicit in `run`, by account and, where AWS
   allows, by resource (`except({ accounts, resources, reason })`). See
   Guardrails → `run`.
   **Secure defaults are only protected in some OUs.** `s3AccountBpa` and
   `ebsDefaultEncryption` are set in every member account, but the SCPs that
   stop turning them off (`denyPublicS3` at Workloads,
   `protectEncryptionDefaults` at Prod/NonProd) leave Sandbox, Security,
   Infrastructure and custom top-level OUs able to turn them off. Needs:
   move those protections to Root.
7. *Settled 2026-11-01:* always-on watched changes (guardrails, buckets,
   CloudTrail, Identity Center, blocked attempts, keys), opt-out per area
   in config with a reason; break-glass watching can't be disabled. See
   Notifications → Watched changes.
   **Detection claims rely on opt-in services.** Log Archive tampering is
   "caught by Config and Access Analyzer", and state-bucket lifecycle
   changes "by the dashboard"; with defaults (CloudTrail only) nothing
   watches either. Needs: cheap EventBridge rules on bucket policy,
   lifecycle and Object Lock changes for the log and state buckets, to the
   notification topic (`critical`).
8. *Settled 2026-11-01:* destroy now removes everything Anvil added except
   the bare security minimum (accounts, OUs, break glass and its alerts,
   `denyLeaveOrg`, `centralRoot`, trail and logs, public-access blocks,
   `denyOutsideOrg`, `requireTls`, `imdsV2`, Identity Center); Anvil's roles
   are removed last; local only. See Destroy.
   **Release leaves a standing admin path.** `anvil destroy` keeps the CI
   deploy role, OIDC provider and `anvil-org-automation` (admin in every
   account, exempt from protect rules) with nothing managing them. Needs:
   release deletes or disables the CI roles and says what's left; docs for
   removing the automation roles by hand.
9. *Settled 2026-11-01:* the dashboard confirms email subscriptions with
   `AuthenticateOnUnsubscribe=true` (paste the confirmation link); no
   preview warnings. See Notifications → Listeners and rules.
   **SNS email unsubscribe links are unauthenticated.** Anyone holding a
   notification email can unsubscribe that address. The change alert
   helps, but the listener is gone. Needs: confirm subscriptions with
   `AuthenticateOnUnsubscribe` (the dashboard confirms on the user's behalf),
   or prefer chat listeners for `critical`.
10. *Deferred 2026-11-01:* designed with the hosted dashboard when it's
    built (SSO, per-group access, scoped backend roles, no user
    credentials). See Org dashboard → Hosting.
    **Notification UI permissions are undefined.** Slack/Teams listeners need
    Amazon Q Developer channel configs (and an IAM role for the channel),
    which `securityOperator` isn't granted; and the local dashboard reads as
    one credential (management) but writes subscriptions as
    `securityOperator` in Audit. Needs: the dashboard's credential model and
    the exact chat permissions.
11. *Settled 2026-11-01:* `denyAll` exempts only
    `OrganizationAccountAccessRole`; any break-glass use in a Suspended
    account sends a dedicated `critical` notification. See Moving down a
    tier and Rules by OU.
    **`Suspended` `denyAll` also blocks break glass.** Nobody can recover
    data or clean up in a suspended account before closing. Needs: decide
    whether `denyAll` exempts `OrganizationAccountAccessRole` (AWS's
    Suspended pattern is deny-all; recovery is then "move it back").
12. *Settled 2026-11-01:* the example routes deploys that create accounts
    to a separate `org-accounts` environment with its own required
    reviewers, in the same run (`anvil preview --github-output` detects new
    accounts). See Accounts → Confirming account creation. *Open:* the
    detection mechanism (preview output routing) to be revisited.
    **CI example always passes `--yes-create-accounts`**, so the hard stop
    never applies in that pipeline; only the environment approval remains.
    Needs: example uses a manual workflow input for account creation.

**Low**

13. *Settled 2026-11-01:* kept as is (locks once both `ci` and `orgAdmin`
    exist); local-only orgs are expected to be rare. Contradiction B3 fixed:
    removing either re-opens the bucket with a warning.
    **State bucket stays open when an org never sets `ci`.** It only locks
    once both `ci` and `orgAdmin` exist, so local-only orgs are open to every
    management admin forever. Needs: lock once either exists.
14. *Settled 2026-11-01:* `denyMemberRoot` (exempts `aws:AssumedRoot`
    sessions) and `denyRegionOptIn` added to the Root baseline; weakened in
    `run` with a reason like other rules. See Rules by OU.
    **No SCP on member root or region opt-in.** Centralized root access
    removes root credentials, but Control Tower also denies root actions
    and `account:EnableRegion`/`DisableRegion` by SCP. Consider both at Root.
15. *Settled 2026-11-01:* freeze SCP works and AWS resets it each period;
    AWS can't stop instances in another account, so `hard` is split into
    opt-in `freeze` and `stop` (Anvil's own stop role). New opt-in
    `billing.schedules` (starting point). AWS Spend Limits noted as a
    watch item. See Accounts → Budgets, Schedules.
    **Hard budgets need verifying:** whether a budget in management can stop
    EC2/RDS in a member account, and whether a freeze SCP lifts itself on the
    1st (budget actions may need reversing by Org on a schedule).

### B. Contradictions in the spec

*All fixed 2026-11-01.*

| # | Where | Says | But |
|---|---|---|---|
| 1 | Rules by OU (`protectKmsKeys`) | "break-glass role excepted" | Anvil's exemption: "exactly one principal is exempt: `anvil-org-automation`" |
| 2 | Security OU table | Audit lockdown "only changeable by the delegated admin role" | Only `anvil-org-automation` is exempt |
| 3 | ~~State → Hardening (7)~~ *Fixed 2026-11-01* | Locks "once **both** are set" | Lock-out error: "Keep ci.github, **or** add at least one user" |
| 4 | Management account stays empty | Lists state bucket, permission sets, org trail, policies | Also in management now: CI roles and OIDC provider, break-glass rules (every region) and forwarder role, management-only trail, budgets and budget topic |
| 5 | Account emails | "whoever reads that inbox can reset root" | `centralRoot` removes member root credentials and blocks root recovery |
| 6 | Secure account defaults | Set "via the access role" | Now `anvil-org-automation`; readiness step 4 doesn't list them |
| 7 | Notifications | "no email addresses or other PII sit in config" | `identityCenter.management` users and account emails are in config (AWS needs them); say so |
| 8 | `anvil org access` example | `management · Platform · orgAdmin`; `website-prod · Engineers · readOnly` | Management is named users only; the assignments example uses `viewOnly` for prod |
| 9 | Full example preview | Lists `securityOperator` | Its config doesn't enable it |
| 10 | Full example footnote | "how Anvil packs the baseline (not designed yet)" | Policy size and slots is designed |
| 11 | Tiers table | Audit is admin "for GuardDuty, Security Hub, Access Analyzer" | Audit is the default admin for every service, CloudTrail included |
| 12 | Research table | "SCP lockout at Root … no staging path (review gap 1)" | Testing policies resolved it |
| 13 | Review → Intentional deviations | "Security services opt-in" | CloudTrail is on by default |
| 14 | Open topics | Notifications "Later: GuardDuty / Security Hub / compliance sources" | Designed in Notifications |
| 15 | Hardening (1) note | Lifecycle gap "covered by detection, not prevention" | (7) now prevents it (once locked) |
| 16 | Security review → 5 | "Fix: preview errors when the ID changes" | Settled as a warning (the settled line is right; the fix text reads as current) |

## Audit 2 (2026-11-02)

*All settled 2026-11-02* (A3 deferred as rare). Second pass after the first audit's changes: (A) best
practice, loopholes and lockouts; (B) contradictions.

### A. Best practice, loopholes and lockouts

**High**

1. *Settled 2026-11-02:* explicit `ProtectManagementSecurity` deny on
   every Org identity (no stop/delete of the trail, no delete/disable of
   break-glass or watch rules; updates allowed) plus a paired watcher in
   every region. Spec claims corrected (B1–B3). See Management account
   stays empty → Protecting management resources.
   **Nothing in management is protected by SCPs, but the spec says it is.**
   The org trail is owned by management, so `protectSecurityServices` never
   applies to it. Any management admin, including `orgAdmin`, deployer sets
   and hand-made admins, can `StopLogging` the org trail, delete the
   break-glass rules or their forwarder role, delete watched-changes rules,
   or change the budget topic. The CI role's denies cover only itself, the
   break-glass principals and the break-glass rules. Needs: explicit denies
   on every Org identity in management (no `StopLogging`/`DeleteTrail`, no
   deleting or disabling `anvil-break-glass-*` / watch rules, updates still
   allowed so deploys work), plus a watcher that alerts on those changes
   from a second rule, so deleting one rule is caught by the other.
2. *Settled 2026-11-02:* the deploy role trusts exactly `environment` and
   `accountsEnvironment` (default `<environment>-accounts`). See Accounts →
   CI roles.
   **CI deploys that create accounts can't sign in.** The deploy role
   trusts only `repo:<repo>:environment:org`, but the example's
   `deploy-with-accounts` job runs in `org-accounts`. Needs: trust both
   environments, and `ci.github` gains the accounts environment name.
3. *Deferred 2026-11-02 (rare):* `orgAdmin` can tag and close for now;
   "close suspended only" guards against mistakes, not a compromised
   `orgAdmin`. Considered: a separate `accountCloser` set (separation of
   duties). Revisit later.
   **`orgAdmin` and the `anvil:lifecycle` tag.** "Every permission set is
   denied changing `anvil:*` tags" so the suspended tag can't be forged, but
   `orgAdmin` is the local deployer and deploys write that tag (moving down
   a tier). Either local deploys can't suspend accounts, or `orgAdmin` can
   tag any account suspended and then close it, which makes
   `CloseOnlySuspended` a speed bump. Needs: a decision, e.g. closing moves
   to its own set (`accountCloser`) that can't tag, or tagging is left to
   CI/deploys and the deny applies to every other set.

**Medium**

4. *Settled 2026-11-02:* severity by who and whether through Anvil (user
   agent): deployer via Anvil `info`, deployer by hand `warning`, anyone
   else `critical`. See Notifications → Watched changes.
   **Watched changes can't tell a deploy from a deployer clicking in the
   console.** Changes by deployer principals are `info`, so `orgAdmin`
   detaching an SCP by hand is reported as routine. Needs: classify by
   principal **and** that the call came from an Anvil deploy (the CLI sets
   its user agent and session name); a deployer acting outside a deploy is
   `critical`.
5. *Settled 2026-11-02:* Destroy keeps `denyMemberRoot` and an SCP
   protecting the kept break-glass bus, rule, topic and key in Audit;
   removes `denyRegionOptIn`. See Destroy.
   **Destroy misses the newer rules and leaves kept alerting unprotected.**
   `denyMemberRoot` and `denyRegionOptIn` aren't in either Destroy table, and
   removing "the rest of `protectSecurityServices`" also removes what
   protects the kept Audit event bus and notification topic, so after
   destroy anyone in Audit can silence break-glass alerts. Needs: keep
   `denyMemberRoot`; decide `denyRegionOptIn`; keep the protection for kept
   break-glass resources in Audit.
6. *Fixed 2026-11-02:* example rewritten with exact deployer ARNs and
   `deployers` roles; break glass by pattern.
   **State bucket policy doesn't match Deployers.** The example policy
   still uses the `AWSReservedSSO_orgAdmin_*` and `break-glass-*` patterns
   and omits deployer roles, while Deployers says exact ARNs are pinned in
   it. Needs: rewrite the example with pinned deployer ARNs (break glass by
   pattern, as decided).

**Low**

7. *Settled 2026-11-02:* condition is "`aws:AssumedRoot` does not exist"
   (`Null` true), the same exemption Control Tower adds to its root
   guardrail. Confirm on a real deploy that direct root is denied and
   `AssumeRoot` tasks work.
   **`denyMemberRoot`'s condition may not match.** `Bool aws:AssumedRoot
   false` doesn't match when the key is absent from a request. Needs:
   `BoolIfExists`, verified against AWS's docs for root sessions.
8. *Settled 2026-11-02:* moving an account down deletes
   `anvil-org-automation`, the scan role and the SSM parameter first, while
   Anvil can still reach it. See Moving down a tier.
   **Suspended accounts keep `anvil-org-automation`.** They're released
   from state and `denyAll` blocks the role, so it's harmless, but destroy
   can't remove it. Needs: remove the role (or narrow its trust) before
   moving an account to Suspended.
9. *Fixed 2026-11-02:* reworded. **Hand-made deployer roles don't carry `BreakGlassUntouched`.** Org
   can't add a deny to `deployers.roles`; the "neither deployer can reach
   break glass" line overstates it. Covered by the creation alert; reword.
10. *Fixed 2026-11-02:* example shows both forms.
    **RCP exceptions use the wrong condition key in the example.**
    `aws:PrincipalAccount` excludes callers in SCPs; RCP exceptions exclude
    resource owners (`aws:ResourceAccount`). Needs: show both forms.

### B. Contradictions

| # | Where | Says | But |
|---|---|---|---|
| 1 | *Fixed* Log Archive table (Detection) | "`protectSecurityServices` stops disabling the trail" | SCPs don't apply to the management-owned trail (A1) |
| 2 | *Fixed* Destroy → kept | "the CloudTrail part of `protectSecurityServices`" protects the trail | Same as 1 |
| 3 | *Fixed* Watched changes | "rules in management by the deploy role's deny, like the break-glass rules" | The deny covers only `rule/anvil-break-glass-*`, and only the CI role |
| 4 | *Fixed* Who can close accounts | "every permission set is denied changing `anvil:*` tags" | `orgAdmin` deploys locally and writes those tags (A3) |
| 5 | *Fixed* CI roles table | Deploy role trusted for `environment:<environment>` only | CI example uses a second environment, `org-accounts` (A2) |
| 6 | *Fixed* State → Hardening (7) | Policy uses the `orgAdmin` SSO pattern, no deployer roles | Deployers: exact ARNs pinned in the state bucket (A6) |
| 7 | *Fixed* Accounts → Deployers | Ends with bullets about the automation role (StackSets, drift, adopted accounts, readiness output) | They belong to Automation role; the section split them off |
| 8 | *Fixed* Preset permission sets | `orgAdmin` "1h suggested" | Preset preview shows `orgAdmin (4h)` |

## Audit 3 (2026-11-02)

*To work through.* Third pass, after the spec was split into files. Run as
six parallel reviews (access and identity, guardrails, security services
and notifications, accounts/budgets/destroy, API/linking/dashboard, and
cross-file contradictions), then merged and de-duplicated. File references
are to this folder.

### A. Best practice, loopholes and lockouts

**High**

1. *Settled 2026-11-03:* `denyPublicS3` denies only
   `s3:PutAccountPublicAccessBlock`; `protectEncryptionDefaults` keeps only
   the EBS part; `s3AccountBpa.except({ resources })` means the account is
   excepted plus a `critical` alert on any other bucket going public; Anvil's
   CI test org runs App deploys against the baseline. See 06 → Rules by OU.
   **Root S3 protections break every App bucket.** `denyPublicS3` and
   `protectEncryptionDefaults` (06) deny bucket-level Block Public Access and
   encryption changes, but SCPs can't see whether a call turns protection
   on or off. Every Anvil `Bucket` registers its own `BucketPublicAccessBlock`
   and encryption config (`provider/aws/bucket/bucket.go:160`, `:185`), so
   every App deploy fails. S3 default encryption also can't be turned off
   since 2023. Fix: deny only `s3:PutAccountPublicAccessBlock` (account level
   overrides buckets), drop the S3 part of `protectEncryptionDefaults` (keep
   EBS); run App deploys against the baseline in the CI test org.
2. *Settled 2026-11-03:* the role is disabled, not deleted (trust deny-all,
   inline policies deleted, managed policies detached last), left inert and
   tagged; destroy disables roles before removing `protectAnvilRoles`;
   resumable. See 03 → Running it, 05 → Moving down a tier.
   **`anvil-org-automation` can't delete itself** (03 destroy, 05 moving
   down). `DeleteRole` needs every policy detached first; once it detaches
   its own admin policy it can't call `DeleteRole`, and `protectAnvilRoles`
   blocks everyone else. Destroy and downgrades stop part-way and can't
   resume. Fix: disable instead of delete (trust set to deny-all, then
   detach as the last call), resumable when already gone.
3. *Settled 2026-11-03:* lock adds `DeleteBucketPolicy`, versioning,
   encryption, replication and ownership changes; reads add
   `GetObjectVersion`; destroy removes policy → versions → bucket. See 01 →
   Hardening (7), 03. Also settled: no chaining into management from other
   accounts (preflight errors on deployer roles trusting outside
   principals; see 05 → Deployers).
   **State bucket lock removable in one call** (01). The deny lists
   `PutBucketPolicy` but not `DeleteBucketPolicy`, `PutBucketVersioning`,
   `PutEncryptionConfiguration`, `PutReplicationConfiguration`,
   `PutBucketOwnershipControls`; reads miss `GetObjectVersion`. Also: destroy
   must remove the policy, delete versions, then the bucket ((1) blocks
   emptying it otherwise).
4. *Settled 2026-11-03:* alert on a person (not the CI role) assuming
   `anvil-org-automation`: `warning`, `critical` into the Security OU.
   Deployers are trusted named users, so no role split and no extra Log
   Archive denies. See 02 → Every use alerts.
   **`orgAdmin` can act as the SCP-exempt automation role by hand** (05,
   06). The automation role trusts `orgAdmin` directly and is the only
   principal exempt from `protect*`; Security OU allowed sets are only
   checked in preview. A stolen `orgAdmin` session assumes it in Log Archive
   and rewrites the bucket policy; no break-glass alert, and the user agent
   can claim Anvil. Fix: alert `critical` on `AssumeRole` of
   `anvil-org-automation` by a human principal (Security OU accounts
   especially); deny log deletion and `BypassGovernanceRetention` even to the
   automation role in Log Archive.
5. *Settled 2026-11-03:* weakening trail changes are `critical` whoever
   makes them; `protectSecurityServices` in Audit denies delete, stop,
   update and selector changes on the org trail; the `cloudtrail` watch also
   runs in Audit. See 07 → Delegated admin, 09 → Watched changes.
   **The trail can be silenced by update, or deleted from Audit** (05, 07).
   `ProtectManagementSecurity` allows `UpdateTrail` / event selectors (repoint
   the bucket, single-region, exclude `sts`/`signin`); the CloudTrail
   delegated admin (Audit) can update and delete org trails, outside the
   management deny and the management-only watch. Fix: any weakening trail
   change is `critical` whoever makes it; `protectSecurityServices` in Audit
   denies delete/stop/update on the org trail; `cloudtrail` watch in Audit.
6. *Settled 2026-11-03:* disabling `cloudtrail` leaves a minimal org trail
   (every account, advanced selectors for the watched event sources) instead
   of a management-only trail; verify selector behaviour with EventBridge on
   AWS. See 07 → CloudTrail.
   **`cloudtrail` disabled silences every member-account watch** (07, 09).
   EventBridge only gets CloudTrail events from accounts a trail covers; the
   management-only trail covers management only, so `blockedAttempts`,
   bucket, key, topic and subscription alerts in members stop. Fix: keep a
   minimal org trail for watched event sources, or list every watch that
   stops in the warning.
7. *Settled 2026-11-03:* `ProtectForwarder` deny on the forwarder role,
   `PutTargets` added to the management deny, pattern changes outside a
   deploy `critical`, Audit bus policy protected, and a second alert path (a
   management SNS topic plus a `FailedInvocations` alarm) that doesn't use
   the forwarder. See 05 → Protecting management resources.
   **The break-glass forwarder is an unprotected single point of failure**
   (02, 05). Nothing denies or watches IAM changes to
   `anvil-break-glass-forwarder`, rule targets (`PutTargets`) or the Audit bus
   policy; deleting the role's policy silences every management alert,
   including the watcher (which forwards through it). Fix: deny IAM changes
   to the forwarder, `PutTargets` and bus-policy changes; watch them over a
   path that doesn't use the forwarder; alarm on `FailedInvocations`.
8. *Deferred 2026-11-03:* revisit with the open CI account-approval
   mechanism (plan requirement for required reviewers on private repos,
   `main`-only deployment branches, `ref` claim in the deploy role's trust,
   preview check for reviewers).
   **The GitHub approval gates may not exist** (05). Required reviewers are
   only available on private repos with GitHub Enterprise (public repos on
   any plan), and environments aren't limited to `main`, so a branch can edit
   the `if:` and target `org`. Fix: docs state the plan requirement and
   require "Deployment branches: main" on both environments; a fallback for
   non-Enterprise private repos.

**Medium**

9. **Management denies are a speed bump for `orgAdmin`** (05, 08): it can
   edit its own permission set and sign in again; `PutRule` (never-matching
   pattern) and `PutTargets` are allowed; `DisableAWSServiceAccess` and
   `DeregisterDelegatedAdministrator` aren't denied. Say "deters", not
   "prevents"; deny `sso:` writes on Org-identity permission sets except by
   another deployer; add the missing actions.
10. **The deny blocks destroy and watch opt-outs** (03, 05): destroy runs as
    `orgAdmin` and must delete `anvil-watch-*` rules; `watch.<area>.disable`
    and switching off the management-only trail delete denied resources.
    Also the CI role's deny blocks `PutRule` while text says CI can update.
    Define how each removal lifts the deny, or rename non-permanent rules.
11. **Budget freeze SCP can freeze an account permanently** (05, 03): the
    action attaches it outside Pulumi; deleting the budget mid-period leaves
    it attached with nothing to reset it, and `DeletePolicy` fails while
    attached. Reverse executed actions and detach first; list the Budgets
    execution role and `iam:PassRole`.
12. **`stop` and schedules fight Auto Scaling; RDS restarts itself** (05):
    stopped ASG instances are replaced (service-linked role, not blocked by
    the freeze). Set desired capacity to 0 or skip ASG instances; `stop`
    should re-check RDS after 7 days.
13. **Account Management trusted access isn't enabled** (01, 05): renames
    (`PutAccountName`) and contact reads fail without it; with it, root email
    changes work from management (`StartPrimaryEmailUpdate`).
14. **Closing step 2 can't work for Anvil-created accounts** (05): removing
    an account from the org needs standalone sign-up details entered in the
    account, 4+ days age, and it can't be a delegated admin. Document the
    real path; delete `OrganizationAccountAccessRole` after removal.
15. **`imdsV2` only sets a default** (06): needs `http_tokens_enforced:
    enabled` (and hop limit ≥2); docs warn IMDSv1 AMIs then fail to launch.
16. **`denyOutsideOrg` identity statement underspecified** (06): pin AWS's
    sample action list (excludes SAML/web identity, which carry no
    `aws:PrincipalOrgID`, or Identity Center sign-in breaks); add
    `except({ principalAccounts })`; scope `sourceAccounts` to accounts.
17. **WAF logs in us-east-1 hold request data** (06): "hold no data" is
    wrong; residency orgs need it said plainly or made opt-in.
18. **Notifications need an enrichment function** (09): message attributes,
    account name/OU lookups, "new findings only" and hourly grouping can't be
    done by EventBridge alone. One function in Audit (extend the budget
    forwarder) with a small dedupe store; add to costs and protections.
19. **Security Hub aggregation region undefined** (07, 09): set the
    aggregator/home region (Audit's), linked regions from `allowedRegions`,
    central configuration, and how the admin changes.
20. **Disabling the notifications key silences its own alert** (09): deny
    key disable/deletion/policy changes on the notification and budget-topic
    keys; send that alert by a path that doesn't use the key.
21. **A role path defeats break-glass alerts** (02): `role/break-glass-*`
    matches `/break-glass-x/evil`, and assumed-role ARNs drop the path.
    Require path `/` for break-glass roles; alert on paths starting
    `/break-glass-`.
22. **Re-created `orgAdmin` SSO role leaves a dead trust** (05): if both
    users are deprovisioned, the role is deleted; local-only orgs then reach
    members only by break glass. Preflight detects stale principals and gives
    non-break-glass steps; recommend a second deployer.
23. **CI preview can't reach member accounts** (05): add a read-only
    `anvil-org-read` role in each member trusted by `anvil-org-preview`.
24. **A delegated Identity Center admin bypasses Security OU allowed sets**
    (08): SCP on that account denying assignments on Security OU accounts.
25. **The automation role's wide trust lasts for pending accounts** (05):
    narrow as soon as IAM is reachable; alert on non-deployer `AssumeRole`.
26. **Deployer sets can't tag** (05): they're denied `anvil:*` tags, which
    deploys write. Scope the deny to `anvil:lifecycle` for non-deployers.
27. **API types** (12): the `ou` type rejects `Sandbox: { disable: true }`
    (intersection with the `Record`); `renamedFrom` missing on workloads and
    OUs.
28. **App linking can't verify org or account** (11): the parameter has
    names only; add `orgId`/`accountId` and check against
    `GetCallerIdentity` / `DescribeOrganization`.
29. **Local dashboard writes** (10, 09): list the protections (Host check,
    header token, CSP) and require writes as POST via `/api`; the "calls SNS
    with the user's credentials" wording reads as if the browser does it.

**Low**

30. `denyMemberRoot` shows only the `Null` condition; show it with
    `aws:PrincipalArn` `arn:*:iam::*:root`, or it reads as denying everyone.
31. `protectAnvilRoles` misses role paths (`role/*/anvil-*`).
32. Lambda@Edge carve-outs: also deny `UpdateFunctionConfiguration` adding
    VPC/layers; scope the edge log-write exemption.
33. RCP service coverage behind AWS samples (`sts` for TLS; `logs`,
    `dynamodb`, `ecr`, `cognito-idp`); omit empty `sourceAccounts`.
34. Legacy ELB access-log bucket policies break under `denyOutsideOrg`;
    flag them.
35. `ebsDefaultEncryption` regions undefined without `allowedRegions`.
36. Chained role sessions are capped at 1h (deploy role → automation role);
    set `MaxSessionDuration` and re-assume per account.
37. Destroy deletes Policy Staging OU while it holds accounts.
38. Preview role runs PR code with state read; GitHub's approval dialog
    doesn't show job outputs (summary does).
39. `githubOidc` RCP doesn't cover management (where Org's CI roles are).
40. `aws:PrincipalArn` conditions match re-created roles (string compare);
    "doesn't keep access" holds only for trust `Principal` elements.
41. Management users looked up by email; use `UserName`, error on >1 match.
42. Read-only break-glass calls don't alert unless rules use
    `ENABLED_WITH_ALL_CLOUDTRAIL_MANAGEMENT_EVENTS`.
43. Periodic Config cost overstated (~$3.60, not $36).
44. Management account enrolment in GuardDuty/Config/CSPM not stated.
45. Filter-policy changes (`SetSubscriptionAttributes`) don't alert.
46. Object Lock recovery needs the Log Archive SCP detached first; say so.
47. Dashboard read access needs admin in management; consider a read-only
    set.
48. Downgrade leaves `anvil-budget-stop`, `anvil-scheduler`, destroyUntagged,
    budgets and assignments behind; "workloads in them stop" is wrong
    (running resources keep billing).

### B. Contradictions and stale text

**Stale decisions:** 01 automation trust "to the Org deploy role" (now
deployers); 05 CI preview shows one environment; 09 "Budget hard freeze";
03 "Prod/NonProd data protections" (one is at Root); 06 "core services
list" (now protected services); 05 planned `billing` sketch has an old
budgets shape; 05 readiness "the access role"; reviews research row
"security services opt-in"; 10 "Prowler once org-wide compliance exists";
01 "backup when used" (benched).

**Contradictions:** CI deny on `PutRule` vs "updates allowed from CI";
"never removed" resources that destroy/opt-outs remove; override delegated
admin "gets Audit's lockdown" vs 04 "this account only"; management
assignment error text omits deployer sets; `protectSecurityServices` "same
list as securityOperator" (it's a superset); "config can only strengthen"
vs `lambdaEdge`, `cloudtrail.disable`, `watch.*.disable` in config; config
style `enable` used inconsistently, `security: {}` blank object;
`ProtectManagementSecurity` missing from the CI and `orgAdmin` deny lists;
services caught by App preview, but the SSM summary has no services.

**Broken references:** `#tiers` and `#custom-ous` anchors now in other
files; Security OU baseline and lockdowns not in Rules by OU; SSM parameter,
member watch rules, topic and bus said to be SCP-protected but no rule
lists them.

**Examples:** `ou`/`workloads` types lack `renamedFrom`; state bucket
principals lack deployer permission sets; automation trust JSON lacks a
deployer entry; readiness preview omits BPA/EBS/tags; full example's
Bedrock SCP can never take effect under `allowedRegions`; first example
creates 11 accounts with no quota error; Outputs use an undeclared account.

**Incomplete lists:** Management account stays empty misses paired
watchers, keys and the management-only trail bucket; Destroy tables miss
Security OU lockdowns, Identity Center delegated-admin SCPs, the Log Archive
KMS key, state key, schedules/stop roles, `OrgSettings`, `ebsDefaultEncryption`;
Destroy's "opt-in guardrails" row lists baseline rules; notification
sources table misses budget `stop`, break-glass principal changes and
Suspended use.

### Audit 3 outcome (2026-11-03)

- **Billing parked:** budgets, freeze/stop, schedules and billing exports
  moved to [later-billing.md](later-billing.md), out of v1. M11, M12 and L48
  are recorded there.
- **Quick fixes applied:** M14 (account removal path), M17 (WAF logs hold
  request data), M28 (App linking checks `orgId`/`accountId`), M29 (local
  dashboard protections, writes via `/api`), L30 (`denyMemberRoot` full
  condition), L34 (legacy ELB log buckets), L35 (EBS encryption regions
  without `allowedRegions`), L41 (users looked up by user name), L43
  (Config cost), L44 (management enrolled in security services); broken
  `#tiers` / `#custom-ous` links fixed.
- **Later, when they come up in the build:** M10, M13, M15, M16, M18, M19,
  M22, M23, M26, M27, L36, L37, and the remaining contradictions and stale
  text in section B. (M16 matters most: pin `denyOutsideOrg`'s STS actions to
  AWS's sample so Identity Center SAML sign-in isn't blocked.)
- **Accepted risk (only after a compromise; MFA and SSO make it rare):** M9,
  M20, M21, M24, M25, L31, L32, L33, L38, L39, L40, L42, L45, L46, L47.

## Decision: dedicated emergency account (2026-11-06)

After checking AWS Well-Architected SEC03-BP03, break-glass credentials
move out of management into a **dedicated emergency account** (`emergency`,
Security OU, every tier):

- hand-made `break-glass-*` IAM users with hardware MFA; the account's root
  is kept (the one exception to `centralRoot` and `denyMemberRoot`) so the
  users can be created, protected by MFA in a vault;
- `protectBreakGlass` lockdown on the account (can't be weakened);
- `OrganizationAccountAccessRole` in every member trusts only those users,
  with MFA;
- alert rules in the emergency account, every region, to Audit;
  management keeps rules for its root, `AssumeRole` on the break-glass
  role, and people assuming `anvil-org-automation`;
- the management root user is the last resort for SCP lockouts (the
  emergency account is a member, so it can't detach SCPs);
- break glass no longer has state-bucket access; management-side
  `BreakGlassUntouched` denies removed;
- `minimum` tier now creates 3 accounts.

See [Break glass](02-break-glass.md).

## Decision: tiers move to init; config lists everything (2026-11-08)

`tier: 'minimum'` in config hid what Org creates: six OUs (and later three
accounts) appeared from one word. Now:

- **Config implies nothing.** `ou` lists every OU, each `{ enable: true }`,
  parents included; Org manages exactly what's listed. No built-in OUs, no
  `Sandbox: { disable: true }` switch (delete the line instead).
- **Tiers live in `anvil org init`.** It asks for `minimum`, `network` or
  `sra`, then Sandbox and Policy Staging, explaining each OU, writes them out
  with a comment per line, and prints the tree. Flags (`--tier`,
  `--sandbox=false`, `--policy-staging`) answer for scripts.
- **No state migration.** OUs are named by path in state, so writing the
  same paths out explicitly previews with no changes. An old `tier:` field
  fails with the replacement `ou` block to paste.
- Core accounts follow the same rule when account creation lands: init
  writes them; how a core account is marked is decided then.
- "Moving down a tier" becomes "Removing an account" (same steps).
- **Nested OUs (same day).** `ou` nests the way OUs sit in AWS: keys are
  names, each OU's own `ou` holds its children
  (`Workloads: { enable: true, ou: { Prod: {...}, NonProd: {...} } }`).
  Flat `'Workloads/Prod'` keys were hard to read and needed a "parent isn't
  listed" rule; a path key now fails with the nested form to write. Paths
  stay the way everything else names an OU (accounts, guardrails, state),
  so deployed OUs see no change.

## Decision: accounts written inside their OU (2026-11-08)

Same reasoning as dropping `tier`: nothing generated, the file is the org.

- **Every account is written inside the OU it lives in** (`Prod: { enable:
  true, accounts: { 'payments-prod': {...} } }`). No top-level `accounts`,
  no `ou` field on an account.
- **`workloads`, `stages` and `sandboxes` are dropped.** They generated
  accounts (`payments` × `prod` → `payments-prod`) that never appeared by
  name. Naming `<workload>-<environment>` is now a docs recommendation.
- **The account name is its identity in state.** Adding accounts never
  touches others; reordering changes nothing; moving a block is
  `MoveAccount`; removing one suspends and releases it (never closes);
  renames need `renamedFrom`. Names are unique across the org.
- **App linking names accounts directly:** `org: { accounts: { prod:
  'payments-prod', dev: 'payments-dev' } }` or `org: { account: '…' }`.
  The SSM parameter drops `workload` and `stage`.
- Identity Center loses the `stages` target (accounts, OUs, `all` remain).
- Import is implemented in the new shape; a top-level `accounts` fails with
  the nested form to write.
- Every account entry carries `enable: true`, imports included (decided
  same day), so OUs and accounts all look the same.
- Open: core-account marking.

## Decision: names stay the keys, renames use renamedFrom (2026-11-08)

Considered a list with a `name` field, and a handle key with a separate
`name`. Kept the name as the key: it's what shows in the console, preview and
errors, and a `Record` maps directly to a dict or map in Python and Go.

- **`renamedFrom` on accounts and OUs** is built (TypeScript), mapped to
  Pulumi aliases. An OU rename moves its children's state too, without their
  own `renamedFrom`.
- **Imported accounts keep their AWS name**: the rename is config and state
  only. Created accounts will be renamed with `PutAccountName`.
- **The CLI checks renames against state before Pulumi runs**: an imported
  account ID under a new name without `renamedFrom` is an error; an OU
  removed and one added under the same parent is a warning.

## Decision: account creation built, removal is conservative for now (2026-11-08)

- **`import` is optional**: an entry without it is created. Created accounts
  need `email` (org-wide pattern with `{account}`, or per account), and a
  top-level `Suspended` OU, since removed accounts move there.
- **Hard stop** on deploy: type `yes`; in CI `--yes-create-accounts`, and
  `--yes-suspend-accounts` for removals. Preview lists both and checks the
  account quota (found by name in Service Quotas; tell only).
- **Removal doesn't disable `anvil-org-automation` yet.** The spec disables
  it and relies on break glass (`OrganizationAccountAccessRole`) to get back
  in, but break glass isn't built, and created accounts don't get
  `OrganizationAccountAccessRole` (CreateAccount makes Anvil's role
  instead). Disabling it now would leave a suspended account reachable only
  by root recovery. Revisit when break glass lands.
- **Removed accounts keep `anvil:org`** and lose `anvil:managed`, so writing
  the block back adopts the account from Suspended instead of failing on its
  email.
- **Still open:** core-account marking, and init writing the core accounts.
