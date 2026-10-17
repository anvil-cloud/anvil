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

Grants are how Anvil wires permissions between resources. Instead of writing IAM policies by hand, you call `CreateGrant()` and Anvil handles both the IAM role policy and the environment variable injection automatically.

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
            bucket, err := anvilaws.NewBucket(ctx.PulumiCtx(), "uploads", &anvilaws.BucketArgs{
                DataClassification: pulumi.String("sensitive"),
            })
            if err != nil {
                return err
            }

            fn, err := anvilaws.NewLambda(ctx.PulumiCtx(), "processor", &anvilaws.LambdaArgs{
                Runtime: pulumi.String("nodejs20.x"),
                Handler: pulumi.String("index.handler"),
                Code:    pulumi.String("./src"),
            })
            if err != nil {
                return err
            }

            // Grants the Lambda read access to the bucket and scopes down to specific paths.
            // Anvil creates the IAM policy and injects UPLOADS_BUCKET_NAME
            // into the Lambda's environment automatically.
            return anvil.CreateGrant(ctx.PulumiCtx(), bucket, fn, anvil.GrantOptions{
                Actions: []string{"read"},
                Path:    []string{"user/*"},
            })
        },
    })
}
```

What Anvil does under the hood:

- Creates an IAM `RolePolicy` scoped to the exact actions requested
- Injects the resource identifier as an environment variable on the target (e.g. `UPLOADS_BUCKET_NAME`)
- No manual ARN wiring, no forgotten permissions

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
    Hatch: &anvilaws.BucketHatchArgs{
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
