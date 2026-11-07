# Org — Guardrails

Part of the [Org design](README.md).

## Guardrails

| Tool | Controls | Example |
|---|---|---|
| SCP | What principals **in** your accounts can do | Nobody can stop CloudTrail |
| RCP | What can be done **to** your resources, including by outsiders | S3 unreachable from outside the org |
| Declarative policy | Service settings org-wide | EC2 must use IMDSv2 |
| Centralized root access | Root credentials on member accounts | No member root password exists |
| AI opt-out policy | AWS using your data for AI training | Org-wide switch |
| Tag policy | Required tag keys and allowed values, checked by IaC | Every RDS instance has `CostCenter` |

### Baseline control

**Every baseline rule is on by default. Config can only make guardrails
stricter; anything that weakens or customises a rule lives in `run(ctx)`.**
A reviewer knows where to look for risk, and preview lists every weakening.

Rejected: *always on* (the only workaround is moving an account out of its
OU, which drops every rule there, not just the one in the way), *opt in*
(against secure defaults), and weakening in config (`enabled` / `except` on
the `guardrails` block, an earlier draft).

#### Config: strengthening only

`guardrails` in config turns on opt-in rules and sets their parameters.
Parameterised rules exist where Anvil can't pick a safe default for the user
(regions, instance types, allowed services, required tags):

```ts
guardrails: {
  allowedRegions: { regions: ['ap-southeast-2', 'ap-southeast-4'] },
},
```

#### allowedRegions

Denies every request outside `regions`, on Root, with built-in exemptions.

**Global services, always exempt.** IAM, Organizations, STS, Route 53,
CloudFront, Support and other global services sign their requests as
us-east-1, so a plain region deny breaks them. Anvil exempts them with a
`NotAction` list, as AWS's example region-deny SCP and Control Tower do. This
list can't be turned off; Anvil keeps it up to date.

**Anvil's us-east-1 needs.** Some Anvil features must create resources in
us-east-1 whatever the org's regions are:

| Key | Default | Allows | Needed by |
|---|---|---|---|
| `acm` | On | ACM certificates in us-east-1 | Custom domains on any CloudFront site, edge API Gateway, Cognito hosted UI |
| `waf` | On | WAFv2, plus log groups named `aws-waf-logs-*`, in us-east-1 | `Waf` with CloudFront scope |
| `lambdaEdge` | **Off** | Lambda in us-east-1 (edge-shaped only), plus edge log groups in every region | SvelteKitSite `edge-oac` |

```ts
// default: global services + acm + waf. edge-oac sites fail to deploy.
allowedRegions: { regions: ['ap-southeast-2'] },

// opt in to Lambda@Edge for edge-oac sites
allowedRegions: { regions: ['ap-southeast-2'], usEast1: { lambdaEdge: true } },

// no CloudFront at all
allowedRegions: { regions: ['ap-southeast-2'], usEast1: { acm: false, waf: false } },
```

```
+ allowedRegions   Root
    allow       ap-southeast-2
    exempt      global services (iam, organizations, sts, route53, cloudfront, support, …)
    us-east-1   acm, waf (aws-waf-logs-* only)
                lambdaEdge on (usEast1.lambdaEdge: true), edge-shaped only
    logs        /aws/lambda/us-east-1.* in every region (Lambda@Edge execution logs)
```

`acm` and `waf` are on by default because they cost little and every
CloudFront site needs them. Certificates hold no data, but **WAF logs do**:
CloudFront-scope WAF logs must live in us-east-1 and contain client IPs,
URIs and headers. Docs and preview say so plainly; an org that set
`allowedRegions` for data residency should turn `waf` off or keep WAF
logging off. Turning them off is a strengthening, so it
lives in config. Turning `lambdaEdge` on is a loosening the user chooses in
config, shown in preview.

**Why `lambdaEdge` is opt-in** (docs must say plainly): Lambda@Edge runs at
CloudFront locations worldwide by design, and the region SCP can't stop it.
An org that set `allowedRegions` for data residency would have request data
processed outside its regions without choosing to. Turning it on is an
explicit decision that edge processing is acceptable. Orgs that want to stay
in region use `oac` sites.

When `lambdaEdge` is off, App preview for an `edge-oac` SvelteKitSite errors
before deploy, naming the setting (see App ↔ account linking).

**How Lambda@Edge behaves under the SCP** (from AWS docs and a third-party
test; still to confirm on an Anvil deploy):

| Part | Who does it | Subject to the SCP? |
|---|---|---|
| Replicating the function to other regions | `AWSServiceRoleForLambdaReplicator` (service-linked role) | No: SCPs don't affect service-linked roles |
| Running the function at the edge | CloudFront / Lambda service | No: SCPs only restrict IAM users and roles in member accounts. Classmethod confirmed the function runs in blocked regions |
| Error logs | `AWSServiceRoleForCloudFrontLogger` (service-linked role) | No |
| Execution logs, in the region where it ran | The function's execution role | **Yes**: dropped outside allowed regions |
| AWS calls from the function code | The function's execution role | **Yes**: fail outside allowed regions |

Anvil's edge signer makes no AWS calls (its role only has
`AWSLambdaBasicExecutionRole`), so only logs are affected. With `lambdaEdge`
on, the SCP exempts `logs:CreateLogGroup`, `logs:CreateLogStream` and
`logs:PutLogEvents` on `arn:aws:logs:*:*:log-group:/aws/lambda/us-east-1.*`
(the names Lambda@Edge gives its log groups), in every region.

**Edge-shaped denies.** An SCP can't tell an edge function from a normal one,
so with `lambdaEdge` on, us-east-1 Lambda also gets denies for everything
Lambda@Edge never uses: `lambda:InvokeFunction` (CloudFront's invocation isn't
an IAM principal, so it's unaffected), `lambda:AddPermission`,
`lambda:CreateEventSourceMapping`, `lambda:CreateFunctionUrlConfig`, and
`lambda:CreateFunction` with a VPC or layers. Deploy needs
`lambda:GetFunction` and `lambda:EnableReplication*`, which stay allowed.
A us-east-1 function can then only be run by CloudFront.

*To confirm on a real deploy:* an `edge-oac` site deploys and serves under
the SCP, execution logs appear in a non-allowed region, and a direct
`lambda invoke` in us-east-1 is denied.

#### instanceTypes

Limits instance sizes per service. Every service key is optional, and each
takes **exactly one** of `allowed` or `deny`; preview errors if a key has both
or neither. Wildcards work (`t3.*`).

| Key | Condition key | Applies to | Source |
|---|---|---|---|
| `ec2` | `ec2:InstanceType` | `ec2:RunInstances` | AWS example SCP ([Require Amazon EC2 instances to use specific instance type](https://github.com/aws-samples/service-control-policy-examples/tree/main/Cost-optimization)), an allow list |
| `rds` | `rds:DatabaseClass` | create/modify DB instance | Anvil addition, documented key |
| `elasticache` | `elasticache:CacheNodeType` | create/modify cluster, replication group | Anvil addition, documented key |
| `sagemaker` | `sagemaker:InstanceTypes` | notebooks, training, endpoints | Anvil addition, documented key |

```ts
instanceTypes: {
  ec2: { deny: ['p*', 'g*', 'x*', 'u-*', '*.metal', '*.48xlarge'] },
  rds: { allowed: ['db.t4g.*', 'db.r7g.large', 'db.r7g.xlarge'] },
  sagemaker: { deny: ['ml.p*', 'ml.g*'] },
},
```

```
+ instanceTypes   Workloads
    ec2           deny     p*, g*, x*, u-*, *.metal, *.48xlarge
    rds           allow    db.t4g.*, db.r7g.large, db.r7g.xlarge
    sagemaker     deny     ml.p*, ml.g*
```

```
✗ guardrails.instanceTypes.rds: set either allowed or deny, not both
```

Each service keeps its own naming (`p5.48xlarge`, `db.x2g.16xlarge`,
`ml.p5.48xlarge`), so Anvil never translates patterns between them.

Docs must say:
- Only `ec2` with `allowed` is an AWS-published example. `deny` lists and the
  other services are common practice built on documented condition keys, not
  AWS examples. AWS's main cost controls are Budgets and Cost Anomaly
  Detection.
- `allowed` is the tightest, but blocks every new AWS family until it's added,
  and breaks managed services that launch EC2 in the account (EKS node groups,
  Batch, EMR, Elastic Beanstalk) with confusing errors.

*Open, revisit later:* the default. Whether `instanceTypes` is off unless set,
or on with a built-in deny list, and if so which services and patterns.

#### services

Blocks AWS services accounts must never use. **Deny list only**: an allow
list (deny everything not named) was dropped as too strict; orgs that want
one write their own SCP in `run`.

| Setting | Result |
|---|---|
| Not set | No services SCP; every service allowed as normal. Other guardrails still apply. |
| `deny` | Every service allowed except those listed |

```ts
services: { deny: ['gamelift', 'braket', 'groundstation'] },
```

```
+ services   Workloads
    deny     gamelift, braket, groundstation
```

**Protected services.** A `deny` list can't name services whose absence
breaks deploys, the security baseline or Anvil itself (sts, iam,
organizations, kms, logs, cloudwatch, tag, support, ssm, the security
services). Anvil keeps the list up to date:

```
✗ guardrails.services.deny: ssm can't be denied; every App reads
  /anvil/org/account from Parameter Store
```

- `anvil-org-automation` skips the rule (like the `protect*` rules), so
  Anvil's own setup (EBS encryption default, enabling security services as
  members) works whatever is denied.
- Written as a plain `Deny` on the listed services' actions, on top of
  `FullAWSAccess`, like every other rule.
- Anvil components that use a denied service are caught by App preview
  (see App ↔ account linking).

#### requireTags

Uses an Organizations **tag policy** with `report_required_tag_for`, AWS's
mechanism for required tags in IaC (added November 2025). CloudFormation,
Terraform and Pulumi check required tags at deploy. The tag policy also
enforces value case and allowed values. Not set: no tag policy.

```ts
requireTags: {
  keys: ['CostCenter', 'Owner'],
  values: { CostCenter: ['eng', 'data', 'ops'] },  // optional
},
```

Required keys apply to **every service tag policies support**; there's no
per-service scoping. AWS has no global wildcard, so Anvil expands to
`<service>:ALL_SUPPORTED` for each service in its generated list of
supported services, and splits across tag policies when a policy would pass
the 10,000-character limit (10 tag policies per target; AWS merges them).
This matches AWS's tagging whitepaper (tag consistently across all
resources) and Anvil's default tags, which already put the keys on every
resource.

If one service turns out noisy, an `except: ['iam']` escape hatch can be added
later without changing the shape. Not in v1.

```
+ requireTags   Root  (tag policies ×2)
    required    CostCenter, Owner  on every supported service
    values      CostCenter ∈ eng, data, ops
```

- Org enables the tag policy type on Root if it isn't already.
- Anvil runs the check itself. Pulumi's own check is a Pulumi Cloud policy
  pack, which Anvil's S3 backend doesn't use. App preview calls
  `tag:ListRequiredTags` (Resource Groups Tagging API), which a member account
  can call, so it gets the effective required tags without reading the tag
  policy.
- Fits Anvil's default tags: an App sets `CostCenter` and `Owner` once and
  every resource carries them. App preview checks the App's tags against the
  org's required tags and fails before deploy (see App ↔ account linking):

  ```
  ✗ Org requires tag Owner on rds:*; add it to the App's default tags
  ```

- Not an SCP. AWS publishes no SCP example for requiring tags, and a
  require-on-create SCP needs every create action listed, can't cover
  resources that can't be tagged on create, and breaks console wizards.
- Docs must say: a console or CLI user who creates a resource with no tags
  isn't blocked; it shows as non-compliant in Resource Groups. A user who
  needs a hard block on one resource type writes their own SCP in `run`.

#### destroyUntagged

Opt-in scheduled cleanup of resources missing the required tag keys, in
accounts the user names. Never runs anywhere not listed.

```ts
requireTags: {
  keys: ['CostCenter', 'Owner'],
  destroyUntagged: {
    accounts: ['sandbox-alice', 'sandbox-bob', 'payments-dev'],
    schedule: 'daily',   // default
    graceDays: 7,        // default
  },
},
```

**Where it's allowed.** Sandbox accounts, and workload or free-form accounts
outside Prod. Preview errors for any account in a Prod OU (a stage mapped to
`Prod`, or a custom OU under `Workloads/Prod`), the Security OU (Log Archive,
Audit, and custom accounts there), Infrastructure, or the management account:

```
✗ guardrails.requireTags.destroyUntagged.accounts: payments-prod is in Workloads/Prod;
  cleanup can't run on Prod accounts
```

**How a run works:**
1. Find resources in the account missing any required key.
2. Skip AWS-managed resources (default VPC, service-linked roles, Lambda@Edge
   replicas, service-created log groups, Security Hub/Config/GuardDuty
   resources) and anything Anvil's guardrails protect (`protectBackups`,
   `protectKmsKeys`, the baseline's security services).
3. Tag new finds `anvil:delete-after=<today + graceDays>` and notify.
4. Delete resources whose date has passed and that still lack the keys.
   Tagging a resource within the grace period saves it.

```
destroyUntagged  sandbox-alice (777777777777)
  ⚠ 3 untagged: i-0abc (EC2), my-test-bucket (S3), sg-0def
    marked anvil:delete-after=2026-11-04, notified
  ✗ deleted 1: vol-0123 (EBS), marked 2026-10-21
```

- A scheduled Lambda in each listed account, with a role that can delete
  only in that account. The role exists only in listed accounts, so the
  delete permission never reaches Prod.
- Docs must say: AWS's automated-cleanup pattern is for sandbox accounts; for
  production, the tagging whitepaper recommends reporting and fixing tags.
- Notifications go to the org notification topic (marked: `warning`,
  deleted: `info`; see Notifications).
- *Open topic:* deletion engine.

#### `run`: weakening and customising

Two levels:

```ts
run(ctx) {
  // 1. tweak a built-in rule
  ctx.guardrails.protectBackups.except({ stages: ['dev'], reason: 'dev tears down its own data' });
  ctx.guardrails.protectKmsKeys.except({ stages: ['dev'], reason: 'dev tears down its own data' });
  ctx.guardrails.protectEncryptionDefaults.except({ stages: ['dev', 'perf'], reason: 'unencrypted volume benchmarks' });
  ctx.guardrails.denyIamUsers.except({ accounts: ['legacy-app'], reason: 'vendor tool needs access keys (TICKET-123)' });
  ctx.guardrails.imdsV2.disable({ reason: 'no EC2 in this org' });

  // 2. replace a built-in rule with your own version
  new anvil.aws.guardrails.AllowedRegions({
    reason: 'Bedrock models only in us-east-1',
    regions: ['ap-southeast-2', 'ap-southeast-4'],
    exemptServices: ['bedrock:*'],
    attach: [ctx.ou.workloads],
  });
}
```

```
⚠ Guardrails weakened in run()
  protectBackups, protectKmsKeys        except stage dev
                                          payments-dev (444444444444)
                                          "dev tears down its own data"
  protectEncryptionDefaults             except stages dev, perf
                                          "unencrypted volume benchmarks"
  denyIamUsers                          except legacy-app (666666666666)
                                          "vendor tool needs access keys (TICKET-123)"
  imdsV2                                disabled
                                          "no EC2 in this org"
  allowedRegions                        replaced (Workloads; + bedrock:* exempt)
                                          "Bedrock models only in us-east-1"
```

- `except({ accounts?, stages?, resources?, reason })` (plus
  `sourceAccounts` on `denyOutsideOrg`) and `disable({ reason })` on
  `ctx.guardrails.<rule>`. **`reason` is required** on every weakening,
  including replacement components; it's shown in preview and written into
  the policy description (and later the dashboard), so it's clear later why
  a guardrail was loosened.
  AWS caps policy descriptions at 512 characters, so preview errors if a
  rule's combined reasons don't fit; the full text also stays in state.
  Account and stage names are typed; unknown names are compile errors in
  TypeScript and preview errors in Python and Go.
- **Exceptions are explicit and as narrow as AWS allows.** `resources`
  scopes an exception to named ARNs inside the excepted accounts, on rules
  that act on resources. Rules that are account-wide AWS settings
  (`ebsDefaultEncryption`) reject `resources` with a clear error. Example,
  one public bucket:

  ```ts
  ctx.guardrails.s3AccountBpa.except({
    accounts: ['marketing-site'],
    resources: ['arn:aws:s3:::acme-legacy-public'],
    reason: 'legacy public bucket, moving to CloudFront by Dec (TICKET-301)',
  });
  ```

  ```
  ⚠ Guardrails weakened in run()
    s3AccountBpa     marketing-site: account-level Block Public Access off
                     expected public bucket: acme-legacy-public
                     any other bucket going public alerts critical (watched changes)
                     "legacy public bucket, moving to CloudFront by Dec (TICKET-301)"
  ```

  Anvil turns the account-level setting off in that account only. S3 can't
  be held per bucket by SCP (an SCP sees the API called, not the values
  sent, so "turn bucket Block Public Access on" and "off" look the same), so
  the excepted account can have public buckets; watched changes alert
  `critical` on any bucket other than the named ones becoming public.

  ```
  ✗ ebsDefaultEncryption can't be excepted per resource: it's an account-wide
    setting in AWS. Except the whole account (accounts: [...]), or copy the
    snapshot with encryption instead.
  ```

- Exceptions can never be made from inside an account (SCPs beat IAM):
  always a reviewed change in the Org project.
- Stage exceptions resolve to account IDs at deploy, so new accounts in that
  stage are covered automatically, and preview shows the change.
- Exceptions keep the rule attached and exclude accounts with a condition,
  so there is still one policy and the exemption is visible in it:

  ```json
  // SCP: exclude callers in the excepted account
  "Condition": { "StringNotEquals": { "aws:PrincipalAccount": ["444444444444"] } }

  // RCP: exclude resources owned by the excepted account
  "Condition": { "StringNotEquals": { "aws:ResourceAccount": ["444444444444"] } }
  ```

- A replacement component (`anvil.aws.guardrails.<Rule>`) takes over that
  rule entirely: attachment, actions, exemptions, conditions. If a rule is
  both configured and replaced, the `run` version wins and preview says so.
  If the replacement covers less than the rule it replaces, preview warns:

  ```
  ⚠ allowedRegions replaced at Workloads; it was on Root. Security, Sandbox
    and Suspended no longer have a region rule.
  ```
- `disable()` together with `except()` on the same rule is a preview error.
- Known cost, accepted as low risk: weakening can sit inside helpers or
  loops, so the file alone doesn't show it. Org code is reviewed and only
  deployable by a few people; preview (and later the dashboard) is the source
  of truth.

### Rules by OU

Follows AWS's framing: Prod protects data and recovery; NonProd (AWS: SDLC)
is for build and test; Sandbox is isolated.

```
Root           denyLeaveOrg, protectSecurityServices, protectAccessRole,
               protectAnvilRoles, denyIamUsers, denyCommitmentPurchases,
               requireTls, denyOutsideOrg, imdsV2, centralRoot, aiOptOut,
               s3AccountBpa, ebsDefaultEncryption, snapshotBpa, amiBpa, serialConsoleOff,
               denyPublicS3, protectEncryptionDefaults, denyMemberRoot, denyRegionOptIn
├── Workloads
│   ├── Prod     + protectBackups, protectKmsKeys
│   └── NonProd  + same data protections by default (see below)
├── Sandbox    denyNetworkConnectivity, denyExpensiveResources
└── Suspended  denyAll
```

| Rule | Type | Attached to | Blocks |
|---|---|---|---|
| `denyLeaveOrg` | SCP | Root | An account leaving the org |
| `protectSecurityServices` | SCP | Root | Stopping/deleting CloudTrail, GuardDuty, Config, Security Hub, Access Analyzer, Inspector, Macie, Detective, including disassociating members and changing org configuration (same list as `securityOperator`'s deny) |
| `protectAccessRole` | SCP | Root | Changing `OrganizationAccountAccessRole` (break glass; can't be weakened) |
| `protectBreakGlass` | SCP | emergency account | Anything except the break-glass users managing their own password/MFA, reading, and assuming `OrganizationAccountAccessRole`; no other IAM principals or resources; alert rules and forwarder untouched (can't be weakened; see Break glass) |
| `protectAnvilRoles` | SCP | Root | Creating, changing, tagging or deleting `role/anvil-*` except by `anvil-org-automation` (can't be weakened) |
| `denyIamUsers` | SCP | Root | IAM users and access keys (moved up from Workloads so Sandbox and Security are covered). Exception: `user/break-glass-*` in the emergency account |
| `denyCommitmentPurchases` | SCP | Root | Reserved Instance / Savings Plans purchases outside management |
| `requireTls` | RCP | Root | Non-TLS access to S3, KMS, SQS, Secrets Manager |
| `denyOutsideOrg` | RCP | Root | Principals outside the org accessing resources, and AWS services acting for accounts outside the org (confused deputy, see below). Service principals acting for the org pass (CloudFront OAC reading S3, CloudTrail/Config writing to Log Archive) |
| `imdsV2` | Declarative | Root | EC2 without IMDSv2 |
| `centralRoot` | Root access | Org | Member root credentials (deleted everywhere except the emergency account, whose root is kept for break glass) |
| `denyMemberRoot` | SCP | Root | Any action by a member account's root user (second layer to `centralRoot`, e.g. a root password reset by email). Exempts AWS's own root recovery sessions, which break glass uses to unlock bucket and queue policies: `aws:AssumedRoot` is only present on `AssumeRoot` sessions, so the deny applies to the root principal when the key is absent: `"StringLike": { "aws:PrincipalArn": "arn:*:iam::*:root" }` **and** `"Null": { "aws:AssumedRoot": "true" }`, as Control Tower's root guardrail does with its AssumeRoot exemption (the `Null` check alone would match every caller). Exception: the emergency account's root may manage `user/break-glass-*` |
| `denyRegionOptIn` | SCP | Root | Member accounts enabling or disabling regions (`account:EnableRegion`/`DisableRegion`), so no unwatched region appears between deploys; regions are enabled from management |
| `aiOptOut` | AI opt-out | Root | AI training on your data |
| `s3AccountBpa` | Account setting + SCP | Every member account | Public S3 buckets (all 4 account-level Block Public Access settings on, which overrides every bucket's own setting; `denyPublicS3` stops turning them off) |
| `ebsDefaultEncryption` | Account setting + SCP | Every member account, every allowed region (every enabled region when `allowedRegions` isn't set; set again when a region is enabled) | Unencrypted EBS volumes (`protectEncryptionDefaults` stops turning it off) |
| `snapshotBpa` | Declarative | Root | Public EBS snapshots (`block_all_sharing`) |
| `amiBpa` | Declarative | Root | Publishing public AMIs (`block_new_sharing`) |
| `serialConsoleOff` | Declarative | Root | EC2 serial console access |
| `denyPublicS3` | SCP | Root | Changing account-level S3 Block Public Access (`s3:PutAccountPublicAccessBlock`) only. Bucket-level calls stay allowed: Apps set them on every bucket, an SCP can't tell on from off, and the account setting overrides them anyway |
| `protectBackups` | SCP | Prod, NonProd | Deleting Backup vaults/recovery points, changing vault lock |
| `protectKmsKeys` | SCP | Prod, NonProd | Scheduling KMS key deletion or disabling keys |
| `protectEncryptionDefaults` | SCP | Root | Turning off EBS encryption-by-default (`ec2:DisableEbsEncryptionByDefault`). No S3 part: S3 encrypts every new object by default since January 2023 and it can't be turned off, while denying `s3:PutEncryptionConfiguration` would block Apps setting their own encryption |
| `denyNetworkConnectivity` | SCP | Sandbox | TGW attachments, VPC peering, VPN, Direct Connect |
| `denyExpensiveResources` | SCP | Sandbox | GPU / very large instance types (`p*`, `x*`, `u-*`, `*.metal`), Marketplace subscriptions |
| `denyAll` | SCP | Suspended | Everything, for every principal except break glass (`OrganizationAccountAccessRole`), so data can be recovered or cleaned up before closing. Any use in a Suspended account alerts `critical` |

**Legacy ELB access logs and `denyOutsideOrg`.** In older regions, ELB
access-log bucket policies grant an AWS-owned ELB account (e.g. Sydney
`783225319266`), which is outside the org, so `denyOutsideOrg` blocks log
delivery. Docs (and the adopted-org preflight, when built) point to the
`logdelivery.elasticloadbalancing.amazonaws.com` service principal instead,
which passes.

### Confused-deputy protection

`denyOutsideOrg` carries a second statement, from AWS's sample RCP. When an
AWS service acts for an account (`aws:SourceAccount` is present) and that
account isn't in the org or allow-listed, it's denied. Without it, someone
could point CloudTrail, Config or S3 logging in **their own** account at
your bucket and the service-principal exemption would let it through.

```json
{ "Sid": "EnforceConfusedDeputyProtection",
  "Effect": "Deny",
  "Principal": "*",
  "Action": ["s3:*", "sqs:*", "kms:*", "secretsmanager:*", "sts:*"],
  "Resource": "*",
  "Condition": {
    "StringNotEqualsIfExists": {
      "aws:SourceOrgID": "o-abc123",
      "aws:SourceAccount": ["<sourceAccounts from except()>"] },
    "Bool": { "aws:PrincipalIsAWSService": "true" },
    "Null": { "aws:SourceAccount": "false" } } }
```

Services that don't send a source account are unaffected (`Null` check).
Legitimate outside sources (a vendor delivering to your bucket, partner S3
replication) are allowed explicitly, with a reason:

```ts
ctx.guardrails.denyOutsideOrg.except({
  sourceAccounts: ['999988887777'],
  reason: 'Acme delivers billing exports to payments-prod',
});
```

Same rule, not a separate one: no extra RCP slot, ~400 characters.

### Policy size and slots

AWS limits: SCPs 10,240 characters and 10 per target; RCPs 5,120 and 5 per
target. `RCPFullAWSAccess` is always attached and **counts toward the 5**,
so Anvil has 4 RCP slots per target. Inherited policies don't count; only
ones attached directly.

1. **Minified on upload.** The console strips whitespace before counting,
   but the API counts the document exactly as sent, so Anvil always uploads
   minified JSON.
2. **Packed, then spilled.** Baseline rules are packed into as few policies
   as possible (one Sid per rule). If a packed policy would exceed the
   limit, Anvil splits it on Sid boundaries into a second policy at the same
   target. Rules are never split mid-statement.
3. **Out of slots: preview error**, before anything changes, with sizes and
   slot usage:

   ```
   ✗ RCPs at Root need 5 slots, 4 available (RCPFullAWSAccess uses 1)
       anvil-baseline-rcp-1   5,080 / 5,120
       anvil-baseline-rcp-2   2,310 / 5,120
       payments-data-rcp      4,960 / 5,120
       vendor-access-rcp      3,700 / 5,120
       partner-rcp            1,200 / 5,120   ← doesn't fit
     Attach custom RCPs to an OU or account instead (each has its own 4 slots),
     or merge them.
   ```

4. **Warns at 80%** of a policy's size so it's visible before it's a
   problem.

Realistically the baseline RCPs stay well under: each `sourceAccounts` or
`except` account adds ~15 characters, so it takes a couple of hundred
exceptions to fill one.

### Anvil's exemption

Anvil has to manage things its own protection rules block (security
services, break-glass role, account defaults, SSM parameter, scan role,
alert rules). **Exactly one principal is exempt: `anvil-org-automation`**,
by its exact ARN, from the `protect*` rules and the `services` deny list.
Region, instance-type and tag rules still apply to it. The scan role and the
destroyUntagged role get no exemption.

```json
{ "Sid": "ProtectSecurityServices",
  "Effect": "Deny",
  "Action": ["guardduty:DeleteDetector", "config:StopConfigurationRecorder", "..."],
  "Resource": "*",
  "Condition": { "ArnNotLike": {
    "aws:PrincipalArn": "arn:aws:iam::*:role/anvil-org-automation" } } }
```

`protectAnvilRoles` makes the name trustworthy:

```json
{ "Sid": "ProtectAnvilRoles",
  "Effect": "Deny",
  "Action": ["iam:CreateRole", "iam:DeleteRole", "iam:UpdateAssumeRolePolicy",
             "iam:AttachRolePolicy", "iam:DetachRolePolicy", "iam:PutRolePolicy",
             "iam:DeleteRolePolicy", "iam:UpdateRole", "iam:TagRole", "iam:UntagRole",
             "iam:PutRolePermissionsBoundary", "iam:DeleteRolePermissionsBoundary"],
  "Resource": "arn:aws:iam::*:role/anvil-*",
  "Condition": { "ArnNotLike": {
    "aws:PrincipalArn": "arn:aws:iam::*:role/anvil-org-automation" } } }
```

- Role names are unique per account and AWS creates `anvil-org-automation`
  at `CreateAccount`, so the name can't be squatted.
- Nobody else can change its trust or permissions, or create look-alike
  `anvil-*` roles (covers the scan and destroyUntagged roles too).
- `aws:PrincipalArn` is the role ARN, not caller-controlled.
- Not by tag (`aws:PrincipalTag`, anyone with `iam:TagRole` can set it) or
  IAM path (chosen at creation); AWS's examples exempt by `aws:PrincipalArn`.
- All of Anvil's power sits in one role, so who can assume it matters: only
  Org's deployers, by exact ARN (see Accounts → Automation role).

Example outcomes:

```
# payments-prod: aws kms schedule-key-deletion ...           ✗ protectKmsKeys
# sandbox-alice: aws ec2 run-instances --instance-type p4d.24xlarge
#                                                            ✗ denyExpensiveResources
# sandbox-alice: aws ec2 create-transit-gateway-vpc-attachment ...
#                                                            ✗ denyNetworkConnectivity
# payments-prod: aws backup delete-recovery-point ...        ✗ protectBackups
```

### Baseline versions and upgrades

The baseline (rule contents, global-service exemptions, tag-policy service
list, core services list, scanner image) has its own **semantic version**,
separate from the CLI's, in one file in the provider:

```json
// provider/aws/org/baseline.json
{ "version": "1.3.0", "rules": { … } }
```

| Bump | Meaning |
|---|---|
| Major | Can deny something that worked before (new deny, tightened rule, removed exemption) |
| Minor | Only adds opt-in features or allows more (new opt-in guardrail, new exemption, new preset) |
| Patch | Same effective permissions (descriptions, Sids, repacking statements) |

The version only changes when the maintainer bumps it in `baseline.json`;
CLI releases without baseline changes leave it alone. `CHANGELOG-org.md`
lists each rule added, changed or removed, and why.

**Each CLI ships one baseline** (the `baseline.json` it was built with).
The version an org has **accepted** is recorded in `anvil.org.yaml`, written by
the CLI:

```yaml
# anvil.org.yaml (managed by the CLI)
project: acme-org
org:
  baseline: 1.2.0
```

When the CLI's baseline is newer than the accepted one, preview shows the
diff against what's deployed (from state) and **blocks** until it's
accepted:

```
✗ This CLI ships Org baseline 1.3.0; this org has accepted 1.2.0.
  See what changes: anvil org upgrade --preview
  Accept it:        anvil org upgrade   (updates anvil.org.yaml; commit it)
```

```
anvil org upgrade --preview

Baseline 1.2.0 → 1.3.0 (minor)

  + allowedImages       opt-in, not enabled
  ~ allowedRegions      SCP  Root
      + exempt  bedrock-agentcore (new global endpoint)
```

- `anvil org upgrade` only edits `anvil.org.yaml`; the change ships as a one-line
  PR diff, and CI's preview shows the full policy diff and impact check.
- A CLI older than the accepted baseline refuses to deploy (`baseline 1.3.0
  needs Anvil ≥ 1.8`).
- No `baseline` in `anvil.org.yaml` (older projects): the CLI writes its own and
  says so.
- Cost of one baseline per CLI: an org can't take a CLI fix without also
  accepting that CLI's baseline (or staying on an older CLI).

*Later:* the org release workflow (cutting org releases with git tags, e.g.
`org-v1.3.0`; embedding previous tagged baselines so a newer CLI can still
deploy an older accepted one; a CI check that the version bump matches the
change; whether patches apply without `anvil org upgrade`).

### Secure account defaults

New AWS accounts start with some account-wide switches in the unsafe
position. The baseline turns them on everywhere and keeps them on, using the
strongest mechanism available:

- **Declarative policies** (AWS enforces org-wide, including future accounts
  and regions; accounts can't change them): `snapshotBpa`, `amiBpa`,
  `serialConsoleOff`.
- **Set per account, protected by SCP** (no declarative policy covers them):
  `s3AccountBpa` and `ebsDefaultEncryption`, set in the configure step of
  new-account readiness via `anvil-org-automation` (EBS in every allowed
  region).

Why default on: AWS Foundational Security Best Practices checks them
(S3.1, EC2.7, EC2.1), so new accounts pass AWS's own standard from day one;
SRA blocks S3 public access for all existing and future accounts; and they
only block things nobody should do by accident. Weakened in `run` with a
reason like any baseline rule:

```ts
ctx.guardrails.s3AccountBpa.except({ accounts: ['marketing-site'], reason: 'legacy public bucket, moving to CloudFront (TICKET-301)' });
ctx.guardrails.serialConsoleOff.disable({ reason: 'EC2 fleet needs serial console for recovery' });
```

**Opt-in** (they block legitimate setups or need org-specific lists):

```ts
guardrails: {
  vpcBlockPublicAccess: { enable: true, mode: 'block_ingress', exclusionsAllowed: true },
  allowedImages: { enable: true, providers: ['amazon', '123456789012'], mode: 'audit' },
},
```

| Opt-in | Why not default |
|---|---|
| `vpcBlockPublicAccess` | Blocks every public load balancer and public-subnet server unless excluded. Docs: a good fit when all traffic enters through CloudFront/API Gateway, as Anvil's components do |
| `allowedImages` | Needs the org's own list of AMI providers; has an `audit` mode to try first |
| VPC encryption controls | Later: AWS says monitor before enforce; possible per-VPC cost to verify |

**Cost:** all five baseline switches, SCPs and declarative policies are
free. Knock-on cost: cross-account sharing of encrypted AMIs/snapshots needs a
customer-managed KMS key (~$1/key/month) because `aws/ebs` can't be shared.

### NonProd strictness

**NonProd gets Prod's data protections by default**, so staging behaves like
prod. Loosening them (typically for `dev`) is a stage exception in `run`:

```ts
ctx.guardrails.protectBackups.except({ stages: ['dev'], reason: 'dev tears down its own data' });
```

(Earlier options, stage exceptions in the config `guardrails` block or a
`relax` / `protection` field on each stage, are superseded by moving all
weakening into `run`.)

### Custom policies

**Config holds the Anvil-managed shape; `run(ctx)` holds the user's own
additions.** Custom SCPs and RCPs are components inside `run`, the same
model as `App`. Attachments are declared on the policy (`attach`).

```ts
export default new Org({
  tier: 'network',
  email: 'aws+{account}@acme.com',
  stages: { prod: { ou: 'Prod' }, staging: { ou: 'NonProd' }, dev: { ou: 'NonProd' } },
  workloads: {
    website:   { stages: ['prod', 'dev'] },
    analytics: { stages: ['prod', 'dev'] },
  },

  // Card-handling accounts share PCI controls, so they share an OU.
  ou: { 'Workloads/Prod/PCI': { enable: true, tags: { compliance: 'pci' } } },
  accounts: {
    'payments-ledger': { ou: 'Workloads/Prod/PCI' },
    'payments-cards':  { ou: 'Workloads/Prod/PCI' },
  },

  run(ctx) {
    new anvil.aws.ServiceControlPolicy('denyNonApprovedRegions', {
      description: 'Only ap-southeast-2 and ap-southeast-4',
      policy: approvedRegions, // imported from ./policies/regions.ts
      attach: [ctx.ou.workloads],
    });

    new anvil.aws.ServiceControlPolicy('pciControls', {
      policy: anvil.aws.policyFile('./policies/pci.json'),
      attach: [ctx.ou['Workloads/Prod/PCI']],
    });

    new anvil.aws.ResourceControlPolicy('s3OrgOnly', {
      policy: anvil.aws.policyFile('./policies/s3-org-only.json'),
      attach: [ctx.ou.root],
    });
  },
});
```

- Components: `anvil.aws.ServiceControlPolicy`, `anvil.aws.ResourceControlPolicy`.
  Declarative and AI opt-out policies stay Anvil-managed only.
- `policy` takes a **policy value**, the one type every Anvil policy field
  uses (SCP/RCP `policy`, permission set `inlinePolicy`):
  - `anvil.aws.policyDocument({ statements: [...] })`: typed. Write it inline
    or in its own `.ts` / `.py` / `.go` file and import it like any code, so
    editor checks, autocomplete and reuse work across files.
  - `anvil.aws.policyFile('./x.json')`: plain JSON, e.g. shared with other
    tools or copied from AWS docs. Read and validated at preview.

  ```ts
  // policies/regions.ts
  export const approvedRegions = anvil.aws.policyDocument({
    statements: [{
      effect: 'Deny',
      notActions: ['iam:*', 'sts:*', 'organizations:*', 'cloudfront:*', 'route53:*', 'support:*'],
      resources: ['*'],
      conditions: { StringNotEquals: { 'aws:RequestedRegion': ['ap-southeast-2', 'ap-southeast-4'] } },
    }],
  });
  ```

  Preview validates each policy for where it's used (Access Analyzer with
  the SCP, RCP or identity policy type): `principals` in an SCP is an error,
  an RCP without `principals: '*'` is an error. Anvil minifies and creates
  it on deploy; editing the source updates the policy on the next deploy.
- `attach` takes typed handles: `ctx.ou.root` / `.workloads` / `.prod` /
  `.nonProd` / `.sandbox` / ..., `ctx.ou['<custom path>']` (keys from
  config), `ctx.accounts['<name>']`, and `ctx.accountsInStage('<stage>')`.
  Typos are compile errors in TypeScript. `ctx.ref` and `ctx.export` work as
  in `App`.
- Preview validates before deploy: IAM Access Analyzer `ValidatePolicy`, the
  10,240-character SCP limit (5,120 for RCPs), and the 10-SCPs-per-target limit (5 for RCPs, including
  `RCPFullAWSAccess`) (preview prints slot usage per target; see Guardrails →
  Policy size and slots). It warns on region-deny policies about global
  services.
- Anvil packs its baseline into as few policies as possible to leave slots
  free, with **one Sid per rule** (`"Sid": "protectKmsKeys"`) so packed
  policies stay readable in the console and diffs, tags custom policies `anvil:managed=true` (prune leaves them alone),
  never attaches to the management account, and never detaches
  `FullAWSAccess`.
- Removing a component detaches and deletes the policy.
- **Anvil ships no compliance-named policies** (e.g. "PCI"): no SCP alone
  makes an account compliant, and the name would imply it does. Compliance
  checking is Prowler's job.
- *Future:* `anvil init --org` scaffolds a `policies/` folder with a
  commented example; a small library of building blocks
  (`anvil.aws.policies.denyRegions([...])`) for common SCPs.

### Testing policies

AWS strongly recommends testing SCPs before attaching them to Root. Two
pieces work together.

**Policy Staging OU (opt-in).** A top-level OU directly under Root, created
only when enabled, holding test accounts:

```ts
ou: { PolicyStaging: { enable: true } },
accounts: { 'policy-test': { ou: 'PolicyStaging' } },

run(ctx) {
  new anvil.aws.ServiceControlPolicy('denyRoute53Domains', {
    policy: …,
    attach: [ctx.ou.policyStaging],   // or attach directly while testing
  });
}
```

```
Root
├── PolicyStaging    policy-test
├── Security
└── …
```

- Gets the Root baseline like every OU, plus whatever is being tested.
- Docs: test with an account that looks like real workloads.

**`staged` on a policy.** Write the real target from the start;
`staged: true` holds the policy in the Policy Staging OU (every test account
in it) until removed:

```ts
new anvil.aws.ServiceControlPolicy('denyRoute53Domains', {
  policy: denyRoute53DomainsPolicy,
  attach: [ctx.ou.workloads],
  staged: true,   // attaches to PolicyStaging only, for now
});
```

```
+ denyRoute53Domains   SCP  PolicyStaging (staged; real target: Workloads)
```

Deleting `staged: true` rolls it out to the real target, with the impact
check at that point:

```
~ denyRoute53Domains   SCP
    - PolicyStaging (staged)
    + Workloads   ✗ impact: route53domains used by website-prod (2026-10-26)
                    Type "yes" to continue: _
```

- **Root is the main case**: a bad deny there hits every account at once.

  ```ts
  new anvil.aws.ResourceControlPolicy('s3OrgOnly', {
    policy: anvil.aws.policyFile('./policies/s3-org-only.json'),
    attach: [ctx.ou.root],
    staged: true,
  });
  ```

  ```
  + s3OrgOnly   RCP  PolicyStaging (staged; real target: Root — every account)
  ```

- Covers the user's own policies. Anvil's built-in baseline can't be
  `staged`: it's tested in Anvil's CI orgs, and on adopted orgs it's the
  first-rollout question (review gap 2).
- The real target is in code and review from day one; rollout is a one-line
  removal.
- Works on `ServiceControlPolicy`, `ResourceControlPolicy` and replacement
  guardrail components.
- Preview errors if `staged` is set but Policy Staging isn't enabled, or has
  no accounts.

**Impact check in preview (always on).** AWS also recommends examining
service last accessed data before denying anything. From management,
Organizations reports which services each account and OU used and when;
preview checks every new or changed deny (SCP or RCP, built-in or custom)
against the last 90 days:

```
+ denyRoute53Domains   SCP  Workloads
    ✗ impact: route53domains used in the last 90 days by
        website-prod (2026-10-26), analytics-prod (2026-09-30)
      these calls would be denied after deploy
      Type "yes" to continue: _
```

```
+ bedrockUsEast1Only   SCP  Workloads
    ✓ impact: bedrock not used outside us-east-1 in the last 90 days*
      * service-level data only; regions and actions aren't checked
```

- A deny that hits a service used in the last 90 days is a **hard stop**:
  typed `yes` interactively, `--yes-policy-impact` in CI (plain `--yes`
  doesn't cover it), like account creation.
- Attachments to the Policy Staging OU skip the stop (that's what it's for);
  the impact is still shown.
- Data is per service (some services per action); regions and conditions
  aren't checked, and preview says so.
- Anvil's own baseline is tested in Anvil's CI orgs before each release, including real App deploys (every component type) against the baseline, so a rule that breaks Apps is caught before release.

**Lock-out check in preview (always on).** Every custom deny (SCP or RCP,
including replacement guardrails) is checked against what Anvil, log
delivery and break glass need, at every target it attaches to:

| Would block | Severity | Why |
|---|---|---|
| `anvil-org-automation` (actions Org uses in that account) | ✗ error | Deploys break part-way |
| CloudTrail or Config writing to Log Archive (RCPs) | ✗ error | Audit trail silently stops |
| `OrganizationAccountAccessRole` (break glass) | ⚠ warning | Recoverable: detach the SCP from management first |

```
✗ regionsOnly (SCP Root) denies anvil-org-automation guardduty:CreateDetector
  in us-east-1.
  To fix, do one of:
    1. Add an exemption: ArnNotLike aws:PrincipalArn arn:aws:iam::*:role/anvil-org-automation
    2. Use guardrails.allowedRegions, which has Anvil's exemptions built in.

✗ s3OrgOnly (RCP Root) would block cloudtrail.amazonaws.com writing to
  log-archive/anvil-org-logs-o-ab12cd.
  To fix: allow service principals with aws:SourceOrgID o-ab12cd (see denyOutsideOrg).

⚠ lockIam (SCP Workloads) also blocks break glass (OrganizationAccountAccessRole)
  in 4 accounts. In an emergency, detach it from management first.
```

- Static check of actions, resources and principal conditions. Conditions
  it can't evaluate (exotic keys, tags) give a "couldn't verify" warning,
  never a silent pass.
- Anvil never rewrites a user's policy; the fix is always shown, not applied.
