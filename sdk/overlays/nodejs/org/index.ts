// Org: the entry point. Each feature lives in its own folder (see README.md);
// this file only plans every feature, then creates them in order.
import * as pulumi from '@pulumi/pulumi';
import * as aws from '@pulumi/aws';
import { OrgConfig } from './config';
import { planOrgOus } from './ous/plan';
import { createOus } from './ous/create';
import { planOrgAccounts } from './accounts/plan';
import { createAccounts } from './accounts/create';
import { isDiscovery, writeOrgSettings } from './discovery';

export * from './config';
export { PlannedOu, planOrgOus } from './ous/plan';
export { PlannedAccount, planOrgAccounts } from './accounts/plan';

/**
 * Org manages an AWS Organization with secure defaults: the multi-account
 * counterpart to App. Deploy it from the organization's management account
 * with `anvil org deploy`. Org never creates or deletes the organization
 * itself; create it in the console first, with all features enabled.
 *
 * Everything Org manages is listed in config: nothing is implied. Org
 * creates the listed OUs, creates the listed accounts (or imports existing
 * ones) and places each in its OU. `anvil org init` scaffolds the
 * recommended layout.
 *
 * @example
 * ```typescript
 * import * as anvil from "@anvil-cloud/sdk";
 *
 * export default new anvil.Org({
 *   email: "aws+{account}@acme.com",
 *   ou: {
 *     Security: { enable: true },
 *     Workloads: {
 *       enable: true,
 *       ou: {
 *         Prod: {
 *           enable: true,
 *           accounts: {
 *             "payments-prod": { enable: true },
 *             "legacy-site": { enable: true, import: { accountId: "123456789012" } },
 *           },
 *         },
 *         NonProd: { enable: true },
 *       },
 *     },
 *     Suspended: { enable: true },
 *   },
 *   defaults: { tags: { company: "acme" } },
 * });
 * ```
 */
export class Org {
  /** Stack outputs: OU and account ids, the organization id and the management account id. */
  [key: string]: any;

  constructor(config: OrgConfig) {
    // Plan every feature before anything else, so a bad config fails in
    // discovery, before any AWS call.
    const plannedOus = planOrgOus(config);
    const plannedAccounts = planOrgAccounts(config, plannedOus);
    writeOrgSettings(plannedOus, plannedAccounts);
    // Discovery only needs to know this is an Org; it never creates resources.
    if (isDiscovery()) return;

    // The CLI looks the organization up during preflight and passes it in, so
    // the organization itself is never a resource in state and can't be deleted.
    const anvilConfig = new pulumi.Config('anvil');
    const rootId = anvilConfig.require('orgRootId');
    const orgId = anvilConfig.require('orgId');
    const managementAccountId = anvilConfig.require('managementAccountId');
    const project = pulumi.getProject();
    // Exact ARNs of Org's deployers, resolved by the CLI (today: whoever is
    // deploying, the bootstrap deployer).
    const deployerArns: string[] = anvilConfig.getObject<string[]>('deployerArns') ?? [];

    const provider = new aws.Provider('anvil-org-provider', {
      defaultTags: {
        tags: { ...config.defaults?.tags, 'anvil:managed': 'true', 'anvil:org': project },
      },
    });

    const ouIds = createOus(plannedOus, rootId, provider);
    const accountIds = createAccounts(plannedAccounts, ouIds, project, config.defaults?.tags ?? {}, deployerArns);

    this.ou = ouIds;
    this.accounts = accountIds;
    this.orgId = orgId;
    this.rootId = rootId;
    this.managementAccountId = managementAccountId;
  }
}
