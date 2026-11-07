// Config for `ou` in new Org({...}): Sandbox and Policy Staging switches, and
// custom OUs keyed by full path.

/** A custom OU, keyed by its full path in `ou` (e.g. "Workloads/Prod/Data"). */
export interface OrgOuConfig {
  enable: true;
  /** Tags on this OU, merged over `defaults.tags`. */
  tags?: Record<string, string>;
}

/** Recommended OUs that can be switched, plus custom OUs keyed by full path. */
export type OrgOus = {
  /** Sandbox is the only recommended OU that can be turned off. */
  Sandbox?: { disable: true };
  /** Opt-in top-level OU for testing policies before they reach real OUs. */
  PolicyStaging?: { enable: true };
} & { [path: string]: OrgOuConfig | { disable: true } | { enable: true } | undefined };
