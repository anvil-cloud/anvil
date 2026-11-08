// createOus: makes the planned OUs under Root.
import * as pulumi from '@pulumi/pulumi';
import * as aws from '@pulumi/aws';
import { PlannedOu } from './plan';

/**
 * One OU as an Anvil component, so `anvil org preview` lists it by path
 * ("OrganizationalUnit  Workloads/Prod") like App components; the AWS
 * resource underneath shows with --verbose.
 */
class OrganizationalUnit extends pulumi.ComponentResource {
  public readonly id: pulumi.Output<string>;

  constructor(
    p: PlannedOu,
    parentId: pulumi.Input<string>,
    provider: pulumi.ProviderResource
  ) {
    // A rename (this OU's or a parent's) changes the path, so alias the old
    // path: state moves and the OU is renamed in place, never recreated.
    const prev = p.previousPath;
    super('anvil:org:OrganizationalUnit', p.path, {}, prev ? { aliases: [{ name: prev }] } : {});
    const ouType = 'aws:organizations/organizationalUnit:OrganizationalUnit';
    const ou = new aws.organizations.OrganizationalUnit(
      `ou-${p.path}`,
      {
        name: p.name,
        parentId,
        tags: Object.keys(p.tags).length > 0 ? p.tags : undefined,
      },
      {
        parent: this,
        provider,
        aliases: prev ? [pulumi.createUrn(`ou-${prev}`, ouType, pulumi.createUrn(prev, 'anvil:org:OrganizationalUnit'))] : [],
      }
    );
    this.id = ou.id;
    this.registerOutputs({ id: this.id });
  }
}

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
    const parentId = p.parent === undefined ? rootId : ids[p.parent];
    ids[p.path] = new OrganizationalUnit(p, parentId, provider).id;
  }
  return ids;
}
