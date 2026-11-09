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
      handler: 'src/index.handler',
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

Grants are how Anvil wires permissions between resources. Instead of writing IAM policies by hand, you call a grant method on the resource being accessed, and Anvil creates a least-privilege IAM policy on the target's role.

A Lambda reading from a Bucket:

```typescript
import { App } from '@anvil-cloud/sdk';
import * as anvil from '@anvil-cloud/sdk';

export default new App({
  run(ctx) {
    const bucket = new anvil.aws.Bucket('uploads', {});

    const fn = new anvil.aws.Lambda('processor', {
      runtime: 'nodejs24.x',
      handler: 'src/index.handler',
      environment: { BUCKET_NAME: bucket.bucketName },
    });

    // s3:GetObject + s3:ListBucket on the bucket, scoped to user/*
    bucket.grantRead(fn, ['user/*']);
  },
});
```

What Anvil does under the hood:

- Creates an IAM `RolePolicy` on the target's role, scoped to the exact actions and resources
- No manual ARN wiring, no hand-written policy JSON

### Through a VPC endpoint

When a Lambda runs in a VPC and reaches an AWS service through an Anvil `VpcEndpoint`, pass the endpoint on the grant:

```typescript
const vpc = new anvil.aws.Vpc('core', {});
const net = { vpcId: vpc.vpcId, privateSubnetIds: vpc.privateSubnetIds };

const sqsEndpoint = new anvil.aws.VpcEndpoint('sqs', { ...net, service: 'sqs' });
const queue = new anvil.aws.Queue('orders', {});
const fn = new anvil.aws.Lambda('processor', {
  runtime: 'nodejs24.x',
  handler: 'src/index.handler',
  vpc: net,
});

queue.grantSendMessage(fn, { endpoint: sqsEndpoint });
```

The grant then also:

- adds ingress 443 on the endpoint's security group from the Lambda's own security group, and egress 443 on the Lambda's security group to the endpoint (once per Lambda and endpoint, however many grants share them);
- limits the IAM statement to that endpoint with `aws:SourceVpce`, so the permission doesn't work if the credentials are used from anywhere else.

The Lambda only ever carries its own security group, so compute sharing an endpoint can't reach each other. The endpoint's `service` must match the grant (`sqs` for a queue, `s3` for a bucket, and so on), or the grant fails at preview. With private DNS, every call to that service from inside the VPC resolves to the endpoint, so grant every Lambda that uses the service through it, even if the VPC also has NAT.

`dsql.grantConnect(fn, ...)` does the same on port 5432 when the cluster was created with `vpc` and without `hasNat`.

### Services without an Anvil component

For services like Secrets Manager, KMS, SSM, STS, ECR or CloudWatch Logs, use `grantEndpointAccess`:

```typescript
const secretsEndpoint = new anvil.aws.VpcEndpoint('secrets', { ...net, service: 'secretsmanager' });

anvil.grantEndpointAccess(fn, secretsEndpoint, {
  actions: ['secretsmanager:GetSecretValue'],
  resources: [dbSecretArn],
});
```

It opens the same security group rules as a resource grant. With `actions` and `resources`, it also adds an IAM statement limited to the endpoint with `aws:SourceVpce`. Every action must belong to the endpoint's service. Leave both out to open the network only, when the IAM permissions come from elsewhere.

### Allowing all outbound (opt-in)

Every security group Anvil creates (Lambda, VPC endpoint, DSQL endpoint) starts with no outbound rules: outbound is only what grants, `cidrs` and `hasNat` open. To put back AWS's default rule (all traffic to 0.0.0.0/0), opt in for the whole app or per resource. A resource's own setting wins, and `anvil preview` warns for every group it applies to. The VPC's default security group always stays locked.

```typescript
export default new App({
  defaults: {
    vpc: { allowAllOutbound: true },   // every Anvil security group
  },
  run(ctx) {
    // ...
    new anvil.aws.Lambda('payments', {
      runtime: 'nodejs24.x',
      handler: 'backend/payments.handler',
      vpc: { ...net, allowAllOutbound: false },   // this one stays locked
    });
  },
});
```

`VpcEndpoint` takes `allowAllOutbound` directly, and `DSQL` takes it in `vpc`.

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
