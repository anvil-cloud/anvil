import { GrantConfig } from '../types';

export const KINESIS_GRANT_CONFIG: GrantConfig = {
  tsFile: 'aws/kinesisStream.ts',
  pyFile: 'aws/kinesis_stream.py',
  className: 'KinesisStream',
  arnProperty: 'arn',
  supportsPaths: false,
  grants: [
    {
      method: 'grantRead',
      actions: [
        'kinesis:GetRecords',
        'kinesis:GetShardIterator',
        'kinesis:DescribeStream',
        'kinesis:DescribeStreamSummary',
        'kinesis:ListShards',
      ],
    },
    {
      method: 'grantWrite',
      actions: [
        'kinesis:PutRecord',
        'kinesis:PutRecords',
        'kinesis:DescribeStreamSummary',
        'kinesis:ListShards',
      ],
    },
    {
      method: 'grantFullAccess',
      actions: [
        'kinesis:PutRecord',
        'kinesis:PutRecords',
        'kinesis:GetRecords',
        'kinesis:GetShardIterator',
        'kinesis:DescribeStream',
        'kinesis:DescribeStreamSummary',
        'kinesis:ListShards',
      ],
      isFullAccess: true,
    },
  ],
};
