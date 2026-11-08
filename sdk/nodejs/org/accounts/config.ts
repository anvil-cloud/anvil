// Config for an OU's `accounts` in new Org({...}): every account is written
// inside the OU it lives in, keyed by a name unique across the org. An entry
// is created unless it has `import`.

/** Bring an existing member account under Org. */
export interface OrgAccountImport {
  /** The account's 12-digit ID. Accounts are matched by ID, never by email. */
  accountId: string;
  /**
   * Off: Org places and tags the account but never goes inside it (no use of
   * OrganizationAccountAccessRole, no anvil-org-automation), so Org can't
   * configure anything in the account. On by default.
   */
  bootstrap?: { disable: true };
}

/**
 * One account, keyed by its name in its OU's `accounts` (also its name in AWS
 * for accounts Org creates). The name is how Org tracks the account: moving the block to another OU moves the account,
 * removing it never closes it (created: moved to Suspended; imported:
 * released); renaming the key needs
 * `renamedFrom`.
 */
export interface OrgAccountConfig {
  enable: true;
  /**
   * Import an existing member account instead of creating one. Without it,
   * Org creates the account (permanent: deploy asks you to type "yes").
   */
  import?: OrgAccountImport;
  /**
   * Root email for a created account, instead of the org-wide `email`
   * pattern. Set at creation only: changing it later needs the root user,
   * in the console.
   */
  email?: string;
  /**
   * The account's old name, to rename it without releasing it. Created
   * accounts are renamed in AWS too; imported ones keep their AWS name.
   * Remove it after the deploy that renames.
   */
  renamedFrom?: string;
  /** Tags on this account, merged over `defaults.tags`. */
  tags?: Record<string, string>;
}

/** Accounts keyed by name. Names are unique across the whole org. */
export type OrgAccounts = Record<string, OrgAccountConfig>;
