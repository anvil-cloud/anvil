# Org — Notifications

Part of the [Org design](README.md).

## Notifications

**Always created; who listens is configured in the UI, never in code.**
There's no config key. Org creates one topic and wires every source to it
on every deploy. Subscriptions (who gets notified, and which messages) live
only in AWS, so no listener email addresses sit in config, Pulumi state or
git. (Account emails and `identityCenter.management` users are the only
personal data in config, because AWS needs them; docs recommend role
mailboxes and generic names.)

```
code / Org (always)                  AWS only (managed in a UI)
───────────────────────────          ──────────────────────────────────
topic audit/anvil-org-notifications  subscriptions + filter rules:
  KMS-encrypted (own key)              secops@acme.com      critical only
  sources wired:                       platform@acme.com    source = destroyUntagged
    break-glass       critical         Slack #sec-alerts    critical, warning
    destroyUntagged   warning/info
```

### Sources and messages

Every message has the same attributes, so subscriptions can filter on them
(SNS filter policies):

| Attribute | Values |
|---|---|
| `severity` | `critical`, `warning`, `info` |
| `source` | `break-glass`, `destroyUntagged`, `watch` (watched changes), plus each enabled service (below) |
| `account` | account name, e.g. `payments-prod` |
| `ou` | OU path, e.g. `Workloads/Prod` |

| Source | Severity |
|---|---|
| Break-glass use, management root activity | `critical` |
| destroyUntagged: resources marked for deletion | `warning` |
| destroyUntagged: resources deleted | `info` |
| Subscription added or removed | `warning` |

**Enabled services add their own sources.** When a security service is on,
Org wires its findings to the topic on the same deploy; turning it off
removes the wiring. Listeners pick them up through filter rules in the UI.

| Service | `source` | `critical` | `warning` |
|---|---|---|---|
| `guardduty` | `guardduty` | Severity ≥ 7 (High/Critical) | 4–6.9 (Medium) |
| `securityHub` / `securityHubCspm` | `securityHub` | New CRITICAL findings | New HIGH findings |
| `inspector` | `inspector` | CRITICAL | HIGH |
| `macie` | `macie` | High | Medium |
| `compliance` (Prowler) | `compliance` | | Scan finished with new failures (`info` when clean) |

- Wired in Audit, where the delegated admin receives findings for every
  account. Regional services (GuardDuty, Inspector, Macie) get a rule in
  each allowed region (and us-east-1) forwarding to the Audit home-region
  bus; Security Hub uses its aggregation region.
- Only new findings and severity increases notify, not updates, so
  listeners aren't flooded.

### Watched changes

**Always on, with opt-out per area in config.** EventBridge rules watch
everything that protects the org and send to the notification topic.
Severity depends on who made the change and whether it came through Anvil:

| Who | Through the Anvil CLI or provider | Severity |
|---|---|---|
| Deployer or `anvil-org-automation` | Yes | `info` |
| Deployer | No (console, raw CLI) | `warning` |
| Anyone else | Either | `critical` |

Attempts a `protect*` rule blocked are `warning`. "Through Anvil" is the
request's user agent (`anvil/<version>`, recorded by CloudTrail on every
event). A deployer could fake it, but deployers are already trusted and the
management denies apply regardless; it's a "worked outside the process"
signal, not a security boundary.

| Key | Watches | Where the rule runs |
|---|---|---|
| `guardrails` | SCP/RCP detached, edited or deleted; policy type disabled; account moved between OUs, outside an Anvil deploy | Management, us-east-1 (Organizations is global) |
| `buckets` | Log Archive, its access-log bucket and the Org state bucket: policy, lifecycle, versioning, encryption, Object Lock, Block Public Access, delete | Each bucket's home region |
| `cloudtrail` | `StopLogging`, `DeleteTrail`, `UpdateTrail`, event selector changes. **Any change that weakens the trail is `critical` whoever makes it** (bucket or KMS key changed, multi-region off, management events dropped or reduced, selectors excluding events); changes that add coverage are `info` | Management and Audit (the delegated admin), home region |
| `identityCenter` | Assignments or permission sets created or changed outside Anvil | Management, Identity Center region |
| `blockedAttempts` | Actions denied by `protect*` rules (disabling a security service, changing an `anvil-*` role, making a bucket public), grouped per account per hour | Each member account, allowed regions + us-east-1 → Audit |
| `keys` | Disabling or scheduling deletion of Anvil's KMS keys (state, Log Archive, notifications) | Key's account and region |

Break-glass watching (use, creation, changes; see Break glass) isn't in
this list and can't be turned off.

Opting out is a weakening, so it needs a reason (like `cloudtrail`):

```ts
watch: {
  blockedAttempts: { disable: true, reason: 'too noisy while migrating legacy apps (TICKET-412)' },
},
```

```
⚠ watch.blockedAttempts disabled: "too noisy while migrating legacy apps (TICKET-412)"
  Blocked attempts against protect* rules won't notify.
  To fix: remove watch.blockedAttempts.disable
```

Scenarios:

```
LegacyAdmin detaches anvil-baseline-scp-1 from Workloads in the console
  → critical  "SCP anvil-baseline-scp-1 detached from Workloads by role/LegacyAdmin,
               not by an Anvil deploy"

role/app-deploy in payments-prod tries guardduty:DeleteDetector, iam:DeleteRole anvil-compliance-scan
  → warning   "2 actions blocked by protectSecurityServices / protectAnvilRoles in payments-prod"

jsmith assigns AdministratorAccess on audit to themselves in the console
  → critical  "Assignment created outside Anvil: jsmith → AdministratorAccess on audit"
```

- Cost: rules are free; cross-account forwarding ~$1 per million events;
  these are rare, so effectively $0.
- Rules in members are protected by SCP (`protectSecurityServices`); rules
  in management (`anvil-watch-*`) by the denies on Org identities and the
  paired watcher (see Management account stays empty → Protecting
  management resources).
- Replaces the detection promises that relied on Config, Access Analyzer
  or the dashboard (Log Archive, state bucket lifecycle).

### Listeners and rules (UI)

- **Anvil dashboard** (`anvil org dashboard` → Notifications): add or remove
  email and Slack/Teams listeners and their filter rules. The CLI behind the
  dashboard calls SNS (and Amazon Q Developer in chat applications) with
  the signed-in user's own credentials; the browser never sees them, and
  nothing goes through code or state.
- **AWS console**, works from day one: SNS → Subscribe (email confirms by
  link), or a channel in Amazon Q Developer in chat applications.
- **Email listeners are confirmed authenticated.** SNS emails carry an
  unsubscribe link that works for anyone holding the email, unless the
  subscription was confirmed with `AuthenticateOnUnsubscribe=true`. The
  dashboard does that: add the email, then paste the confirmation link into
  the dashboard instead of clicking it. Unsubscribing then needs AWS
  credentials. (Console: "Confirm subscription" and paste the link.) No
  preview warning for link-confirmed subscriptions; it's rare and the
  subscription-change alert still fires.
- Slack and Teams workspaces are authorized once in the console (OAuth,
  manual like Identity Center); only workspace and channel IDs are stored,
  in AWS.
- No SMS (no phone numbers anywhere) and no webhook URLs (secrets).
- Org never manages or prunes subscriptions; they aren't in state.

### Protecting it

Subscriptions aren't PR-reviewed, so someone could quietly unsubscribe the
security team before using break glass:

- Only `securityOperator` in Audit gets `sns:Subscribe`, `sns:Unsubscribe`
  and `sns:SetSubscriptionAttributes` on the topic (dashboard users sign in
  with it).
- `protectSecurityServices` denies deleting the topic, changing its
  policy, encryption or key, and changing the Audit bus rules, to everyone
  except `anvil-org-automation`.
- **Every subscription change notifies the remaining listeners**
  (`warning`): *"secops@acme.com was unsubscribed by sam (securityOperator)"*.

### Preview

Counts only, never addresses:

```
  notifications   audit/anvil-org-notifications · 3 confirmed (2 email, 1 slack)
                  critical → 2 listeners · warning → 3 · info → 1
```

```
⚠ notifications: no confirmed listeners for critical messages; break-glass
  alerts go nowhere.
  To fix: anvil org dashboard → Notifications → Add listener,
          or SNS console (audit, ap-southeast-2) → anvil-org-notifications → Create subscription
```

- Cost: ~$1/month for the KMS key (EventBridge can't publish to a topic
  encrypted with AWS's default SNS key). SNS email and chat delivery are
  effectively free at this volume.
- *Later:* AWS User Notifications (org mode, GA 2025) could sit on top of
  the same topic for history and mobile push, once custom-event support and
  Pulumi coverage are verified.
