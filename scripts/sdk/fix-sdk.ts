// scripts/sdk/fix-sdk.ts
//
// Patches the auto-generated SDKs after pulumi gen-sdk:
//
// TypeScript (sdk/nodejs/):
//   1. Wires the hand-written overlay barrel (_extras) into index.ts
//   2. Patches package.json build mechanics (main/types/version/build script)
//
// Python (sdk/python/):
//   1. Fills pyproject.toml version (gen-sdk emits a 0.0.0 placeholder)
//   2. Copies/creates README.md
//   3. Patches _utilities.py for correct package name lookup
//   4. Patches __init__.py with wrap_namespace + the overlay barrel (_extras)
//
// SDK metadata (name, description, license, deps, pyproject) is authored in
// provider/base-schema.json and emitted natively by gen-sdk — not patched here.
// The version is the single source of truth in base-schema.json, passed in via
// ANVIL_VERSION by build.go (see getVersion).
//
// Usage:
//   npx ts-node scripts/sdk/fix-sdk.ts           # patches both TS and Python
//   npx ts-node scripts/sdk/fix-sdk.ts --ts      # patches only TypeScript
//   npx ts-node scripts/sdk/fix-sdk.ts --python  # patches only Python

import * as fs from 'fs';
import * as path from 'path';
import ts from 'typescript';

const cliArgs = process.argv.slice(2);
const tsOnly = cliArgs.includes('--ts');
const pyOnly = cliArgs.includes('--python');
const doTS = !pyOnly;
const doPython = !tsOnly;

const schemaPath = path.join(
  __dirname,
  '..',
  '..',
  'provider',
  'base-schema.json'
);

function getVersion(): string {
  // Prefer the version passed in by build.go (single read of base-schema.json).
  // Fall back to reading base-schema.json directly so the script still works
  // when run by hand. base-schema.json remains the single source of truth.
  if (process.env.ANVIL_VERSION) {
    return process.env.ANVIL_VERSION;
  }
  if (fs.existsSync(schemaPath)) {
    const schema = JSON.parse(fs.readFileSync(schemaPath, 'utf8'));
    return (schema.version as string) || '0.0.1';
  }
  return '0.0.1';
}

// ════════════════════════════════════════════════════════════
// TypeScript
// ════════════════════════════════════════════════════════════

function patchTypeScript(): void {
  const sdkDir = path.join(__dirname, '..', '..', 'sdk', 'nodejs');

  console.log('📦 Patching TypeScript SDK...');

  // ── 0. Patch tsconfig.json ─────────────────────────────
  // gen-sdk emits moduleResolution: "node" (a.k.a. node10), which newer
  // TypeScript flags as deprecated and exits non-zero on — breaking the
  // `tsc && cp package.json bin/` build chain. Silence the deprecation so
  // the build completes and package.json is copied into bin/ (required at
  // runtime by utilities.getVersion()).
  const tsconfigPath = path.join(sdkDir, 'tsconfig.json');
  if (fs.existsSync(tsconfigPath)) {
    const { config: tsconfig, error: tsconfigError } = ts.readConfigFile(
      tsconfigPath,
      ts.sys.readFile
    );
    if (tsconfigError)
      throw new Error(
        ts.flattenDiagnosticMessageText(tsconfigError.messageText, '\n')
      );
    tsconfig.compilerOptions = tsconfig.compilerOptions || {};
    if (tsconfig.compilerOptions.ignoreDeprecations !== '6.0') {
      tsconfig.compilerOptions.ignoreDeprecations = '6.0';
      fs.writeFileSync(tsconfigPath, JSON.stringify(tsconfig, null, 4) + '\n');
      console.log('  ✔ Patched tsconfig.json → ignoreDeprecations: "6.0"');
    }
  }

  // ── 1. Wire the hand-written overlay barrel into index.ts ──
  // The hand-written App/Block/grants modules + Pulumi re-exports live as real
  // files in sdk/overlays/nodejs/ (copied into sdk/nodejs/ by build.go before
  // this runs). gen-sdk emits a fresh index.ts that knows nothing about them,
  // so append a single re-export of the overlay barrel (_extras.ts).
  //
  // NOTE: forward-reference auto-registration is NOT wired here. The generated
  // `export { aws, gcp, types }` namespace form is kept as-is so that
  // `anvil.aws.X` stays usable as a *type*. Construction wrapping is injected
  // into utilities.lazyLoad instead (see patchUtilities below / stack.ts).
  const indexPath = path.join(sdkDir, 'index.ts');
  if (fs.existsSync(indexPath)) {
    let indexContent = fs.readFileSync(indexPath, 'utf8');

    if (!indexContent.includes('./_extras')) {
      indexContent =
        indexContent.trimEnd() +
        '\n\n// Hand-written overlay exports (App, Block, grants, Pulumi primitives)\n' +
        'export * from "./_extras";\n';
      fs.writeFileSync(indexPath, indexContent);
      console.log('  ✔ Wired index.ts → export * from "./_extras"');
    }
  }

  // ── 1b. Patch utilities.ts → lazyLoad auto-registration ─
  // Wrap component classes as they're resolved by lazyLoad so constructing one
  // auto-registers it into the active Stack (powers ctx.ref forward references).
  // Done here (not around the namespace export) so `anvil.aws.X` stays a usable
  // type. See sdk/nodejs/stack.ts (maybeWrapComponent).
  const utilsPath = path.join(sdkDir, 'utilities.ts');
  if (fs.existsSync(utilsPath)) {
    let utils = fs.readFileSync(utilsPath, 'utf8');
    let utilsChanged = false;

    if (!utils.includes('maybeWrapComponent')) {
      // 1. import
      utils = utils.replace(
        'import * as pulumi from "@pulumi/pulumi";',
        'import * as pulumi from "@pulumi/pulumi";\nimport { maybeWrapComponent } from "./stack";'
      );
      // 2. wrap the lazyLoad getter's return value
      utils = utils.replace(
        'return loadModule()[property];',
        'return maybeWrapComponent(loadModule()[property]);'
      );
      utilsChanged = utils.includes('maybeWrapComponent');
      if (!utilsChanged) {
        console.warn(
          '  ⚠ utilities.ts: lazyLoad anchor not found — ctx.ref auto-registration ' +
            'NOT wired. The gen-sdk output may have changed; update fix-sdk.ts.'
        );
      }
    }

    if (utilsChanged) {
      fs.writeFileSync(utilsPath, utils);
      console.log(
        '  ✔ Patched utilities.ts → lazyLoad component auto-registration'
      );
    }
  }

  // ── 2. Patch package.json ──────────────────────────────
  const pkgPath = path.join(sdkDir, 'package.json');
  if (fs.existsSync(pkgPath)) {
    const pkg = JSON.parse(fs.readFileSync(pkgPath, 'utf8'));

    pkg.name = '@anvil-cloud/sdk';
    if (!pkg.version || pkg.version.includes('${')) {
      pkg.version = getVersion();
    }
    pkg.main = 'bin/index.js';
    pkg.types = 'bin/index.d.ts';

    pkg.scripts = pkg.scripts || {};
    pkg.scripts.build = 'tsc && cp package.json bin/';

    fs.writeFileSync(pkgPath, JSON.stringify(pkg, null, 2) + '\n');
    console.log('  ✔ Patched package.json → @anvil-cloud/sdk');
  }

  console.log('✔ TypeScript SDK patching complete\n');
}

// ════════════════════════════════════════════════════════════
// Python
// ════════════════════════════════════════════════════════════

function patchPython(): void {
  const sdkDir = path.join(__dirname, '..', '..', 'sdk', 'python');
  const readmeSrc = path.join(
    __dirname,
    '..',
    '..',
    'docs',
    'python',
    'README.md'
  );
  const readmeDst = path.join(sdkDir, 'README.md');
  const version = getVersion();

  console.log('📦 Patching Python SDK...');

  // ── 1. Fill pyproject.toml version ─────────────────────
  // gen-sdk's native pyproject (language.python.pyproject.enabled) emits a
  // placeholder `version = "0.0.0"`, like nodejs's `${VERSION}`. Substitute the
  // real version (base-schema.json via getVersion) so wheels publish correctly.
  const pyprojectPath = path.join(sdkDir, 'pyproject.toml');
  if (fs.existsSync(pyprojectPath)) {
    let pyproject = fs.readFileSync(pyprojectPath, 'utf8');
    if (pyproject.includes('version = "0.0.0"')) {
      pyproject = pyproject.replace(
        'version = "0.0.0"',
        `version = "${version}"`
      );
      fs.writeFileSync(pyprojectPath, pyproject);
      console.log(`  ✔ Patched pyproject.toml → version ${version}`);
    } else if (!pyproject.includes(`version = "${version}"`)) {
      console.warn(
        '  ⚠ pyproject.toml: placeholder `version = "0.0.0"` not found — version ' +
          'NOT set. gen-sdk output may have changed; update scripts/sdk/fix-sdk.ts.'
      );
    }
  }

  // ── 2. Copy/create README ──────────────────────────────
  if (fs.existsSync(readmeSrc)) {
    fs.copyFileSync(readmeSrc, readmeDst);
    console.log('  ✔ Copied README.md into sdk/python/');
  } else {
    const fallback = `# Anvil — Python SDK

Secure-by-default cloud infrastructure components for [Pulumi](https://www.pulumi.com/).

## Install

\`\`\`bash
pip install anvil-cloud
\`\`\`

## Quick start

\`\`\`python
import anvil_cloud as anvil

def main(ctx):
    bucket = anvil.aws.Bucket("my-data", data_classification="sensitive")
    ctx.export("bucket_name", bucket.bucket_name)

anvil.run(anvil.AppConfig(run=main))
\`\`\`

## Links

- [GitHub](https://github.com/anvil-cloud/anvil)
- [npm (Node SDK)](https://www.npmjs.com/package/@anvil-cloud/sdk)
- [Go SDK](https://pkg.go.dev/github.com/anvil-cloud/anvil/sdk/go/anvil)

## License

Apache-2.0
`;
    fs.writeFileSync(readmeDst, fallback);
    console.log('  ✔ Created fallback README.md');
  }

  // ── 3. Patch _utilities.py ─────────────────────────────
  const utilitiesPath = path.join(sdkDir, 'anvil_cloud', '_utilities.py');
  if (fs.existsSync(utilitiesPath)) {
    let src = fs.readFileSync(utilitiesPath, 'utf8');
    const before = src;
    src = src.replace(
      'importlib.metadata.version(root_package)',
      'importlib.metadata.version("anvil-cloud")'
    );
    if (src !== before) {
      fs.writeFileSync(utilitiesPath, src);
      console.log(
        '  ✔ Patched _utilities.py → importlib.metadata.version("anvil-cloud")'
      );
    } else {
      console.log(
        '  ⚠ Could not find importlib.metadata.version(root_package) in _utilities.py — check manually'
      );
    }
  }

  // ── 4. Patch __init__.py ───────────────────────────────
  const initPath = path.join(sdkDir, 'anvil_cloud', '__init__.py');
  if (fs.existsSync(initPath)) {
    let init = fs.readFileSync(initPath, 'utf8');
    let changed = false;

    // Forward-reference namespace wrapping.
    // gen-sdk emits `aws = _utilities.lazy_import('anvil_cloud.aws')` (and gcp)
    // in the runtime branch. Wrap them with wrap_namespace() so constructing any
    // component auto-registers it into the active Stack — powering ctx.ref(...)
    // forward references. See stack.py.
    if (!init.includes('wrap_namespace')) {
      const rawLazyImports =
        /    aws = _utilities\.lazy_import\('anvil_cloud\.aws'\)\n    gcp = _utilities\.lazy_import\('anvil_cloud\.gcp'\)/;
      const wrappedLazyImports = [
        '    from .stack import wrap_namespace as _wrap_namespace',
        '    # Wrap the component namespaces so constructing any component',
        '    # auto-registers it into the active Stack under its logical name,',
        '    # powering ctx.ref(...) forward references. See stack.py.',
        "    aws = _wrap_namespace(_utilities.lazy_import('anvil_cloud.aws'))",
        "    gcp = _wrap_namespace(_utilities.lazy_import('anvil_cloud.gcp'))",
      ].join('\n');

      if (rawLazyImports.test(init)) {
        init = init.replace(rawLazyImports, wrappedLazyImports);
        changed = true;
      } else {
        console.warn(
          '  ⚠ Could not find the generated lazy_import block in __init__.py ' +
            '— forward-reference wrapping NOT applied. The gen-sdk output format ' +
            'may have changed; update scripts/sdk/fix-sdk.ts.'
        );
      }
    }

    // The hand-written App/Block/types/grants modules + the `export` re-export
    // live as real files in sdk/overlays/python/ (copied into anvil_cloud/ by
    // build.go). _extras.py pulls them into the package namespace; wire it with
    // a single barrel import instead of injecting each import as a string here.
    if (!init.includes('from ._extras import')) {
      init +=
        '\n# Hand-written overlay exports (run, App, Block, types, grants, export)\nfrom ._extras import *  # noqa: F401,F403\n';
      changed = true;
    }

    if (changed) {
      fs.writeFileSync(initPath, init);
      console.log(
        '  ✔ Patched __init__.py → wrap_namespace + from ._extras import *'
      );
    }
  }

  console.log(`✔ Python SDK patched → anvil-cloud v${version}`);
}

// ════════════════════════════════════════════════════════════
// Main
// ════════════════════════════════════════════════════════════

if (doTS) patchTypeScript();
if (doPython) patchPython();
