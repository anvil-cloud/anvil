import { GrantConfig } from '../types';

export const QUEUE_GRANT_CONFIG: GrantConfig = {
  tsFile: 'aws/queue.ts',
  pyFile: 'aws/queue.py',
  className: 'Queue',
  arnProperty: 'arn',
  endpointService: 'sqs',
  supportsPaths: false,
  grants: [
    {
      method: 'grantSendMessage',
      actions: ['sqs:SendMessage'],
    },
    {
      method: 'grantConsumeMessages',
      actions: [
        'sqs:ReceiveMessage',
        'sqs:DeleteMessage',
        'sqs:GetQueueAttributes',
      ],
    },
    {
      method: 'grantFullAccess',
      actions: [
        'sqs:SendMessage',
        'sqs:ReceiveMessage',
        'sqs:DeleteMessage',
        'sqs:GetQueueAttributes',
        'sqs:ChangeMessageVisibility',
        'sqs:PurgeQueue',
      ],
      isFullAccess: true,
    },
  ],
};
