# Org — Destroy

Part of the [Org design](README.md).

## Destroy

**`anvil org destroy` removes everything Anvil added, except the bare minimum
that keeps the org secure and recoverable.** Accounts are never closed.
Afterwards the org can be run by hand, by another tool (e.g. Control
Tower), or re-adopted by a new Org project.

The rule for what stays: **it stops a takeover, keeps break glass working,
keeps the audit record, or keeps people able to work.** Everything else is
Anvil's machinery or an org choice, and goes.

### What's kept, and why

| Kept | Why it's the minimum |
|---|---|
| **Accounts** | Never closed by Anvil (closing stays a console action, see Who can close accounts) |
| **OUs that hold accounts** | The kept SCPs below attach to them; Suspended keeps `denyAll` on accounts awaiting closure. Empty Anvil OUs are removed |
| **Break glass:** the emergency account (its root and `break-glass-*` users), `protectBreakGlass`, `OrganizationAccountAccessRole` in every member (trusting the emergency users), `protectAccessRole`, the break-glass alert rules in the emergency account, the Audit event bus and the notification topic (with its listeners) | The only guaranteed way in that doesn't depend on Identity Center, and the alert when it's used. Without it a lost identity provider means a lost org |
| **`denyLeaveOrg`** | Stops a compromised account leaving the org and taking its guardrails and logs with it |
| **`centralRoot`** (no member root credentials) and **`denyMemberRoot`** | Turning them off brings back root passwords nobody holds, or lets a recovered root act: a classic takeover path. `AssumeRoot` recovery still works |
| **Protection for kept alerting in Audit:** an SCP denying deletion or disabling of the break-glass event bus, its rule, the notification topic and its KMS key, for everyone | Without it, anyone in Audit could silence break-glass alerts after destroy |
| **Org trail, Log Archive bucket and every existing log**, plus the Log Archive lockdown (SCP, in Log Archive) and the paired watcher on the trail and break-glass rules in management (no SCP can protect the management-owned trail; `orgAdmin` and deployer sets keep their `ProtectManagementSecurity` deny) | Deleting logs destroys the audit record (often a compliance breach), and stopping the trail is an attacker's first move. With `cloudtrail` disabled, the minimal org trail is kept instead |
| **Data-exposure blocks:** `s3AccountBpa` + `denyPublicS3`, `snapshotBpa`, `amiBpa`, `denyOutsideOrg` (with confused-deputy statement), `requireTls`, `imdsV2` | Without them a data leak is one API call away. Free, and they rarely block anything legitimate |
| **Identity Center:** permission sets and assignments, including `orgAdmin`, `billing` and deployer sets | People keep working. Removing them would leave break glass as the only way in, which is a lockout, not security |
| **Exceptions on kept rules** (`except` with reasons) | Removing them would suddenly block what the org deliberately allowed |

Kept SCPs and RCPs are consolidated into policies named
`org-security-minimum-*`, with a description saying they were left by Anvil
on destroy and what each Sid does. The `anvil:managed` / `anvil:org` tags are
removed from everything kept; kept resources get `anvil:kept=true` so it's
clear where they came from.

### What's removed, and why

| Removed | Why |
|---|---|
| **Anvil's access:** `anvil-org-automation` in every member (**disabled**, see below), the CI roles (`anvil-org-deploy`, `anvil-org-preview`) and GitHub OIDC provider, deployer trust | No standing admin path into every account with nothing managing it. Done **last** (Anvil uses them to remove everything else) |
| **Rules that only protect Anvil's machinery:** `protectAnvilRoles`, the rest of `protectSecurityServices`, the scan role, `/anvil/org/account`, Anvil's exemptions in kept policies | What they protected is gone |
| **Security services:** GuardDuty, Security Hub, Security Hub CSPM, Config, Inspector, Macie, Detective, Access Analyzer, org-wide Prowler | Their cost and configuration are org choices. ⚠ **Disabling deletes their findings history** (GuardDuty, Security Hub, Macie, Detective); preview says so and needs typed confirmation. Config history already delivered to Log Archive stays |
| **Watched changes** (except break glass and the paired watcher), destroyUntagged | Monitoring and automation for a project that no longer exists |
| **Opt-in guardrails:** `allowedRegions`, `instanceTypes`, `services`, `requireTags` (tag policies), `vpcBlockPublicAccess`, `allowedImages`, `githubOidc`, `aiOptOut`, `serialConsoleOff`, `denyIamUsers`, `denyCommitmentPurchases`, `denyRegionOptIn`, Sandbox rules, and your own SCPs/RCPs from `run` | Org choices, not baseline safety. Anything the org still wants is re-created by its next tool. (`denyRegionOptIn` only mattered while security services watched per region; the multi-region trail already covers new regions) |
| **Prod/NonProd data protections:** `protectBackups`, `protectKmsKeys`, `protectEncryptionDefaults` | Limit damage after a breach rather than prevent one. ⚠ Keep them with `--keep-data-protections` |
| **Org state bucket**, cost allocation tag activation, Policy Staging OU | Nothing left to manage. State is deleted last, after everything it tracks: the deployer removes the bucket policy, deletes every version, then the bucket (hardening (1) refuses version and bucket deletes while the policy is in place) |

Account settings Org turned on (S3 account Block Public Access, EBS
encryption by default) stay on; only the SCP stopping EBS encryption being
turned off goes (with `protectEncryptionDefaults`).

### Running it

**Local only, as a deployer** (`orgAdmin` or one in `deployers`). CI can't
run it: it deletes the CI roles, and the deploy role can't change itself.

```
✗ anvil org destroy can't run from CI: it removes the CI roles themselves.
  Run it locally as an orgAdmin user or another deployer.
```

```
anvil org destroy

  Removing Anvil from acme. Kept (bare minimum security):
    = 12 accounts, 7 OUs, Identity Center (8 permission sets, 23 assignments)
    = break glass: OrganizationAccountAccessRole (12 accounts), alerts → audit topic (3 listeners)
    = denyLeaveOrg, centralRoot, denyMemberRoot, org trail + Log Archive logs (1y retention)
    = protection for the break-glass bus and notification topic (audit)
    = s3AccountBpa, denyPublicS3, snapshotBpa, amiBpa, denyOutsideOrg, requireTls, imdsV2
      → consolidated into org-security-minimum-scp, org-security-minimum-rcp
  Removed:
    - anvil-org-automation disabled (12 accounts), anvil-org-deploy, anvil-org-preview, GitHub OIDC provider
    - guardduty, securityHubCspm, config        ⚠ GuardDuty and Security Hub findings history deleted
    - allowedRegions, requireTags, 3 custom SCPs, watched changes, compliance scanner
    - protectBackups, protectKmsKeys, protectEncryptionDefaults   (keep with --keep-data-protections)
    - Org state bucket

  Type the org name to continue: acme
```

Order: opt-in resources in members → security services → policies not
kept → consolidate kept policies → disable automation roles →
`protectAnvilRoles` → CI roles → state. If it stops part-way, re-running
`anvil org destroy` resumes from state; accounts whose role is already disabled
or gone are skipped.

**Why disabled, not deleted.** AWS only deletes a role once every policy is
detached, and `protectAnvilRoles` lets only `anvil-org-automation` touch
`anvil-*` roles, so the role would have to delete itself after removing its
own permissions, which fails. Instead it disables itself, in this order:

1. trust policy set to deny everyone (nobody, including Anvil, can assume it again);
2. inline policies deleted;
3. managed policies detached, as the very last call.

The inert role stays, tagged `anvil:disabled=true` with a description saying
why. Once `protectAnvilRoles` is removed (from management, which SCPs don't
bind), a person can delete it by hand or leave it.

### Scenarios

```
Moving to Control Tower
  anvil org destroy → Anvil's roles, services and opt-in guardrails gone;
  Control Tower enrols the accounts without fighting Anvil's SCPs;
  logs and break glass stay throughout the move.

A developer key leaks a month after destroy
  can't make a bucket public, leave the org, stop the trail or find a root
  password; everything it does is still logged to Log Archive.

Identity Center breaks after destroy
  break glass still works; its use still alerts the topic's listeners.
```

### Docs must say

- Exactly the two tables above: what stays, what goes, and why.
- Findings history for GuardDuty, Security Hub, Macie and Detective is
  deleted when they're disabled; export it first if it's needed.
- What's left is unmanaged: nothing re-applies it if someone changes it.
  The kept policies say so in their descriptions.
- To remove a single thing, delete it from config and deploy; `destroy` is
  for removing Anvil from the org.
- Re-adopting later: a new Org project bootstraps each account once
  through break glass (labelled alert), then manages it as new.
