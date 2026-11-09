import { GrantConfig, GrantTargetConfig } from '../types';

export const LAMBDA_GRANT_CONFIG: GrantConfig = {
  tsFile: 'aws/lambda.ts',
  pyFile: 'aws/lambda_.py',
  className: 'Lambda',
  arnProperty: 'arn',
  endpointService: 'lambda',
  supportsPaths: false,
  grants: [{ method: 'grantInvoke', actions: ['lambda:InvokeFunction'] }],
};

export const LAMBDA_GRANT_TARGET_CONFIG: GrantTargetConfig = {
  tsFile: 'aws/lambda.ts',
  pyFile: 'aws/lambda_.py',
  className: 'Lambda',
  roleArnProperty: 'roleArn',
  pyRoleArnProperty: 'role_arn',
  securityGroupIdProperty: 'securityGroupId',
  pySecurityGroupIdProperty: 'security_group_id',
};
