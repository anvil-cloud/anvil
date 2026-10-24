# Site Asset Sync

**Status:** Proposal, not built. The two quick fixes it builds on are shipped:
precompressed `.br`/`.gz` copies are no longer uploaded, and the CLI writes DIY
state gzipped (`PULUMI_DIY_BACKEND_GZIP`). Version retention and compression are configurable per
app; see [state-retention.md](state-retention.md).

## Why

Every file a site build produces is its own `aws:s3/bucketObjectv2` resource
(`awssite.UploadSiteAssets`). Pulumi rewrites the whole stack state to the S3
backend after every resource operation, and those saves run one at a time. So a
deploy costs about _operations × state size_, and both grow with the number of
files.

Measured on the anvil-fe docs site (`anvvil-docs`, stage `damienpace`):

| | Resources | State (plain) | Bytes uploaded per save |
| --- | --- | --- | --- |
| Before | 310 (285 assets) | 3.75 MB | 3.75 MB |
| Skip `.br`/`.gz` | 136 (111 assets) | 1.65 MB | 1.65 MB |
| + gzip state | 136 | 1.65 MB | ~55 KB |
| + this proposal | ~26 (1 sync) | ~60 KB | ~5 KB |

Gzip already takes most of the bytes out. What gzip doesn't fix is the number of
saves: each changed file is still at least one serial round trip (HeadObject,
CopyObject for the `.bak`, PutObject), so roughly 0.3–0.5 s per file. SvelteKit
renames every `_app/immutable` chunk whenever its content changes, so a typical
deploy creates and deletes dozens of them.

## Proposal: one resource for the whole asset set

Replace the per-file resources with a single custom resource in the anvil
provider, `anvil:aws:SiteAssets`. The provider has only components so far, but
`infer.NewProviderBuilder().WithResources(infer.Resource(...))` supports custom
resources with Create/Update/Delete/Diff.

**Inputs**

- `bucket`: the bucket name.
- `manifest`: `{key: {sha256, contentType, cacheControl}}`, built at program
  time by walking the static directory. This uses the same walk,
  precompressed-copy skip and cache-header rules as `UploadSiteAssets` does today.
- `sourceDir`: a local path, used only to read file bodies. It is not part of
  the diff.
- `immutablePrefixes` and `retainGenerations` (see below).

**State.** The manifest plus a retained-key table. That's about 150 bytes per
file, so roughly 17 KB for anvil-fe, instead of about 6 KB per file today.

**Diff.** Compare manifests and report a `DetailedDiff` summary such as
`+12 ~3 -0 (40 retained)`, so `anvil deploy` previews stay readable.

**Update**

1. PutObject every added or changed key, using a bounded worker pool (16 workers)
   inside the provider. This is real parallelism: the engine only sees one
   operation, so there is one state save at the end.
2. Handle removed keys:
   - Outside the immutable prefixes, delete them, but only after the uploads
     succeed.
   - Inside the immutable prefixes, retain them (next section).
3. Return the new manifest. If an upload fails, return a partial-state error with
   what did land. Retrying then re-uploads only what's missing.

**Delete.** Empty the keys the resource owns. The bucket already has
`forceDestroy`, so this only matters when the bucket outlives the resource.

**Credentials and region.** The provider builds its own AWS SDK client from the
`aws:region` config and the default credential chain. That's the same identity
the `aws` provider uses under the Automation API.

### Keeping previous `_app/immutable` assets

Today a deploy deletes the previous build's hashed chunks. A browser that still
has the old HTML then 404s on its next lazy import. That happens with open tabs,
and HTML can also sit in CloudFront for `s-maxage=86400` plus 8640 s
stale-while-revalidate. SvelteKit reports this as a failed dynamic import and
falls back to a full reload at best.

Rule: a key under an immutable prefix that leaves the manifest moves into
`retained` with the deploy generation it left in. It is deleted once both of
these hold:

- it has been absent for `retainGenerations` deploys (default 2), and
- it has been absent for longer than the longest HTML cache lifetime (`86400 +
  8640` s).

A key that comes back (a revert) moves out of `retained` without being
re-uploaded if its hash matches.

Alternative with no bookkeeping: tag the departing objects
`anvil-stale=<date>`, and add an S3 lifecycle rule on the site bucket that
expires tagged objects after 2 days. This is simpler and needs no further
deploys to clean up. The cost is a lifecycle rule on every site bucket, and
cleanup happens at lifecycle granularity (daily), not per deploy. Either
approach is fine. The generation table is easier to test locally.

### Migration from per-file resources

The difficulty is that dropping the old resources from the program makes Pulumi
delete them, and Pulumi runs deletes *after* creates. Those deletes would remove
the very keys the new resource just uploaded. This is the same create-then-delete
hazard that the key-based naming in `UploadSiteAssets` already works around.

Options, in order of preference:

1. **CLI state surgery before `up`.** If the stack has
   `aws:s3/bucketObjectv2` children of a site component and no `SiteAssets`
   resource, `anvil deploy` exports the stack, drops those entries and imports it
   back. These are state-only edits; the objects stay. The new resource's
   first Create then HEADs each key and skips the ones whose ETag/hash already
   match, so it adopts the existing objects. This happens once per stack. Print
   it as a single line.
2. Ship one transitional provider version that registers the old resources with
   `RetainOnDelete(true)`, so the following version can drop them safely. This
   needs two deploys and relies on users taking the intermediate version, which
   makes it fragile.

### Out of scope / follow-ups

- **CloudFront invalidation.** Only HTML and other mutable keys need it. The sync
  resource knows exactly which mutable keys changed, so it could issue one
  targeted invalidation.
- **`optup.Parallel(10)`.** It isn't the bottleneck today, because the saves are
  serialized. With a ~60 KB state, provider operations may start to dominate.
  Measure before changing it.
- **Other frameworks.** Astro and future site frameworks reuse the same resource
  through `sites/types.go`.
