// createAccounts: brings planned accounts under Org. Neither resource ever
// closes or removes an account on delete:
// - OrgAccount creates the account; on delete it's moved to Suspended,
//   tagged anvil:lifecycle=suspended and released.
// - OrgImportedAccount brings in an existing one; on delete it's released.
import * as pulumi from '@pulumi/pulumi';
import { OrgAccount } from '../../aws/orgAccount';
import { OrgImportedAccount } from '../../aws/orgImportedAccount';
import { PlannedAccount } from './plan';

/** Returns each account's id by name, for outputs. */
export function createAccounts(
  planned: PlannedAccount[],
  ouIds: Record<string, pulumi.Output<string>>,
  orgName: string,
  defaultTags: Record<string, string>,
  deployerArns: string[]
): Record<string, pulumi.Output<string>> {
  const ids: Record<string, pulumi.Output<string>> = {};
  for (const a of planned) {
    const opts: pulumi.CustomResourceOptions = a.renamedFrom ? { aliases: [{ name: a.renamedFrom }] } : {};
    const tags = { ...defaultTags, ...a.tags };
    if (a.kind === 'create') {
      const account = new OrgAccount(
        a.name,
        {
          name: a.name,
          email: a.email!,
          ouId: ouIds[a.ou],
          suspendedOuId: ouIds['Suspended'],
          orgName,
          tags,
          deployerArns,
        },
        opts
      );
      ids[a.name] = account.accountId;
      continue;
    }
    const account = new OrgImportedAccount(
      a.name,
      {
        accountId: a.accountId!,
        ouId: ouIds[a.ou],
        orgName,
        tags,
        bootstrap: a.bootstrap,
        deployerArns: a.bootstrap ? deployerArns : [],
      },
      opts
    );
    ids[a.name] = account.accountId;
  }
  return ids;
}
