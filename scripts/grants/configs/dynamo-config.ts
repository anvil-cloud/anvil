// Add to scripts/grants/configs.ts alongside QUEUE_GRANT_CONFIG etc.
// Confirm tsFile and pyFile filenames after running gen-sdk — the generator
// may produce dynamoDB.ts or dynamo_d_b.py depending on naming conventions.

import { GrantConfig } from '../types';

export const DYNAMODB_GRANT_CONFIG: GrantConfig = {
  tsFile: 'aws/dynamoDB.ts',
  pyFile: 'aws/dynamo_db.py',
  className: 'DynamoDB',
  arnProperty: 'tableArn',
  endpointService: 'dynamodb',
  supportsPaths: false, // DynamoDB grants are always table + /index/* — no user-facing path scoping
  supportsIndexes: true,
  grants: [
    {
      method: 'grantRead',
      actions: [
        'dynamodb:GetItem',
        'dynamodb:BatchGetItem',
        'dynamodb:Query',
        'dynamodb:Scan',
      ],
    },
    {
      method: 'grantWrite',
      actions: [
        'dynamodb:PutItem',
        'dynamodb:UpdateItem',
        'dynamodb:BatchWriteItem',
      ],
    },
    {
      method: 'grantReadWrite',
      actions: [
        'dynamodb:GetItem',
        'dynamodb:BatchGetItem',
        'dynamodb:Query',
        'dynamodb:Scan',
        'dynamodb:PutItem',
        'dynamodb:UpdateItem',
        'dynamodb:BatchWriteItem',
      ],
    },
    {
      method: 'grantDelete',
      actions: [
        'dynamodb:DeleteItem',
        // NOTE: dynamodb:DeleteTable intentionally excluded.
        // Table destruction is handled exclusively by Anvil's destroy command.
      ],
    },
  ],
};
