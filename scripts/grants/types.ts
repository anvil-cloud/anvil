export interface Grant {
  /** TS method name, e.g. "grantRead" — Python derived automatically via camelCase → snake_case */
  method: string;
  /** IAM actions, e.g. ["s3:GetObject", "s3:ListBucket"] */
  actions: string[];
  /** Marks as escape hatch — logs warning if no justification provided */
  isFullAccess?: boolean;
}

export interface GrantConfig {
  /** Path relative to sdk/nodejs/, e.g. "aws/bucket.ts" */
  tsFile: string;
  /** Path relative to sdk/python/anvil_cloud/, e.g. "aws/bucket.py" */
  pyFile: string;
  /** Generated class name to patch, e.g. "Bucket" */
  className: string;
  /** Property on class that holds the resource ARN, e.g. "arn" */
  arnProperty: string;
  /** Whether grant methods accept path scoping, e.g. ["uploads/*"] */
  supportsPaths: boolean;
  supportsIndexes?: boolean;
  /** Grant methods to inject on this resource */
  grants: Grant[];
}

export interface DynamoGrantOptions {
  indexes?: string[];
  justification?: string;
}

export interface GrantTargetConfig {
  /** Path relative to sdk/nodejs/ */
  tsFile: string;
  /** Path relative to sdk/python/anvil_cloud/ */
  pyFile: string;
  /** Generated class name to patch */
  className: string;
  /** TS property name for IAM role ARN, e.g. "roleArn" */
  roleArnProperty: string;
  /** Python property name for IAM role ARN, e.g. "role_arn" */
  pyRoleArnProperty: string;
}

/**
 * CustomGrantConfig is for resources with non-standard grant patterns
 * that can't be expressed by the generic generators. The method body
 * is provided as raw TS/Python strings.
 */
export interface CustomGrantConfig {
  tsFile: string;
  pyFile: string;
  detectMethod: string;
  // Declaration-merged method signature, e.g. `grantX(target: grants.GrantTarget, opts: {...}): void;`.
  // Must match tsMethod's parameters — this is what callers are type-checked against.
  tsSignature: string;
  tsMethod: string;
  pyMethod: string;
  tsImports?: string[];
  tsPropertyDeclarations?: string[];
  tsResourceInputs?: string[];
  // Python counterparts. Without these, a custom grant whose body references an
  // injected output (e.g. DSQL.roles_table_name) compiles in TS but raises
  // AttributeError in Python. pyMethod handles imports via a local `from ...`.
  pyPropertyDeclarations?: string[]; // class-level @property getters (full indentation)
  pyResourceInputs?: string[]; // `__props__.__dict__["x"] = None` lines for _internal_init
}

/**
 * GrantMapConfig is for resources where the ARN property is
 * Output<Record<string, string>> rather than Output<string>.
 * Used when a component exposes multiple ARNs keyed by region
 * (e.g. DSQL clusterArns). The grant method iterates all values
 * in the map and scopes the IAM policy to every ARN.
 */
export interface GrantMapConfig {
  /** Path relative to sdk/nodejs/, e.g. "aws/dsql.ts" */
  tsFile: string;
  /** Path relative to sdk/python/anvil_cloud/, e.g. "aws/dsql.py" */
  pyFile: string;
  /** Generated class name to patch, e.g. "DSQL" */
  className: string;
  /** Property on class that holds the ARN map, e.g. "clusterArns" */
  arnMapProperty: string;
  /** Python snake_case property name, e.g. "cluster_arns" */
  pyArnMapProperty: string;
  /** Grant methods to inject on this resource */
  grants: Grant[];
}
