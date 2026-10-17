# @anvil-cloud/sdk

**Cloud infrastructure that's secure by default — not by accident.**

Anvil wraps raw cloud resources into opinionated, production-ready components. No 200-line Terraform modules. No copy-pasting security configs. Just declare what you need and Anvil handles the rest.

Built on [Pulumi](https://www.pulumi.com/).

## Install

```bash
npm install @anvil-cloud/sdk
```

## Secure by default

Every Anvil component ships with defaults aligned to production from day one — public access blocked, encryption enforced, cost tags applied. The goal isn't to make compliance automatic, but to make it a platform you can actually build on.

## The App class

Every Anvil program starts with `new App()`. The `run` callback receives a `Context` with:

- `ctx.stage` — current deployment stage (defaults to your OS username for dev isolation)
- `ctx.project` — project name from `anvil.yaml`
- `ctx.export(name, value)` — export stack outputs
- `ctx.providers` — named cloud providers for multi-region / multi-account
- `ctx.ref(id)` — reference a resource declared elsewhere (see below)

## Forward references (`ctx.ref`)

Normally you reference a resource through the variable it's assigned to, which
means it must be declared first. `ctx.ref('id')` lets you reference a resource by
its **logical name** — even one declared **later** in the program:

```typescript
import { App } from '@anvil-cloud/sdk';
import * as anvil from '@anvil-cloud/sdk';

export default new App({
  run(ctx) {
    const fn = new anvil.aws.Lambda('processor', {
      runtime: 'nodejs20.x',
      handler: 'index.handler',
      code: './src',
      environment: {
        // forward reference — `db` is declared below
        DSQL_ENDPOINT: ctx.ref<anvil.aws.DSQL>('db').endpoint,
      },
    });

    const db = new anvil.aws.DSQL('db', { /* ... */ });
  },
});
```

`ctx.ref('db').endpoint` resolves to the *same* output as `db.endpoint` — they're
interchangeable. The generic (`<anvil.aws.DSQL>`) is just for autocomplete.

This is an **escape hatch** for cases where declaration order is awkward; most
code references resources directly. Two things to know:

- **Keep references flowing one direction.** Because you can point either way, you
  can accidentally create a cycle (A references B's output *and* B references A's).
  That deadlocks the deploy rather than erroring cleanly — so don't do it.
- A typo or never-declared id fails with a clear error before anything deploys.

## Grants

Grants are how Anvil wires permissions between resources. Instead of writing IAM policies by hand, you call `.grant()` on a resource and Anvil handles both the IAM role policy and the environment variable injection automatically.

A Lambda reading from a Bucket:

```typescript
import { App } from '@anvil-cloud/sdk';
import * as anvil from '@anvil-cloud/sdk';

export default new App({
  run(ctx) {
    const bucket = new anvil.aws.Bucket('uploads', {
      dataClassification: 'sensitive',
    });

    const fn = new anvil.aws.Lambda('processor', {
      runtime: 'nodejs20.x',
      handler: 'index.handler',
      code: './src',
    });

    // Grants the Lambda read access to the bucket and scopes down to specific bucket paths
    // Anvil creates the IAM policy and injects UPLOADS_BUCKET_NAME
    // into the Lambda's environment automatically.
    //
    bucket.grant(fn, { actions: ['read'], path: ['user/*'] });
  },
});
```

What Anvil does under the hood:

- Creates an IAM `RolePolicy` scoped to the exact actions requested
- Injects the resource identifier as an environment variable on the target (e.g. `UPLOADS_BUCKET_NAME`)
- No manual ARN wiring, no forgotten permissions

## SvelteKit deployment

Deploy a SvelteKit app to AWS with a single component. Anvil provisions S3, CloudFront, ACM, Lambda (via Lambda Web Adapter), and Route53 — with HTTPS and a custom domain out of the box:

```typescript
import { App } from '@anvil-cloud/sdk';
import * as anvil from '@anvil-cloud/sdk';

export default new App({
  run(ctx) {
    const site = new anvil.aws.SvelteKitSite('web', {
      path: 'web', // the SvelteKit project folder — Anvil runs the build
      domain: 'myapp.com',
    });

    ctx.export('url', site.url);
  },
});
```

## Escape hatch

Every component accepts a `hatch` argument — an escape hatch to override the underlying resource config when you need to break from the defaults:

```typescript
const bucket = new anvil.aws.Bucket('custom', {
  dataClassification: 'non-sensitive',
  hatch: {
    bucket: { forceDestroy: true, tags: { env: 'dev' } },
  },
});
```

## Links

- [Docs](https://anvilcloud.dev)
- [GitHub](https://github.com/anvil-cloud/anvil)
- [Python SDK](https://pypi.org/project/anvil-cloud/)
- [Go SDK](https://pkg.go.dev/github.com/anvil-cloud/anvil/sdk/go/anvil)

## License

Apache-2.0
