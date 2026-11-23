# Compliance Scanning (Prowler) and the Local Dashboard

**Status:** Built and verified end to end on `testWaf` (2026-10-08): shared
scanner created on first deploy, a scan on deploy, a scheduled scan, a manual
scan, and the dashboard reading all three. Not yet done: idle shutdown for the
dashboard, a teardown hint on `anvil destroy`, and an import test of the Python
SDK path. See [Known limitations](#known-limitations) and
[Future work](#future-work).

---

## Contents

1. [What it is](#what-it-is)
2. [Using it](#using-it)
3. [How it fits together](#how-it-fits-together)
4. [The shared scanner (per account and region)](#the-shared-scanner-per-account-and-region)
5. [The per-app schedule](#the-per-app-schedule)
6. [How `anvil deploy` handles compliance](#how-anvil-deploy-handles-compliance)
7. [What happens during a scan](#what-happens-during-a-scan)
8. [Results and retention](#results-and-retention)
9. [The local dashboard](#the-local-dashboard)
10. [Commands](#commands)
11. [Security model](#security-model)
12. [Costs](#costs)
13. [Known limitations](#known-limitations)
14. [Code map](#code-map)
15. [Testing](#testing)
16. [Upgrading Prowler](#upgrading-prowler)
17. [Decisions and rejected alternatives](#decisions-and-rejected-alternatives)
18. [Test history](#test-history)
19. [Future work](#future-work)

---

## What it is

Anvil's components are aligned to SOC 2 and ISO 27001 controls. Compliance
scanning checks that claim against the **live account**: an app opts in, Anvil
schedules [Prowler](https://github.com/prowler-cloud/prowler) scans of that
app's deployed resources, and `anvil compliance dashboard` shows the results on
a local web page, grouped by the Anvil component that created each resource.

It is a **post-deploy posture check**. Prowler inspects deployed resources, not
code or Pulumi plans, so it complements rather than replaces pre-deploy checks.

Two constraints shaped everything:

- **Prowler never ships in the CLI.** It's a large Python application. It runs
  in AWS CodeBuild inside your account, in Prowler's own unmodified container
  image. The CLI only starts scans and reads results.
- **The scanner can't change or read your data.** The scan role reads
  configuration only, is explicitly denied data reads (object contents, logs,
  function code, secrets), and can write nothing except its own results.

## Using it

### Turning it on

Add `compliance` to your `App`:

```ts
export default new App({
  providers: { aws: { region: 'ap-southeast-2' } },
  compliance: {
    frameworks: ['soc2', 'iso27001', 'cis'], // required
    schedule: 'daily', // default
    retention: '1y', // default
    scanOnDeploy: true, // default false
  },
  run(ctx) {
    /* ... */
  },
});
```

Prowler scans **deployed resources**, not code or plans. This is a post-deploy
posture check, not a pre-deploy policy gate.

### Hard constraints

- **Prowler never ships in the CLI.** It's Python with a large dependency tree.
  It runs in the cloud, in the upstream container image; the CLI only reads
  results.
- **Nothing in the scanner writes to scanned resources.** The scan role is
  read-only, plus writes to its own results prefix.

### Non-goals (v1)

- Org-wide scanning (management / delegated-admin account). Planned as a separate
  app type later — see [Future work](#future-work).
- GCP. Prowler supports GCP, but label-scoped scanning is unverified.
- A CI gate (`--fail-on`). Explicitly dropped.
- Object Lock / immutable results. Deferred; enabling it on an existing bucket is
  supported by S3, so nothing here blocks it.
- Pre-deploy (IaC) scanning.

## Config API

`compliance` is a **top-level `App` option**, not a provider entry. `providers`
maps to cloud credentials/regions and rejects unknown prefixes.

```ts
compliance: {
  frameworks: ['soc2', 'iso27001', 'cis'],  // required, non-empty — explicit opt-in
  schedule: 'daily',                        // 'daily' | 'weekly' | 'none' | { cron, timezone }
  retention: '1y',                          // '30d' | '90d' | '180d' | '1y' | '2y' | '7y'
  scanOnDeploy: false,
}
```

| Option         | Default             | Notes                                                            |
| -------------- | ------------------- | ---------------------------------------------------------------- |
| `frameworks`   | **none — required** | Friendly names mapped to pinned Prowler IDs per Anvil release    |
| `schedule`     | `'daily'`           | Presets, or `{ cron, timezone }` passed to EventBridge Scheduler |
| `retention`    | `'1y'`              | Fixed tiers. SOC 2 Type II audits typically look back 12 months  |
| `scanOnDeploy` | `false`             | Fire-and-forget scan after a successful `anvil deploy`           |

### Frameworks

> **For the user docs:** `frameworks` accepts **two forms**, mixed freely:
>
> - **Friendly names** — short, and always point at the newest version Anvil
>   pins. Upgrading Anvil can move e.g. `cis` from CIS 7.0 to a later CIS in
>   lockstep with the pinned Prowler image.
> - **Raw Prowler IDs** — any AWS compliance ID from the pinned Prowler version
>   (`prowler aws --list-compliance`). These stay fixed across Anvil upgrades, so
>   use them to pin an older version (`cis_6.0_aws`) or for frameworks without a
>   friendly name (`ens_rd2022_aws`, `ffiec_aws`, `mitre_attack_aws`, …).
>
> ```ts
> compliance: {
>   frameworks: ['soc2', 'cis', 'ens_rd2022_aws'];
> }
> ```
>
> Unknown values fail at deploy with a "did you mean …?" suggestion — never
> later inside the scan.

Friendly names (Prowler 5.44.0):

| Friendly           | Prowler ID                                           |
| ------------------ | ---------------------------------------------------- |
| `soc2`             | `soc2_aws`                                           |
| `iso27001`         | `iso27001_2022_aws`                                  |
| `cis`              | `cis_7.0_aws`                                        |
| `nist-800-53`      | `nist_800_53_revision_5_aws`                         |
| `nist-800-171`     | `nist_800_171_revision_2_aws`                        |
| `nist-csf`         | `nist_csf_2.0_aws`                                   |
| `pci`              | `pci_4.0_aws`                                        |
| `hipaa`            | `hipaa_aws`                                          |
| `fsbp`             | `aws_foundational_security_best_practices_aws`       |
| `gdpr`             | `gdpr_aws`                                           |
| `nis2`             | `nis2_aws`                                           |
| `dora`             | `dora_2022_2554`                                     |
| `fedramp-low`      | `fedramp_low_revision_4_aws`                         |
| `fedramp-moderate` | `fedramp_moderate_revision_4_aws`                    |
| `cmmc`             | `cmmc_2.0`                                           |
| `essential-eight`  | `asd_essential_eight_aws`                            |
| `well-architected` | `aws_well_architected_framework_security_pillar_aws` |
| `c5`               | `c5_aws`                                             |
| `csa-ccm`          | `csa_ccm_4.0`                                        |

Raw IDs are validated against the **exact list** for the pinned Prowler version,
not a pattern: several real IDs don't end in `_aws` (`dora_2022_2554`,
`cmmc_2.0`, `csa_ccm_4.0`, `cis_controls_8.1`, `fedramp_20x_*`). The map and list
live in `provider/aws/complianceScanner/frameworks.go` and
`cmd/anvil/compliance/frameworks.go` — keep them identical, and update both when
the Prowler image is bumped.

**Cost of multiple frameworks is small.** Prowler runs _checks_; frameworks are
mappings of requirements onto checks. `--compliance a b c` runs the **union** of
mapped checks, and SOC 2 / ISO / CIS overlap heavily. Worst case equals an
unfiltered run.

### Schedule

- Presets: `'daily'`, `'weekly'`, `'none'` (deploy-triggered and on-demand only).
- Advanced: `{ cron: '0 3 * * ? *', timezone: 'Australia/Sydney' }`.
- **Jitter:** presets get a deterministic start time derived from
  `hash(project + stage)`, so many apps don't all fire at 00:00 and an app's time
  is stable across deploys.
- **Minimum interval** ~6h; more frequent is rejected at construction.

## Architecture

Two layers with **different owners**, because a shared resource can't be owned by
any one app's Pulumi stack (the second app would fail with `AlreadyExists`, and
destroying the first would delete it from under everyone).

```
                ┌────────────────────── shared, per account + region ──────────────────────┐
                │  (managed by the CLI via direct AWS SDK calls — no Pulumi state)          │
anvil deploy ───┼─► CodeBuild project ── scan role (read-only)                              │
 (ensure step)  │   Invoke role ── Scheduler group `anvil-compliance` ── Log group (30d)    │
                │   Results bucket (encrypted, versioned, lifecycle tiers)                  │
                └───────────────────────────────────────────────────────────────────────────┘
                                ▲ StartBuild(env overrides)
                ┌───────────────┴──────── per app (project + stage) ────────────────────────┐
anvil deploy ───┼─► anvil:internal:ComplianceScanner (Pulumi component in the app stack)    │
 (pulumi up)    │     └─ EventBridge Scheduler schedule in group `anvil-compliance`          │
                └───────────────────────────────────────────────────────────────────────────┘

anvil compliance ── read latest results → filter to project/stage → 127.0.0.1 dashboard
```

## Shared layer (per account + region)

Managed **imperatively by the CLI**, in the same style as `anvil bootstrap`. No
Pulumi stack, no state bucket.

### Resources

All named `anvil-compliance-<account>-<region>` unless noted, and tagged
`ManagedBy=anvil`, `anvil:component=compliance`.

| Resource          | Converge steps                                                                                                                                                 |
| ----------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Results bucket    | create → public access block, SSE-KMS, versioning, ownership controls, bucket policy (TLS-only), lifecycle tiers                                               |
| Scan role         | create → attach `SecurityAudit` + `ViewOnlyAccess` → inline policy (Prowler's additional read-only permissions, `tag:GetResources`, results bucket write, KMS) |
| Invoke role       | create → inline policy (`codebuild:StartBuild` on the project only)                                                                                            |
| Log group         | create → retention 30 days                                                                                                                                     |
| CodeBuild project | create / `UpdateProject` — no VPC, upstream Prowler image pinned by digest, inline buildspec, ~30 min timeout, `concurrentBuildLimit` 3–5                      |
| Scheduler group   | `anvil-compliance` — create                                                                                                                                    |

The results bucket's secure defaults must match the `Bucket` component's. Extract
bootstrap's state-bucket hardening into a shared helper and use it for both.

### Converge semantics

- Every step is **idempotent**: `AlreadyExists` is success, then desired settings
  are applied. Most calls are whole-document replaces (`PutBucketLifecycleConfiguration`,
  `PutRolePolicy`, `UpdateProject`), so re-applying also **corrects drift**.
- **Partial failure self-heals** on the next deploy.
- **Concurrent first deploys** race harmlessly — the loser sees `AlreadyExists`.
  No locks.
- **Removals need explicit code.** If a release drops a policy statement or
  resource, it ships a matching cleanup step. (This is the main thing given up by
  not using Pulumi here.)

### Version marker and fast path

Tag on the CodeBuild project: `anvil:compliance-version=<cli version>`.

| Marker              | Action                                                            |
| ------------------- | ----------------------------------------------------------------- |
| Missing             | Full converge (first-time create — **prompts**, see below)        |
| Older than this CLI | Full converge (upgrade: new Prowler digest, tiers, policy)        |
| Same or newer       | Skip entirely. An older CLI never downgrades; warn on a large gap |

### Bucket-name squatting

S3 names are global, so anyone who knows the account ID could pre-create
`anvil-compliance-<account>-<region>`. **Every S3 call passes
`ExpectedBucketOwner`.** If the name is owned by another account, fail with a
clear error — never write to it.

### Teardown

`anvil compliance teardown` deletes **by deterministic name** (no tag search —
the tagging API doesn't reliably return IAM roles). Tags are the **safety check**:
a same-named resource without `anvil:component=compliance` is skipped with a
warning.

- Refuses while the `anvil-compliance` schedule group contains any schedules
  (apps still registered).
- Keeps the results bucket unless `--delete-results` (which must empty all
  versions first).

## Per-app layer

`anvil:internal:ComplianceScanner` — a **provider component** defined once in Go
and the schema, so TS / Python / Go all get it from codegen. Namespaced
`internal`; `App` constructs it only when `compliance` is set. Users aren't
expected to construct it directly.

It creates **one EventBridge Scheduler schedule** in group `anvil-compliance`:

- **Target:** universal target `codebuild:startBuild` on the shared project, using
  the shared invoke role. ARNs are built from account + region — no cross-stack
  references.
- **Input:** the app's scope as environment variable overrides (below).
- **Retry policy** covers transient `StartBuild` failures (e.g. account build
  concurrency).
- **Lifecycle:** a normal Pulumi resource — added, updated and removed with the
  app.

Because each app owns its schedule, per-app settings **never conflict**.

### Build-mode manifest

In `ANVIL_BUILD_MODE`, the component writes a `compliance` section to
`.anvil/build-manifest.json` (same mechanism as Lambda discovery in
`cmd/anvil/cmd/build.go`). This tells the CLI **before deploy** that the shared
layer must exist, and gives `anvil compliance scan` the app's settings.

```json
"compliance": {
  "frameworks": ["soc2_aws", "iso27001_2022_aws", "cis_3.0_aws"],
  "schedule": "daily",
  "retention": "1y",
  "scanOnDeploy": false,
  "regions": ["ap-southeast-2"]
}
```

## The scan

### Scope (tags)

Every Anvil resource carries `ManagedBy`, `project`, `stage`. Prowler's
`--resource-tags` **ANDs** multiple tags, so each scan targets exactly one app:

```bash
prowler aws \
  --resource-tags ManagedBy=anvil project=testwaf stage=damienpace \
  --region ap-southeast-2 \
  --compliance soc2_aws iso27001_2022_aws cis_3.0_aws \
  --output-formats json-ocsf
```

Multiple apps in one account are isolated by `project` + `stage`. Scoping to the
app's regions is the biggest runtime win.

**`ManagedBy` coverage.** Every Anvil component sets `ManagedBy=anvil` on the
resources it creates. Only raw Pulumi resources added directly in `run()` lack it
(`App`'s auto tags are `{ stage, project }`), and those fall out of scope. That's
arguably correct — they aren't Anvil-managed — but it's worth stating in the user
docs. New components must keep setting it.

### Scanner script

`cmd/anvil/compliance/scan.py` — stdlib + boto3 only, so it runs in the
unmodified Prowler image. Resolves ARNs, runs Prowler with `--resource-arns`
and `--ignore-exit-code-3`, drops out-of-scope findings, writes findings →
manifest → `latest.json`. `ANVIL_RESULTS_DIR` swaps S3 for a local directory
(object tags written as `*.tags.json` sidecars). A failed scan still writes a
manifest (`status: failed`) but never moves `latest.json`. Run locally:

```bash
docker run --rm -v ~/.aws:/home/prowler/.aws:ro -v "$PWD/cmd/anvil/compliance":/scan:ro -v "$PWD/out":/results \
  -e ANVIL_PROJECT=testwaf -e ANVIL_STAGE=damienpace -e ANVIL_REGIONS=ap-southeast-2 \
  -e "ANVIL_FRAMEWORKS=soc2_aws iso27001_2022_aws cis_7.0_aws" -e ANVIL_RESULTS_DIR=/results \
  --entrypoint /home/prowler/.venv/bin/python prowlercloud/prowler:5.44.0 -B /scan/scan.py
```

First run on `testwaf`: 9 resources, 36 findings (16 pass / 20 fail), 4
account-level findings dropped, 57s.

### Build inputs

Passed as `environmentVariablesOverride` by both the schedule and the CLI:

| Variable           | Example                                  |
| ------------------ | ---------------------------------------- |
| `ANVIL_PROJECT`    | `testwaf`                                |
| `ANVIL_STAGE`      | `damienpace`                             |
| `ANVIL_REGIONS`    | `ap-southeast-2`                         |
| `ANVIL_FRAMEWORKS` | `soc2_aws iso27001_2022_aws cis_3.0_aws` |
| `ANVIL_RETENTION`  | `1y`                                     |
| `ANVIL_TRIGGER`    | `scheduled` \| `deploy` \| `manual`      |

**The buildspec validates every variable against a strict pattern** (and
frameworks/retention against allowlists) before it reaches the Prowler command
line. A crafted tag value must not become shell injection.

### Results layout

```
s3://anvil-compliance-<account>-<region>/
  results/<project>/<stage>/<scan-id>/     # scan-id = <utc timestamp>-<trigger>
    findings.ocsf.json
    manifest.json                          # written LAST
  results/<project>/<stage>/latest.json    # pointer to newest complete scan
```

- `manifest.json`: scope, regions, frameworks, Prowler version, status, duration.
  **A scan counts as complete only once its manifest exists** — the dashboard
  never reads a half-written result.
- Every object is tagged `retention=<tier>` at upload.

## Retention

Per-object, via **object tags + fixed lifecycle rules**. A bucket has one
lifecycle configuration, so per-app rules would clobber each other; tiers avoid
any cross-stack coordination.

- One rule per tier: `retention=30d|90d|180d|1y|2y|7y`. Adding a tier is cheap.
- Rules must include **`NoncurrentVersionExpiration`** and **expired delete marker
  cleanup** — on a versioned bucket, expiration alone only adds a delete marker
  and nothing is actually removed.
- Changing an app's retention **applies to new scans only**. Existing results
  keep their tier — shortening retention never suddenly deletes audit evidence.
- CloudWatch build logs: fixed **30 days**. Findings in S3 are the evidence; logs
  are for debugging.

## Compute: CodeBuild (no VPC)

Prowler calls AWS APIs across every scanned region, so it needs internet egress;
per-service, per-region VPC endpoints aren't practical.

|             | CodeBuild, no VPC (**chosen**)                                          | Fargate, public subnet                                               |
| ----------- | ----------------------------------------------------------------------- | -------------------------------------------------------------------- |
| Networking  | AWS-managed, egress included                                            | Needs a VPC (often deleted per CIS), IGW, subnets, routes, flow logs |
| Self-scan   | Clean                                                                   | Fails Security Hub **ECS.2** (public IP on tasks)                    |
| Footprint   | Project + role                                                          | Cluster, task def, 2 roles, SG, networking                           |
| Wrapper     | Inline buildspec around the upstream image — no custom image to publish | Custom image or sidecar                                              |
| Concurrency | `concurrentBuildLimit`                                                  | Build it yourself                                                    |
| Cost        | ~$0.01/min (medium) — a 20 min daily scan ≈ $6/mo                       | Cheaper per minute; irrelevant at this scale                         |

If a future org requires VPC-only compute, add `compliance: { network: 'vpc' }`
(CodeBuild-in-VPC + NAT) rather than changing the default.

**Gotcha:** new AWS accounts can have a low account-level CodeBuild concurrency
quota. Scans compete with any CodeBuild CI in the account. Document it; the CLI
reports "scan already running / throttled" rather than failing.

## Permissions and security

- **Scan role:** read-only (`SecurityAudit`, `ViewOnlyAccess`, Prowler's
  additional read-only policy, `tag:GetResources`) + `s3:PutObject` /
  `s3:PutObjectTagging` on `results/*` + KMS data key on the bucket key.
- **Invoke role:** `codebuild:StartBuild` on the one project. Assumable only by
  `scheduler.amazonaws.com` with an `aws:SourceAccount` condition.
- **`StartBuild` implies buildspec override** — any principal with it can run
  arbitrary commands as the scan role. Keep `StartBuild` limited to the invoke
  role and deployers. Investigate whether an IAM condition can deny buildspec
  overrides.
- **Image supply chain:** upstream Prowler image **pinned by digest**, bumped only
  with an Anvil release (keeps the dashboard parser and Prowler's output format
  in lockstep). Later: mirror into a private ECR repo (immutable tags, image
  scanning) for availability and rate limits.

## CLI

### Deploy flow

```
anvil deploy
  1. discovery          (existing)  → manifest, now incl. compliance
  2. build functions    (existing)
  3. ensure shared      (new)       only if manifest has compliance; fast path via version tag
  4. up app stack       (existing)  → creates/updates the app's schedule
  5. scan on deploy     (new)       only if scanOnDeploy → StartBuild, don't wait
                                    "Compliance scan started — run `anvil compliance` to view"
```

Step 3 must precede step 4: the schedule targets the shared project.

### First-time confirmation

Only on **first creation** of the shared layer (not upgrades):

```
  This app enables compliance scanning. No scanner exists yet in
  account 123456789012 (ap-southeast-2), so Anvil will create shared resources:

    • CodeBuild project + read-only scan role   anvil-compliance-123456789012-ap-southeast-2
    • Results bucket (encrypted, versioned)     anvil-compliance-123456789012-ap-southeast-2
    • Scheduler group + invoke role             anvil-compliance

  These are shared by every Anvil app in this account and region.
  Create them? (y/N)
```

- `--yes` skips the prompt (CI, scripts) — matching `destroy` / `unlock`.
- **Non-TTY without `--yes` → error**, never silent creation:
  `Compliance requires shared account resources. Re-run with --yes to create them.`
- No → abort before any change; hint to remove `compliance` or re-run with
  `--yes`.
- `CI=true` does **not** auto-accept. Creating account-wide resources should be
  an explicit decision in a pipeline.

### Commands

| Command                      | Does                                                                                                                                                                                                                      |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `anvil compliance dashboard` | Local dashboard on 127.0.0.1 (random port, per-run token): latest scan for this project/stage, scan picker, components from Pulumi state, changes vs the previous complete scan, Rescan. `--stage`, `--port`, `--no-open` |
| `anvil compliance scan`      | `StartBuild` scoped to this app using manifest settings. `--wait` streams progress; `--all` scans every registered app                                                                                                    |
| `anvil compliance status`    | Running builds, last scan per app (time, result, duration), next scheduled run                                                                                                                                            |
| `anvil compliance teardown`  | Remove the shared layer. Refuses while apps are registered. `--delete-results`                                                                                                                                            |

**Registry:** the schedules in group `anvil-compliance` _are_ the list of
registered apps — used by `status`, `scan --all`, and teardown's safety check.

### Lifecycle edge cases

| Change                                | Result                                                                                                 |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `compliance` added to an existing app | Next deploy ensures shared layer (prompt if new) and creates the schedule                              |
| Settings changed                      | Schedule updated in place                                                                              |
| `compliance` removed                  | Schedule removed; shared layer and past results remain                                                 |
| `anvil destroy`                       | Schedule removed; if the group is now empty, hint at `anvil compliance teardown` — never auto-teardown |

## Local dashboard

**Built:** `anvil compliance dashboard` (`cmd/anvil/cmd/compliance_dashboard.go`, page in
`cmd/anvil/compliance/dashboard/index.html`, data model in `compliance/view.go`). Verified on
`testWaf` against manual, deploy and scheduled scans. Scores are strict: a requirement passes only
when every mapped check passes. Cross-provider frameworks (DORA, CMMC, CSA CCM, CIS Controls,
FedRAMP 20x) aren't scored: Prowler doesn't attach their requirements to findings. "Powered by
Prowler" credit in the sidebar (name only, no logo).

A small static HTML/JS bundle **embedded in the binary** (`embed`), served by the
CLI. No Python, no Prowler locally.

- Binds **`127.0.0.1` only**, random port, **random token in the URL**. Opens the
  browser; Ctrl+C stops it.
- The CLI downloads findings with the user's AWS credentials and serves them via
  local `/api/*`. **The browser never sees AWS credentials.**
- Findings are a list of the account's weaknesses — **held in memory, not cached
  to disk**. (`.anvil/` is gitignored if caching is ever needed.)
- **Component mapping:** the CLI exports the app's Pulumi state (`s.Export`) and
  joins finding ARN → resource URN → parent component.

### Data

From Prowler (OCSF, per finding): check ID + title, status (PASS / FAIL / MANUAL
/ muted), status detail, severity, resource (ARN, name, type, service, region,
tags), risk + remediation + references, compliance requirement mappings, scan
metadata.

From Anvil: owning component, component inputs (to classify _why_), resources in
state vs. resources scanned (coverage), last deploy time (staleness).

### Views

v1:

1. **Overview** — score per framework, FAILs by severity, new/fixed since last
   scan, scan age (warn if older than last deploy), coverage %.
2. **By component** — component → resources → failing checks. Each failure
   classified **default** (Anvil's secure default didn't hold → Anvil bug) or
   **override** (user's explicit choice, e.g. `protection: 'none'` → accept or
   fix). The differentiator over Prowler's own dashboard.
3. **Finding detail** — everything above, plus first/last seen and mute reason.

Later: **By framework** (requirements with pass/fail/manual — MANUAL needs human
evidence), **History** (trends across scans). The results layout supports
history from day one.

Filters: severity, status, service, region, framework, stage (+ project with
`--all`).

## Decisions and rejected alternatives

| Decision | Rejected | Why |
| -------- | -------- | --- |
| Prowler runs in your account | Running Prowler from the CLI (installed, Docker, managed Python) | CLI size; a Python dependency; different results on different machines |
| CodeBuild without a VPC | Fargate in a public subnet | Needs a VPC, fails ECS.2 on itself, much more infrastructure |
| Shared scanner managed by direct AWS calls | A Pulumi stack with an account-level state bucket | A small fixed set of resources; no state to host, lock or orphan; bootstrap already works this way |
| Delete by name, tags as a safety check | Finding resources by tag search | The tagging API doesn't reliably list IAM roles; names are predictable anyway |
| One schedule per app | One account-wide scan split by tags | No settings conflicts; the schedule follows the app; similar total work |
| Collect ARNs, scan with `--resource-arns` | Prowler's `--resource-tags` | Tags miss IAM roles, which the tagging API doesn't return |
| Object-tag retention tiers | One bucket-wide rule ("longest wins") | Exact retention per app with no coordination |
| Settings via the discovery manifest | A hidden stack output read after deploy | Known **before** deploy, when the shared scanner must exist |
| `anvil:aws:ComplianceScanner` | `anvil:internal:ComplianceScanner` | `internal` clashes with the generated Go SDK and breaks the Python SDK patch script |
| Our own embedded dashboard | `prowler dashboard` | Prowler's needs Python and can't group by Anvil component |
| A local server for the dashboard | A static HTML file, a terminal UI, Security Hub, a hosted dashboard | Data stays on the machine, nothing to install or host, and it can read live results, switch scans and start rescans |
| `frameworks` required | Defaulting to SOC 2 + ISO 27001 | Explicit opt-in |
| Explicit Deny on data reads | Trusting the managed policies | Holds even if AWS widens `SecurityAudit` or `ViewOnlyAccess` |
| Results bucket versioned | Unversioned | Results can't be silently overwritten; required for Object Lock later |
| Strict requirement scoring | Share of passing checks | Honest: a requirement only passes if all its checks pass |
| Object Lock deferred | Shipping it now | Not needed yet; can be enabled on the existing bucket later |

## Test history

**Spike (2026-10-08),** Prowler 5.44.0 in local Docker against `testwaf`:

- Findings carry resource tags as `resources[].labels` (`"Key:Value"`), which
  makes per-app filtering possible.
- `--compliance` takes several frameworks; `--output-bucket` exists but can't tag
  objects, so the scanner uploads its own results.
- Exit code 3 means "checks failed", not an error.
- CloudFront, CloudFront-scoped WAF and Lambda@Edge only appear when `us-east-1`
  is scanned.
- IAM roles are invisible to tag-scoped scans; `--resource-arns` scans them.
- Runtime: 14–57 seconds for one app.

**Bugs found and fixed along the way:**

- Internal `us-east-1` providers didn't carry the app's default tags, so the WAF,
  edge Lambda and ACM certificate fell out of scans (and lost any user tags, and
  ignored `assumeRole`/`profile`). Fixed by setting `region: "us-east-1"` per
  resource on the app's own provider.
- Bootstrap resources had no tags. The state bucket now carries the app's tags;
  the logging bucket and trail carry `ManagedBy` and `Component`.
- The Lambda and DSQL components would have deleted other sections of the
  discovery manifest.
- Raw framework IDs not ending in `_aws` were wrongly rejected.
- EventBridge Scheduler rejected camelCase `StartBuild` parameters.

**End to end on `testWaf`:** shared scanner created on first deploy, a scan on
deploy (12:31), a scheduled scan (12:32), a manual scan, and the dashboard
showing all three with components mapped from 56 ARNs in Pulumi state.

**Still untested:** how CodeBuild handles a build beyond the concurrency limit
(queued or rejected); whether IAM can block build-file replacement on
`StartBuild`; why CloudFront-scoped WAFs get no findings.

## Future work

- **Dashboard:** stop after a period with no requests; document use over SSH and
  in remote dev environments (`--no-open` plus port forwarding); show "No
  applicable checks" instead of `0%` for frameworks with nothing to check; group
  requirements by section and level (e.g. CIS "2 Storage, Level 1"); region and
  service filters; scan history and trends.
- **Export:** `anvil compliance export --format csv|html` for auditors. Writes
  findings to disk, so explicit only.
- **Accepted exceptions (designed, deferred).** Accepted findings show as muted
  with a reason, never hidden. Two sources, merged into one Prowler mutelist at
  scan time:
  - _Anvil's own design choices:_ a mutelist shipped with Anvil, keyed by
    `Component=<type>` tags, plus small tags recording relevant inputs (e.g.
    `anvil:protection=none`).
  - _User exceptions:_ `compliance: { accept: [{ check, reason, until? }] }` on a
    component (its resources) or on `App` (the whole app). Recorded during
    discovery, passed back as stack config, and written by the component to
    `config/<project>/<stage>/accept.yaml`.
  - Needs every component to tag resources with `Component=<type>` and a
    logical-name tag.
- **`anvil destroy`** suggests `anvil compliance teardown` when the last app's
  schedule is removed.
- **Org mode** as a separate app type: a scanner in a delegated-admin security
  account (not the management account), a scan role in every member account via
  a StackSet, and the account-level checks that app scans leave out.
- **Object Lock** (`immutable: 'governance' | 'compliance'`), applied per object
  so only apps that opt in are locked. Possibly conditional writes so results
  can be created but never overwritten.
- **Coverage report:** Prowler's inventory compared with Pulumi state, to show
  resources that can't be tagged or were missed.
- **Failure notifications:** EventBridge on failed builds → SNS
  (`compliance: { notify }`).
- **A private ECR mirror** of the Prowler image.
- **GCP.**

## Sources

- [Prowler: tag-based scans](https://docs.prowler.com/user-guide/providers/aws/tag-based-scan)
- [Prowler: reporting and output formats](https://docs.prowler.com/user-guide/cli/tutorials/reporting)
- [Prowler: outputs developer guide](https://docs.prowler.com/developer-guide/outputs)
- # [DefectDojo: Prowler v3+ parser](https://documentation.defectdojo.com/integrations/parsers/file/aws_prowler_v3plus)
  | Scanner runs in-account | Run Prowler from the CLI (PATH / Docker / managed venv) | CLI size; Python dependency; inconsistent environments |
  | CodeBuild, no VPC | Fargate in a public subnet | Needs a VPC, fails ECS.2 on itself, much more infra |
  | Shared layer via direct SDK calls | Pulumi shared stack + account state bucket | Small fixed resource set; no state to host, lock or orphan; bootstrap already uses this style |
  | Delete by name, tags as safety check | Find by tag search | Tagging API doesn't reliably list IAM roles; names are already deterministic |
  | Per-app schedules | One account-wide scan split by tags | No settings conflicts; lifecycle follows the app; similar total work |
  | Object-tag retention tiers | Bucket-wide "longest wins" | Exact per-app retention with no cross-stack coordination |
  | Discovery manifest | Hidden stack output read after deploy | Known **before** deploy, when the shared layer must be ensured |
  | Custom embedded dashboard | `prowler dashboard` | Reintroduces Python; can't show Anvil component mapping |
  | Explicit `frameworks` | Default to SOC 2 + ISO | Opt-in only |
  | Object Lock deferred | Ship governance/compliance modes now | Not needed while testing; can be enabled later on the existing bucket |

## Spike results (2026-10-08)

Ran `prowlercloud/prowler:5.44.0` (digest `sha256:9dbcc3c9abd7…`) locally in
Docker against `testwaf` / `damienpace`:

```bash
prowler aws --resource-tags ManagedBy=anvil project=testwaf stage=damienpace \
  --region ap-southeast-2 [us-east-1] [--compliance soc2_aws iso27001_2022_aws cis_3.0_aws] \
  --output-formats json-ocsf
```

| #   | Question                                 | Result                                                                                                                                                                                                                                                   |
| --- | ---------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | Tags in OCSF output                      | ✅ `resources[].labels` as `"Key:Value"` strings, e.g. `["ManagedBy:anvil","project:testwaf","stage:damienpace","Component:SvelteKitSite"]`. Per-app split on `--all` works. Some resources also carry `Component:<type>` — useful for component mapping |
| 2   | Multiple `--compliance`                  | ✅ Accepts a list. All target IDs exist; CIS is now up to `cis_7.0_aws`, so pin newer than 3.0                                                                                                                                                           |
| 3   | Account-level checks                     | Mostly excluded by the tag filter. **One leaked:** `s3_account_level_public_access_blocks` (empty `labels`). The dashboard drops findings whose labels don't match the app                                                                               |
| 4   | CodeBuild at `concurrentBuildLimit`      | **Untested** (needs throwaway resources in the account)                                                                                                                                                                                                  |
| 5   | Direct-to-S3 output                      | ✅ `--output-bucket/-B` (and `-D`, no-assume). **But it can't tag objects**, and retention depends on object tags → the buildspec uploads itself (`put-object` with `Tagging`)                                                                           |
| 6   | IAM condition to deny buildspec override | **Untested**                                                                                                                                                                                                                                             |
| 7   | Runtime                                  | **14s** (3 frameworks, 1 region, 19 checks) / **23s** (all checks, 2 regions, 48 checks) for 3 resources. Cost is negligible; timeout can be far below 2h                                                                                                |

### New findings

- **Exit code 3 = FAIL findings present**, not an error. The buildspec must
  treat `0` and `3` as success.
- **Global services live in `us-east-1`.** CloudFront (and CloudFront-scoped WAF,
  Lambda@Edge) only appeared once `us-east-1` was added. Scan regions must be
  `app regions ∪ us-east-1`.
- **IAM roles are invisible to tag-scoped scans.** Prowler's tag filter uses the
  Resource Groups Tagging API, which returned exactly the 3 resources Prowler
  scanned — and not the stack's 2 IAM roles, despite correct tags. Option: the
  buildspec resolves ARNs itself (tagging API per region + IAM roles filtered by
  tag) and passes `--resource-arns` instead of `--resource-tags` (the two flags
  are mutually exclusive). **Needs a follow-up test** that IAM checks run under
  `--resource-arns`.
- **Anvil bug — `us-east-1` resources lack `project`/`stage` tags.** The WAF
  WebACL and edge-signer Lambda carry only `ManagedBy=anvil`. Internal us-east-1
  providers are created without `DefaultTags`
  (`provider/internal/awssite/util.go` `CreateUSEast1Provider`,
  `provider/aws/waf/waf.go`), so `App`'s default tags (incl. user `defaults.tags`)
  don't reach them. They fall out of every app-scoped scan. Fix independent of
  this feature.
- **First real findings against Anvil defaults** (see the spike output): the
  SvelteKitSite assets bucket fails `s3_bucket_secure_transport_policy`,
  `s3_bucket_object_versioning`, `s3_bucket_server_access_logging_enabled`,
  `s3_bucket_kms_encryption`; CloudFront fails `logging_enabled`; the site Lambda
  fails `using_cross_account_layers` (High), `no_dead_letter_queue`,
  `env_vars_not_encrypted_with_cmk`. Each needs a default-vs-intentional
  decision — exactly what the dashboard's default/override split is for.

### `--resource-arns` follow-up (after the us-east-1 tagging fix)

- The tagging API now returns the WAF WebACL, its log group and the edge Lambda
  (6 resources, up from 3); IAM roles still come from `iam:ListRoles` + tags.
- **`--resource-arns` scans IAM roles.** Both stack roles got findings
  (`iam_role_cross_service_confused_deputy_prevention`, High) — but only without
  `--compliance`: none of SOC 2 / ISO 27001 / CIS 7.0 map role-level checks.
- **WAF WebACL produced no findings** even with all checks (Prowler has
  `wafv2_webacl_logging_enabled`, `wafv2_webacl_with_rules`). Likely Prowler
  doesn't match CLOUDFRONT-scoped (`global/webacl`) ARNs. Open.
- More untagged account-level findings leak under `--resource-arns`
  (`iam_support_role_created`, `iam_securityaudit_role_created`, a CloudWatch
  metric-filter check). The "drop untagged findings" rule covers them.
- Runtime ~45s (IAM adds overhead).
- **Decision:** the buildspec resolves ARNs (tagging API per region + IAM roles
  by tag) and passes `--resource-arns`. Shell-quoting note: pass ARNs as
  separate arguments.

### Still to verify

1. CodeBuild behaviour at `concurrentBuildLimit` (queued vs rejected).
2. IAM condition to deny buildspec override on `StartBuild`.
3. Why CLOUDFRONT-scoped WAF WebACLs get no findings.

## Settled edge cases

- **Zero resources matched** (new stack, tagging-API index lag, tagging mistake):
  the manifest records `resources: 0`. No special handling in v1.
- **Untagged findings** (e.g. `s3_account_level_public_access_blocks`) are
  dropped from app reports — an app report contains only findings labelled with
  that app's `project` + `stage`. Account-level findings belong to org mode.

## Future work

- **Accepted exceptions (designed, deferred from v1).** Accepted findings are
  shown as MUTED with a reason, never hidden. Two sources, merged into one
  Prowler mutelist at scan time (Prowler mutelists match on resource tags):

  - _Anvil's own design choices_ — a mutelist shipped and versioned with Anvil,
    keyed by `Component=<type>` tags, plus small input tags (e.g.
    `anvil:protection=none`) for input-dependent cases.
  - _User exceptions_ — `compliance: { accept: [{ check, reason, until? }] }` on a
    **component** (applies to that component's resources) or on `App` (whole app;
    no `resource` field, so no string references). Components record accepts in
    the build manifest during discovery; the CLI passes them back as stack config
    (as `setFunctionArtifacts` does for Lambda zips); `ComplianceScanner` writes
    `config/<project>/<stage>/accept.yaml`. `check` is a plain string validated
    against the pinned Prowler check list ("did you mean …").
  - Needs every component to tag resources with `Component=<type>` and a logical
    name tag (e.g. `ComponentName=web`).

- **Org mode** as a separate app type (not a flag on `App`): scanner in a
  **delegated-admin security/audit account** (not the management account), scan
  role deployed to every member via a service-managed StackSet, Prowler assuming
  into each account. Includes account-level checks excluded from app scans.
- **Object Lock** (`immutable: 'governance' | 'compliance'`), applied per object
  at upload so only opted-in apps are locked.
- **Coverage report:** Prowler inventory vs. Pulumi state, to surface untagged /
  untaggable resources.
- **Failure notifications:** EventBridge on build `FAILED` → SNS
  (`compliance: { notify }`).
- **Export** (`anvil compliance export --format csv|html`) for auditors.
- **Private ECR mirror** of the Prowler image.
- **GCP** support.

## Sources

- [Prowler — tag-based scan](https://docs.prowler.com/user-guide/providers/aws/tag-based-scan)
- [Prowler — reporting / output formats](https://docs.prowler.com/user-guide/cli/tutorials/reporting)
- [Prowler — outputs developer guide](https://docs.prowler.com/developer-guide/outputs)
- [DefectDojo — Prowler v3+ parser (v3 `ResourceTags`)](https://documentation.defectdojo.com/integrations/parsers/file/aws_prowler_v3plus)
