// planOrgAccounts: validates the accounts written inside each OU and returns
// every account to manage. Pure: no AWS calls (the resource checks the account
// exists in this org, on preview).
import { OrgConfig } from '../config';
import { PlannedOu } from '../ous/plan';
import { OrgAccountConfig } from './config';

/** One account Org will manage. */
export interface PlannedAccount {
  /** The account's name in config. */
  name: string;
  /** Path of its OU. */
  ou: string;
  /** 'create' for accounts Org creates, 'import' for existing ones. */
  kind: 'create' | 'import';
  /** Existing account ID (imports only). */
  accountId?: string;
  /** Root email (created accounts only). */
  email?: string;
  /** Whether Org bootstraps anvil-org-automation inside it (imports; created accounts always are). */
  bootstrap: boolean;
  /** Its name before this deploy, when renamed with renamedFrom. */
  renamedFrom?: string;
  /** Tags for this account only (merged over the org-wide tags). */
  tags: Record<string, string>;
}

/** AWS: account names are 1-50 characters. */
const accountNamePattern = /^[^\s].{0,48}[^\s]$|^[^\s]$/;
const accountIdPattern = /^\d{12}$/;
/** AWS: account emails are at most 64 characters. */
const emailPattern = /^[^@\s]+@[^@\s]+\.[^@\s]+$/;
const maxEmailLength = 64;

export function planOrgAccounts(config: OrgConfig, ous: PlannedOu[]): PlannedAccount[] {
  if ((config as any).accounts !== undefined) {
    throw new Error(
      `Top-level accounts moved into the OUs: write each account inside its OU's accounts. ` +
        `For example, { 'my-app': { ou: 'Workloads/Prod', import: {...} } } becomes ` +
        `Workloads: { enable: true, ou: { Prod: { enable: true, accounts: { 'my-app': { enable: true, import: {...} } } } } }.`
    );
  }
  const seenNames = new Map<string, string>();
  const seenIds = new Map<string, string>();
  const seenEmails = new Map<string, string>();
  const pattern = (config as any).email;
  if (pattern !== undefined && (typeof pattern !== 'string' || !pattern.includes('{account}'))) {
    throw new Error(`email must be a pattern with {account} in it, e.g. email: 'aws+{account}@acme.com'.`);
  }
  const planned: PlannedAccount[] = [];
  const renames = new Map<string, string>();

  for (const ou of ous) {
    for (const [name, value] of Object.entries(ou.accounts)) {
      const where = `Account '${name}' (in ${ou.path})`;
      if (!accountNamePattern.test(name)) {
        throw new Error(`${where}: account names must be 1-50 characters with no leading or trailing spaces.`);
      }
      const otherOu = seenNames.get(name.toLowerCase());
      if (otherOu !== undefined) {
        throw new Error(`Account '${name}' is written in both ${otherOu} and ${ou.path}. Account names must be unique across the org; keep one.`);
      }
      seenNames.set(name.toLowerCase(), ou.path);

      if (!value || typeof value !== 'object' || Array.isArray(value)) {
        throw new Error(`${where} must be an object, e.g. { enable: true, import: { accountId: '123456789012' } }.`);
      }
      const account = value as Partial<OrgAccountConfig> & Record<string, unknown>;
      if ('ou' in account) {
        throw new Error(`${where}: drop ou. An account lives in the OU it's written in; move its block to move it.`);
      }
      if ((account as any).disable !== undefined) {
        throw new Error(`${where}: delete its block to remove the account (it's released, never closed).`);
      }
      if (account.enable !== true) {
        throw new Error(`${where} must have enable: true, e.g. { enable: true, import: { accountId: '123456789012' } }.`);
      }
      const unknownKeys = Object.keys(account).filter((k) => !['enable', 'import', 'email', 'renamedFrom', 'tags'].includes(k));
      if (unknownKeys.length > 0) {
        throw new Error(`${where}: unknown option ${unknownKeys.map((k) => `'${k}'`).join(', ')}. Accounts take enable, import, email, renamedFrom and tags.`);
      }
      let kind: 'create' | 'import';
      let id: string | undefined;
      let email: string | undefined;
      let bootstrap: unknown;
      if (account.import !== undefined) {
        kind = 'import';
        if (account.email !== undefined) {
          throw new Error(`${where}: drop email. Imported accounts keep the email they have; email is for accounts Org creates.`);
        }
        id = String(account.import.accountId ?? '');
        if (!accountIdPattern.test(id)) {
          throw new Error(`${where}: import.accountId must be the 12-digit account ID (got '${id}').`);
        }
        const other = seenIds.get(id);
        if (other) {
          throw new Error(`Accounts '${other}' and '${name}' both import account ${id}.`);
        }
        seenIds.set(id, name);
        bootstrap = account.import.bootstrap;
        if (bootstrap !== undefined && !(typeof bootstrap === 'object' && (bootstrap as any).disable === true)) {
          throw new Error(`${where}: import.bootstrap only accepts { disable: true }. Bootstrapping is on by default.`);
        }
      } else {
        kind = 'create';
        if (account.email !== undefined && typeof account.email !== 'string') {
          throw new Error(`${where}: email must be an address, e.g. email: 'aws-${name}@acme.com'.`);
        }
        if (account.email === undefined && pattern === undefined) {
          throw new Error(
            `${where} is created by Org, so it needs a root email. Add a pattern at the top of new Org({...}), ` +
              `e.g. email: 'aws+{account}@acme.com' (a shared mailbox, not one person's inbox), or set email on the account. ` +
              `To bring in an existing account instead, add import: { accountId: '<12-digit id>' }.`
          );
        }
        email = account.email ?? (pattern as string).split('{account}').join(name);
        if (email.length > maxEmailLength || !emailPattern.test(email)) {
          throw new Error(`${where}: email '${email}' must be a valid address of at most ${maxEmailLength} characters. Shorten the name or the pattern, or set email on the account.`);
        }
        const other = seenEmails.get(email.toLowerCase());
        if (other !== undefined) {
          throw new Error(`Accounts '${other}' and '${name}' would both use ${email}. Every account needs its own email; set email on one.`);
        }
        seenEmails.set(email.toLowerCase(), name);
      }

      const from = account.renamedFrom;
      if (from !== undefined) {
        if (typeof from !== 'string' || !accountNamePattern.test(from)) {
          throw new Error(`${where}: renamedFrom must be the account's old name (1-50 characters), e.g. renamedFrom: 'data-lake'.`);
        }
        if (from === name) {
          throw new Error(`${where}: renamedFrom is the same as its name. Drop renamedFrom.`);
        }
        const other = renames.get(from.toLowerCase());
        if (other !== undefined) {
          throw new Error(`Accounts '${other}' and '${name}' are both renamedFrom '${from}'. Keep it on one.`);
        }
        renames.set(from.toLowerCase(), name);
      }

      planned.push({
        name,
        ou: ou.path,
        kind,
        ...(id !== undefined ? { accountId: id } : {}),
        ...(email !== undefined ? { email } : {}),
        bootstrap: kind === 'create' || bootstrap === undefined,
        ...(from !== undefined ? { renamedFrom: from } : {}),
        tags: { ...account.tags },
      });
    }
  }

  // The old name can't still be written anywhere in the tree.
  for (const [oldLower, renamed] of renames) {
    const old = planned.find((a) => a.name.toLowerCase() === oldLower && a.name !== renamed)?.name;
    if (old !== undefined) {
      const stillIn = seenNames.get(oldLower);
      throw new Error(`Account '${renamed}' is renamedFrom '${old}', but '${old}' is still written in ${stillIn}. Delete the '${old}' block, or drop renamedFrom.`);
    }
  }
  if (planned.some((a) => a.kind === 'create') && !ous.some((o) => o.path === 'Suspended')) {
    throw new Error(
      `Accounts Org creates move to the Suspended OU when removed from config (they're never closed), so it must exist. ` +
        `Add Suspended: { enable: true } to ou.`
    );
  }
  return planned;
}
