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

Anvil wraps raw cloud resources into opinionated, production-ready components. Instead of a 200-line Terraform module or a Pulumi program full of copy-pasted security configuration, you declare what you need — a bucket, a function, a queue — and Anvil fills in the defaults a production system actually requires:

- **Public access blocked** and **encryption on** across storage, compute, and networking
- **Least-privilege IAM**, wired between resources with grants
- **Audit logging** where it matters
- **Enforced tagging**, so costs are attributable from day one
- **Aligned to SOC 2 and ISO 27001 controls** — not certified out of the box, but configured to do the heavy lifting when you're building toward compliance

Components start from the secure configuration and let you opt out or override, rather than starting from nothing and hoping you remembered everything.

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
import { App } from '@anvil-cloud/sdk';
import * as anvil from '@anvil-cloud/sdk';

export default new App({
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

`destroy` keeps protected resources that hold data (DSQL clusters, DynamoDB tables, Cognito user pools). Add `--force` to delete them too.

See the [Quickstart](https://anvilcloud.dev/docs/quickstart) for Python and Go.

## Permissions without writing IAM

Resources grant each other access. Anvil writes the least-privilege IAM policy, scoped to exactly the actions and paths you ask for:

```ts
run(ctx) {
  const uploads = new anvil.aws.Bucket('uploads');

  const resize = new anvil.aws.Lambda('resize', {
    runtime: 'nodejs22.x',
    entry: 'functions/resize/index.ts',
    handler: 'handler',
  });

  // Read-only, and only under incoming/
  uploads.grantRead(resize, ['incoming/*']);
},
```

No ARNs to wire, no `s3:*` policies left over from debugging.

## Compliance scanning built in

Add one block to your app and Anvil scans what you deployed against the frameworks you're working toward, using [Prowler](https://github.com/prowler-cloud/prowler):

```ts
export default new App({
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

## Three SDKs, one engine

Every component behaves identically across all three SDKs.

| Language   | Package                                             | Requirement |
| ---------- | --------------------------------------------------- | ----------- |
| TypeScript | `npm install @anvil-cloud/sdk`                      | Node.js 18+ |
| Python     | `pip install anvil-cloud`                           | Python 3.9+ |
| Go         | `go get github.com/anvil-cloud/anvil/sdk/go/anvil` | Go 1.25+    |

## Comparing Anvil

The same image upload stack built five ways.

| Framework      | Infra lines | Components | Manual permissions | Deploys with     |
| -------------- | ----------- | ---------- | ------------------ | ---------------- |
| **Anvil**      | **58**      | **4**      | **1**              | Pulumi engine    |
| CDK            | 74 (1.3×)   | 7 (1.8×)   | 1 (1.0×)           | CloudFormation   |
| CloudFormation | 134 (2.3×)  | 12 (3.0×)  | 4 (4.0×)           | CloudFormation   |
| Pulumi         | 150 (2.6×)  | 17 (4.3×)  | 6 (6.0×)           | Pulumi engine    |
| Terraform      | 194 (3.3×)  | 17 (4.3×)  | 7 (7.0×)           | Terraform engine |

## Components

**AWS**

| Category   | Components                                             |
| ---------- | ------------------------------------------------------ |
| Compute    | Lambda                                                 |
| Storage    | Bucket                                                 |
| Database   | DynamoDB, DSQL, DSQLConnect                            |
| Networking | Vpc, VpcEndpoint                                       |
| Messaging  | Queue, EventBus                                        |
| API & Auth | HttpApi, CognitoUserPool, CognitoAuth, OAuthAuthorizer |
| Hosting    | SvelteKitSite                                          |
| Security   | Waf, ComplianceScanner                                 |

**GCP** (early): StorageBucket, Function

Full arguments, outputs, and examples for each are in the [component docs](https://anvilcloud.dev/docs/components/aws/storage/bucket).

## Local development

**Prerequisites:** Go 1.25+, Node.js 18+, Python 3.9+, Pulumi CLI

```sh
git clone https://github.com/anvil-cloud/anvil.git
cd anvil
go run ./build build
```

Add `bin/` to your PATH to use the local provider:

```sh
export PATH="$PATH:$(pwd)/bin"
```

### Build commands

| Command                         | What it does                                                 |
| ------------------------------- | ------------------------------------------------------------ |
| `go run ./build build`          | Full pipeline: generate → merge → registry → compile → SDKs  |
| `go run ./build binary`         | CLI binary only (fast, for CLI-only changes)                 |
| `go run ./build build-provider` | Compile the provider binary                                  |
| `go run ./build install`        | Build and install `anvil` + the provider to `/usr/local/bin` |
| `go run ./build build-sdk`      | Generate + build the Node.js SDK                             |
| `go run ./build gen-python-sdk` | Generate Python SDK                                          |
| `go run ./build clean`          | Remove build artifacts                                       |

## Contributing

Contributions are welcome, from typo fixes to new components.

- Start with a [good first issue](https://github.com/anvil-cloud/anvil/labels/good%20first%20issue)
- Ask questions or share ideas in [Discussions](https://github.com/anvil-cloud/anvil/discussions)
- Read [CONTRIBUTING.md](CONTRIBUTING.md) for setup and how a component is put together

If Anvil saves you time, a ⭐ helps other people find it.

## License

[Apache-2.0](LICENSE)
