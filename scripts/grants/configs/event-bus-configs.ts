import { GrantConfig } from '../types';

export const EVENTBUS_GRANT_CONFIG: GrantConfig = {
  tsFile: 'aws/eventBus.ts',
  pyFile: 'aws/event_bus.py',
  className: 'EventBus',
  arnProperty: 'arn',
  endpointService: 'events',
  supportsPaths: false,
  grants: [
    {
      method: 'grantPutEvents',
      actions: ['events:PutEvents'],
    },
  ],
};
