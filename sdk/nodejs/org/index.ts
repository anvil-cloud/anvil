// Org: the entry point. Each feature lives in its own folder (see README.md);
// this file only plans every feature, then creates them in order.
import * as pulumi from '@pulumi/pulumi';
import * as aws from '@pulumi/aws';
import { OrgConfig } from './config';
import { planOrgOus } from './ous/plan';
import { createOus } from './ous/create';
import { isDiscovery, writeOrgSettings } from './discovery';

export * from './config';
export { PlannedOu, planOrgOus } from './ous/plan';

/**
 * Org manages an AWS Organization with secure defaults: the multi-account
 * counterpart to App. Deploy it from the organization's management account
 * with `anvil org deploy`. Org never creates or deletes the organization
 * itself; create it in the console first, with all features enabled.
 *
 * This release creates the recommended OU tree for the `minimum` tier plus
 * any custom OUs.
 *
 * @example
 * ```typescript
 * import * as anvil from "@anvil-cloud/sdk";
 *
 * export default new anvil.Org({
 *   tier: "minimum",
 *   ou: {
 *     Sandbox: { disable: true },
 *     "Workloads/Prod/Data": { enable: true },
 *   },
 *   defaults: { tags: { company: "acme" } },
 * });
 * ```
 */
export class Org {
  /** Stack outputs: OU ids by path, the organization id and the management account id. */
  [key: string]: any;

  constructor(config: OrgConfig) {
    const planned = planOrgOus(config);
    writeOrgSettings();
    // Discovery only needs to know this is an Org; it never creates resources.
    if (isDiscovery()) return;

    // The CLI looks the organization up during preflight and passes it in, so
    // the organization itself is never a resource in state and can't be deleted.
    const anvilConfig = new pulumi.Config('anvil');
    const rootId = anvilConfig.require('orgRootId');
    const orgId = anvilConfig.require('orgId');
    const managementAccountId = anvilConfig.require('managementAccountId');
    const project = pulumi.getProject();

    const provider = new aws.Provider('anvil-org-provider', {
      defaultTags: {
        tags: { ...config.defaults?.tags, 'anvil:managed': 'true', 'anvil:org': project },
      },
    });

    const ids = createOus(planned, rootId, provider);

    this.ou = ids;
    this.orgId = orgId;
    this.rootId = rootId;
    this.managementAccountId = managementAccountId;
  }
}
