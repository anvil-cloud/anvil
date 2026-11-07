# Org — AWS Organizations with secure defaults

**Status:** Design mostly settled; first pass implemented (TypeScript `Org` with the `minimum` OU tree and custom OUs; `anvil org preview` / `deploy` with discovery, preflight and the Org state bucket). Not yet tried against a real organization. Decisions below are
agreed; sections marked *Open* are still being discussed.

`Org` is the multi-account counterpart to `App`. `App` deploys one project
into one account per stage; `Org` manages an AWS Organization following AWS
best practice (OUs, guardrail policies, org-wide security services). Both are
exposed in the TypeScript, Python and Go SDKs.

```ts
export default new Org({ ... });
```

## Config style

- A field is either a boolean or an object, never `boolean | {…}`.
- No blank objects. An object that switches something on carries
  `enable: true`; one that switches a default off carries `disable: true`.
  (`securityHubCspm: { enable: true }`, `ou: { Sandbox: { disable: true } }`,
  `'Partners/Acme': { enable: true }`.)

## Contents

- [Adoption and state](01-adoption-and-state.md)
- [Break glass](02-break-glass.md)
- [Destroy](03-destroy.md)
- [OU layout](04-ou-layout.md)
- [Accounts](05-accounts.md)
- [Guardrails](06-guardrails.md)
- [Security services](07-security-services.md)
- [Identity Center](08-identity-center.md)
- [Notifications](09-notifications.md)
- [Org dashboard](10-dashboard.md)
- [App ↔ account linking](11-app-linking.md)
- [API](12-api.md)
- [Later: billing](later-billing.md) (parked)
- [Reviews and audits](reviews.md) (latest: Audit 3; highs settled, quick fixes applied, rest noted for the build): history of the best-practice review, pain-point research, security review and audits, with each finding's decision

## Open topics

- Guardrails: instanceTypes default (off, or on with a built-in deny list)
- Account creation approval in CI: another way than routing by preview
  output (see Confirming account creation); includes Audit 3 H8 (required reviewers need
  GitHub Enterprise on private repos; limit environments to `main`)
- destroyUntagged deletion engine (own code or aws-nuke)
- Control Tower mode (later): `controlTower: true` to adopt orgs already on
  Control Tower. Touches ~9 topics (adoption, tiers/Security OU, OU
  registration, Account Factory, Suspended, guardrail slots and region deny,
  forced CloudTrail/Config, collisions, destroy) and needs a separate test org
  per cycle
- Org release workflow: git tags (`org-v1.3.0`), embedding previous baselines, version-bump CI check, auto-patch (see Baseline versions and upgrades)
- Billing (budgets, freeze/stop, schedules, data exports, anomaly detection): parked, not in v1 (see [Later: billing](later-billing.md))
- `anvil org dashboard`: page details per level
- Hosted dashboard (later): Identity Center sign-in, private/VPN-only option, backend read role; notification listener management and chat permissions (audit A10)
- App preview guardrail checks: which components check which guardrails (regions/edge-oac, instance types, services), beyond the shape in App ↔ account linking
