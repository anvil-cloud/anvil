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
  /**
   * The security group on the target's network interfaces. Only needed for
   * grants that pass an `endpoint`; resolves to undefined when the target is
   * not in a VPC.
   */
  grantSecurityGroupId?(): pulumi.Output<string | undefined>;
}

/**
 * An Anvil `VpcEndpoint`. Pass it as `endpoint` on a grant to route the
 * granted calls through it.
 */
export interface GrantEndpoint {
  endpointId: pulumi.Output<string>;
  securityGroupId: pulumi.Output<string>;
  service: pulumi.Output<string>;
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
  /**
   * The VPC endpoint the target reaches this service through. Anvil then:
   *  - adds ingress 443 on the endpoint's SG from the target's own SG, and
   *    egress 443 on the target's SG to the endpoint's SG (once per pair);
   *  - limits this grant's IAM statement to the endpoint with aws:SourceVpce,
   *    so the permission does not work from outside it.
   * The endpoint's service must match the granted service (e.g. "sqs").
   */
  endpoint?: GrantEndpoint;
}

/**
 * Returns the constructor args a component was built with. Stashed by the
 * construction wrapper (stack.ts) next to the name. Undefined when the
 * component was not built through the anvil namespace.
 * @internal
 */
export function grantResourceArgs(resource: object): any {
  return (resource as any).__anvilArgs;
}

/**
 * SG rule pairs already created in this program, keyed by
 * "<endpoint>|<target>|<port>". Several grants from one target to the same
 * endpoint share one pair: AWS rejects duplicate rules.
 */
const wiredEndpointPairs = new Set<string>();

/**
 * Opens one-way network access from a target to a VPC endpoint:
 * ingress on the endpoint SG from the target SG, and egress on the target SG
 * to the endpoint SG, both on `port`. Created once per (endpoint, target, port).
 * The target never carries the endpoint SG, so targets never trust each other.
 *
 * The rules have no parent: they read the endpoint's and the target's outputs,
 * and a child of a component that reads that component's outputs hangs the
 * program (see createGrant). The App's default-provider transformation gives
 * them the app's AWS provider.
 *
 * @param endpointName logical name of the endpoint, used in the rule names
 * @internal
 */
export function connectToEndpoint(
  endpointName: string,
  endpointSecurityGroupId: pulumi.Input<string>,
  target: GrantTarget,
  port: number
): void {
  if (typeof target.grantSecurityGroupId !== 'function') {
    throw new Error(
      `Anvil grant: ${target.grantName()} cannot use a VPC endpoint because it has no security group. ` +
        'Only VPC-attached compute (e.g. a Lambda with vpc set) can be granted through an endpoint.'
    );
  }
  const key = `${endpointName}|${target.grantName()}|${port}`;
  if (wiredEndpointPairs.has(key)) {
    return;
  }
  wiredEndpointPairs.add(key);

  const targetSgId = target.grantSecurityGroupId().apply((id) => {
    if (!id) {
      throw new Error(
        `Anvil grant: ${target.grantName()} is not in a VPC, so it cannot be granted through endpoint ${endpointName}. ` +
          'Set vpc on the target, or drop the endpoint option.'
      );
    }
    return id;
  });
  const tags = { ManagedBy: 'anvil' };

  new aws.vpc.SecurityGroupIngressRule(
    `${endpointName}-from-${target.grantName()}-${port}`,
    {
      securityGroupId: endpointSecurityGroupId,
      referencedSecurityGroupId: targetSgId,
      ipProtocol: 'tcp',
      fromPort: port,
      toPort: port,
      tags,
    }
  );
  new aws.vpc.SecurityGroupEgressRule(
    `${target.grantName()}-to-${endpointName}-${port}`,
    {
      securityGroupId: targetSgId,
      referencedSecurityGroupId: endpointSecurityGroupId,
      ipProtocol: 'tcp',
      fromPort: port,
      toPort: port,
      tags,
    }
  );
}

/**
 * Throws when the endpoint's service is known synchronously (from its
 * constructor args) and differs from the granted service.
 */
function checkEndpointServiceSync(
  endpointName: string,
  endpoint: GrantEndpoint,
  service: string
): void {
  const declared = grantResourceArgs(endpoint)?.service;
  if (typeof declared === 'string' && declared !== service) {
    throw new Error(
      `Anvil grant: endpoint ${endpointName} routes "${declared}", but this grant is for "${service}". ` +
        `Pass an endpoint created with service: "${service}".`
    );
  }
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
  opts?: GrantOptions,
  service?: string
): void {
  const endpoint = opts?.endpoint;
  if (endpoint) {
    if (!service) {
      throw new Error(
        `Anvil grant: ${name} does not support the endpoint option.`
      );
    }
    const endpointName = grantResourceName(endpoint);
    checkEndpointServiceSync(endpointName, endpoint, service);
    connectToEndpoint(
      endpointName,
      endpoint.securityGroupId,
      target,
      443
    );
  }

  const policyDocument = pulumi
    .all([
      pulumi.all(resourceArns),
      endpoint ? endpoint.endpointId : undefined,
      endpoint ? endpoint.service : undefined,
    ])
    .apply(([arns, endpointId, endpointService]) => {
      if (endpoint && endpointService !== service) {
        throw new Error(
          `Anvil grant: ${name} is for "${service}", but its endpoint routes "${endpointService}".`
        );
      }
      const statement: Record<string, unknown> = {
        Effect: 'Allow',
        Action: actions,
        Resource: arns,
      };
      if (endpointId) {
        // Only works through this endpoint: credentials used from anywhere
        // else (e.g. leaked from the function's environment) are denied.
        statement.Condition = {
          StringEquals: { 'aws:SourceVpce': endpointId },
        };
      }
      return JSON.stringify({ Version: '2012-10-17', Statement: [statement] });
    });

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

/**
 * Options for grantEndpointAccess.
 */
export interface EndpointAccessOptions {
  /**
   * IAM actions to allow through the endpoint, e.g. ["secretsmanager:GetSecretValue"].
   * Every action must belong to the endpoint's service. Requires `resources`.
   */
  actions?: string[];
  /** ARNs the actions apply to. Pass ["*"] only for actions that take no resource. */
  resources?: pulumi.Input<string>[];
}

/**
 * IAM action prefixes for VPC endpoint services whose names differ from
 * them. Any other service's actions must use the service name as prefix.
 */
const endpointActionPrefixes: Record<string, string[]> = {
  'kinesis-streams': ['kinesis'],
  'ecr.api': ['ecr'],
  'ecr.dkr': ['ecr'],
  monitoring: ['cloudwatch'],
};

/** @internal */
export function actionsOutsideService(service: string, actions: string[]): string[] {
  const allowed = endpointActionPrefixes[service] ?? [service];
  return actions.filter((a) => !allowed.includes(a.split(':')[0]));
}

/** 32-bit FNV-1a, hex. Keeps policy names short and stable. */
function shortHash(s: string): string {
  let h = 0x811c9dc5;
  for (const byte of Buffer.from(s, 'utf8')) {
    h ^= byte;
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h.toString(16).padStart(8, '0');
}

/** Endpoint access policies created in this program, by resource name. */
const endpointAccessPolicies = new Set<string>();

/**
 * Grants a VPC compute resource access to an AWS service through a VPC
 * endpoint. Use it for services Anvil has no component for (Secrets Manager,
 * KMS, SSM, STS, ECR, CloudWatch Logs, ...). For Anvil resources, prefer the
 * resource's own grant with `{ endpoint }`, which writes the actions for you.
 *
 * Always opens the network: ingress 443 on the endpoint's SG from the
 * target's own SG, and egress 443 on the target's SG to the endpoint's SG
 * (once per pair, shared with resource grants).
 *
 * With `actions` and `resources`, also adds an IAM statement on the target's
 * role, limited to this endpoint with aws:SourceVpce. Without them, the
 * target's IAM permissions for the service must come from elsewhere.
 *
 * @example
 *   anvil.grantEndpointAccess(fn, secretsEndpoint, {
 *     actions: ['secretsmanager:GetSecretValue'],
 *     resources: [dbSecretArn],
 *   });
 */
export function grantEndpointAccess(
  target: GrantTarget,
  endpoint: GrantEndpoint,
  opts?: EndpointAccessOptions
): void {
  const endpointName = grantResourceName(endpoint);
  const actions = opts?.actions ?? [];
  const resources = opts?.resources ?? [];
  if ((actions.length === 0) !== (resources.length === 0)) {
    throw new Error(
      `Anvil grant: grantEndpointAccess(${target.grantName()}, ${endpointName}) needs both actions and resources, or neither.`
    );
  }

  connectToEndpoint(endpointName, endpoint.securityGroupId, target, 443);
  if (actions.length === 0) {
    return;
  }

  const declared = grantResourceArgs(endpoint)?.service;
  if (typeof declared === 'string') {
    const outside = actionsOutsideService(declared, actions);
    if (outside.length > 0) {
      throw new Error(
        `Anvil grant: endpoint ${endpointName} routes "${declared}", so it cannot carry ${outside.join(', ')}.`
      );
    }
  }

  const name = `${endpointName}-${target.grantName()}-access-${shortHash([...actions].sort().join(','))}`;
  if (endpointAccessPolicies.has(name)) {
    throw new Error(
      `Anvil grant: ${target.grantName()} is already granted these actions through ${endpointName}. ` +
        'Put all the resources for the same actions in one grantEndpointAccess call.'
    );
  }
  endpointAccessPolicies.add(name);

  const policyDocument = pulumi
    .all([pulumi.all(resources), endpoint.endpointId, endpoint.service])
    .apply(([arns, endpointId, service]) => {
      const outside = actionsOutsideService(service, actions);
      if (outside.length > 0) {
        throw new Error(
          `Anvil grant: endpoint ${endpointName} routes "${service}", so it cannot carry ${outside.join(', ')}.`
        );
      }
      return JSON.stringify({
        Version: '2012-10-17',
        Statement: [
          {
            Effect: 'Allow',
            Action: actions,
            Resource: arns,
            Condition: { StringEquals: { 'aws:SourceVpce': endpointId } },
          },
        ],
      });
    });

  const roleName = target.grantRoleArn().apply((arn) => {
    const idx = arn.lastIndexOf('/');
    return idx >= 0 ? arn.substring(idx + 1) : arn;
  });

  // No parent: it reads the endpoint's and the target's outputs (see createGrant).
  new aws.iam.RolePolicy(name, { role: roleName, policy: policyDocument });
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
