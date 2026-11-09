// sdk/nodejs/vpc_defaults.ts
// Hand-written. Copied in from sdk/overlays/nodejs during gen-sdk.
//
// App-wide defaults for the security groups Anvil creates.

import * as pulumi from '@pulumi/pulumi';

/** App-wide defaults for VPC networking (App `defaults.vpc`). */
export interface VpcDefaultsConfig {
  /**
   * Put back the rule AWS adds to a new security group (all traffic to
   * 0.0.0.0/0) on every security group Anvil creates: Lambda, VPC endpoint
   * and DSQL endpoint. Off by default. A resource's own `allowAllOutbound`
   * wins over this. `anvil preview` warns for every group it applies to.
   */
  allowAllOutbound?: boolean;
}

/** Fill in allowAllOutbound on a vpc block that doesn't set it. */
function withVpcDefault(vpc: pulumi.Input<any>): pulumi.Output<any> {
  return pulumi.output(vpc).apply((v: any) =>
    v && v.allowAllOutbound === undefined ? { ...v, allowAllOutbound: true } : v
  );
}

/**
 * Registers a stack transformation that applies `defaults.vpc` to every
 * Anvil component that owns a security group, unless it sets the value itself.
 * @internal
 */
export function registerVpcDefaults(vpc?: VpcDefaultsConfig): void {
  if (!vpc?.allowAllOutbound) {
    return;
  }
  pulumi.runtime.registerStackTransformation((args) => {
    switch (args.type) {
      case 'anvil:aws:VpcEndpoint':
        if (args.props.allowAllOutbound !== undefined) return undefined;
        return { props: { ...args.props, allowAllOutbound: true }, opts: args.opts };
      case 'anvil:aws:Lambda':
      case 'anvil:aws:DSQL':
        if (args.props.vpc === undefined || args.props.vpc === null) return undefined;
        return { props: { ...args.props, vpc: withVpcDefault(args.props.vpc) }, opts: args.opts };
      default:
        return undefined;
    }
  });
}
