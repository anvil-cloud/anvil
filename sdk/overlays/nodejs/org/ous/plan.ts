// planOrgOus: validates `tier` and `ou` and returns every OU to create,
// parents before children. Pure: no AWS calls.
import { OrgConfig, OrgTier } from '../config';
import { OrgOuConfig } from './config';

/** One OU Org will create. */
export interface PlannedOu {
  /** Full path, e.g. "Workloads/Prod". */
  path: string;
  /** The OU's name, the last path segment. */
  name: string;
  /** Parent path, or undefined for a top-level OU under Root. */
  parent?: string;
  /** Tags for this OU only (merged over the org-wide tags). */
  tags: Record<string, string>;
  /** True for OUs Org recommends, false for the user's own. */
  recommended: boolean;
}

/** AWS allows five levels of OUs below Root. */
const maxOuDepth = 5;

/** Recommended OUs per tier, parents before children. */
const recommendedOus: Record<OrgTier, string[]> = {
  minimum: ['Security', 'Workloads', 'Workloads/Prod', 'Workloads/NonProd', 'Sandbox', 'Suspended'],
};

/** Keys in `ou` that switch a recommended OU instead of adding one. */
const switchKeys = new Set(['Sandbox', 'PolicyStaging']);

/** AWS: OU names are 1-128 characters. Org also rules out "/", the path separator. */
const ouNamePattern = /^[^/]{1,128}$/;

/**
 * Validates the config and returns every OU Org will create, parents before
 * children. Throws with a message saying exactly what to fix.
 */
export function planOrgOus(config: OrgConfig): PlannedOu[] {
  if (!config || typeof config !== 'object') {
    throw new Error('new Org({...}) needs a config object.');
  }
  const tiers = Object.keys(recommendedOus);
  if (!tiers.includes(config.tier)) {
    throw new Error(
      `tier must be one of: ${tiers.map((t) => `'${t}'`).join(', ')} (got ${JSON.stringify(config.tier)}).`
    );
  }

  const ou = config.ou ?? {};

  const sandbox = ou.Sandbox;
  if (sandbox !== undefined && !isDisable(sandbox)) {
    throw new Error("ou.Sandbox only accepts { disable: true }. Sandbox is on by default.");
  }
  const staging = ou.PolicyStaging;
  if (staging !== undefined && !isEnable(staging)) {
    throw new Error('ou.PolicyStaging only accepts { enable: true }. Policy Staging is off by default.');
  }

  const recommended = recommendedOus[config.tier].filter((p) => !(p === 'Sandbox' && sandbox));
  if (staging) recommended.push('PolicyStaging');

  const known = new Set(recommended);
  const planned: PlannedOu[] = recommended.map((path) => ({
    ...split(path),
    path,
    tags: {},
    recommended: true,
  }));

  // Custom OUs: sort by depth so parents are planned before children.
  const custom = Object.keys(ou)
    .filter((k) => !switchKeys.has(k))
    .sort((a, b) => depth(a) - depth(b) || a.localeCompare(b));

  for (const path of custom) {
    const value = ou[path];
    if (!isEnable(value)) {
      throw new Error(
        `ou['${path}'] must be { enable: true } (optionally with tags). ` +
          `Only Sandbox can be { disable: true }.`
      );
    }
    const segments = path.split('/');
    if (segments.some((s) => !ouNamePattern.test(s) || s.trim() !== s)) {
      throw new Error(`ou['${path}']: each part of an OU path must be 1-128 characters with no leading or trailing spaces.`);
    }
    if (segments.length > maxOuDepth) {
      throw new Error(`ou['${path}'] is ${segments.length} levels deep; AWS allows ${maxOuDepth} below Root.`);
    }
    if (known.has(path)) {
      throw new Error(`ou['${path}'] is already a recommended OU for tier '${config.tier}'; remove it from ou.`);
    }
    const { name, parent } = split(path);
    if (parent !== undefined && !known.has(parent)) {
      if (parent === 'Sandbox' && sandbox) {
        throw new Error(
          `ou['${path}']: Sandbox is turned off (ou.Sandbox: { disable: true }). ` +
            `Remove that to keep Sandbox, or put this OU somewhere else.`
        );
      }
      if (parent === 'PolicyStaging' && !staging) {
        throw new Error(`ou['${path}']: Policy Staging is off. Add ou.PolicyStaging: { enable: true } first.`);
      }
      const hint = suggest(parent, [...known]);
      throw new Error(
        `ou['${path}']: parent OU '${parent}' doesn't exist. Add it to ou as { enable: true }` +
          (hint ? `, or did you mean '${hint}/${name}'?` : '.')
      );
    }
    known.add(path);
    planned.push({ path, name, parent, tags: { ...(value as OrgOuConfig).tags }, recommended: false });
  }

  // OU names must be unique under the same parent (AWS rejects duplicates).
  const seen = new Map<string, string>();
  for (const p of planned) {
    const key = `${p.parent ?? ''}\u0000${p.name.toLowerCase()}`;
    const other = seen.get(key);
    if (other) {
      throw new Error(`ou['${p.path}'] and '${other}' have the same name under the same parent.`);
    }
    seen.set(key, p.path);
  }

  return planned;
}

function isEnable(v: unknown): boolean {
  return typeof v === 'object' && v !== null && (v as any).enable === true && (v as any).disable === undefined;
}

function isDisable(v: unknown): boolean {
  return typeof v === 'object' && v !== null && (v as any).disable === true && (v as any).enable === undefined;
}

function depth(path: string): number {
  return path.split('/').length;
}

function split(path: string): { name: string; parent?: string } {
  const i = path.lastIndexOf('/');
  return i === -1 ? { name: path } : { name: path.slice(i + 1), parent: path.slice(0, i) };
}

/** Closest known path by case-insensitive match or edit distance ≤ 2. */
function suggest(target: string, candidates: string[]): string | undefined {
  const lower = target.toLowerCase();
  const exact = candidates.find((c) => c.toLowerCase() === lower);
  if (exact) return exact;
  let best: string | undefined;
  let bestDistance = 3;
  for (const c of candidates) {
    const d = editDistance(lower, c.toLowerCase());
    if (d < bestDistance) {
      best = c;
      bestDistance = d;
    }
  }
  return best;
}

function editDistance(a: string, b: string): number {
  const row = Array.from({ length: b.length + 1 }, (_, i) => i);
  for (let i = 1; i <= a.length; i++) {
    let prev = row[0];
    row[0] = i;
    for (let j = 1; j <= b.length; j++) {
      const tmp = row[j];
      row[j] = Math.min(row[j] + 1, row[j - 1] + 1, prev + (a[i - 1] === b[j - 1] ? 0 : 1));
      prev = tmp;
    }
  }
  return row[b.length];
}
