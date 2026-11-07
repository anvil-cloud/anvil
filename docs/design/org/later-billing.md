# Org — Later: billing

Part of the [Org design](README.md). **Parked 2026-11-03, not in v1.** The
first build focuses on the core org; this is kept so the work isn't lost.
Budgets, freeze/stop and schedules were designed; billing exports and
anomaly detection were only sketched.

Known issues to resolve before building (Audit 3):

- **M11:** a freeze SCP attached by a budget action lives outside Pulumi;
  deleting the budget mid-period leaves it attached with nothing to reset
  it, and `DeletePolicy` fails while it's attached. Reverse executed actions
  and detach first; list the Budgets execution role and `iam:PassRole`.
- **M12:** `stop` and schedules fight Auto Scaling (stopped instances are
  replaced by a service-linked role the freeze doesn't block); RDS restarts
  itself after 7 days stopped.
- **L48:** moving an account down must clean up `anvil-budget-stop`,
  `anvil-scheduler` and its budget.

## Budgets

Opt-in, `billing.budgets`. Every budget is created in management, filtered
by linked account (budget actions that attach SCPs must run there, and it
keeps budgets out of reach of account admins). Amounts are set by where an
account lives, with per-account overrides; new accounts get their scope's
budget automatically. Accounts with no matching amount get no budget.

Each budget alerts, and can opt in to two actions at 100%:

| | When | Does | Doesn't |
|---|---|---|---|
| **Alert** (`alertAt`) | At each % | `warning` notification | Change anything |
| **Freeze** (`freeze: true`) | 100% | Budget action attaches a freeze SCP from management (denies create and modify; read, stop and delete still allowed, so the owner can clean up); `critical` notification. **Reset automatically by AWS at the start of the next period** | Stop what's already running |
| **Stop** (`stop: true`) | 100% | Switches off running EC2, RDS and Aurora in the account (budget forwarder → `anvil-budget-stop` role in that account) | Delete or terminate anything; disks and snapshots stay |

`stop` is Anvil's own: AWS budget actions can only stop instances in the
budget's own account, and every budget is in management. The role exists
only in accounts with `stop`, can only stop instances, and is protected by
`protectAnvilRoles`.

```ts
billing: {
  budgets: {
    enable: true,
    currency: 'USD',
    monthly: {
      sandbox:  { amount: 200, alertAt: [50, 80], freeze: true, stop: true },
      nonProd:  { amount: 500, alertAt: [80, 100], freeze: true },
      prod:     { amount: 3000, alertAt: [80, 100, 120] },
      security: { amount: 150, alertAt: [100] },
    },
    accounts: {
      'payments-prod': { amount: 8000, alertAt: [90, 100] },
    },
  },
},
```

```
+ budgets (management)   USD
    sandbox-alice, sandbox-bob     $200   alerts 50%, 80% · at 100%: freeze + stop
    website-dev, analytics-dev     $500   alerts 80%, 100% · at 100%: freeze
    website-prod, analytics-prod   $3000  alerts 80%, 100%, 120%
    payments-prod                  $8000  alerts 90%, 100% (override)
    log-archive, audit             $150   alert 100%
```

**`freeze` and `stop` are never allowed on Prod, Security or
Infrastructure**: freezing prod takes production down on a traffic spike,
and freezing Log Archive or Audit stops logging and security tooling.

```
✗ billing.budgets.monthly.prod.stop: stop isn't allowed on Prod accounts; use alertAt
```

- Scopes: `sandbox`, `nonProd`, `prod`, `security`, `infrastructure`;
  `accounts` overrides by name.
- Billing data lags up to ~a day, so a freeze can overshoot; `stop` caps it
  much closer than freeze alone.
- Alerts publish to the org notification topic (severity `warning`, freeze
  and stop `critical`) through the budget forwarder (below).
- Cost: to confirm on the Budgets pricing page (regular budgets believed
  free; action-enabled budgets free for the first two, then a small daily
  charge).
- *Watch:* AWS Spend Limits (launched September 2026): project-level caps
  that pause resources, console-only as reported, no Organizations or API
  support described, and data deleted after 90 days paused. Revisit when
  AWS documents an API and Organizations support.

### Budget notifications

AWS Budgets can only publish to an SNS topic **in the same account** as the
budget, and every budget is in management:

```
Budgets (management) → anvil-budget-alerts topic (management, KMS)
   └─ subscribed cross-account ─▶ forwarder Lambda (audit)
                                    adds severity / source / account / ou
                                    └─▶ anvil-org-notifications (audit)
```

- Centralised in Audit on purpose: one topic, one set of listeners, one UI.
- One topic in management and one small function in Audit; Budgets
  messages are plain text, so the forwarder adds the attributes listeners
  filter on.
- The management topic's policy allows only `budgets.amazonaws.com`
  (`aws:SourceAccount` = management) to publish and only the Audit function
  to subscribe.
- Sources when built: `budgets`; budget threshold `warning`, freeze and stop
  `critical`.

## Schedules

*Starting point; to flesh out when designed in detail.* Opt-in operating
hours per scope or account: outside them, EC2, RDS and Aurora are stopped,
and what Anvil stopped is started again at the start time.

```ts
billing: {
  schedules: {
    enable: true,
    scopes: {
      sandbox: { timezone: 'Australia/Sydney', days: 'weekdays', start: '08:00', stop: '18:00' },
      nonProd: { timezone: 'Australia/Sydney', days: 'weekdays', start: '07:00', stop: '20:00' },
    },
    accounts: {
      'payments-dev': { timezone: 'America/New_York', days: 'weekdays', start: '09:00', stop: '17:00' },
    },
  },
},
```

- A scheduled function per scheduled account, role `anvil-scheduler` that
  can only stop and start instances, protected by `protectAnvilRoles`.
- Checks every 15 minutes outside hours; only restarts what it stopped;
  opt out per resource with `anvil:schedule=always`.
- Stops rather than blocks: SCP conditions can't express a daily window.
- Never on Prod, Security or Infrastructure.
- *To design:* other resource types (ASG, ECS, SageMaker), holidays,
  notifications on stop/start, interaction with budget `stop`.

## Types (when built)

```ts
type Budget = { amount: number; alertAt?: number[]; freeze?: boolean; stop?: boolean };
type Schedule = { timezone: string; days: 'weekdays' | 'everyday' | string[]; start: string; stop: string };

billing?: {
  budgets?: {
    enable: true;
    currency: string;
    monthly: Partial<Record<'sandbox' | 'nonProd' | 'prod' | 'security' | 'infrastructure', Budget>>;
    accounts?: Record<string, Budget>;
  };
  schedules?: {
    enable: true;
    scopes?: Partial<Record<'sandbox' | 'nonProd', Schedule>>;
    accounts?: Record<string, Schedule>;
  };
  dataExports?: { enable: true; bucket: { account: string; name: string }; format: 'parquet' | 'csv' };
  anomalyDetection?: { enable: true };
};
```

## Billing exports and anomaly detection (sketch)

A `billing` block on Org so legitimate management-only billing work lives
in the one reviewed project that already deploys from management, and
billing data lands in a member account (finance never needs management
access). Until then, these are set up in the console.
