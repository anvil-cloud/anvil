import { GrantMapConfig, CustomGrantConfig } from '../types';

export const DSQL_GRANT_CONFIG: GrantMapConfig = {
  tsFile: 'aws/dsql.ts',
  pyFile: 'aws/dsql.py',
  className: 'DSQL',
  arnMapProperty: 'clusterArns',
  pyArnMapProperty: 'cluster_arns',
  grants: [],
};

export const DSQL_CUSTOM_GRANT_CONFIG: CustomGrantConfig = {
  tsFile: 'aws/dsql.ts',
  pyFile: 'aws/dsql.py',
  detectMethod: 'grantConnect',
  tsSignature:
    'grantConnect(target: grants.GrantTarget, opts: { schemas: { schema: string; roleName: string }[] }): void;',
  tsImports: [
    'import { DSQLConnect } from "./dsqlconnect";',
  ],
  tsPropertyDeclarations: [
    '    declare public /*out*/ readonly rolesTableName: pulumi.Output<string>;',
  ],
  tsResourceInputs: [
    '            resourceInputs["rolesTableName"] = undefined /*out*/;',
  ],
  pyResourceInputs: [
    '            __props__.__dict__["roles_table_name"] = None',
  ],
  pyPropertyDeclarations: [
    `    @_builtins.property
    @pulumi.getter(name="rolesTableName")
    def roles_table_name(self) -> pulumi.Output[_builtins.str]:
        """
        DynamoDB table name for role bootstrap. Pass to DSQLConnect (via grant_connect) so it can create the IAM role-mapping item. Empty when no roles are configured.
        """
        return pulumi.get(self, "roles_table_name")`,
  ],
  tsMethod: `
    /**
     * Grants a compute resource connect access to this DSQL cluster.
     *
     * Pass one or more { schema, roleName } pairs. The compute may be mapped to
     * multiple roles and connects as whichever one it chooses at runtime. Each
     * role must be declared in this component's schemas, in that schema, or
     * the deploy fails.
     *
     * Creates an anvil:aws:DSQLConnect component that handles:
     * 1. IAM policy (dsql:DbConnect) on all cluster ARNs
     * 2. One DynamoDB IAM mapping item per role — the bootstrap Lambda runs
     *    AWS IAM GRANT "<roleName>" TO 'arn:...' for each.
     *
     * Network access is not wired here. When the cluster has VPC endpoints,
     * declare vpcEndpointSecurityGroupIds in the target's vpc.vpcEndpoints;
     * the endpoint SG lets its members reach the endpoint on port 5432.
     *
     * @param target - The compute resource to grant access to (Lambda, ECS, etc.).
     * @param opts.schemas - The { schema, roleName } pairs this compute may connect as.
     *
     * @example
     *   dsql.grantConnect(lambda, { schemas: [{ schema: 'my_app', roleName: 'app_role' }] });
     */
    public grantConnect(
        target: grants.GrantTarget,
        opts: { schemas: { schema: string; roleName: string }[] },
    ): void {
        const dbRoles = opts.schemas.map((s) => s.roleName);
        const args: any = {
            clusterArns: this.clusterArns,
            targetRoleArn: target.grantRoleArn(),
            targetName: target.grantName(),
            dbRoles: dbRoles,
            declaredRoles: this.declaredRoles,
            dbRoleSchemas: Object.fromEntries(opts.schemas.map((s) => [s.roleName, s.schema])),
        };
        if (this.rolesTableName) {
            args.rolesTableName = this.rolesTableName;
        }
        // NOT parented to this DSQL component: DSQLConnect depends on this
        // component's outputs (clusterArns, rolesTableName). Parenting would
        // create a cycle — the parent's outputs only finalize after its
        // children complete, but this child needs those outputs to construct.
        new DSQLConnect(\`\${this.__name}-\${target.grantName()}-connect\`, args);
    }`,
  pyMethod: `
    def grant_connect(self, target: "grants.GrantTarget", schemas: list = None) -> None:
        """Grants a compute resource connect access to this DSQL cluster.

        Pass one or more {"schema", "role_name"} pairs. The compute may be mapped
        to multiple roles and connects as whichever one it chooses at runtime.
        Each role must be declared in this component's schemas, in that schema,
        or the deploy fails.

        Network access is not wired here. When the cluster has VPC endpoints,
        declare vpc_endpoint_security_group_ids in the target's vpc.vpc_endpoints;
        the endpoint SG lets its members reach the endpoint on port 5432.

        Args:
            target: The compute resource to grant access to (Lambda, ECS, etc.).
            schemas: List of {"schema": ..., "role_name": ...} pairs.
        """
        from .dsql_connect import DSQLConnect
        schemas = schemas or []
        db_roles = [s["role_name"] for s in schemas]
        DSQLConnect(f"{self._name}-{target.grant_name()}-connect",
            cluster_arns=self.cluster_arns,
            target_role_arn=target.grant_role_arn(),
            target_name=target.grant_name(),
            db_roles=db_roles,
            declared_roles=self.declared_roles,
            db_role_schemas={s["role_name"]: s["schema"] for s in schemas},
            roles_table_name=self.roles_table_name,
            opts=pulumi.ResourceOptions(parent=self),
        )
`,
};
