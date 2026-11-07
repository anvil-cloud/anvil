// sdk/nodejs/grants.ts
// Hand-written. Backed up/restored during gen-sdk like app.ts and block.ts.
//
// Provides the runtime grant execution for all resource grant methods.
// Each resource's grant methods (injected by fix-sdk-grants.js) delegate here.

import * as pulumi from '@pulumi/pulumi';
import * as aws from '@pulumi/aws';

/**
 * GrantTarget is any Anvil compute resource that can receive IAM permissions.
 * Compute resources (Lambda, SvelteKitSite, etc.) satisfy this interface.
 */
export interface GrantTarget {
  grantName(): string;
  grantRoleArn(): pulumi.Output<string>;
}

/**
 * Returns the logical name a component was constructed with. The name is
 * stashed on the instance by the construction wrapper (stack.ts), so grant
 * methods can build synchronous child-resource names without the generated
 * SDK class declaring the field itself.
 * @internal
 */
export function grantResourceName(resource: object): string {
  const name = (resource as any).__anvilName;
  if (typeof name !== 'string') {
    throw new Error(
      'Anvil grant: could not resolve the resource name. Construct components ' +
        'via the anvil namespace (e.g. `new anvil.aws.Bucket(...)`) so grants work.'
    );
  }
  return name;
}

/**
 * Optional metadata for grant methods.
 */
export interface GrantOptions {
  justification?: string;
}

/**
 * Creates a scoped IAM RolePolicy granting the specified actions on the
 * specified resource ARNs to the target's execution role.
 *
 * @internal
 */
export function createGrant(
  parent: pulumi.Resource,
  name: string,
  target: GrantTarget,
  actions: string[],
  resourceArns: pulumi.Output<string>[],
  opts?: GrantOptions
): void {
  const policyDocument = pulumi.all(resourceArns).apply((arns) =>
    JSON.stringify({
      Version: '2012-10-17',
      Statement: [
        {
          Effect: 'Allow',
          Action: actions,
          Resource: arns,
        },
      ],
    })
  );

  const roleName = target.grantRoleArn().apply((arn) => {
    const idx = arn.lastIndexOf('/');
    return idx >= 0 ? arn.substring(idx + 1) : arn;
  });

  const policyName = opts?.justification
    ? `${name}-${sanitize(opts.justification)}`
    : name;

  // NOT parented to the granting resource: the policy reads that resource's
  // ARN, and depending on a component waits for all of its children, so a
  // child policy would wait on itself and hang the program (as with
  // DSQLConnect in dsql.grants.ts). The alias keeps policies created when
  // grants were parented, so upgrading doesn't delete and recreate them.
  new aws.iam.RolePolicy(
    policyName,
    {
      role: roleName,
      policy: policyDocument,
    },
    { aliases: [{ parent }] }
  );
}

/** @internal */
function sanitize(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .slice(0, 40);
}

/**
 * Builds the list of ARNs for a grant based on a base ARN and optional path scoping.
 * @internal
 */
export function buildResourceArns(
  baseArn: pulumi.Output<string>,
  paths?: string[] | null
): pulumi.Output<string>[] {
  const arns: pulumi.Output<string>[] = [baseArn];

  if (paths === null) {
    // Explicit null = base ARN only, no sub-paths (used by DynamoDB index grants)
    return arns;
  }

  if (!paths || paths.length === 0) {
    arns.push(pulumi.interpolate`${baseArn}/*`);
  } else {
    for (const p of paths) {
      arns.push(pulumi.interpolate`${baseArn}/${p}`);
    }
  }

  return arns;
}
