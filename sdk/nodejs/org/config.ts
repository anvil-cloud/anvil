// OrgConfig: the shape of new Org({...}), one line per feature. Each feature's
// types and docs live in its own folder (e.g. ous/config.ts) and its plan.ts
// validates that part.
import { OrgOus } from './ous/config';

export { OrgOus, OrgOuConfig } from './ous/config';

/**
 * Which recommended OUs and accounts Org creates. The first release supports
 * `minimum` only.
 */
export type OrgTier = 'minimum';

/** Default options for everything Org creates. */
export interface OrgDefaultsConfig {
  /** Tags on every OU (and later account) Org creates. */
  tags?: Record<string, string>;
}

/** Configuration for the Org class. */
export interface OrgConfig {
  /** Which recommended OUs and accounts Org creates. */
  tier: OrgTier;
  /**
   * Turn Sandbox off, turn Policy Staging on, and add custom OUs keyed by full
   * path. Every segment's parent must be a recommended OU or another custom OU.
   *
   * @example
   * ou: {
   *   Sandbox: { disable: true },
   *   'Workloads/Prod/Data': { enable: true },
   *   'Partners': { enable: true, tags: { kind: 'partner' } },
   * }
   */
  ou?: OrgOus;
  /** Default options for everything Org creates. */
  defaults?: OrgDefaultsConfig;
}
