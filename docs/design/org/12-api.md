# Org — API

Part of the [Org design](README.md).

## API

Agreed shape (TypeScript; Python and Go mirror it in their own conventions,
as `App` does).

```ts
/** Every security service. delegatedAdmin defaults to audit; Security OU
 *  accounts only (not Log Archive). */
type Service = { enable: true; delegatedAdmin?: string };


/** From anvil.aws.policyDocument({...}) or anvil.aws.policyFile('./x.json'). */
type PolicyValue = anvil.aws.PolicyDocument;

/** Security Hub CSPM standards beyond the automatic fsbp + cis. */
type CspmStandard = 'tagging' | 'ai' | 'pci' | 'nist-800-53' | 'nist-800-171' | 'cri';

/** Exactly one of allowed or deny. */
type TypeRule = { allowed: string[] } | { deny: string[] };

interface OrgConfig<S extends string> {
  /** Which recommended accounts and OUs Org creates. Moving down suspends accounts (see Moving down a tier). */
  tier: 'minimum' | 'network' | 'sra';

  /** Email pattern for every account Org creates. Must contain {account}. */
  email: string;

  /** Stages, and the OU each one's accounts live in. */
  stages: Record<S, {
    ou: 'Prod' | 'NonProd';
    renamedFrom?: string;
  }>;

  /** Workload accounts: one per workload per stage. */
  workloads?: Record<string, {
    stages: S[];                    // only stages defined above (type-checked)
    email?: string;                 // pattern override, must contain {account}
    tags?: Record<string, string>;
  }>;

  /** Developer sandbox accounts, in the Sandbox OU. */
  sandboxes?: string[];

  /**
   * Sandbox toggle, plus custom OUs keyed by full path
   * ('Partners', 'Partners/Acme', 'Workloads/Prod/Data').
   */
  ou?: { Sandbox?: { disable: true }; PolicyStaging?: { enable: true } } & Record<string, {
    enable: true;
    tags?: Record<string, string>;
  }>;

  /** Accounts that aren't workloads or sandboxes. */
  accounts?: Record<string, {
    ou: string;                     // path of a built-in or custom OU
    renamedFrom?: string;           // also on workloads, stages and custom OUs
    email?: string;                 // overrides the org pattern
    tags?: Record<string, string>;
  }>;

  defaults?: {
    /** Tags on every account and OU Org creates. */
    tags?: Record<string, string>;
    state?: {
      retainVersions?: number;          // default 10 for Org
      encryption?: 'sse-s3' | 'kms';    // default 'sse-s3'
    };
  };

  /** Strengthening only: opt-in rules and their parameters. */
  guardrails?: {
    allowedRegions?: {
      regions: string[];
      /** Anvil's us-east-1 needs. acm, waf on; lambdaEdge off. */
      usEast1?: { acm?: boolean; waf?: boolean; lambdaEdge?: boolean };
    };
    /** Per service, each exactly one of allowed or deny. Default: open. */
    instanceTypes?: {
      ec2?: TypeRule; rds?: TypeRule; elasticache?: TypeRule; sagemaker?: TypeRule;
    };
    /** Not set: all allowed. Deny list only. */
    services?: { deny: string[] };
    /** Tag policy with report_required_tag_for on every supported service. Not set: no tag policy. */
    requireTags?: {
      keys: string[];
      values?: Record<string, string[]>;
      /** Opt-in scheduled cleanup. Named non-Prod accounts only. */
      destroyUntagged?: { accounts: string[]; schedule?: 'daily' | 'weekly'; graceDays?: number };
    };
    /** EC2 declarative policy; blocks public LBs unless excluded. */
    vpcBlockPublicAccess?: { enable: true; mode: 'block_ingress' | 'block_bidirectional'; exclusionsAllowed?: boolean };
    /** EC2 declarative policy. */
    allowedImages?: { enable: true; providers: string[]; mode?: 'enabled' | 'audit' };
    /** RCP: OIDC from GitHub only for these owners, org-wide. */
    githubOidc?: { owners: string[] };
  };

  /** Extra principals allowed to deploy (orgAdmin and the CI role always are). Exact ARNs pinned. */
  deployers?: { roles?: string[]; permissionSets?: string[] };

  /** Management-only CI roles for this Org project; least privilege generated. */
  ci?: { github: { repo: string; environment: string; accountsEnvironment?: string } };

  /** Org-wide security services. All off unless set, except cloudtrail (on by default). */
  security?: {
    cloudtrail?: { delegatedAdmin: string } | { disable: true; reason: string };
    accessAnalyzer?: Service;
    config?: Service & { recording?: 'continuous' | 'periodic' }; // default continuous
    guardduty?: Service & { plans?: { s3?: boolean; lambda?: boolean; rds?: boolean; eks?: boolean; runtime?: boolean; malwareEc2?: boolean; malwareS3?: boolean } };
    securityHub?: Service; inspector?: Service;
    macie?: Service; detective?: Service;
    /** enable = fsbp + cis. standards adds to them. */
    securityHubCspm?: Service & { standards?: CspmStandard[] };
    /** Log bucket for cloudtrail/config. */
    logArchive?: { retention?: string; accessLogRetention?: string; objectLock?: { enable: true }; kms?: { enable: true } };
  };

  // billing (budgets, schedules, data exports, anomaly detection): parked, see later-billing.md

  /** Org-wide Prowler from Audit. Same fields as App compliance. */
  compliance?: { enable: true; frameworks: string[]; schedule?: string; retention?: string };

  /** Instance set up manually in the console; Anvil manages permission sets. */
  identityCenter?: {
    /** Opt-in: Infrastructure OU account (SRA: Shared Services). */
    delegatedAdmin?: string;
    /** Preset names reserved; any other name is a custom set. */
    permissionSets?: Record<string, {
      enable: true;
      session?: string;                 // default '4h', 1h–12h
      managedPolicies?: string[];       // custom only
      inlinePolicy?: PolicyValue;       // custom only: policyDocument() or policyFile()
    }>;
    /** group name → permission set → exactly one target */
    groups?: Record<string, Record<string,
      | { accounts: string[] } | { stages: S[] } | { ou: string[] }
      | { all: true }>>;
    /** Management account: named users only (AWS). orgAdmin and billing only. */
    management?: { orgAdmin?: { users: string[] }; billing?: { users: string[] } }
      & Record<string, { users: string[] }>;   // plus sets in deployers.permissionSets

  };

  /** Watched changes, all on by default; opt out per area with a reason. Break-glass watching can't be disabled. */
  watch?: Partial<Record<'guardrails' | 'buckets' | 'cloudtrail' | 'identityCenter' | 'blockedAttempts' | 'keys',
    { disable: true; reason: string }>>;

  /** Remove undeclared policies, attachments and empty OUs. Off by default. */
  prune?: boolean;

  /** Weakening built-in rules and user additions (custom SCPs/RCPs). */
  run?: (ctx: OrgContext) => void;
}
```

### Example

```ts
import * as anvil from '@anvil-cloud/sdk';

export default new anvil.Org({
  tier: 'network',
  email: 'aws+{account}@acme.com',
  stages: { prod: { ou: 'Prod' }, staging: { ou: 'NonProd' }, dev: { ou: 'NonProd' } },
  workloads: {
    payments:  { stages: ['prod', 'staging', 'dev'], tags: { team: 'billing' } },
    analytics: { stages: ['prod', 'dev'],            tags: { team: 'data' } },
  },
  sandboxes: ['alice', 'bob'],
  defaults: { tags: { company: 'acme' } },
});
```

Deploy preview:

```
Org acme-org · management 111111111111 · tier network

  OUs
  + Security
  + Infrastructure
  + Workloads
  + Workloads/Prod
  + Workloads/NonProd
  + Sandbox
  + Suspended

  Accounts
  + log-archive        Security
  + audit              Security
  + emergency          Security
  + network            Infrastructure
  + payments-prod      Workloads/Prod
  + analytics-prod     Workloads/Prod
  + payments-staging   Workloads/NonProd
  + payments-dev       Workloads/NonProd
  + analytics-dev      Workloads/NonProd
  + sandbox-alice      Sandbox
  + sandbox-bob        Sandbox

  Accounts   1 in org + 11 new = 12 · quota 15 (raised beforehand; the default 10 would fail preflight)

  11 new accounts are permanent: their emails can never be reused,
  and closing an account takes 90 days.
  Type "yes" to create them: _
```

Result:

```
Root
├── (management account 111111111111, at Root; read only)
├── Security
│   ├── log-archive
│   ├── audit
│   └── emergency
├── Infrastructure
│   └── network
├── Workloads
│   ├── Prod
│   │   ├── payments-prod
│   │   └── analytics-prod
│   └── NonProd
│       ├── payments-staging
│       ├── payments-dev
│       └── analytics-dev
├── Sandbox
│   ├── sandbox-alice
│   └── sandbox-bob
└── Suspended
```

| Account | OU | Email | Tags |
|---|---|---|---|
| log-archive | Security | aws+log-archive@acme.com | `anvil:role=log-archive` |
| audit | Security | aws+audit@acme.com | `anvil:role=audit` |
| emergency | Security | aws+emergency@acme.com | `anvil:role=emergency` |
| network | Infrastructure | aws+network@acme.com | `anvil:role=network` |
| payments-prod | Workloads/Prod | aws+payments-prod@acme.com | `anvil:workload=payments` `anvil:stage=prod` `team=billing` |
| analytics-prod | Workloads/Prod | aws+analytics-prod@acme.com | `anvil:workload=analytics` `anvil:stage=prod` `team=data` |
| payments-staging | Workloads/NonProd | aws+payments-staging@acme.com | `anvil:workload=payments` `anvil:stage=staging` `team=billing` |
| payments-dev | Workloads/NonProd | aws+payments-dev@acme.com | `anvil:workload=payments` `anvil:stage=dev` `team=billing` |
| analytics-dev | Workloads/NonProd | aws+analytics-dev@acme.com | `anvil:workload=analytics` `anvil:stage=dev` `team=data` |
| sandbox-alice | Sandbox | aws+sandbox-alice@acme.com | `anvil:sandbox=alice` |
| sandbox-bob | Sandbox | aws+sandbox-bob@acme.com | `anvil:sandbox=bob` |

Every account and OU also gets `anvil:managed=true`, `anvil:org=<project>`
and `defaults.tags`.

### Full example: policies and permission sets

```
acme-org/
├── index.ts
└── policies/
    ├── regions.ts            # typed SCP
    ├── pci.ts                # typed SCP for the PCI OU
    ├── data-engineer.ts      # typed permission set policy
    ├── s3-org-only.json      # plain JSON RCP
    └── vendor-support.json   # plain JSON permission set policy
```

```ts
// policies/regions.ts
import * as anvil from '@anvil-cloud/sdk';

export const bedrockInUsEast1 = anvil.aws.policyDocument({
  statements: [{
    effect: 'Deny',
    actions: ['bedrock:*'],
    resources: ['*'],
    conditions: { StringNotEquals: { 'aws:RequestedRegion': ['us-east-1'] } },
  }],
});
```

```ts
// policies/pci.ts
import * as anvil from '@anvil-cloud/sdk';

export const pciControls = anvil.aws.policyDocument({
  statements: [
    {
      sid: 'DenyUnencryptedRds',
      effect: 'Deny',
      actions: ['rds:CreateDBInstance', 'rds:CreateDBCluster'],
      resources: ['*'],
      conditions: { Bool: { 'rds:StorageEncrypted': 'false' } },
    },
    {
      sid: 'DenyPublicSnapshots',
      effect: 'Deny',
      actions: ['rds:ModifyDBSnapshotAttribute', 'ec2:ModifySnapshotAttribute'],
      resources: ['*'],
    },
  ],
});
```

```ts
// policies/data-engineer.ts
import * as anvil from '@anvil-cloud/sdk';

export const dataEngineerPolicy = anvil.aws.policyDocument({
  statements: [
    { effect: 'Allow', actions: ['glue:*', 'athena:*'], resources: ['*'] },
    { effect: 'Allow', actions: ['s3:GetObject', 's3:PutObject'], resources: ['arn:aws:s3:::acme-data-*/*'] },
  ],
});
```

```ts
// index.ts
import * as anvil from '@anvil-cloud/sdk';
import { bedrockInUsEast1 } from './policies/regions';
import { pciControls } from './policies/pci';
import { dataEngineerPolicy } from './policies/data-engineer';

export default new anvil.Org({
  tier: 'minimum',
  email: 'aws+{account}@acme.com',
  stages: { prod: { ou: 'Prod' }, dev: { ou: 'NonProd' } },
  workloads: {
    website: { stages: ['prod', 'dev'] },
    analytics: { stages: ['prod', 'dev'] },
  },

  ou: { 'Workloads/Prod/PCI': { enable: true, tags: { compliance: 'pci' } } },
  accounts: {
    'payments-ledger': { ou: 'Workloads/Prod/PCI' },
    'payments-cards': { ou: 'Workloads/Prod/PCI' },
  },

  // config: strengthening only
  guardrails: {
    allowedRegions: { regions: ['ap-southeast-2', 'ap-southeast-4'] },
    requireTags: { keys: ['CostCenter', 'Owner'] },
  },

  security: {
    config: { enable: true },
    guardduty: { enable: true },
    securityHubCspm: { enable: true, standards: ['pci'] },
  },

  identityCenter: {
    permissionSets: {
      admin: { enable: true, session: '1h' },
      developer: { enable: true },
      readOnly: { enable: true },
      security: { enable: true },
      orgAdmin: { enable: true, session: '1h' },
      dataEngineer: {
        enable: true,
        managedPolicies: ['ReadOnlyAccess'],
        inlinePolicy: dataEngineerPolicy,
        session: '8h',
      },
      vendorSupport: {
        enable: true,
        inlinePolicy: anvil.aws.policyFile('./policies/vendor-support.json'),
      },
    },
  },

  // run: weakening built-in rules, and your own SCPs/RCPs
  run(ctx) {
    ctx.guardrails.protectBackups.except({ stages: ['dev'], reason: 'dev tears down its own data' });

    new anvil.aws.ServiceControlPolicy('bedrockUsEast1Only', {
      description: 'Bedrock models only in us-east-1',
      policy: bedrockInUsEast1,
      attach: [ctx.ou.workloads],
    });

    new anvil.aws.ServiceControlPolicy('pciControls', {
      description: 'Encryption and snapshot controls for card data',
      policy: pciControls,
      attach: [ctx.ou['Workloads/Prod/PCI']],
    });

    new anvil.aws.ResourceControlPolicy('s3OrgOnly', {
      description: 'S3 reachable only from inside the org',
      policy: anvil.aws.policyFile('./policies/s3-org-only.json'),
      attach: [ctx.ou.root],
    });

    new anvil.aws.ServiceControlPolicy('denyRoute53Domains', {
      description: 'Domain registration is done centrally',
      policy: anvil.aws.policyDocument({
        statements: [{ effect: 'Deny', actions: ['route53domains:*'], resources: ['*'] }],
      }),
      attach: [ctx.ou.workloads],
    });
  },
});
```

```
Org acme (tier minimum)

  Guardrails (config)
  + allowedRegions        Root        ap-southeast-2, ap-southeast-4 + global, acm, waf
  + requireTags           Root        CostCenter, Owner · every supported service

  Guardrails (run)
  + bedrockUsEast1Only    SCP  Workloads              ./policies/regions.ts       ✓ ValidatePolicy
  + pciControls           SCP  Workloads/Prod/PCI     ./policies/pci.ts           ✓ ValidatePolicy
  + s3OrgOnly             RCP  Root                   ./policies/s3-org-only.json ✓ ValidatePolicy
  + denyRoute53Domains    SCP  Workloads              inline                      ✓ ValidatePolicy

  ⚠ Guardrails weakened in run()
    protectBackups        except stage dev
                            website-dev (444444444444), analytics-dev (555555555555)
                            "dev tears down its own data"

  SCP slots
    Root                  4/10
    Workloads             3/10
    Workloads/Prod/PCI    2/10

  Identity Center         ap-southeast-2 · source Okta (external)
  + permission sets
      admin               AdministratorAccess                       1h
      developer           PowerUserAccess                           4h
      readOnly            ReadOnlyAccess                            4h
      security            SecurityAudit, ViewOnlyAccess + logs      4h
      orgAdmin            AdministratorAccess (management) − denies 1h
      dataEngineer        ReadOnlyAccess + inline (2 statements)    8h   ✓ ValidatePolicy
      vendorSupport       inline (./policies/vendor-support.json)   4h   ✓ ValidatePolicy
```

```
✗ ServiceControlPolicy bad (index.ts:96): Access Analyzer: Principal isn't supported
  in service control policies
```

| | Config | `run` |
|---|---|---|
| Built-in guardrails | Strengthen (`allowedRegions`, `requireTags`, …) | Weaken (`.except`, `.disable`, with a `reason`) |
| Your SCPs / RCPs | | `new ServiceControlPolicy` / `ResourceControlPolicy` (needs `ctx.ou`) |
| Permission sets | Presets and custom (no `ctx` needed) | |
| Policy source | `policyDocument()` inline or imported, or `policyFile()` | Same |

Source file shown per policy and SCP slot counts are illustrative; see
Guardrails → Policy size and slots for how Anvil packs the baseline.

### Outputs

```ts
org.accounts['payments-prod'].id   // "222222222222"
org.accounts['payments-prod'].ou   // "Workloads/Prod"
org.ou.prod.id                     // "ou-ab12-prod1234"
org.management.id                  // "111111111111"
```
