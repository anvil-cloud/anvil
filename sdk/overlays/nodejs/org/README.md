# `Org` (TypeScript SDK)

`new anvil.Org({...})` manages an AWS Organization. The design lives in
[`docs/design/org/`](../../../../docs/design/org/README.md); this folder is the
implementation, one folder per feature so each is easy to find.

## Layout

```
org/
├── index.ts          Org class: plan every feature, then create in order, then outputs
├── config.ts         OrgConfig: one line per feature, types imported from the features
├── context.ts        ctx for run() (ctx.guardrails, ctx.ou, …)          (when run() lands)
├── discovery.ts      what the CLI's discovery run (ANVIL_BUILD_MODE) needs
├── shared/           used by several features, no feature logic          (as needed)
│   ├── errors.ts     config errors, "did you mean"
│   ├── naming.ts     resource names, tag keys
│   ├── policy.ts     policyDocument(), policyFile(), minify, size checks
│   └── providers.ts  management and per-account providers
├── ous/              04-ou-layout
├── accounts/         05-accounts (tiers, workloads, automation role, readiness, setup)
├── guardrails/       06-guardrails
│   └── rules/        one file per baseline rule (denyLeaveOrg.ts, allowedRegions.ts, …)
├── breakGlass/       02-break-glass (emergency account, OrganizationAccountAccessRole, alerts)
├── security/         07-security-services (cloudtrail.ts, logArchive.ts, guardduty.ts, …)
├── identityCenter/   08-identity-center (permissionSets.ts, assignments.ts, presets.ts)
└── notifications/    09-notifications (topic.ts, watch.ts)
```

Only `ous/` exists so far; the rest are added as each feature is built.

## Rules

- **Every feature folder has the same three files**, with plain names (the
  folder says which feature):
  - `config.ts`: the feature's public config types and their docs.
  - `plan.ts`: a pure `plan…` function that validates that part of the
    config and returns what to create. No AWS calls. Errors say exactly
    what to fix.
  - `create.ts`: a `create…` function that makes the resources from the plan.
  Extra files in a feature folder are for its sub-parts (e.g.
  `guardrails/pack.ts`, `security/guardduty.ts`).
- **`config.ts` at the top only assembles `OrgConfig`**, one line per
  feature, importing each feature's types.
- **`index.ts` holds no feature logic**: it plans every feature first (so a
  bad config fails in discovery, before any AWS call), then creates them in
  order and sets outputs.
- **Guardrail rules are one file each** in `guardrails/rules/`: the rule's
  statement, exemptions and docs together.
- **Python and Go mirror these folder and file names**, in each language's
  conventions, so a feature has the same name in all three SDKs.

The CLI side is `cmd/anvil/cmd/org*.go`: `org.go` (commands), `org_project.go`
(anvil.org.yaml, discovery), `org_preflight.go`, `org_state.go`, and more
`org_*.go` files as features need them.
