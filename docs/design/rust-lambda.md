# Rust Lambdas — `runtime: "rust"`

**Status:** Design agreed, **not implemented**. Builds on the existing
`anvil:aws:Lambda` component and the `anvil deploy` / `anvil generate` CLI flows.

## What it is

Rust as a first-class Lambda runtime. The user declares the function in their
Anvil config (TypeScript, Python or Go); Anvil scaffolds the Cargo package,
builds it with `cargo lambda`, and deploys it. The user never creates a file by
hand.

```ts
new anvil.aws.Lambda('api', { runtime: 'rust', handler: 'functions/api' });
new anvil.aws.Lambda('worker', { runtime: 'rust', handler: 'functions/worker' });
```

```
my-app/
  anvil.config.ts
  Cargo.toml            # [workspace] members = ["functions/api", "functions/worker", "gen/db"]
  Cargo.lock
  functions/
    api/                # user-owned: created once, never touched again
    worker/
  gen/
    db/                 # Anvil-owned: typed DSQL client, rewritten on every generate
```

The pain point this removes: in SST v3 a Rust Lambda meant hand-creating the
package, editing the workspace `members`, and wiring dependencies yourself.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Runtime value | **`runtime: "rust"`** → `provided.al2023`, handler `bootstrap` | AWS has no managed Rust runtime; Rust runs on the OS-only runtime as an executable named `bootstrap`. `"rust"` reads better and tells the CLI which builder to use |
| `handler` | **The one code setting, for every runtime**, in the usual Lambda form | The industry-standard name (SST, SAM). Node: `"functions/api/index.handler"`, the file without extension plus the export, so custom export names work. Rust: a path. No separate `entry` |
| `handler` for Rust | **Required** | No placeholder deploy for Rust, and scaffolding needs a location. Node keeps an optional `handler` + placeholder |
| What a Rust `handler` points at | **A package directory or a `.rs` file** | `cargo metadata` maps a `.rs` file to its exact bin target, including renamed `[[bin]]` targets. No `binaryName` field |
| Build tool | **`cargo lambda build`** | The standard tool: cross-compiles from macOS via Zig, names the output `bootstrap`, handles workspaces |
| Missing cargo-lambda | **Build in Docker** (`ghcr.io/cargo-lambda/cargo-lambda`, pinned by digest); error only if neither is available | Both CDK Rust constructs fall back to Docker the same way. Only the build runs in the container; a local Rust toolchain is required for `cargo metadata` and scaffolding |
| Release profile | **None generated**; cargo-lambda's `--release` defaults apply | It already sets `strip`, `lto = "thin"`, `codegen-units = 1`, `panic = "abort"`. A generated profile would only override them |
| Edition / resolver | **Edition 2024, `resolver = "3"`**, set once in `[workspace.package]` | AWS's own sample uses 2024; the virtual manifest needs an explicit resolver. Requires Rust ≥ 1.85 (`lambda_runtime` already needs 1.84) |
| Toolchain | **Generated `rust-toolchain.toml`, pinned version** + the Lambda targets | Consistent compiler between laptop and CI; rustup installs the cross-compile target automatically. User-owned, so editing it is the override |
| Architecture | **`arm64`** (unchanged Lambda default) | Graviton; `x86_64` still allowed |
| Change detection | **Always run cargo; Pulumi compares the zip** | Cargo's incremental build is near-instant when nothing changed and understands shared crates. No Anvil source hash for Rust |
| Workspace location | **Project root** (next to `anvil.config.*`) | Cargo convention: one workspace per repo, virtual manifest at the root. One lockfile and one `target/` regardless of where functions live; rust-analyzer finds it without setup |
| Scaffolding command | **Part of `anvil generate`**, also run by `anvil deploy` before building | Generate already means "make generated code match my infrastructure" and already runs discovery. A `cargo anvil` subcommand would only wrap Anvil, since the input is the Anvil config |
| Handler template | **One blank async handler**, with logging set up and a marked spot for clients | No trigger-specific templates; the user picks the event types. AWS recommends initialising logging and SDK clients once, outside the handler |
| DSQL client for Rust | **An Anvil-owned crate at `gen/db`**, in v1, **only when the project has a DSQL component with schemas** (and Rust Lambdas, or `--lang rust`) | Replaces today's loose `mod.rs` + "add these to your Cargo.toml" comment. Pre-release, so no migration. No schemas, no crate, and no `db` dependency on new Lambdas |

## Component changes (`provider/aws/lambda`)

- `runtime: "rust"` → `runtime: "provided.al2023"`, Lambda handler `bootstrap`.
- **`handler` is the only code setting.** For Node it is
  `"<file without extension>.<export>"`. The component passes `index.<export>`
  to Lambda (the bundle is always `index.js`), and the build manifest carries
  the path part to the CLI, which finds the file's extension: exactly one of
  `.ts`, `.js`, … must exist. A Node handler written as a file path
  (`"functions/api/index.ts"`) is an error suggesting `index.handler`. For Rust
  the handler is the path as-is.
- `runtime: "rust"` with no `handler` is an error, raised during discovery so it
  surfaces in `anvil generate` / `anvil deploy` before any build or AWS call:

  ```
  lambda "api": runtime "rust" requires handler, e.g. handler: "functions/api"
  ```

- Nothing else changes. IAM role, log group, function URL, VPC wiring, grants
  and `hatch` are all runtime-agnostic.
- The build manifest carries `runtime`, `handler`, the code path and
  `architecture`, which is everything the CLI needs.

## Resolving a Rust handler

The CLI runs `cargo metadata --format-version 1` from the handler's location and
matches against the packages' bin targets:

| `handler` | Resolves to |
|---|---|
| `functions/api` (package with one bin) | that bin |
| `functions/api/src/main.rs` | the bin whose `src_path` is that file |
| `functions/tools/src/bin/sync.rs` | the bin whose `src_path` is that file (works for renamed `[[bin]]` too) |
| `functions/tools` (package with several bins) | error, listing the choices |

```
lambda "api": functions/tools has 3 binaries (api, worker, sync).
  Point handler at one of them, e.g. handler: "functions/tools/src/bin/api.rs"
```

`cargo metadata` also returns the workspace root and `target_directory`, so
the CLI never guesses the layout.

## Build

Hooked in at `buildFunction` in `cmd/anvil/cmd/build.go`, next to the Node case
(previously anything non-Node was `unsupported runtime`). Implemented in
`cmd/anvil/cmd/bundler/cargo.go` (resolution, grouping) and
`cmd/anvil/cmd/bundler/aws/rust.go` (builders, zips).

1. Resolve every Rust function's package and bin (above).
2. **Group by (workspace root, architecture)** and run one build per group:

   ```
   cargo lambda build --release --target aarch64-unknown-linux-gnu \
     --manifest-path <root>/Cargo.toml --lambda-dir <target>/lambda/arm64 \
     --bin api --bin worker
   ```

   Cargo locks `target/`, so parallel per-function builds would only queue.
   One grouped run compiles shared dependencies once.
3. Take `<target_directory>/lambda/<arch>/<bin>/bootstrap` and zip it as
   `bootstrap`. The per-architecture directory keeps arm64 and x86_64 builds
   of the same bin apart, and never touches the user's own
   `cargo lambda build` output (`target/lambda/<bin>`).
4. Two packages with a binary of the same name in one workspace are an
   error: Cargo selects binaries by name, so it can't build them apart.
5. Hand the zip path to the provider through the existing `fn-<name>` config.
   A failed group fails every function in it; `--partial` deploys the rest.

### Which builder

| Available | Build runs |
|---|---|
| `cargo lambda` on PATH | locally (Zig cross-compile) |
| no cargo-lambda, Docker running | in `ghcr.io/cargo-lambda/cargo-lambda` |
| neither | error (below) |

A local Rust toolchain is required either way: `cargo metadata` (handler
resolution) and `cargo new` / `cargo add` (scaffolding) always run on the host.
This matches both CDK Rust constructs, which only move the build itself into
the container.

```
Rust Lambdas need cargo-lambda or Docker to build.
  Install cargo-lambda: https://www.cargo-lambda.info/guide/installation.html
  (or start Docker to build in a container)
```

### Docker build

```
docker run --rm \
  --user <uid>:<gid> -e HOME=/tmp \
  -v <workspace root>:/workspace -w /workspace \
  -v $CARGO_HOME/registry:/usr/local/cargo/registry \
  -v $CARGO_HOME/git:/usr/local/cargo/git \
  -e CARGO_TARGET_DIR=/workspace/target/docker \
  --entrypoint cargo \
  ghcr.io/cargo-lambda/cargo-lambda:1.9.2@sha256:aa011e79… \
  lambda build --release --target aarch64-unknown-linux-gnu \
    --lambda-dir /workspace/target/docker/lambda/arm64 --bin api --bin worker
```

- **Image pinned by digest,** bumped deliberately with Anvil releases. A
  floating tag would let an outside image change what gets compiled into the
  user's Lambdas.
- **Separate target dir** (`target/docker`). The container's Linux host
  artifacts and the Mac's would otherwise keep invalidating each other's
  cache. Output is read from `target/docker/lambda/<arch>/<bin>/bootstrap`.
- **`--entrypoint cargo`** so the command doesn't depend on the image's own
  entrypoint. The image is multi-arch (amd64 and arm64), so it runs natively
  on Apple silicon.
- **Registry and git caches mounted** from the host, so crates aren't
  re-downloaded on every build.
- **`--user`** so files in `target/` aren't owned by root on Linux hosts;
  `HOME=/tmp` gives that user somewhere to write Zig's cache.
- **Toolchain (still to verify with Docker running):** whether the image
  honours `rust-toolchain.toml`. If it ships rustup it will try to install
  the pinned toolchain as the non-root `--user`, which may fail or need its
  own cache mount.

### Deterministic zip

Pulumi decides whether the code changed by hashing the **zip** (CDK hashes the
binary before zipping). So the zip must be byte-identical when the binary is:

- fixed modification time on every entry
- `bootstrap` written with Unix mode `0755` (today `bundler.ZipFile` uses
  `w.Create`, which sets no mode bits, and Lambda would fail to execute it)

With that, an unchanged binary means no Lambda update, exactly like CDK's
`assetHashType: OUTPUT`.

### Not reused from the Node path

- `bundler.NeedsRebuild` / `HashDirectory`: they don't exclude `target/` and
  can't see changes in shared workspace crates. Cargo decides instead.
- `BlankBundle`: Rust always has a handler.

## Scaffolding

Runs inside `anvil generate` and as a step in `anvil deploy` (see CLI flow). It
reads the Rust functions from the build manifest. `anvil preview` never writes
files.

### Rules

1. **Create only.** If the handler's path already exists (a directory with a
   `Cargo.toml`, or the `.rs` file), skip it. The only edit to an existing file
   is adding a **new** package to the workspace `members`.
2. **Never delete.** Removing a Lambda from the config leaves its code.
3. **Workspace:**
   - an existing `[workspace]` at or above the handler's path: **join it**
   - none: **create one at the project root**
   - the project root `Cargo.toml` is a plain `[package]`: **stop** with a
     message. Converting a user's package is never done automatically
4. **An existing package outside the workspace's `members`:** left alone.
   It's user code, so no automatic edit; Cargo's own error at build time
   ("believes it's in a workspace when it's not") names the fix.
5. **A `.rs` handler inside an existing package:** create just the file. Cargo
   discovers `src/bin/*.rs` on its own. With no package above it, stop and
   suggest a directory handler instead (Anvil won't guess where a package
   should go).
6. **An existing directory without `Cargo.toml`** becomes a package, but a
   `src/main.rs` already in it is kept.
7. **`gen/db` is never scaffolded over.** It's Anvil-owned and handled by the
   DSQL generator.

### Package names

A new package is named after the Lambda, kebab-cased (`testRust` →
`test-rust`), not after its directory. A handler directory can then be called
anything, even a name Cargo rejects as a package (`test`, `std`, a Rust
keyword). A Lambda whose own name is reserved, or is `db` (the generated
client), gets a `-fn` suffix.

### How files are written

TOML edits go through Cargo, which preserves comments and formatting:

- `cargo init --bin --vcs none <handler path>`: creates the package and adds
  it to the enclosing workspace's `members`, inheriting `edition` and
  `rust-version` from `[workspace.package]`. `init` rather than `new` so an
  existing directory (and any `src/main.rs` in it) works.
- `cargo add lambda_runtime tokio serde_json`: in Anvil's own workspace this
  writes `{ workspace = true }`; when joining a user's workspace it also
  passes `--features lambda_runtime/tracing,tokio/macros`, since that
  workspace may not define them.

Implemented in `cmd/anvil/cmd/scaffold`; verified with real cargo
(`ANVIL_RUST_E2E=1`): scaffold two Lambdas into an empty project, build
them, and a second run creates nothing.

Anvil writes directly only the files it creates from nothing: the root
`Cargo.toml`, `rust-toolchain.toml` and `src/main.rs`. It also adds `/target`
to `.gitignore`. **`Cargo.lock` is committed**, never ignored: the Cargo team's
guidance is to commit it by default, and for deployed binaries it pins exactly
what ships.

### Generated workspace manifest

```toml
[workspace]
resolver = "3"
members = ["functions/api"]

[workspace.package]
edition = "2024"
rust-version = "<pinned toolchain version>"

[workspace.dependencies]
lambda_runtime = { version = "…", features = ["tracing"] }
tokio = { version = "1", features = ["macros"] }
serde = { version = "1", features = ["derive"] }
serde_json = "1"
```

- Packages inherit with `edition.workspace = true`,
  `rust-version.workspace = true` and `{ workspace = true }` dependencies, so
  versions are bumped in one place.
- **No `[profile.release]`.** `cargo lambda build --release` already applies
  `strip`, `lto = "thin"`, `codegen-units = 1` and `panic = "abort"`; anything
  generated here would only override those. Users can still add a profile
  themselves.
- `resolver = "3"` prefers dependency versions compatible with the workspace's
  `rust-version`. A virtual manifest has no edition, so it must be explicit.

### Generated `rust-toolchain.toml`

```toml
[toolchain]
channel = "<pinned version, e.g. 1.xx.0>"
components = ["rust-analyzer", "rustfmt", "clippy"]
targets = ["aarch64-unknown-linux-gnu", "x86_64-unknown-linux-gnu"]
```

- **`components`** keeps editor tooling on the pinned version. Without
  `rust-analyzer` here, the VS Code extension warns that the pinned toolchain
  is too old for its bundled rust-analyzer; rustup installs the listed
  components on first use.

- **Pinned,** so laptop and CI compile with the same Rust. Anvil pins the
  host's current `rustc` version (already installed, and what the user
  develops with) when it creates the file, and never bumps it afterwards.
  Older than 1.85 is an error asking for `rustup update`.
- **`targets`** makes rustup install the cross-compile targets automatically;
  otherwise the first build fails until the user installs them.
- **Overriding:** the file is user-owned (create-only), so editing `channel`
  (to a newer version, or `"stable"`) is the override and sticks. rustup's
  one-off overrides (`cargo +nightly …`, `RUSTUP_TOOLCHAIN`) also work.
- Not created when the workspace already exists (join case): the user's own
  toolchain setup wins.

### Handler template

A blank async handler. JSON in, JSON out; the user swaps in event types from
`aws_lambda_events` or `lambda_http` as needed. Logging is initialised once
(it follows Lambda's log level settings), and the spot for SDK clients and
connections is marked, since AWS recommends creating them outside the handler
so they're reused across invocations.

```rust
use lambda_runtime::{run, service_fn, tracing, Error, LambdaEvent};
use serde_json::{json, Value};

async fn handler(event: LambdaEvent<Value>) -> Result<Value, Error> {
    let (_payload, _context) = event.into_parts();
    Ok(json!({ "ok": true }))
}

#[tokio::main]
async fn main() -> Result<(), Error> {
    tracing::init_default_subscriber();

    // Create SDK clients and DB connections here, once per cold start,
    // and pass them into the handler.

    run(service_fn(handler)).await
}
```

New packages get `db = { path = "<relative>/gen/db" }` when the project has
DSQL schemas. Existing packages are never edited to add it.

## DSQL client as a crate (`gen/db`)

`codegen/rust.go` used to write `gen/<schema>.rs` + `mod.rs`, with a comment
listing the dependencies (and the tokio-postgres features) to copy into
`Cargo.toml` by hand. It now writes a complete crate:

```
gen/db/
  Cargo.toml     # deps + features computed from the schema (the logic behind today's comment)
  src/lib.rs     # pub mod <schema>;
  src/<schema>.rs
```

- **Anvil-owned:** fully rewritten on every generate, marked "do not edit".
- **Generated whenever the project has Rust Lambdas and DSQL schemas,**
  regardless of the saved `--lang` targets. A TypeScript config with Rust
  Lambdas still auto-detects `typescript` for its own types; `gen/db` is
  produced alongside. `--lang rust` also produces the crate.
- A workspace member, so it compiles once for every Lambda that uses it. The
  first generate runs `cargo init --lib` on it (which adds it to `members`,
  creating the root workspace if needed), then the generator replaces the
  files.
- Adding a `uuid` column and regenerating adds the right feature. No more
  trait-bound compile errors from a stale `Cargo.toml`.
- Output is deterministic (schemas in sorted order). A module whose schema
  was removed is deleted on regenerate; only files carrying the generated
  header are removed, so hand-added files in `src/` survive.
- New Lambdas get `db = { path = "…/gen/db" }` via `cargo add --path`.
  Whether a Lambda's `cargo add` needs `--features` is decided by reading the
  workspace manifest (does it already define `lambda_runtime` with
  `tracing` and `tokio` with `macros`?), not by who created it.
- Verified with real cargo (`ANVIL_RUST_E2E=1`): an empty project with a
  schema and two Rust Lambdas gets the workspace, `gen/db` and both packages,
  and the generated client compiles against the real `tokio-postgres`.

## CLI flow

### `anvil generate`

Does two jobs and labels each line:

```
  created Cargo.toml                 (workspace)
  created functions/api/
  updated gen/db/
  updated gen/                       (TypeScript types)
  updated .gitignore
```

- Scaffolding is stage-independent and ignores the saved per-stage targets.
  Those keep controlling only the DSQL languages and paths.
- "no database schemas found" becomes "nothing to generate", raised only when
  there are neither schemas nor Rust Lambdas.

### `anvil deploy`

Generation moves **before** the build:

```
1. discover    (unchanged — the one build-mode preview)
2. generate    scaffold missing Rust packages, write gen/ and gen/db
3. build       Node via esbuild, Rust via cargo lambda
4. deploy
```

The refresh used to run after the deploy. Moving it is required for Rust: a
new Lambda depends on `gen/db`, which must exist before its first build. It
also fixes a Node issue: the generated TS includes runtime query helpers, and
a Lambda importing them was bundled before the refresh, so the deploy that
changed a schema shipped the previous helpers. Schemas come from the config
(discovery), not deployed state, so generating first is safe.

Because it now happens before the build, **a generation failure stops the
deploy** (it used to be a warning after a finished deploy): shipping code
built against stale generated types is worse than stopping.

## End-to-end

1. Declare `new anvil.aws.Lambda('api', { runtime: 'rust', handler: 'functions/api' })`.
2. `anvil generate` (or straight to `anvil deploy`): workspace, package, blank
   handler and `gen/db` appear.
3. Write the handler. Add crates with `cargo add`.
4. `anvil deploy`: builds and ships.

Daily Rust work uses Cargo unchanged (`cargo build`, `cargo test`,
`cargo clippy`, `cargo lambda watch`). Anything Anvil generates must keep
working with those.

**One-time machine setup:** the Rust toolchain (rustup), plus either
cargo-lambda or Docker. cargo-lambda's installers (e.g.
`brew install cargo-lambda/tap/cargo-lambda`) also install Zig. cargo-lambda is
no longer published to crates.io, so `cargo install cargo-lambda` (still shown
in AWS's docs) is out of date; messages link to the installation page instead.

## Known trade-offs

- **Feature unification.** A grouped build merges dependency features across
  the bins in it; if one Lambda enables `tokio/full`, the others built with it
  get it too. Slightly larger binaries, accepted for the faster build.
- **Non-reproducible binaries** (toolchain bump, laptop vs CI paths) cause an
  unnecessary redeploy. Harmless. `--remap-path-prefix` can reduce it if it
  ever matters.
- **Root `Cargo.toml` and `target/`** sit in a mostly non-Rust project root.
  `target/` is gitignored.
- **`panic = "abort"`** (cargo-lambda's release default): a panic ends the
  process, so the next request is a cold start instead of an error response.
- **Native (`*-sys`) dependencies.** Zig handles pure Rust and most C deps;
  crates like `diesel` or OpenSSL-backed `reqwest` can fail. cargo-lambda's
  answer is `--compiler cross` (future work below).

## For the user docs

- `AWS_LC_SYS_NO_JITTER_ENTROPY = "1"` in `.cargo/config.toml` cuts cold-start
  time for functions using the AWS SDK (via `aws-lc-rs`), at the cost of
  dropping AWS-LC's extra entropy source and relying on the OS RNG alone.
  **Anvil never sets it.** Document it as a user opt-in only.
- Prefer `rustls` features over OpenSSL (`native-tls`) to avoid native
  dependency build problems.

## Implementation order

Each step is its own commit and testable alone; one branch, since 3 and 4 share
the workspace code.

1. Deterministic zip (`0755`, fixed timestamps); `rust` → `provided.al2023` in
   the component; required-`handler` validation.
2. Rust build: `cargo metadata` resolution, grouped `cargo lambda` runs,
   builder selection (local cargo-lambda, then Docker), the pinned image.
3. Scaffolder: workspace detection/creation, `rust-toolchain.toml`,
   `cargo new` / `cargo add`, blank handler, create-only rules; wired into
   `anvil generate` and deploy step 2.
4. `gen/db` crate in `codegen/rust.go`, reusing step 3's workspace code; move
   generation before the build in `deploy.go`.

Test on a two-member workspace with DSQL from the start, so grouping, path
mapping and the `db` dependency are exercised, not just a single crate.

## Future work

- `compiler: "cross"` for crates with native dependencies Zig can't handle.
- Build options: profile, extra `cargo lambda` flags.
- A `logFormat: "json"` Lambda option; the template's default subscriber
  already follows Lambda's logging controls, so Rust needs no code change.
- Optional `binaryName` alias if users coming from CDK ask for it.
- Rust Lambda extensions as layers.

## Prior art

- [`cargo-lambda-cdk`](https://github.com/cargo-lambda/cargo-lambda-cdk)
  (`RustFunction`): `manifestPath` + optional `binaryName`, `provided.al2023`,
  local cargo-lambda with Docker fallback, output-hash change detection. It
  builds per function (CDK bundles each asset separately); Anvil groups per
  workspace because discovery sees every function up front.
- [`cdklabs/aws-lambda-rust`](https://github.com/cdklabs/aws-lambda-rust)
  (`RustFunction`, experimental): `entry` + optional `binaryName`, cargo-lambda
  with Zig locally, Docker when cargo-lambda is missing, optional `cross`.
- SST v3: Rust support with manual package and workspace management, which is
  what the scaffolder removes.

## Sources

- [AWS: Rust handlers and best practices](https://docs.aws.amazon.com/lambda/latest/dg/rust-handler.html),
  [packaging](https://docs.aws.amazon.com/lambda/latest/dg/rust-package.html),
  [logging](https://docs.aws.amazon.com/lambda/latest/dg/rust-logging.html),
  [GA announcement](https://aws.amazon.com/about-aws/whats-new/2025/11/aws-lambda-rust)
- [aws-lambda-rust-runtime](https://github.com/aws/aws-lambda-rust-runtime) (MSRV, `tracing` feature, AWS-LC note)
- cargo-lambda: [build](https://www.cargo-lambda.info/commands/build.html),
  [release optimizations](https://www.cargo-lambda.info/guide/release-optimizations.html),
  [cross compiling](https://www.cargo-lambda.info/guide/cross-compiling.html),
  [installation](https://www.cargo-lambda.info/guide/installation.html)
- Cargo book: [workspaces](https://doc.rust-lang.org/cargo/reference/workspaces.html),
  [resolver](https://doc.rust-lang.org/cargo/reference/resolver.html);
  [lockfile guidance](https://blog.rust-lang.org/2023/08/29/committing-lockfiles/)
