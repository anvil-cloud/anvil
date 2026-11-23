// planOrgOus: validates `ou` and returns every OU to create, parents before
// children. Pure: no AWS calls. Nothing is implied: only written OUs exist.
import { OrgConfig } from '../config';

/** One OU Org will create. */
export interface PlannedOu {
  /** Full path, e.g. "Workloads/Prod". Names the OU in state, accounts and guardrails. */
  path: string;
  /** The OU's name, the last path segment. */
  name: string;
  /** Parent path, or undefined for a top-level OU under Root. */
  parent?: string;
  /** Tags for this OU only (merged over the org-wide tags). */
  tags: Record<string, string>;
  /** Its path before this deploy's renames (its own or a parent's), when different. */
  previousPath?: string;
  /** The OU's `accounts` as written; planOrgAccounts validates them. */
  accounts: Record<string, unknown>;
}

/** AWS allows five levels of OUs below Root. */
const maxOuDepth = 5;

/** AWS: OU names are 1-128 characters. Org also rules out "/", the path separator. */
const ouNamePattern = /^[^/]{1,128}$/;

/** What `anvil org init` writes for the minimum layout, for the `tier` migration error. */
const minimumLayout = `ou: {
    Security: { enable: true },
    Workloads: {
      enable: true,
      ou: {
        Prod: { enable: true },
        NonProd: { enable: true },
      },
    },
    Sandbox: { enable: true },
    Suspended: { enable: true },
  }`;

/**
 * Validates the config and returns every OU Org will create, parents before
 * children. Throws with a message saying exactly what to fix.
 */
export function planOrgOus(config: OrgConfig): PlannedOu[] {
  if (!config || typeof config !== 'object') {
    throw new Error('new Org({...}) needs a config object.');
  }
  if ((config as any).tier !== undefined) {
    throw new Error(
      `tier was removed: Org now lists every OU in anvil.config.ts, so nothing is hidden. ` +
        `Replace tier: ${JSON.stringify((config as any).tier)} with the OUs you want, e.g.\n  ${minimumLayout}\n` +
        `Keeping the same OUs means no changes on the next deploy.`
    );
  }

  const planned: PlannedOu[] = [];
  walk(config.ou, undefined, undefined, planned);
  return planned;
}

/** Plans one `ou` map (Root's or an OU's), depth-first so parents come first. */
function walk(ous: unknown, parent: string | undefined, parentPrevious: string | undefined, planned: PlannedOu[]): void {
  if (ous === undefined) return;
  const where = parent === undefined ? 'ou' : `OU '${parent}': ou`;
  if (typeof ous !== 'object' || ous === null || Array.isArray(ous)) {
    throw new Error(`${where} must be an object keyed by OU name, e.g. { Prod: { enable: true } }.`);
  }

  const seen = new Map<string, string>();
  const renames = new Map<string, string>();
  for (const [name, value] of Object.entries(ous as Record<string, unknown>)) {
    const path = parent === undefined ? name : `${parent}/${name}`;

    if (name.includes('/')) {
      const [head, ...rest] = name.split('/');
      throw new Error(
        `OU '${name}': write nested OUs inside their parent, not as a path. ` +
          `Put ${rest.join('/')} in ${head}'s ou: ${head}: { enable: true, ou: { ${rest[0]}: { enable: true } } }.`
      );
    }
    if (!ouNamePattern.test(name) || name.trim() !== name) {
      throw new Error(`OU '${path}': OU names must be 1-128 characters with no leading or trailing spaces.`);
    }
    const lower = name.toLowerCase();
    const other = seen.get(lower);
    if (other !== undefined) {
      throw new Error(`OUs '${other}' and '${name}' ${parent === undefined ? 'under Root' : `in '${parent}'`} differ only by case; give them names that differ by more than case.`);
    }
    seen.set(lower, name);

    if (typeof value === 'object' && value !== null && (value as any).disable !== undefined) {
      throw new Error(`OU '${path}': delete its block to remove the OU. Only written OUs exist.`);
    }
    if (typeof value !== 'object' || value === null || Array.isArray(value) || (value as any).enable !== true) {
      throw new Error(`OU '${path}' must be { enable: true } (optionally with tags and ou).`);
    }
    const unknownKeys = Object.keys(value).filter((k) => !['enable', 'renamedFrom', 'tags', 'ou', 'accounts'].includes(k));
    if (unknownKeys.length > 0) {
      throw new Error(`OU '${path}': unknown option ${unknownKeys.map((k) => `'${k}'`).join(', ')}. OUs take enable, renamedFrom, tags, ou and accounts.`);
    }
    const depth = path.split('/').length;
    if (depth > maxOuDepth) {
      throw new Error(`OU '${path}' is ${depth} levels deep; AWS allows ${maxOuDepth} below Root.`);
    }

    const v = value as { renamedFrom?: unknown; tags?: Record<string, string>; ou?: unknown; accounts?: unknown };
    const oldName = v.renamedFrom === undefined ? name : checkOuRename(path, name, v.renamedFrom, renames);
    const base = parentPrevious ?? parent;
    const previous = base === undefined ? oldName : `${base}/${oldName}`;
    if (v.accounts !== undefined && (typeof v.accounts !== 'object' || v.accounts === null || Array.isArray(v.accounts))) {
      throw new Error(`OU '${path}': accounts must be an object keyed by account name, e.g. { 'my-app-prod': {...} }.`);
    }
    planned.push({
      path,
      name,
      parent,
      tags: { ...v.tags },
      ...(previous !== path ? { previousPath: previous } : {}),
      accounts: { ...(v.accounts as Record<string, unknown>) },
    });
    walk(v.ou, path, previous !== path ? previous : undefined, planned);
  }

  // The old name can't still be written next to the new one.
  for (const [oldLower, renamed] of renames) {
    const still = seen.get(oldLower);
    if (still !== undefined && still !== renamed) {
      const at = parent === undefined ? 'under Root' : `in '${parent}'`;
      throw new Error(`OU '${renamed}' is renamedFrom '${still}', but '${still}' is still written ${at}. Delete the '${still}' block, or drop renamedFrom.`);
    }
  }
}

/** Validates an OU's renamedFrom and returns the old name. */
function checkOuRename(path: string, name: string, from: unknown, renames: Map<string, string>): string {
  if (typeof from !== 'string') {
    throw new Error(`OU '${path}': renamedFrom must be the OU's old name, e.g. renamedFrom: 'Data'.`);
  }
  if (from.includes('/')) {
    throw new Error(
      `OU '${path}': renamedFrom is the old name under the same parent, not a path ('${from}'). ` +
        `AWS can't move an OU to another parent: write the new OU, move its accounts into it, then delete the old one.`
    );
  }
  if (!ouNamePattern.test(from) || from.trim() !== from) {
    throw new Error(`OU '${path}': renamedFrom '${from}' isn't a valid OU name (1-128 characters, no leading or trailing spaces).`);
  }
  if (from === name) {
    throw new Error(`OU '${path}': renamedFrom is the same as its name. Drop renamedFrom.`);
  }
  const other = renames.get(from.toLowerCase());
  if (other !== undefined) {
    throw new Error(`OUs '${other}' and '${name}' are both renamedFrom '${from}'. Keep it on one.`);
  }
  renames.set(from.toLowerCase(), name);
  return from;
}
