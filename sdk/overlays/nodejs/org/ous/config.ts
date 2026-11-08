import { OrgAccounts } from '../accounts/config';

// Config for `ou` in new Org({...}): every OU and account Org manages, nested
// the way they sit in AWS. Nothing is implied: an OU or account exists
// because it's written here.
// `anvil org init` writes the recommended layout out in full as a starting point.

/** One OU, keyed by its name in the `ou` map that holds it. */
export interface OrgOuConfig {
  enable: true;
  /**
   * The OU's old name under the same parent, to rename it in place (same OU,
   * same id, accounts stay). Remove it after the deploy that renames.
   */
  renamedFrom?: string;
  /** Tags on this OU, merged over `defaults.tags`. */
  tags?: Record<string, string>;
  /** OUs inside this one, written the same way. */
  ou?: OrgOus;
  /** Accounts in this OU, keyed by name (unique across the whole org). */
  accounts?: OrgAccounts;
}

/**
 * OUs keyed by name, each with its own `ou` for the OUs inside it. Top-level
 * OUs sit under Root. Elsewhere (accounts, guardrails) an OU is named by its
 * path: 'Workloads/Prod'. Removing an OU's block removes it (AWS refuses while
 * it still holds accounts).
 *
 * @example
 * ou: {
 *   Security: { enable: true },
 *   Workloads: {
 *     enable: true,
 *     ou: {
 *       Prod: {
 *         enable: true,
 *         accounts: { 'my-app-prod': { enable: true, import: { accountId: '123456789012' } } },
 *       },
 *       NonProd: { enable: true },
 *     },
 *   },
 *   Suspended: { enable: true },
 * }
 */
export type OrgOus = Record<string, OrgOuConfig>;
