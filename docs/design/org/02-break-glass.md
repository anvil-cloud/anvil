# Org — Break glass

Part of the [Org design](README.md).

## Break glass

Break glass is how people still get in when everything else is broken. It
follows AWS Well-Architected SEC03-BP03 ("Establish emergency access
process"): the human credentials are created by hand and kept offline, the
roles they use in every account are created and protected automatically,
every use alerts, and the credentials live in a **dedicated emergency
access account**, not in management.

| Layer | What | For |
|---|---|---|
| 1 | **Management root user** (manual, MFA in a vault) | Last resort: SCP lockouts and management-account recovery. Management isn't bound by SCPs, so it can detach any SCP or RCP |
| 2 | **Emergency account** (`emergency`, Security OU): hand-made `break-glass-*` IAM users with hardware MFA | The normal break-glass path when Identity Center or the identity provider is down or misconfigured |
| 3 | **`OrganizationAccountAccessRole`** in every member, trusting only the emergency account's `break-glass-*` users | What break glass switches into; created and protected by Org |

```
emergency account: break-glass-1 signs in (password + hardware MFA)
   └─ sts:AssumeRole ─▶ OrganizationAccountAccessRole in log-archive / audit / any member
                          AdministratorAccess, trusts emergency's break-glass-* users only
```

### The emergency account

A third core account, in every starting layout, in the Security OU.
It's used for nothing but break glass: no workloads, no Identity Center
assignments (it must not depend on Identity Center), no Anvil Apps.

**It's the one account whose root user is kept.** Org deletes member root
credentials everywhere else (`centralRoot`) and blocks root (`denyMemberRoot`),
which would leave nobody able to create the break-glass users. In the
emergency account only:

| Rule | Exception |
|---|---|
| `centralRoot` | Root credentials aren't deleted; root is recovered once from the account email and protected with 2+ hardware MFA devices kept in a vault |
| `denyMemberRoot` | Root may manage the `break-glass-*` IAM users (create, MFA, passwords) and nothing else |
| `denyIamUsers` | `user/break-glass-*` allowed |

**Its own lockdown** (`protectBreakGlass`, attached to the account, can't be
weakened): no resources or workloads, no IAM principals other than
`user/break-glass-*`, no changes to the alert rules or forwarder; the
break-glass users may only manage their own password and MFA, read, and
`sts:AssumeRole` into `OrganizationAccountAccessRole`.

**One-time setup** (Org never creates the users or holds their
credentials). Preflight checks it and gives exact steps:

```
✗ emergency (999988887777): no break-glass user yet.
  1. Recover the root user from the account email (aws+emergency@acme.com) and add
     2+ hardware MFA devices; store the password and devices in your vault.
  2. As root, create IAM users break-glass-<n>, each with its own hardware MFA.
     Keep credentials offline.
  3. Deploy again.
```

- Names, not people: `break-glass-1`, `break-glass-2`.
- Few users, one per emergency administrator, so CloudTrail shows who.
- The emergency account's email is a monitored shared mailbox (it's the
  root recovery path).

### Built-in, can't be removed

- **Always present.** Org creates `OrganizationAccountAccessRole` in every
  member during setup (through `anvil-org-automation`):

  ```json
  { "Effect": "Allow",
    "Principal": { "AWS": "arn:aws:iam::<emergency>:root" },
    "Action": "sts:AssumeRole",
    "Condition": {
      "ArnLike": { "aws:PrincipalArn": "arn:aws:iam::<emergency>:user/break-glass-*" },
      "Bool": { "aws:MultiFactorAuthPresent": "true" } } }
  ```

  Nothing in management (CI role, deployers, `orgAdmin`) can assume it,
  and nothing in a workload account can create a principal it trusts:
  only users in the emergency account match, and that account's lockdown
  allows no other IAM principals.
- **Can't be removed or weakened.** `protectAccessRole`, `protectBreakGlass`
  and `protectAnvilRoles` are the only baseline rules with no `except` or
  `disable`:

  ```
  ✗ ctx.guardrails.protectAccessRole.disable: protectAccessRole is break glass
    and can't be weakened
  ```

  Prune never touches the role; `anvil org destroy` keeps it, the emergency
  account and its lockdown (see Destroy).
- **Bound by SCPs in member accounts.** `OrganizationAccountAccessRole` is a
  role inside each account, so every SCP there applies to it. The emergency
  account is a member too, so it can't detach a bad SCP: that's the
  management root user's job (SCP lockout playbook, below). Removing a
  guardrail is a deliberate, logged, alerted step, so stolen break-glass
  credentials can't quietly turn off security services or delete logs.

### Every use alerts

The break-glass users exist only in the emergency account, so Org watches
there:

```
1. break-glass-1 signs in (emergency)          → CloudTrail: ConsoleLogin
2. assumes OrganizationAccountAccessRole        → CloudTrail: AssumeRole (region of the STS endpoint used)
3. EventBridge rule in that region of emergency matches → forwards to Audit
4. Audit event bus anvil-break-glass raises the alert (critical)
```

- **Rules in the emergency account, in every enabled region** (EventBridge
  only sees its own region's events, and a caller can use any region's STS
  endpoint). ~17 rules by default.
- **Matches:** any API call or console sign-in in the emergency account
  (it should never be used day to day), including its root user; and any
  creation or change of an IAM principal there.
- **Management still watches** its root user (any activity is `critical`),
  `AssumeRole` on `*/OrganizationAccountAccessRole` by anyone (catches the
  labelled bootstrap of adopted accounts, whose original role still trusts
  all of management), and a person assuming `anvil-org-automation`
  (`warning`, `critical` into a Security OU account; CI is `info`).
- **Forwarded to one event bus in Audit** through a forwarder role that can
  only `events:PutEvents` on that bus; the Audit rule publishes to the org
  notification topic (see Notifications).
- **Protected by `protectBreakGlass`** in the emergency account (rules,
  targets, forwarder role) and `protectSecurityServices` in Audit (bus,
  bus policy, alert rule). A paired watcher in each region alerts if a rule
  there changes, and a `FailedInvocations` alarm catches a broken delivery
  chain.
- CloudTrail events only reach EventBridge when a logging trail covers the
  account. The org trail is on by default; if it's disabled, the minimal
  org trail stays (part of break glass, can't be removed). See Security
  services → CloudTrail.

### Playbooks (docs)

One process per failure mode, as AWS recommends:

| Failure | Path |
|---|---|
| Identity provider down, or its configuration in Identity Center broken | Emergency account → `break-glass-<n>` → `OrganizationAccountAccessRole` in the account to fix |
| An SCP or RCP blocks break glass in a member | **Management root user** → detach the policy → break glass as above → redeploy restores the policy |
| Identity Center or its region disrupted | As the first row (break glass doesn't use Identity Center) |
| A bucket or queue policy locks everyone out | Management root → `sts:AssumeRoot` (centralized root access) → delete the policy |

Docs must say: break glass is for emergencies only; keep the management
and emergency root MFA devices and the break-glass credentials offline;
review every alert; rotate break-glass passwords after each use.
