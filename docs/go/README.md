# anvil (Go SDK)

**Cloud infrastructure that's secure by default — not by accident.**

Anvil wraps raw cloud resources into opinionated, production-ready components. No boilerplate. No copy-pasting security configs. Just declare what you need.

Built on [Pulumi](https://www.pulumi.com/).

## Install

```bash
go get github.com/anvil-cloud/anvil/sdk/go/anvil
```

## Secure by default

Every Anvil component ships with defaults aligned to production from day one — public access blocked, encryption enforced, cost tags applied. The goal isn't to make compliance automatic, but to make it a platform you can actually build on.

## The App

Every Anvil Go program uses `anvil.Run()` with an `AppConfig`. The `Context` provides:

- `ctx.Stage()` — current deployment stage (defaults to your OS username for dev isolation)
- `ctx.Project()` — project name from `anvil.yaml`
- `ctx.PulumiCtx()` — the underlying Pulumi context for resource creation
- `ctx.Export(name, value)` — export stack outputs

## Grants

Grants are how Anvil wires permissions between resources. In Go you call `anvil.CreateGrant` with the actions and resource ARNs, and Anvil creates a least-privilege IAM policy on the target's role. Wrap a Lambda with `anvil.LambdaTarget`, passing the name you created it with.

A Lambda reading from a Bucket:

```go
package main

import (
    "github.com/anvil-cloud/anvil/sdk/go/anvil"
    anvilaws "github.com/anvil-cloud/anvil/sdk/go/anvil/aws"
    "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() {
    anvil.Run(anvil.AppConfig{
        Run: func(ctx *anvil.Context) error {
            pctx := ctx.PulumiCtx()
            bucket, err := anvilaws.NewBucket(pctx, "uploads", &anvilaws.BucketArgs{})
            if err != nil {
                return err
            }

            fn, err := anvilaws.NewLambda(pctx, "processor", &anvilaws.LambdaArgs{
                Runtime: anvilaws.LambdaRuntime_Nodejs24_x,
                Handler: pulumi.String("src/index.handler"),
            })
            if err != nil {
                return err
            }

            // s3:GetObject + s3:ListBucket on the bucket, scoped to user/*
            return anvil.CreateGrant(pctx, bucket, "uploads-processor-read",
                anvil.LambdaTarget("processor", fn),
                []string{"s3:GetObject", "s3:ListBucket"},
                anvil.BuildResourceArns(bucket.Arn, []string{"user/*"}),
                nil,
            )
        },
    })
}
```

What Anvil does under the hood:

- Creates an IAM `RolePolicy` on the target's role, scoped to the exact actions and resources
- No hand-written policy JSON

### Through a VPC endpoint

When a Lambda runs in a VPC and reaches an AWS service through an Anvil `VpcEndpoint`, pass the endpoint in `GrantOptions`:

```go
sqsEndpoint, err := anvilaws.NewVpcEndpoint(pctx, "sqs", &anvilaws.VpcEndpointArgs{
    VpcId:            vpc.VpcId,
    PrivateSubnetIds: vpc.PrivateSubnetIds,
    Service:          pulumi.String("sqs"),
})
// ...
err = anvil.CreateGrant(pctx, queue, "orders-processor-send",
    anvil.LambdaTarget("processor", fn),
    []string{"sqs:SendMessage"},
    []pulumi.StringInput{queue.Arn},
    &anvil.GrantOptions{Endpoint: anvil.Endpoint("sqs", sqsEndpoint, ctx.Provider("aws"))},
)
```

The grant then also:

- adds ingress 443 on the endpoint's security group from the Lambda's own security group, and egress 443 on the Lambda's security group to the endpoint (once per Lambda and endpoint, however many grants share them);
- limits the IAM statement to that endpoint with `aws:SourceVpce`, so the permission doesn't work if the credentials are used from anywhere else.

The Lambda only ever carries its own security group, so compute sharing an endpoint can't reach each other. All actions in an endpoint grant must belong to the endpoint's service (`sqs:*` actions for an `sqs` endpoint), or the grant fails. With private DNS, every call to that service from inside the VPC resolves to the endpoint, so grant every Lambda that uses the service through it, even if the VPC also has NAT.

For a DSQL cluster created with `Vpc` and without `HasNat`, call `anvil.ConnectToDSQL(pctx, "orders", cluster, target, ctx.Provider("aws"))` to open port 5432 to its endpoint. Pass the provider you created the endpoint or cluster with: security group rules are regional.

### Services without an Anvil component

For services like Secrets Manager, KMS, SSM, STS, ECR or CloudWatch Logs, use `anvil.GrantEndpointAccess`:

```go
err = anvil.GrantEndpointAccess(pctx,
    anvil.Endpoint("secrets", secretsEndpoint, ctx.Provider("aws")),
    anvil.LambdaTarget("processor", fn),
    &anvil.EndpointAccessOptions{
        Actions:   []string{"secretsmanager:GetSecretValue"},
        Resources: []pulumi.StringInput{dbSecretArn},
    },
)
```

It opens the same security group rules as a resource grant. With `Actions` and `Resources`, it also adds an IAM statement limited to the endpoint with `aws:SourceVpce`. Every action must belong to the endpoint's service. Pass `nil` options to open the network only, when the IAM permissions come from elsewhere.

## SvelteKit deployment

Deploy a SvelteKit app to AWS with a single component. Anvil provisions S3, CloudFront, ACM, Lambda (via Lambda Web Adapter), and Route53 — with HTTPS and a custom domain out of the box:

```go
func main() {
    anvil.Run(anvil.AppConfig{
        Run: func(ctx *anvil.Context) error {
            site, err := anvilaws.NewSvelteKitSite(ctx.PulumiCtx(), "web", &anvilaws.SvelteKitSiteArgs{
                Path:   pulumi.String("web"), // the SvelteKit project folder — Anvil runs the build
                Domain: pulumi.String("myapp.com"),
            })
            if err != nil {
                return err
            }

            ctx.Export("url", site.Url)
            return nil
        },
    })
}
```

## Escape hatch

Every component accepts a `Hatch` field — an escape hatch to override the underlying resource config when you need to break from the defaults:

```go
bucket, err := anvilaws.NewBucket(ctx.PulumiCtx(), "custom", &anvilaws.BucketArgs{
    DataClassification: pulumi.String("non-sensitive"),
    Hatch: &anvilaws.BucketHatchArgsArgs{
        Bucket: &anvilaws.BucketOverridesArgs{
            ForceDestroy: pulumi.Bool(true),
            Tags:         pulumi.StringMap{"env": pulumi.String("dev")},
        },
    },
})
```

## Requirements

- Go 1.22+
- Pulumi >= 3.0.0
- Anvil CLI: `curl -fsSL https://raw.githubusercontent.com/anvil-cloud/anvil/master/install.sh | sh`

## Links

- [Docs](https://anvilcloud.dev)
- [GitHub](https://github.com/anvil-cloud/anvil)
- [npm SDK](https://www.npmjs.com/package/@anvil-cloud/sdk)
- [PyPI SDK](https://pypi.org/project/anvil-cloud/)

## License

Apache-2.0
