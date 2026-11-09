# VPC endpoint grants

How compute reaches an interface VPC endpoint, and why.

## The model

A grant that passes an endpoint opens the network and the IAM permission together:

```ts
queue.grantSendMessage(fn, { endpoint: sqsEndpoint });
```

| Layer | What the grant adds |
|---|---|
| Network | `endpoint-sg` ingress 443 from `fn-sg`, and `fn-sg` egress 443 to `endpoint-sg`. Once per (endpoint, target, port), at the stack root. |
| IAM | The grant's statement gets `Condition: { StringEquals: { aws:SourceVpce: <endpointId> } }`. |
| Endpoint policy | Unchanged per grant: `Principal: "*"` limited to this account with `aws:PrincipalAccount`; `overridePermissions` narrows the actions. |

Compute only ever carries its own security group. The endpoint SG starts with zero rules and only gains ingress from granted compute.

DSQL owns its endpoint, so `dsql.grantConnect(fn, ...)` wires the same pair on 5432 without an `endpoint` option. It does not add `aws:SourceVpce` (the IAM side is `dsql:DbConnect`, handled by `DSQLConnect`).

## Services without a component

Most services an endpoint is created for (Secrets Manager, KMS, SSM, STS, ECR, CloudWatch Logs) have no Anvil component to hang a grant on, so there is a free function:

```ts
anvil.grantEndpointAccess(fn, secretsEndpoint, {
  actions: ['secretsmanager:GetSecretValue'],
  resources: [dbSecretArn],
});
```

- Always opens the same rule pair, deduped with resource grants.
- With `actions` + `resources` (both or neither), adds a `aws:SourceVpce`-limited statement. Every action must belong to the endpoint's service; a few services map to a different action prefix (`kinesis-streams` → `kinesis`, `ecr.api`/`ecr.dkr` → `ecr`, `monitoring` → `cloudwatch`).
- The policy is named `<endpoint>-<target>-access-<fnv1a(sorted actions)>`, the same in all three SDKs. Granting the same actions twice for one pair is an error: put all their resources in one call.
- Resource grants stay the preferred path where a component exists, because they write least-privilege actions on the exact ARN.

## What it replaced

Compute used to join the endpoint's security group (`vpc.vpcEndpoints` on Lambda), and the endpoint SG allowed ingress from itself. That had three problems:

1. **Lambda's 5-SG cap.** Own SG plus one per endpoint: a fifth endpoint made `CreateFunction` fail.
2. **Members trusted each other.** The self-ingress rule applies to every ENI in the group, so anything sharing an endpoint could reach every other member on 443 (5432 for DSQL, which also had self-egress). Harmless while members were Lambdas; a lateral path once anything listening joins.
3. **Network access outlived IAM.** Removing a grant left the `vpcEndpoints` entry, and the network path, behind.

`vpc.vpcEndpoints` is removed. Migration: delete it from the Lambda and pass `endpoint` on each grant that uses that service.

## Decisions

- **Explicit, not discovered.** The grant takes the endpoint rather than finding "the SQS endpoint in this VPC". No registry, and the config shows exactly which path each permission uses.
- **Rules belong to the grant, not the constructor.** Grants run after construction, so the rules are standalone `SecurityGroupIngressRule` / `EgressRule` resources, not `vpcConfig` membership.
- **Deduped per pair.** AWS rejects duplicate rules, and several grants (send + consume, two queues) often share one endpoint and target. The runtime keeps a set keyed by `endpoint|target|port`.
- **No parent.** The rules read the endpoint's and the target's outputs, and a child of a component that reads that component's outputs waits on itself and hangs the program (the same reason grant policies aren't parented). At the root, their URNs also don't depend on which grant created them first. TS and Python get the app's AWS provider from the default-provider transformation; Go has none, so `anvil.Endpoint(name, ep, ctx.Provider("aws"))` and `anvil.ConnectToDSQL(..., opts...)` take the provider.
- **Service checked.** The endpoint's `service` output must match the grant (`sqs`, `s3`, `dynamodb`, `events`, `kinesis-streams`, `lambda`). TS and Python also check synchronously from the endpoint's constructor args, so a mismatch fails before anything is planned. Go derives the service from the action prefix.
- **Constructor args are stashed.** The construction wrappers (`stack.ts`, `stack.py`) now keep the constructor args next to the name, so grants can read plain inputs: an endpoint's `service`, or whether a DSQL cluster has an endpoint (`vpc` set without `hasNat`).

## Allowing all outbound (opt-in)

Anvil security groups start with no outbound rules. `allowAllOutbound` puts back AWS's default rule (protocol `-1`, 0.0.0.0/0) and logs a preview warning:

- per resource: Lambda and DSQL `vpc.allowAllOutbound`, VpcEndpoint `allowAllOutbound`;
- app-wide: `defaults.vpc.allowAllOutbound`, applied by a stack transformation in each SDK's App to every Anvil component that owns a security group, unless the resource sets it itself.

It covers destinations too (endpoints), by choice: one switch for every Anvil group. The VPC's default security group stays locked (CIS / Security Hub EC2.2). Grants still add their own outbound rules while it is on, so they keep working if it is turned off. It does not open an endpoint's inbound side.

## Limits and gotchas

- **Private DNS takes over the service hostname for the whole VPC.** Once an SQS endpoint exists, every SQS call from that VPC goes to it. A Lambda without an endpoint grant times out even if the VPC has NAT.
- **`aws:SourceVpce` only matches calls the code makes through the endpoint.** Event source mappings (SQS, Kinesis, DynamoDB streams) poll from the Lambda service, not the VPC, so they keep their own unconditioned policies, created by the trigger. Don't route their permissions through an endpoint grant.
- **60 ingress rules per SG by default.** One per granted compute, so at most 60 consumers per endpoint unless the quota is raised.
- **DSQL `multiRegion` + `vpc` without `hasNat` is rejected.** A VPC lives in one region, so the second region's endpoint can't be created in it.
- **TS/Python need namespace construction** (`new anvil.aws.VpcEndpoint(...)`) for the name and args stash. **Go** has no stash, so `anvil.LambdaTarget(name, fn)` and `anvil.Endpoint(name, ep)` take the logical name.
