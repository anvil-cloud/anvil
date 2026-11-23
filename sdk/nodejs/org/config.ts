// OrgConfig: the shape of new Org({...}), one line per feature. Each feature's
// types and docs live in its own folder (e.g. ous/config.ts) and its plan.ts
// validates that part.
import { OrgOus } from './ous/config';

export { OrgOus, OrgOuConfig } from './ous/config';
export { OrgAccounts, OrgAccountConfig, OrgAccountImport } from './accounts/config';

/** Default options for everything Org creates. */
export interface OrgDefaultsConfig {
  /** Tags on every OU and account Org manages. */
  tags?: Record<string, string>;
}

/** Configuration for the Org class. */
export interface OrgConfig {
  /**
   * Every OU and account Org manages, nested the way they sit in AWS: each
   * OU's own `ou` holds the OUs inside it, and its `accounts` the accounts in
   * it. Nothing is implied: only what's written exists. `anvil org init`
   * writes the recommended layout out in full. Elsewhere (guardrails,
   * ctx.ou) an OU is named by its path, e.g. 'Workloads/Prod'.
   *
   * Accounts: an entry without `import` is created (its email comes from
   * `email`); one with `import: { accountId }` brings in an existing member
   * account. Removing an account never closes it: a created one moves to
   * Suspended (which must be written), an imported one is released.
   *
   * @example
   * ou: {
   *   Security: { enable: true },
   *   Workloads: {
   *     enable: true,
   *     ou: {
   *       Prod: {
   *         enable: true,
   *         accounts: {
   *           'payments-prod': { enable: true },
   *           'curiosity-unlocked': { enable: true, import: { accountId: '669591255121' } },
   *         },
   *       },
   *       NonProd: { enable: true },
   *     },
   *   },
   *   Suspended: { enable: true },
   *   Partners: { enable: true, tags: { kind: 'partner' } },
   * }
   */
  ou?: OrgOus;
  /**
   * Root email for every account Org creates. `{account}` is replaced by the
   * account's name: 'aws+{account}@acme.com'. Use a shared mailbox, never one
   * person's inbox: whoever reads it can recover the account's root user.
   * An account can set its own `email` instead. Required once any account is
   * created (imported accounts keep their email).
   */
  email?: string;
  /** Default options for everything Org creates. */
  defaults?: OrgDefaultsConfig;
}
