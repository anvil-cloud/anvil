<p align="center">
  <a href="https://anvilcloud.dev">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="docs/assets/anvil-wordmark-steel.svg">
      <source media="(prefers-color-scheme: light)" srcset="docs/assets/anvil-wordmark-charcoal.svg">
      <img alt="Anvil" src="docs/assets/anvil-wordmark-charcoal.svg" width="280">
    </picture>
  </a>
</p>

<p align="center">
  <strong>Cloud infrastructure that's secure by default — not by accident.</strong>
</p>

<p align="center">
  TypeScript, Python, and Go SDKs built on <a href="https://pulumi.com">Pulumi</a>.
</p>

<p align="center">
  <a href="https://github.com/anvil-cloud/anvil/releases"><img alt="Release" src="https://img.shields.io/github/v/release/anvil-cloud/anvil?style=flat-square&color=FF6A1A"></a>
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/badge/license-Apache--2.0-17181A?style=flat-square"></a>
  <a href="https://anvilcloud.dev/docs/introduction"><img alt="Docs" src="https://img.shields.io/badge/docs-anvilcloud.dev-FF6A1A?style=flat-square"></a>
</p>

<p align="center">
  <a href="https://anvilcloud.dev/docs/quickstart"><strong>Get Started</strong></a> ·
  <a href="https://anvilcloud.dev/docs/introduction">Docs</a> ·
  <a href="https://anvilcloud.dev/docs/components/aws/storage/bucket">Components</a>
</p>

---

## A full-stack app in one file

A Postgres-compatible database, an API, and a website behind a firewall:

```ts
import * as anvil from '@anvil-cloud/sdk';

export default new anvil.App({
  run(ctx) {
    const db = new anvil.aws.DSQL('db', {
      schemas: [{
        name: 'shop',
        roles: [{ name: 'app_role', grants: ['SELECT', 'INSERT', 'UPDATE'] }],
        tables: [{
          name: 'orders',
          columns: [
            { name: 'id', type: 'uuid', notNull: true },
            { name: 'total', type: 'numeric', precision: 10, scale: 2 },
          ],
          primaryKey: ['id'],
        }],
      }],
    });

    const handler = new anvil.aws.Lambda('orders', {
      runtime: 'nodejs22.x',
      handler: 'functions/orders.handler',
      environment: { DSQL_ENDPOINT: db.endpoint },
    });
    db.grantConnect(handler, { schemas: [{ schema: 'shop', roleName: 'app_role' }] });

    const api = new anvil.aws.HttpApi('api', {
      routes: [{ method: 'GET', path: '/orders', consumer: { lambda: { arn: handler.arn } } }],
    });

    const waf = new anvil.aws.Waf('edge');
    new anvil.aws.SvelteKitSite('web', {
      path: 'web',
      runtimeEnvironment: { API_URL: api.url },
      waf: { arn: waf.arn },
    });
  },
});
```

**What `anvil deploy` gives you without being asked:**

- 🗄️ **Database:** schema, tables, and roles created from code. The Lambda connects as `app_role` with a short-lived IAM token, so there are no passwords and no admin credentials in your pipeline.
- 🔐 **Permissions:** least-privilege IAM written for you. The Lambda can connect to this cluster and nothing else.
- 🚦 **API:** rate limiting on every route, access logs kept for a year, and CORS off until you configure it (`*` is rejected).
- 🛡️ **Website:** CloudFront with AWS managed WAF rules, a server Lambda reachable only through CloudFront, and HSTS, `nosniff`, and frame-protection headers on every response.
- 🏷️ **Tagging:** every resource tagged with its project and stage, plus any tags you add, so costs are traceable from day one.

Each default is a starting point. You can opt out of or override any of them, which beats starting from nothing and hoping you remembered everything. The defaults are aligned to SOC 2 and ISO 27001 controls: that isn't a certification, but it does the heavy lifting when you're working toward one.

## Comparing Anvil

The same image upload stack, built five ways.

| Framework      | Infra lines | Components | Manual permissions | Deploys with     |
| -------------- | ----------- | ---------- | ------------------ | ---------------- |
| **Anvil**      | **58**      | **4**      | **1**              | Pulumi engine    |
| CDK            | 74 (1.3×)   | 7 (1.8×)   | 1 (1.0×)           | CloudFormation   |
| CloudFormation | 134 (2.3×)  | 12 (3.0×)  | 4 (4.0×)           | CloudFormation   |
| Pulumi         | 150 (2.6×)  | 17 (4.3×)  | 6 (6.0×)           | Pulumi engine    |
| Terraform      | 194 (3.3×)  | 17 (4.3×)  | 7 (7.0×)           | Terraform engine |

See all five implementations side by side at [anvilcloud.dev](https://anvilcloud.dev).

## Permissions without writing IAM

Resources grant each other access. Anvil writes the least-privilege IAM policy, scoped to the actions and paths you ask for:

```ts
run(ctx) {
  const uploads = new anvil.aws.Bucket('uploads');

  const resize = new anvil.aws.Lambda('resize', {
    runtime: 'nodejs22.x',
    handler: 'functions/resize/index.handler',
  });

  // Read-only, and objects only under incoming/
  uploads.grantRead(resize, ['incoming/*']);
},
```

<details>
<summary>The policy Anvil attaches to <code>resize</code></summary>

```json
{
  "Effect": "Allow",
  "Action": ["s3:GetObject", "s3:ListBucket"],
  "Resource": [
    "arn:aws:s3:::uploads-…",
    "arn:aws:s3:::uploads-…/incoming/*"
  ]
}
```

</details>

No ARNs to wire, no `s3:*` policies left over from debugging.

## Compliance scanning built in

Add one block to your app and Anvil scans what you deployed against the frameworks you're working toward, using [Prowler](https://github.com/prowler-cloud/prowler):

```ts
export default new anvil.App({
  compliance: {
    frameworks: ['soc2', 'iso27001', 'cis'],
    schedule: 'daily',
    scanOnDeploy: true,
  },
  run(ctx) {
    // ...
  },
});
```

```sh
anvil compliance scan        # scan this app's resources now
anvil compliance status      # latest results
anvil compliance dashboard   # open a local dashboard of findings
```

Supported frameworks include SOC 2, ISO 27001, CIS, NIST 800-53, PCI DSS, HIPAA, GDPR, NIS2, DORA, FedRAMP, and the ASD Essential Eight. Results stay in your own AWS account.

## Install Anvil

**macOS / Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/anvil-cloud/anvil/master/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/anvil-cloud/anvil/master/install.ps1 | iex
```

Installs `anvil` and the provider to `/usr/local/bin`. Works on Apple Silicon and Intel.

## Quickstart

```sh
anvil init --name my-app --lang ts
cd my-app
npm install
```

Declare a bucket in `anvil.config.ts`:

```ts
import * as anvil from '@anvil-cloud/sdk';

export default new anvil.App({
  defaults: {
    tags: {
      project: 'my-app',
      costCenter: 'platform-eng',
    },
  },
  run(ctx) {
    const uploads = new anvil.aws.Bucket('uploads');

    ctx.export('bucketName', uploads.bucketName);
  },
});
```

The bucket is private and encrypted without any extra arguments. Preview, deploy, and tear down:

```sh
anvil preview
anvil deploy
anvil destroy --stage <stage>
```

Every deploy goes to a stage. Without `--stage` it's your active stage (`anvil stage set <name>`), or your username if none is set, so each developer gets their own copy. `destroy` always needs `--stage` spelled out, so you can't tear down the wrong one by accident.

`destroy` keeps protected resources that hold data (DSQL clusters, DynamoDB tables, Cognito user pools). Add `--force` to delete them too.

See the [Quickstart](https://anvilcloud.dev/docs/quickstart) for Python and Go.

## Three SDKs, one engine

Every component behaves identically across all three SDKs.

| Language   | Package                                             | Requirement |
| ---------- | --------------------------------------------------- | ----------- |
| TypeScript | `npm install @anvil-cloud/sdk`                      | Node.js 18+ |
| Python     | `pip install anvil-cloud`                           | Python 3.9+ |
| Go         | `go get github.com/anvil-cloud/anvil/sdk/go/anvil` | Go 1.25+    |

## Components

**AWS**

| Category   | Components                                             |
| ---------- | ------------------------------------------------------ |
| Compute    | Lambda                                                 |
| Storage    | Bucket                                                 |
| Database   | DynamoDB, DSQL, DSQLConnect                            |
| Networking | Vpc, VpcEndpoint                                       |
| Messaging  | Queue, EventBus, KinesisStream                         |
| API & Auth | HttpApi, CognitoUserPool, CognitoAuth, OAuthAuthorizer |
| Hosting    | SvelteKitSite                                          |
| Security   | Waf, ComplianceScanner                                 |

**GCP** (early): StorageBucket, Function

Full arguments, outputs, and examples for each are in the [component docs](https://anvilcloud.dev/docs/components/aws/storage/bucket).

## Contributing

Contributions are welcome, from typo fixes to new components.

- Start with a [good first issue](https://github.com/anvil-cloud/anvil/labels/good%20first%20issue)
- Ask questions or share ideas in [Discussions](https://github.com/anvil-cloud/anvil/discussions)
- Read [CONTRIBUTING.md](CONTRIBUTING.md) to build from source and see how a component is put together

If Anvil saves you time, a ⭐ helps other people find it.

## License

[Apache-2.0](LICENSE)
