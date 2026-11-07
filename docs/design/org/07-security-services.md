# Org — Security services

Part of the [Org design](README.md).

## Security services

**Every org-wide security service is explicit opt-in, except CloudTrail.**
Some users want a proper account structure first, without the cost and
noise of security tooling yet. Nothing else is enabled unless named in
`security`. CloudTrail is on by default because break-glass alerts need it
and the basic org trail costs a few dollars a month (see CloudTrail below):

```ts
security: {
  // cloudtrail: on by default
  config:     { enable: true },
},
```

| Key | What it does | Cost model |
|---|---|---|
| `cloudtrail` | **On by default.** Org trail: every API call in every account → Log Archive | First copy of management events free; S3 storage and PUTs |
| `accessAnalyzer` | Finds resources shared outside the org | External-access analyzer free |
| `guardduty` | Threat detection (compromised keys, crypto-mining, malware) | Usage-based, 30-day trial |
| `config` | Resource configuration history → aggregator in Audit | Per configuration item recorded, per rule evaluation |
| `securityHubCspm` | Security Hub CSPM: posture checks against standards, findings in Audit | Per check; needs `config`; 30-day trial |
| `securityHub` | Security Hub: correlates CSPM, Inspector and GuardDuty findings into exposures, risk ranking, workflows. Turns on `securityHubCspm` | Per resource unit; consolidates CSPM/Inspector/GuardDuty billing; 30-day trial |
| `inspector` | Vulnerability scanning (EC2, ECR, Lambda) | Per resource scanned |
| `macie` | Sensitive data discovery in S3 | Per bucket and GB scanned |
| `detective` | Investigation of GuardDuty findings | Per GB ingested |

Docs must say:
- AWS SRA treats CloudTrail, Config, GuardDuty, Security Hub CSPM and Access
  Analyzer as the core baseline for Audit and Log Archive. Anvil turns on
  CloudTrail and leaves the rest off so the user chooses when to start
  paying; the docs recommend turning them on before production workloads
  arrive.
- **Control Tower parity** is `cloudtrail` + `config`: what Control Tower
  enables by default. Control Tower itself is free; the cost is Config,
  CloudTrail and S3. AWS's own examples: 10 accounts with 5 resources each in
  one region ≈ $3.75/month; 25 accounts in 3 regions ≈ $60.63/month; Config
  on ephemeral workloads can add $372 per account per region per month.
  Config cost scales with resource churn, not account count.
- With only the default CloudTrail, Log Archive holds the trail and Audit
  holds the break-glass event bus and CloudTrail delegated admin.
- `protectSecurityServices` protects whichever services are enabled.
- `guardduty` is opt-in like every other key here: off unless
  `security.guardduty: { enable: true }`.
- If `cloudtrail` is disabled, `anvil bootstrap` keeps the per-stage trail.

### CloudTrail

**On by default, as a basic org trail.** Turning it off needs a reason;
even then a **minimal org trail** stays, so break-glass alerts and every
watched change keep working (EventBridge only receives CloudTrail events
from accounts a logging trail covers).

| Level | When | What | ≈ Cost |
|---|---|---|---|
| Org trail | Default | One multi-region org trail, management events, every account and region → Log Archive (SSE-S3, log file validation, versioning, deny-delete, 1y retention, 10y access logs) | Small org $1–3/month, medium $5–15, large $25–60 |
| Minimal org trail | `cloudtrail: { disable: true, reason }` | One multi-region org trail, every account, with advanced event selectors recording only what the watches need (`iam`, `sts`, `signin`, `organizations`, `cloudtrail`, `kms`, `sns`, `events`, S3 bucket configuration, and denied calls) → Log Archive. Not an audit trail: members otherwise have only AWS's 90-day Event History | ~$0 (a small slice of free management events) |

- Costs: management events are free as the first copy in each account; the
  cost is S3 storage of gzipped logs and the PUTs CloudTrail makes delivering
  files (~every 5 min per account and region with activity). Rough:

  | Org | Events/month | Storage after 1y | S3 PUTs/month | ≈ Monthly |
  |---|---|---|---|---|
  | Small: 5–10 accounts, 2–3 active regions | ~1–2M | ~2–4 GB | ~150–250k | $1–3 |
  | Medium: 30–50 accounts | ~10M | ~20–40 GB | ~1M | $5–15 |
  | Large: 200+ accounts, busy | 50M+ | 100+ GB | 4M+ | $25–60 |

- It also saves money: with the org trail on, `anvil bootstrap` skips each
  App's per-stage trail, which would be a paid second copy ($2 per 100k
  events).
- **The minimal org trail can't be removed.** It's part of break glass
  and watched changes (like `protectAccessRole`): created whenever the full
  org trail is disabled, replaced by it when it's on. Protected like the
  full trail.
- *To verify on AWS:* events excluded by advanced selectors don't reach
  EventBridge, and the included ones (including read-only `sts`/`signin`)
  do.
- Opt-in extras, unchanged: data events ($0.10 per 100k), Insights ($0.35
  per 100k analysed), CloudWatch Logs delivery (~$0.50/GB), `logArchive.kms`
  (~$1/month), `logArchive.objectLock`.
- Delegated admin defaults to Audit, as for every service.

```ts
security: {}                                                  // org trail on
security: { cloudtrail: { delegatedAdmin: 'secops' } }        // org trail on, other admin
security: { cloudtrail: { disable: true, reason: 'evaluating; no workloads yet' } }
                                                              // minimal org trail
```

```
⚠ security.cloudtrail is disabled (evaluating; no workloads yet).
  Member accounts have no audit trail: only AWS's 90-day Event History per account and region.
  Break-glass alerts and watched changes still work (minimal org trail, can't be removed).
  To fix: remove security.cloudtrail.disable
```

### Log Archive and the org trail

Always created (the default org trail writes to it).

**Fixed defaults** (AWS SRA and Control Tower agree):

| | Setting |
|---|---|
| Org trail | From management, multi-region, read + write management events, log file validation on, no data events by default |
| Log bucket | In Log Archive: versioning, Block Public Access, TLS-only, Bucket Owner Enforced |
| Writers | Only the CloudTrail and Config service principals, for this org (`aws:SourceOrgID` in the bucket policy) |
| Readers | `security`-preset users and Audit, read-only; nobody works in Log Archive directly (SRA) |
| Access logs | Second bucket in Log Archive recording every read of the log bucket |
| Config | Delivers to the same bucket, own prefix |
| Detection | Watched changes alert on any policy, lifecycle, encryption or lock change (always on). The trail lives in management, where SCPs don't apply: Org identities are denied stopping or deleting it, and anyone else doing so alerts (see Protecting management resources) |

**Retention and immutability.** Default (Control Tower style): versioning,
bucket policy denying deletes and version deletes, log file validation, and
lifecycle expiry after retention. Object Lock is opt-in:

```ts
security: {
  // cloudtrail on by default
  logArchive: {
    retention: '1y',            // default; logs expire after this
    accessLogRetention: '10y',  // default
    objectLock: { enable: true }, // opt-in: governance mode for the retention period
  },
},
```

```
+ log bucket   log-archive/anvil-org-logs-o-ab12cd
    retention  1y logs, 10y access logs
    lock       Object Lock governance, 1y
```

- Without `objectLock`, someone with enough access (management break glass)
  could change the bucket policy and delete logs; detection catches it,
  nothing stops it. Log file validation still proves tampering.
- With `objectLock`, objects can't be deleted or overwritten during
  retention except with `s3:BypassGovernanceRetention`, which the Log Archive
  lockdown SCP denies in member accounts; in an emergency the management
  root user detaches that SCP first (SCP lockout playbook, see Break glass),
  then break glass can bypass retention.
- Retention can grow later; shortening only affects new objects (locked ones
  keep their date). Maximum 15y, as Control Tower.
- Compliance-mode Object Lock (irreversible, even for root) isn't offered in
  v1.

**Encryption.** SSE-S3 by default (Control Tower's default); a customer
managed KMS key is opt-in:

```ts
logArchive: { kms: { enable: true } },
```

```
+ log bucket   log-archive/anvil-org-logs-o-ab12cd
    encryption SSE-KMS (key in audit, Bucket Keys on)
```

- KMS adds a second layer (S3 read **and** decrypt in the key policy) and
  logs every decrypt. ~$1/month per key plus requests; Bucket Keys cut
  request cost ~99%.
- The key lives in **Audit**: Log Archive holds the data, Audit holds the
  key, so reading logs needs both (separation of duties).
- Anvil writes the key policy: CloudTrail and Config (this org) can encrypt,
  `security`-preset users and Audit can decrypt.
- Switching either way only affects new objects; S3 reads both. A key can't
  be deleted while logs encrypted with it still exist; preview enforces it.
- Docs: SRA recommends SSE-KMS for security logs.

**Trail regions: all, always.** One multi-region org trail covers every
region, including ones `allowedRegions` blocks. No setting.

```
+ org trail   multi-region (all regions) → log-archive
              includes regions blocked by allowedRegions: denied attempts are recorded
```

- The first copy of management events is free in every region, so blocked
  regions cost almost nothing (little to store).
- Denied calls are logged (`errorCode: AccessDenied`) in the region they hit:
  attempts with stolen keys in blocked regions are exactly the signal wanted.
- Matches SRA and Control Tower; Security Lake requires a multi-region trail.
- Docs explain why this differs from the security services, which follow
  `allowedRegions`: those charge per region and can't watch what the SCP
  already stops being created; the trail is free per region and records what
  the SCP stopped.

### GuardDuty protection plans

`guardduty: { enable: true }` turns on **foundational threat detection** only
(CloudTrail management events, VPC Flow Logs, DNS logs). Every protection
plan is opt-in; Anvil sets org-wide auto-enable for each explicitly.

```ts
guardduty: {
  enable: true,
  plans: { s3: true, lambda: true },
},
```

```
+ guardduty   admin audit · all accounts · ap-southeast-2, us-east-1
    foundational   on
    plans          s3, lambda   (rds, eks, runtime, malwareEc2, malwareS3 off)
```

| Plan | Watches | Cost (US East) | Fits |
|---|---|---|---|
| `s3` | S3 data events (unusual reads, exfiltration, bad IPs) | $0.80 per million S3 events | Every App uses S3. CloudFront fetching site assets from S3 generates events, so busy sites cost more |
| `lambda` | Lambda network traffic (crypto-mining, malicious hosts) | $1.00 per GB of Lambda flow logs | Lambda-heavy Anvil apps; usually cheap |
| `rds` | Suspicious database logins | $1.00 per vCPU per month | RDS / Aurora |
| `eks` | Kubernetes audit logs | $1.60 per million events | EKS |
| `runtime` | Agent on EKS, ECS Fargate, EC2 | $1.50 per vCPU per month | Containers, EC2 |
| `malwareEc2` | Scans EBS when a finding suggests malware | $0.03 per GB scanned | EC2; free unless triggered |
| `malwareS3` | Scans new uploads to chosen buckets | $0.09 per GB + $0.215 per 1,000 objects | Buckets that take user uploads |

`malwareS3` is per bucket in AWS: the Org flag allows it org-wide, and the
buckets are chosen where they're declared (an App's `Bucket`; *to design
with the Bucket component*).

Docs recommend `s3` and `lambda` for typical Anvil apps, note that the
console's defaults turn several plans on, and that `guardduty` alone is less
than that. *Later:* dashboard hints for plans that fit what's deployed.

### Config recording

`config` takes a `recording` mode. Continuous is the default.

```ts
config: { enable: true },                          // continuous
config: { enable: true, recording: 'periodic' },   // high-churn accounts
```

| Mode | Records | Cost (US East) |
|---|---|---|
| `continuous` | Every change as it happens | $0.003 per change |
| `periodic` | One daily snapshot per resource that changed that day | $0.012 per resource per day |

Docs must say:
- Continuous is what AWS recommends for security and audit: every change is
  captured, not one snapshot a day.
- For steady infrastructure continuous is cheaper (worked example: ~$0.90
  vs up to ~$3.60/month). Periodic only wins when resources change many times a day,
  e.g. ephemeral workloads that churn constantly.
- Check Config's cost in Cost Explorer after a month before switching.

### Regions

**The management account is enrolled too.** Org-wide auto-enable covers
members; the delegated admin also adds management as a member (GuardDuty,
Security Hub CSPM, Inspector, Macie, Detective) and Config records there,
because management holds break glass and policy control.

Security services are regional. They follow `allowedRegions`:

| `allowedRegions` | Regions each enabled service runs in |
|---|---|
| Set | Those regions + us-east-1 |
| Not set | Every region enabled in the account (AWS's guidance) |

```ts
guardrails: { allowedRegions: { regions: ['ap-southeast-2'] } },
security: { guardduty: { enable: true }, config: { enable: true } },
```

```
+ guardduty   ap-southeast-2, us-east-1
+ config      ap-southeast-2, us-east-1 (global resources recorded here)
```

- AWS recommends GuardDuty in every region, including unused ones, because
  stolen keys get used in regions nobody watches. When the region SCP blocks
  those regions, nothing can be launched there, so the risk is covered.
- us-east-1 is always added: global service events (IAM, STS) and Anvil's
  edge resources land there. Config records global resources (IAM) in
  us-east-1 only, so they aren't counted twice.
- Turning `allowedRegions` off widens coverage on the next deploy, the safe
  direction.

**Cost per extra region.** An enabled region with nothing deployed still has
a default VPC (~20 resources) and a trickle of AWS-generated events. Rough
estimate per account per region per month:

| Key | Unused region | Working |
|---|---|---|
| `guardduty` | ~$0.01–0.05 | A few thousand events × $4/M; no flow-log traffic |
| `config` | ~$0.06 once, then ~$0 | ~20 default-VPC resources recorded once × $0.003 |
| `securityHubCspm` | ~$0.10–0.50 | A few hundred checks on default-VPC resources |
| `securityHub` | ~$0 | Default-VPC resources aren't resource units |
| `inspector`, `macie`, `detective` | ~$0 | Nothing to scan or ingest |
| **Total** | **~$0.10–0.60** | |

Every region (A) vs `allowedRegions` (B) for 17 enabled regions with 2
used: 15 extra regions × ~$0.10–0.60 ≈ $1.50–9 per account per month, or
~$15–90 for 10 accounts. us-east-1 under B adds the same small amount when
unused, plus GuardDuty on global events.

Docs must say: prices differ by region (estimates use US East list
prices; most other regions are higher); check each region's pricing page.

### Delegated admin

A delegated admin is the member account the management account hands one
service's org-wide control to: it enables the service in every account
(new ones automatically), sees all accounts' findings, and is the only place
settings can change. One admin per service per org; the management account
can always revoke it.

**Default: Audit for every service** (AWS SRA). Data stays where SRA puts it:

| Key | Delegated admin | Data lands in |
|---|---|---|
| `cloudtrail` | Audit (the trail itself is always owned by management and Anvil creates it from there; delegation lets the security team query it from Audit). AWS lets the delegated admin update and delete org trails, so `protectSecurityServices` in Audit denies `DeleteTrail`, `StopLogging`, `UpdateTrail` and event-selector changes on the org trail, and changes to the break-glass bus policy (`events:PutPermission`, `RemovePermission`); Anvil only manages it from management | Log Archive bucket |
| `config` | Audit (aggregator) | Log Archive bucket (history), Audit (aggregated view) |
| `accessAnalyzer`, `guardduty`, `securityHubCspm`, `securityHub`, `inspector`, `macie`, `detective` | Audit | Audit |

```
+ delegated admin   audit (222222222222): cloudtrail, config, guardduty, securityHubCspm
```

**Override, strongly advised against.** Another account in the Security OU
can be named per service, for orgs with a separate security operations
account. It gets the same Security OU lockdown as Audit.

```ts
security: {
  guardduty: { enable: true, delegatedAdmin: 'secops' },
},
```

```
⚠ guardduty.delegatedAdmin: secops instead of audit. AWS SRA keeps every
  security service in Audit so findings are in one place.
```

| Target | Allowed | Why |
|---|---|---|
| Audit | ✅ default | AWS SRA |
| Other Security OU account | ⚠ warns | Same lockdown; findings split across accounts |
| Log Archive | ✗ error | Log Archive holds logs only; nobody works in it day to day |
| Management | ✗ error | AWS advises against: most powerful account, keep it empty |
| Any account outside the Security OU | ✗ error | No Security OU lockdown; workload admins could change security settings |

Every key matches AWS SRA, which makes the Security Tooling (Audit) account
the delegated admin for all of them, CloudTrail included, and says to "limit
access to the management account and use the delegated administrator
functionality where it is available". No service *requires* a delegated
admin (without one, the management account administers it); delegating is
the best practice.

**Keep GuardDuty, Detective and Security Hub on the same admin.** Detective's
pivots from GuardDuty and Security Hub CSPM findings only work when one
account is admin for both sides (AWS: "if you do not align administrator
accounts between Detective and Security Hub CSPM, navigating from findings
into Detective will not work"). Preview warns when they differ:

```
⚠ security.detective.delegatedAdmin: secops, but securityHubCspm is audit;
  navigating from Security Hub findings into Detective won't work
```

`securityHub` and `securityHubCspm` are one AWS service with one admin:
`securityHubCspm` takes `securityHub`'s admin when both are on, and preview
errors on an explicit mismatch. CloudTrail allows up to three delegated
admins; Anvil registers only Audit (or the override).

Docs must say: keep everything in Audit unless there's a clear reason
(e.g. a separate security operations team with its own account), and
`securityHub` works best when its sources share one admin.

Full example: a `secops` account in the Security OU takes GuardDuty and
Detective; everything else stays in Audit.

```ts
export default new Org({
  tier: 'minimum',
  email: 'aws+{account}@acme.com',
  stages: { prod: { ou: 'Prod' }, dev: { ou: 'NonProd' } },
  workloads: { payments: { stages: ['prod', 'dev'] } },

  accounts: {
    secops: { ou: 'Security', tags: { team: 'security-operations' } },
  },

  guardrails: { allowedRegions: { regions: ['ap-southeast-2'] } },

  security: {
    config:          { enable: true },
    accessAnalyzer:  { enable: true },
    guardduty:       { enable: true, delegatedAdmin: 'secops' },
    detective:       { enable: true, delegatedAdmin: 'secops' }, // investigates GuardDuty findings, keep together
    securityHubCspm: { enable: true, standards: ['pci'] },
  },
});
```

```
  Security services     regions: ap-southeast-2, us-east-1
  + cloudtrail          admin audit (222222222222)   → log-archive bucket
  + config              admin audit (222222222222)   → log-archive bucket, aggregator in audit
  + accessAnalyzer      admin audit (222222222222)
  + securityHubCspm     admin audit (222222222222)   fsbp, cis, pci · consolidated
  + guardduty           admin secops (777777777777)
  + detective           admin secops (777777777777)

  ⚠ guardduty.delegatedAdmin: secops instead of audit. AWS SRA keeps every
    security service in Audit so findings are in one place.
  ⚠ security.detective.delegatedAdmin: secops, but securityHubCspm is audit;
    navigating from Security Hub findings into Detective won't work
```

```
Root
├── Security
│   ├── log-archive (111111111111)   CloudTrail + Config logs
│   ├── audit       (222222222222)   admin: cloudtrail, config, accessAnalyzer, securityHubCspm
│   └── secops      (777777777777)   admin: guardduty, detective
├── Workloads
│   ├── Prod        payments-prod
│   └── NonProd     payments-dev
├── Sandbox
└── Suspended
```

Errors:

```
✗ security.guardduty.delegatedAdmin: log-archive holds logs only and can't be a delegated admin
✗ security.guardduty.delegatedAdmin: payments-prod is in Workloads/Prod; delegated admins
  must be in the Security OU
```

This example shows why the override is advised against: splitting GuardDuty
and Detective from Security Hub breaks the Detective pivots. Moving
`securityHubCspm` to `secops` too would fix that, at the cost of findings
living outside Audit.

*To verify on AWS:* Security Hub in Audit still receives GuardDuty findings
when GuardDuty's admin is `secops` (GuardDuty sends findings to Security Hub
inside each member account, which then roll up to the Security Hub admin).

### Estimating cost: 1 account, 1 region

Costs are per account, per region, so a user can multiply. US East
(N. Virginia) list prices, checked 2026-10-28; docs must link the pricing
pages and say prices change.

| Key | Formula | Notes |
|---|---|---|
| `cloudtrail` | Free for the first copy of management events; S3 storage for the logs | Data events $0.10 per 100k (not enabled by Anvil). Extra copies $2.00 per 100k |
| `accessAnalyzer` | $0 | External access only. Unused access ($0.20 per role/user) and internal access ($9 per resource) not enabled by Anvil |
| `guardduty` | $4.00 per million management events + $1.00 per GB of VPC Flow/DNS logs (first 500 GB) | Protection plans extra: S3 data events $0.80/M, Lambda $1.00/GB flow logs, RDS $1.00/vCPU, runtime $1.50/vCPU |
| `config` | Continuous: $0.003 per change recorded. Periodic: $0.012 per resource per day | Own rules: $0.001 per evaluation. For steady infra, continuous is far cheaper |
| `securityHubCspm` | $0.0010 per check (first 100k) + $0.00003 per finding after 10k free | AWS's own example for 1 account, 1 region: $0.25/month |
| `securityHub` | $3.75 per resource unit (EC2 = 1, Lambda = 1/12, ECR image = 1/18, IAM role/user = 1/125) + threat analytics $4.00 per million events + $0.55 per GB security data | Replaces the separate CSPM, Inspector and GuardDuty bills, not added on top |
| `inspector` | $1.258 per EC2 instance-month; $0.30 per Lambda function-month (+$0.60 code scanning); $0.09 per new ECR image | |
| `macie` | $0.10 per bucket + $0.01 per 100k objects + $1 per GB inspected (1 GB free) | Most of the cost is GB inspected |
| `detective` | $2.00 per GB ingested (first 1,000 GB) | Ingests CloudTrail, VPC Flow Logs, GuardDuty findings |

All except Access Analyzer and CloudTrail have a free trial: 30 days, or 15
days for Inspector, per account per region.

**Worked example: a small serverless app account.** About 100 resources,
20 Lambda functions, 10 S3 buckets, no EC2, ~500k API calls a month,
~300 configuration changes a month, ~1 GB of VPC Flow/DNS logs:

| Key | Estimate | Working |
|---|---|---|
| `cloudtrail` | ~$0.01 | Free events; under 1 GB of S3 |
| `accessAnalyzer` | $0 | |
| `guardduty` | ~$3 | 0.5M × $4/M + 1 GB × $1 |
| `config` | ~$0.90 | 300 × $0.003 (periodic records only resources that changed that day: at most ~300 × $0.012 ≈ $3.60) |
| `securityHubCspm` | ~$1–10 | Depends on controls × resources × evaluations |
| `securityHub` | ~$7 + events | 20 Lambda ≈ 1.7 units + 30 roles ≈ 0.24 units, × $3.75; replaces Inspector's ~$6 |
| `inspector` | ~$6 | 20 × $0.30 |
| `macie` | ~$5 | 10 × $0.10 + ~4 GB inspected |
| `detective` | ~$4 | ~2 GB ingested |

| Set | Estimate per month |
|---|---|
| Control Tower parity (`cloudtrail`, `config`) | ~$1 |
| SRA core (+ `accessAnalyzer`, `guardduty`, `securityHubCspm`) | ~$5–15 |
| Everything | ~$20–35 |

The worked example's usage figures are assumptions, not measurements. Docs
must say they're illustrative and point users to Cost Explorer after the
free trials, which show real usage.

### Security Hub and Security Hub CSPM

AWS sells two complementary products, and Anvil names them exactly as AWS
does. Both are opt-in.

```ts
security: { config: { enable: true }, securityHubCspm: { enable: true } },  // posture checks only
security: {                                                                 // full layer
  config: { enable: true }, securityHub: { enable: true },
  inspector: { enable: true }, guardduty: { enable: true },
},
```

```
+ securityHubCspm   all accounts → audit   (enabled by securityHub)
+ securityHub       all accounts → audit
```

- `securityHubCspm` alone is enough when the goal is finding
  misconfigurations (AWS's guidance), and it's what compliance standards and
  `anvil compliance dashboard` line up with.
- `securityHub` turns on `securityHubCspm` automatically, as AWS does, and
  preview says so. Its exposure findings need `inspector` and `guardduty` too;
  preview warns when they're off:

  ```
  ⚠ securityHub: exposure findings need inspector and guardduty (both off)
  ```

- Docs must say: AWS recommends enabling both. CSPM is per check, Security Hub
  per resource unit; with lots of EC2, Security Hub costs more (50 instances
  ≈ $190/month vs Inspector $63 + CSPM checks).

**Standards.** Enabling `securityHubCspm` (directly or through
`securityHub`) turns on **FSBP and CIS** automatically, AWS's own console
default. Every other standard is explicit opt-in and adds to them:

```ts
securityHubCspm: { enable: true },                                     // fsbp + cis
securityHubCspm: { enable: true, standards: ['pci', 'tagging'] },  // fsbp + cis + pci + tagging
```

```
+ securityHubCspm   all accounts → audit
    standards       fsbp, cis (latest), pci, tagging
    findings        consolidated
```

| Name | Standard |
|---|---|
| `fsbp` | AWS Foundational Security Best Practices (automatic) |
| `cis` | CIS AWS Foundations Benchmark (automatic) |
| `tagging` | AWS Resource Tagging |
| `ai` | AI Security Best Practices |
| `pci` | PCI DSS |
| `nist-800-53` | NIST SP 800-53 Rev 5 |
| `nist-800-171` | NIST SP 800-171 Rev 2 |
| `cri` | CRI Profile v2.2 |

- Names are typed; Anvil enables the latest version of each.
- **Consolidated control findings** are always on: one finding per control,
  not one per standard. A control shared by several standards is billed once.
- FSBP and CIS can't be dropped in config. Turning one off is a weakening, so
  it goes in `run` with a `reason`, like every other guardrail.
- Cost: FSBP + CIS ≈ $0.25–6 per account per region per month (AWS's own
  1-account example: $0.25). Each extra standard mostly overlaps FSBP, so
  adds well under a dollar to a couple of dollars. Docs point to the CSPM
  console's Usage page during the trial for real projected cost.
- CSPM has no SOC 2 or ISO 27001 standard. Those come from Prowler (see
  Org-wide compliance).

### Org-wide compliance (Prowler)

Opt-in. One central Prowler scanner in **Audit** scans every member account,
whole account (not just App resources), into one results bucket in Audit,
which the org dashboard reads. Covers SOC 2 / ISO 27001 and the other Prowler
frameworks Security Hub CSPM doesn't have.

```ts
compliance: {
  enable: true,
  frameworks: ['soc2', 'iso27001', 'cis'],
  schedule: 'daily',
},
```

```
+ compliance   scanner in audit (222222222222) · daily · soc2, iso27001, cis
    scan role  anvil-compliance-scan in 10 member accounts (read-only, trusts audit scanner only)
    results    audit/anvil-compliance-results
```

- Same scanner as App `compliance` (CodeBuild, upstream Prowler image pinned
  by digest, no VPC), same framework names, schedule presets and retention,
  same results-bucket hardening. See docs/design/compliance-scanning.md.
- Each member account gets only `anvil-compliance-scan`: `SecurityAudit` +
  `ViewOnlyAccess`, explicitly denied data reads (object contents, logs,
  function code, secrets), trusting only Audit's scanner role. Protected by
  SCP like the break-glass role.
- **Regions follow `allowedRegions`**, like the security services: only the
  allowed regions plus us-east-1 (global services and Anvil's edge
  resources). Denied regions aren't scanned; the region SCP stops anything
  being created there. Passed to Prowler as `--region`. Without
  `allowedRegions`, every enabled region is scanned.

  ```
  + compliance   scanner in audit · daily · regions: ap-southeast-2, us-east-1
  ```

- Scans run in batches up to CodeBuild's concurrency limit, so large orgs
  take longer per cycle.
- Cost: ~$6 per account per region per month daily (20 min scan), ~$1.40
  weekly. ~$60/month for 10 accounts in one region, daily.
- **Overlap with App `compliance` is the user's call.** No dedupe logic: if
  both are on, the same resources are scanned twice. Docs say that with
  org-wide compliance on, App `compliance` is usually unnecessary and can be
  removed.
