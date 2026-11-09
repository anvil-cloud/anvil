import { GrantConfig } from '../types';

export const BUCKET_GRANT_CONFIG: GrantConfig = {
  tsFile: 'aws/bucket.ts',
  pyFile: 'aws/bucket.py',
  className: 'Bucket',
  arnProperty: 'arn',
  endpointService: 's3',
  supportsPaths: true,
  grants: [
    { method: 'grantRead', actions: ['s3:GetObject', 's3:ListBucket'] },
    { method: 'grantWrite', actions: ['s3:PutObject'] },
    {
      method: 'grantReadWrite',
      actions: ['s3:GetObject', 's3:ListBucket', 's3:PutObject'],
    },
    { method: 'grantDelete', actions: ['s3:DeleteObject'] },
    {
      method: 'grantFullAccess',
      actions: [
        's3:GetObject',
        's3:ListBucket',
        's3:PutObject',
        's3:DeleteObject',
      ],
      isFullAccess: true,
    },
  ],
};
