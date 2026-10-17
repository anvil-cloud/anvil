package scaffold

import "fmt"

// workspaceManifest is the root Cargo.toml Anvil creates when the project
// has no workspace yet. A virtual manifest has no edition, so resolver must
// be explicit. No [profile.release]: cargo lambda build --release already
// applies strip, thin LTO, codegen-units = 1 and panic = "abort".
func workspaceManifest(rustVersion string) string {
	return fmt.Sprintf(`# Cargo workspace for this project's Rust Lambdas. Created by Anvil; yours
# to edit. anvil generate adds new Lambda packages to members.
[workspace]
resolver = "3"
members = []

[workspace.package]
edition = "2024"
rust-version = %q

[workspace.dependencies]
lambda_runtime = { version = "1", features = ["tracing"] }
serde_json = "1"
tokio = { version = "1", features = ["macros"] }
`, rustVersion)
}

// toolchainFile pins the compiler so laptop and CI build with the same Rust,
// and lists the Lambda targets so rustup installs them automatically. Anvil
// never edits it after creating it: changing channel is the override.
func toolchainFile(rustVersion string) string {
	return fmt.Sprintf(`# Created by Anvil; yours to edit (e.g. bump channel to upgrade Rust).
[toolchain]
channel = %q
targets = ["aarch64-unknown-linux-gnu", "x86_64-unknown-linux-gnu"]
`, rustVersion)
}

// handlerTemplate is the blank async handler every new Rust Lambda starts
// from: JSON in, JSON out. Swap in event types from aws_lambda_events or
// lambda_http as needed.
const handlerTemplate = `use lambda_runtime::{run, service_fn, tracing, Error, LambdaEvent};
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
`
