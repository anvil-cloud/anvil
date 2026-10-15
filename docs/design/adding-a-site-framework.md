# Adding a site framework (Astro, Nuxt, Next.js, …)

**Status:** Checklist for new site components. Written after two bugs found while hosting
the Anvil docs site on `SvelteKitSite` (October 2026); both are explained below so a new
framework doesn't repeat them.

A site component has three parts. Two are shared and one is yours:

| Part | Where | Shared? |
|---|---|---|
| Build the framework, report where its output went | `provider/sites/<framework>/` returning a `sites.BuildResult` | Yours |
| Upload static files to S3 and route them through CloudFront | `provider/internal/awssite/` (`UploadSiteAssets`, `StaticRootBehaviors`) | **Shared: reuse it** |
| Package the server code for Lambda | `create<Framework>ServerArchive` in the component (e.g. `provider/aws/sveltekitsite/sveltekitsite.go`) | **Yours: framework-specific** |

Hosting inputs (`protection`, `waf`, `originProtection`, `securityHeaders`) are also shared;
see the comment above `HostingInputNames` in `provider/sites/types.go`.

## 1. Static files: always upload with `UploadSiteAssets`

Use `awssite.UploadSiteAssets(ctx, parent, name, bucket, buildResult.StaticDir, immutablePrefixes)`.
Do not write your own upload loop.

- **Why it matters.** Each uploaded file is a Pulumi resource. Its resource name must stay the
  same when the file's contents change, so an edit is an in-place update. Earlier versions
  named resources by a hash of the file's *contents*: editing `robots.txt` created a new
  resource and deleted the old one, both pointing at the same S3 key, and the delete removed
  the file that had just been uploaded. The site then served `403 AccessDenied` for it.
  `UploadSiteAssets` now names by key (and aliases the old content-based names so existing
  stacks migrate without deletes). A hand-written loop would bring the bug back.
- **`immutablePrefixes`**: pass the folders whose file names are already content-hashed by the
  framework (`_app/immutable/` for SvelteKit, `_astro/` for Astro). Those get a one-year
  `immutable` cache; everything else revalidates.
- **Root files** (`robots.txt`, `favicon.ico`, `.well-known/`): route them to S3 with
  `awssite.StaticRootBehaviors(staticDir, skip, s3OriginID)`. Pass the framework's own asset
  folders in `skip` (they get their own behaviours), and log the `skipped` list the way
  `SvelteKitSite` does: CloudFront allows a limited number of behaviours, so extra top-level
  entries fall through to the server.

## 2. Server code: make sure its dependencies are in the Lambda package

The Lambda package contains **only the files your archive function copies**.
`createSvelteKitServerArchive` copies the adapter's `build/` folder and nothing else: there
is no `node_modules` and no `npm install` in the package.

That works for SvelteKit only because of how `@sveltejs/adapter-node` bundles: it bundles
everything in **`devDependencies`** into `build/`, and leaves packages in **`dependencies`**
as runtime imports. A server-only package listed under `dependencies` (the docs site's
`turndown`) therefore builds and runs fine locally, then fails in Lambda with a 500 on
whichever route imports it. Other routes keep working, because routes load separately,
which makes it easy to miss.

For each new framework, before shipping:

1. **Find out what its server output imports at runtime.** Adapters differ: some bundle
   everything, some bundle nothing, some follow `dependencies` vs `devDependencies` like
   SvelteKit's. Read the adapter's docs, then check a real build the way Lambda sees it:
   copy exactly what your archive function packages to a folder with no `node_modules`
   above it, and `import()` every server file; any `ERR_MODULE_NOT_FOUND` names a package
   that isn't in the package. (Grepping the output for `from '...'` is not reliable: docs
   sites show example code full of import lines.) The Anvil docs site has a ready-made
   version: `frontend/scripts/check-lambda-imports.mjs` in anvil-fe (`npm run check:lambda`).
2. **Pick one approach and write it down in the component's doc comment:**
   - the adapter bundles everything → copy its output as-is (the SvelteKit approach), and
     tell users in the component docs which `package.json` section their server-only
     packages must go in; or
   - the adapter leaves imports → the archive step must install production dependencies
     (`npm ci --omit=dev` into the package) or bundle the output itself.
3. **Bundled is not the same as runnable.** A library can bundle fine and still crash when
   it loads: `turndown` calls `require()` for its HTML parser, which throws "require is not
   defined" inside SvelteKit's ES-module server build. The import check above catches this
   too, as long as it reports *every* load error, not just missing modules.
4. **Never assume local success means Lambda success.** Local runs resolve imports from the
   project's `node_modules`; Lambda only has the archive.

## 3. Before merging: deploy and check the two traps

Deploy the new component with a small test app (`anvil deploy --stage <your-stage>`), then:

- [ ] **Change a static root file and redeploy** (edit `robots.txt`, deploy again). It must
      still load with `200` afterwards, not `403`.
- [ ] **Hit a server route that imports a third-party package** (not just framework code).
      It must return `200` in the deployed site, not only locally.
- [ ] Every file in the framework's static output loads (`200`), including root files and
      nested folders.
- [ ] The component's Args test calls `sites.MissingHostingInputs` (see
      `provider/aws/sveltekitsite` for the pattern).

## If a static file is already missing on a deployed site

Pulumi still believes the object exists, so a plain deploy won't restore it. Run
`anvil refresh` (Pulumi notices the object is gone) and then `anvil deploy` (it uploads it
again).
