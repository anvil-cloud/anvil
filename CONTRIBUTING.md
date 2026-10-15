# Contributing to Anvil

Thanks for wanting to help. Bug reports, docs fixes, new components and ideas are all welcome.

## Before you start

- **Small fixes** (typos, docs, obvious bugs): open a PR directly.
- **Larger changes** (new components, API changes, new CLI commands): open an issue or a [Discussion](https://github.com/anvil-cloud/anvil/discussions) first so we can agree on the shape before you write the code.
- Looking for somewhere to start? Try a [good first issue](https://github.com/anvil-cloud/anvil/labels/good%20first%20issue).

## Setup

**Prerequisites:** Go 1.25+, Node.js 18+, Python 3.9+, the Pulumi CLI, and an AWS account if you want to deploy what you build.

From wherever you keep your code:

```sh
git clone https://github.com/anvil-cloud/anvil.git
cd anvil
go run ./build build
export PATH="$PATH:$(pwd)/bin"
```

`go run ./build build` runs the full pipeline (generate → merge → registry → compile → SDKs). For CLI-only changes, `go run ./build binary` is much faster. See the build command table in the [README](README.md#build-commands).

## How the repo is laid out

| Path | What lives there |
| --- | --- |
| `cmd/anvil/` | The `anvil` CLI |
| `provider/<cloud>/<resource>/` | One component each: `schema.json` (its inputs and outputs) and the Go implementation |
| `provider/internal/` | Code shared between components |
| `scripts/` | Code generation, schema merge, registry and SDK scripts |
| `sdk/` | Generated SDKs, plus hand-written overlays in `sdk/overlays/` |
| `docs/` | Language guides and design docs |

### Adding a component

1. Create `provider/<cloud>/<resource>/` with a `schema.json` and the Go component.
2. Register it in `provider/cmd/anvil/main.go`.
3. Run `go run ./build build` to regenerate the merged schema and all three SDKs.
4. If the resource can be granted access to, add its grants (see `provider/aws/grant.go` and an existing `grants.go`, such as `provider/aws/queue/grants.go`).

Adding a **site framework** (Astro, Nuxt, Next.js…)? Follow [docs/design/adding-a-site-framework.md](docs/design/adding-a-site-framework.md) as well. It covers two traps that only show up after a real deploy: static files disappearing when they're edited, and server dependencies missing from the Lambda package.

Anvil's defaults are **secure first**. New components should start from the locked-down configuration and let users opt out. Anything that adds ongoing cost should be opt-in, with the cost noted in its description.

## Tests

From the repo root:

```sh
go test ./...
cd provider && go test ./...
```

If you changed a component, please deploy it once with a test app (`anvil deploy --stage <your-stage>`) and mention in the PR that you did.

## Pull requests

- Keep PRs focused on one change.
- Commit generated SDK changes alongside the source change that produced them.
- Describe what changed and how you tested it.

## License

By contributing, you agree that your contributions are licensed under the [Apache-2.0 License](LICENSE).
