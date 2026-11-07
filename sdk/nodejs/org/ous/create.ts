// createOus: makes the planned OUs under Root.
import * as pulumi from '@pulumi/pulumi';
import * as aws from '@pulumi/aws';
import { PlannedOu } from './plan';

/**
 * Creates the planned OUs under Root, parents first. Returns each OU's id by
 * path, for outputs and for anything that attaches to an OU later.
 */
export function createOus(
  planned: PlannedOu[],
  rootId: string,
  provider: pulumi.ProviderResource
): Record<string, pulumi.Output<string>> {
  const ids: Record<string, pulumi.Output<string>> = {};
  for (const p of planned) {
    const ou = new aws.organizations.OrganizationalUnit(
      `ou-${p.path}`,
      {
        name: p.name,
        parentId: p.parent === undefined ? rootId : ids[p.parent],
        tags: Object.keys(p.tags).length > 0 ? p.tags : undefined,
      },
      { provider }
    );
    ids[p.path] = ou.id;
  }
  return ids;
}
