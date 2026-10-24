// scripts/grants/generate-grants.ts
//
// Generates grant methods as COMPANION files next to each generated component.
//   TypeScript: declaration merging — `declare module` + `Class.prototype.x = ...`
//   Python:     monkeypatch module — `Class.grant_x = _grant_x`
//
// The generated component classes are never edited (this replaces the old
// fix-sdk-grants.ts that spliced method bodies *inside* them via fragile
// anchors). Source of truth is unchanged: the declarative configs in ./configs.
//
// The logical resource name is read via grants.grantResourceName(this) /
// grants.grant_resource_name(self) — stashed on the instance by the construction
// wrapper (stack.ts / stack.py), so there is no per-class name injection.
//
// Usage:
//   npx ts-node scripts/grants/generate-grants.ts            # both
//   npx ts-node scripts/grants/generate-grants.ts --ts       # TypeScript only
//   npx ts-node scripts/grants/generate-grants.ts --python   # Python only

import * as fs from 'fs';
import * as path from 'path';
import { GrantConfig, GrantTargetConfig, CustomGrantConfig } from './types';
import {
  BUCKET_GRANT_CONFIG,
  LAMBDA_GRANT_CONFIG,
  LAMBDA_GRANT_TARGET_CONFIG,
  QUEUE_GRANT_CONFIG,
  EVENTBUS_GRANT_CONFIG,
  DYNAMODB_GRANT_CONFIG,
  KINESIS_GRANT_CONFIG,
  DSQL_CUSTOM_GRANT_CONFIG,
} from './configs';

const cliArgs = process.argv.slice(2);
const tsOnly = cliArgs.includes('--ts');
const pyOnly = cliArgs.includes('--python');
const doTS = !pyOnly;
const doPython = !tsOnly;

const tsDir = path.join(__dirname, '..', '..', 'sdk', 'nodejs');
const pyDir = path.join(__dirname, '..', '..', 'sdk', 'python', 'anvil_cloud');

const GRANT_CONFIGS: GrantConfig[] = [
  BUCKET_GRANT_CONFIG,
  LAMBDA_GRANT_CONFIG,
  QUEUE_GRANT_CONFIG,
  EVENTBUS_GRANT_CONFIG,
  DYNAMODB_GRANT_CONFIG,
  KINESIS_GRANT_CONFIG,
];
const GRANT_TARGET_CONFIGS: GrantTargetConfig[] = [LAMBDA_GRANT_TARGET_CONFIG];
const CUSTOM_GRANT_CONFIGS: CustomGrantConfig[] = [DSQL_CUSTOM_GRANT_CONFIG];

const actionsLiteral = (actions: string[]) =>
  actions.map((a) => `"${a}"`).join(', ');

// ════════════════════════════════════════════════════════════
// TypeScript
// ════════════════════════════════════════════════════════════

interface TsMethod {
  signature: string;
  impl: string;
}

function tsGrantMethod(
  config: GrantConfig,
  grant: GrantConfig['grants'][number]
): TsMethod {
  const { className, arnProperty, supportsPaths, supportsIndexes } = config;
  const { method, actions, isFullAccess } = grant;
  const suffix = grantSuffix(method);
  const acts = actionsLiteral(actions);
  const nameExpr =
    '`${grants.grantResourceName(this)}-${target.grantName()}-' + suffix + '`';

  if (isFullAccess) {
    return {
      signature: `${method}(target: grants.GrantTarget, opts?: grants.GrantOptions): void;`,
      impl: `${className}.prototype.${method} = function (this: ${className}, target: grants.GrantTarget, opts?: grants.GrantOptions): void {
    if (!opts?.justification) {
        pulumi.log.warn(
            \`⚠ \${grants.grantResourceName(this)} → \${target.grantName()}: full access granted with no justification. \` +
            \`Consider a scoped grant or add a justification.\`,
            this,
        );
    } else {
        pulumi.log.info(
            \`ℹ \${grants.grantResourceName(this)} → \${target.grantName()}: full access granted. Justification: "\${opts.justification}"\`,
            this,
        );
    }
    const name = ${nameExpr};
    const arns = grants.buildResourceArns(this.${arnProperty}, undefined);
    grants.createGrant(this, name, target, [${acts}], arns, opts);
};`,
    };
  }
  if (supportsIndexes) {
    return {
      signature: `${method}(target: grants.GrantTarget, opts?: { indexes?: string[]; justification?: string }): void;`,
      impl: `${className}.prototype.${method} = function (this: ${className}, target: grants.GrantTarget, opts?: { indexes?: string[]; justification?: string }): void {
    const name = ${nameExpr};
    const indexPaths = opts?.indexes?.map((i) => \`index/\${i}\`) ?? null;
    const arns = grants.buildResourceArns(this.${arnProperty}, indexPaths);
    grants.createGrant(this, name, target, [${acts}], arns, { justification: opts?.justification });
};`,
    };
  }
  if (supportsPaths) {
    return {
      signature: `${method}(target: grants.GrantTarget, paths?: string[], opts?: grants.GrantOptions): void;`,
      impl: `${className}.prototype.${method} = function (this: ${className}, target: grants.GrantTarget, paths?: string[], opts?: grants.GrantOptions): void {
    const name = ${nameExpr};
    const arns = grants.buildResourceArns(this.${arnProperty}, paths);
    grants.createGrant(this, name, target, [${acts}], arns, opts);
};`,
    };
  }
  return {
    signature: `${method}(target: grants.GrantTarget, opts?: grants.GrantOptions): void;`,
    impl: `${className}.prototype.${method} = function (this: ${className}, target: grants.GrantTarget, opts?: grants.GrantOptions): void {
    const name = ${nameExpr};
    const arns = grants.buildResourceArns(this.${arnProperty}, undefined);
    grants.createGrant(this, name, target, [${acts}], arns, opts);
};`,
  };
}

function tsTargetMethods(config: GrantTargetConfig): TsMethod[] {
  return [
    {
      signature: `grantName(): string;`,
      impl: `${config.className}.prototype.grantName = function (this: ${config.className}): string {
    return grants.grantResourceName(this);
};`,
    },
    {
      signature: `grantRoleArn(): pulumi.Output<string>;`,
      impl: `${config.className}.prototype.grantRoleArn = function (this: ${config.className}): pulumi.Output<string> {
    return this.${config.roleArnProperty};
};`,
    },
  ];
}

function tsCustomMethod(
  config: CustomGrantConfig,
  className: string
): TsMethod {
  const impl =
    config.tsMethod
      .replace(
        new RegExp(`public\\s+${config.detectMethod}\\s*\\(`),
        `${className}.prototype.${config.detectMethod} = function (`
      )
      .replace(/this\.__name/g, 'grants.grantResourceName(this)')
      .trimEnd() + ';';
  return {
    signature: `${config.detectMethod}(target: grants.GrantTarget, opts: any): void;`,
    impl,
  };
}

function generateTypeScript(): void {
  console.log('🔐 Generating TypeScript grant companions...');
  interface C {
    className: string;
    module: string;
    companionRel: string;
    extraImports: string[];
    signatures: string[];
    impls: string[];
  }
  const companions = new Map<string, C>();
  const get = (tsFile: string, className: string): C => {
    let c = companions.get(tsFile);
    if (!c) {
      const base = path.basename(tsFile, '.ts');
      c = {
        className,
        module: `./${base}`,
        companionRel: path.join(path.dirname(tsFile), `${base}.grants.ts`),
        extraImports: [],
        signatures: [],
        impls: [],
      };
      companions.set(tsFile, c);
    }
    return c;
  };

  for (const cfg of GRANT_CONFIGS) {
    const c = get(cfg.tsFile, cfg.className);
    for (const g of cfg.grants) {
      const m = tsGrantMethod(cfg, g);
      c.signatures.push(m.signature);
      c.impls.push(m.impl);
    }
  }
  for (const cfg of GRANT_TARGET_CONFIGS) {
    const c = get(cfg.tsFile, cfg.className);
    for (const m of tsTargetMethods(cfg)) {
      c.signatures.push(m.signature);
      c.impls.push(m.impl);
    }
  }
  for (const cfg of CUSTOM_GRANT_CONFIGS) {
    const c = get(cfg.tsFile, 'DSQL');
    (cfg.tsImports ?? []).forEach((i) => c.extraImports.push(i));
    const m = tsCustomMethod(cfg, 'DSQL');
    c.signatures.push(m.signature);
    c.impls.push(m.impl);
  }

  const imports: string[] = [];
  for (const c of companions.values()) {
    const content = `// AUTO-GENERATED by scripts/grants/generate-grants.ts — do not edit.
// Adds grant methods to the generated ${
      c.className
    } class via declaration merging.
import * as pulumi from "@pulumi/pulumi";
import * as grants from "../grants";
import { ${c.className} } from "${c.module}";
${c.extraImports.join('\n')}

declare module "${c.module}" {
    interface ${c.className} {
        ${c.signatures.join('\n        ')}
    }
}

${c.impls.join('\n\n')}
`;
    fs.writeFileSync(path.join(tsDir, c.companionRel), content);
    console.log(
      `  ✔ Wrote ${c.companionRel} → ${c.signatures.length} method(s)`
    );
    imports.push(
      `import "./${c.companionRel
        .replace(/\.ts$/, '')
        .split(path.sep)
        .join('/')}";`
    );
  }

  const indexPath = path.join(tsDir, 'index.ts');
  if (fs.existsSync(indexPath)) {
    let index = fs.readFileSync(indexPath, 'utf8');
    const marker = '// Anvil grant companions';
    if (!index.includes(marker)) {
      index =
        index.trimEnd() +
        '\n\n' +
        marker +
        ' (side-effect imports — attach grant methods to the generated classes)\n' +
        imports.join('\n') +
        '\n';
      fs.writeFileSync(indexPath, index);
      console.log(`  ✔ Wired ${imports.length} companion(s) into index.ts`);
    }
  }
  console.log('✔ TypeScript grant companions complete\n');
}

// ════════════════════════════════════════════════════════════
// Python
// ════════════════════════════════════════════════════════════

function pyGrantMethod(
  config: GrantConfig,
  grant: GrantConfig['grants'][number]
): string {
  const { arnProperty, supportsPaths, supportsIndexes } = config;
  const { method, actions, isFullAccess } = grant;
  const m = toSnakeCase(method);
  const arn = toSnakeCase(arnProperty);
  const suffix = grantSuffix(method);
  const acts = actionsLiteral(actions);

  if (isFullAccess) {
    return `def ${m}(self, target, opts=None) -> None:
    if not opts or not opts.justification:
        pulumi.log.warn(f"⚠ {grants.grant_resource_name(self)} → {target.grant_name()}: full access granted with no justification.", self)
    else:
        pulumi.log.info(f"ℹ {grants.grant_resource_name(self)} → {target.grant_name()}: full access granted. Justification: \\"{opts.justification}\\"", self)
    name = f"{grants.grant_resource_name(self)}-{target.grant_name()}-${suffix}"
    arns = grants.build_resource_arns(self.${arn}, None)
    grants.create_grant(self, name, target, [${acts}], arns, opts)`;
  }
  if (supportsIndexes) {
    return `def ${m}(self, target, indexes=None, justification=None) -> None:
    name = f"{grants.grant_resource_name(self)}-{target.grant_name()}-${suffix}"
    index_paths = [f"index/{i}" for i in indexes] if indexes else None
    arns = grants.build_resource_arns(self.${arn}, index_paths)
    grants.create_grant(self, name, target, [${acts}], arns, grants.GrantOptions(justification=justification) if justification else None)`;
  }
  if (supportsPaths) {
    return `def ${m}(self, target, paths=None, opts=None) -> None:
    name = f"{grants.grant_resource_name(self)}-{target.grant_name()}-${suffix}"
    arns = grants.build_resource_arns(self.${arn}, paths)
    grants.create_grant(self, name, target, [${acts}], arns, opts)`;
  }
  return `def ${m}(self, target, opts=None) -> None:
    name = f"{grants.grant_resource_name(self)}-{target.grant_name()}-${suffix}"
    arns = grants.build_resource_arns(self.${arn}, None)
    grants.create_grant(self, name, target, [${acts}], arns, opts)`;
}

function pyTargetMethods(config: GrantTargetConfig): string {
  const arn = config.pyRoleArnProperty;
  return `def grant_name(self) -> str:
    return grants.grant_resource_name(self)


def grant_role_arn(self):
    return self.${arn}`;
}

function pyCustomMethod(config: CustomGrantConfig): string {
  // pyMethod is authored as a 4-space-indented class member; dedent by 4 to make
  // it a module-level function and swap the name accessor.
  return config.pyMethod
    .replace(/\n {4}/g, '\n')
    .replace(/self\._name/g, 'grants.grant_resource_name(self)')
    .trim();
}

function generatePython(): void {
  console.log('🔐 Generating Python grant companions...');
  interface C {
    className: string;
    module: string;
    companionRel: string;
    defs: string[];
    assigns: string[];
  }
  const companions = new Map<string, C>();
  const get = (pyFile: string, className: string): C => {
    let c = companions.get(pyFile);
    if (!c) {
      const base = path.basename(pyFile, '.py');
      // Avoid a double underscore when the module already ends in one (lambda_).
      const companionBase = base.endsWith('_')
        ? `${base}grants`
        : `${base}_grants`;
      c = {
        className,
        module: `.${base}`,
        companionRel: path.join(path.dirname(pyFile), `${companionBase}.py`),
        defs: [],
        assigns: [],
      };
      companions.set(pyFile, c);
    }
    return c;
  };

  for (const cfg of GRANT_CONFIGS) {
    const c = get(cfg.pyFile, cfg.className);
    for (const g of cfg.grants) {
      c.defs.push(pyGrantMethod(cfg, g));
      c.assigns.push(
        `${cfg.className}.${toSnakeCase(g.method)} = ${toSnakeCase(g.method)}`
      );
    }
  }
  for (const cfg of GRANT_TARGET_CONFIGS) {
    const c = get(cfg.pyFile, cfg.className);
    c.defs.push(pyTargetMethods(cfg));
    c.assigns.push(`${cfg.className}.grant_name = grant_name`);
    c.assigns.push(`${cfg.className}.grant_role_arn = grant_role_arn`);
  }
  for (const cfg of CUSTOM_GRANT_CONFIGS) {
    const c = get(cfg.pyFile, 'DSQL');
    const m = toSnakeCase(cfg.detectMethod);
    c.defs.push(pyCustomMethod(cfg));
    c.assigns.push(`DSQL.${m} = ${m}`);
  }

  const inits: string[] = [];
  for (const c of companions.values()) {
    const content = `# AUTO-GENERATED by scripts/grants/generate-grants.ts — do not edit.
# Adds grant methods to the generated ${c.className} class (monkeypatch).
from typing import Optional
import pulumi
from anvil_cloud import grants
from ${c.module} import ${c.className}


${c.defs.join('\n\n\n')}


${c.assigns.join('\n')}
`;
    fs.writeFileSync(path.join(pyDir, c.companionRel), content);
    console.log(`  ✔ Wrote ${c.companionRel} → ${c.assigns.length} method(s)`);
    const pkg = path.dirname(c.companionRel).split(path.sep).join('.');
    const modBase = path.basename(c.companionRel, '.py');
    inits.push(
      pkg && pkg !== '.'
        ? `from .${pkg} import ${modBase}  # noqa: F401`
        : `from . import ${modBase}  # noqa: F401`
    );
  }

  const initPath = path.join(pyDir, '__init__.py');
  if (fs.existsSync(initPath)) {
    let init = fs.readFileSync(initPath, 'utf8');
    const marker = '# Anvil grant companions';
    if (!init.includes(marker)) {
      init =
        init.trimEnd() +
        '\n\n' +
        marker +
        ' (side-effect imports — attach grant methods)\n' +
        inits.join('\n') +
        '\n';
      fs.writeFileSync(initPath, init);
      console.log(`  ✔ Wired ${inits.length} companion(s) into __init__.py`);
    }
  }
  console.log('✔ Python grant companions complete\n');
}

if (doTS) generateTypeScript();
if (doPython) generatePython();

//helpers

/** camelCase → snake_case */
function toSnakeCase(str: string): string {
  return str.replace(/([a-z])([A-Z])/g, '$1_$2').toLowerCase();
}

/** "grantRead" → "read", "grantFullAccess" → "fullaccess" */
function grantSuffix(method: string): string {
  return method.replace('grant', '').toLowerCase();
}
