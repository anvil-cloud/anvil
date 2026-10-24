# State Retention

**Status:** Built, not yet deployed anywhere.

Each stage keeps its Pulumi state in its own versioned S3 bucket,
`<stage>-anvil-state-<project>-<id>`, created by `anvil bootstrap`. Anvil
controls how many state versions that bucket keeps, and whether state is
gzipped, from the App config.

## Config

TypeScript:

```ts
new App({
  defaults: {
    state: { retainVersions: 3, compressState: true },
  },
  run(ctx) { /* ... */ },
});
```

Python:

```python
anvil.App(
    defaults=anvil.DefaultsConfig(
        state=anvil.StateConfig(retain_versions=3, compress_state=True),
    ),
    run=main,
)
```

Go (`RetainVersions: 0` and `CompressState: nil` both mean "use the default"):

```go
off := false
anvil.Run(anvil.AppConfig{
    Defaults: &anvil.DefaultsConfig{
        State: &anvil.StateConfig{RetainVersions: 3, CompressState: &off},
    },
    Run: run,
})
```

- **`retainVersions`** is how many deploys' state is kept, counting the latest:
  1 = the latest deploy, 2 = the deploy before, 3 = the one before that. The
  default is 3, and every stage gets it even when the field is omitted. Allowed
  values are 2 to 101. Each SDK rejects a bad value when the App is
  constructed, and the CLI checks again.
- **`compressState`** sets `PULUMI_DIY_BACKEND_GZIP`. The default is true.

## How the CLI learns the settings

The settings must be known before Pulumi runs, so they ride along with the
discovery run.

1. During discovery (`ANVIL_BUILD_MODE=true`), the App validates the settings
   and writes them to `ANVIL_APP_SETTINGS_PATH`
   (`.anvil/app-settings.json`):

   ```json
   {"state": {"retainVersions": 3, "compressState": true}}
   ```

2. The CLI deletes that file before each discovery run, so removing a setting
   from the program takes effect.

This is a separate file rather than a section in `.anvil/build-manifest.json`.
The Lambda and DSQL components rewrite the manifest from fixed structs, which
would drop any section they don't know about. The SDKs also write this file
directly, without going through a provider component, so no schema or SDK
regeneration is needed.

Commands that don't run discovery (`destroy`, `refresh`, standalone
`bootstrap`) use the file from the last discovery run, or the defaults if there
is none.

## What the CLI does with them

Every Pulumi run gets:

| Env var | Value |
| --- | --- |
| `PULUMI_DIY_BACKEND_GZIP` | `compressState` |
| `PULUMI_DIY_BACKEND_DISABLE_CHECKPOINT_BACKUPS` | `true` |

Turning off checkpoint backups stops Pulumi copying every update's state into
`.pulumi/backups/`. Pulumi still copies each update's final state into
`.pulumi/history/`, which is what retention keeps.
`PULUMI_SKIP_CHECKPOINTS` is never set, so state is still saved after every
operation.

Switching compression either way is safe:

- Pulumi reads plain or gzipped state, and picks whichever of
  `<stack>.json` / `<stack>.json.gz` is newer.
- On its next save it writes the configured format and moves the other file to
  `.bak`.
- `anvil list` accepts both file names.

The state bucket's lifecycle configuration is set when the bucket is
bootstrapped, and rewritten on every `anvil deploy`. That way existing stages
pick it up and config changes apply. `PutBucketLifecycleConfiguration` replaces
the whole configuration, so Anvil always writes the full set below. A failure
on deploy is a warning, not a deploy failure.

The rules are the same whatever `retainVersions` is, since deploy retention is
done by pruning `.pulumi/history/` (below):

```json
{
  "Rules": [
    {
      "ID": "anvil-state-saves-cleanup",
      "Status": "Enabled",
      "Filter": { "Prefix": ".pulumi/stacks/" },
      "NoncurrentVersionExpiration": { "NoncurrentDays": 1 }
    },
    {
      "ID": "anvil-pruned-history-cleanup",
      "Status": "Enabled",
      "Filter": { "Prefix": ".pulumi/history/" },
      "NoncurrentVersionExpiration": { "NoncurrentDays": 1 }
    },
    {
      "ID": "anvil-checkpoint-backups-cleanup",
      "Status": "Enabled",
      "Filter": { "Prefix": ".pulumi/backups/" },
      "Expiration": { "Days": 1 },
      "NoncurrentVersionExpiration": { "NoncurrentDays": 1 }
    },
    {
      "ID": "anvil-bucket-cleanup",
      "Status": "Enabled",
      "Filter": { "Prefix": "" },
      "Expiration": { "ExpiredObjectDeleteMarker": true },
      "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 1 }
    }
  ]
}
```

What each rule does:

- **`anvil-state-saves-cleanup`** deletes old versions of the files under
  `.pulumi/stacks/` (the state file and its `.bak`) a day after they're
  replaced. 1 day is S3's minimum. These are the saves Pulumi makes during a
  deploy, after every resource operation, so a deploy leaves hundreds of them.
  The current state file is never expired.
- **`anvil-pruned-history-cleanup`** deletes, a day later, the old version a
  pruned checkpoint copy leaves behind (the bucket is versioned, so a delete
  only hides the object). History files that are never deleted, including
  every `.history.json` summary, have no old versions and are untouched.
- **`anvil-checkpoint-backups-cleanup`** deletes the backups written before
  this change, along with their old versions.
- **`anvil-bucket-cleanup`** removes delete markers with nothing behind them.
  Each update's lock file leaves one under `.pulumi/locks/`. It also aborts
  abandoned multipart uploads. It never touches a live object.

## Retention counts deploys, not saves

S3 versions can't express "keep the last 3 deploys": Pulumi saves the state
after every resource operation, so one deploy creates hundreds of versions of
the state file. Each update's final state is instead the checkpoint copy
Pulumi writes to `.pulumi/history/<project>/<stack>/<stack>-<unix ns>.checkpoint.json`
(`.gz` when compressed), next to a ~2 KB `.history.json` summary.

After every `anvil deploy`, successful or not (Pulumi records failed updates
too), the CLI lists `.pulumi/history/` and deletes all but the newest
`retainVersions` checkpoint copies per stack (`pruneHistoryCheckpoints`). It
never deletes a `.history.json` summary or a file whose name it can't parse.
A failure is a warning, not a deploy failure. The latest checkpoint is the same
state as the current state file.

The summaries are kept forever. At ~2 KB each, ten deploys a day for a year is
about 7 MB.

To recover the state from N deploys ago, copy that checkpoint over the state
file (`.pulumi/stacks/<project>/<stack>.json[.gz]`) and run `anvil refresh`.
