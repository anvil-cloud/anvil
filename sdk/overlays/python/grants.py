# sdk/python/anvil_cloud/grants.py
# Hand-written. Backed up/restored during gen-sdk like app.py and block.py.
#
# Provides the runtime grant execution for all resource grant methods.

from typing import Any, List, Optional, Protocol, Set

import pulumi
import pulumi_aws as aws


class GrantTarget(Protocol):
    """Any Anvil compute resource that can receive IAM permissions."""

    def grant_name(self) -> str: ...
    def grant_role_arn(self) -> pulumi.Output[str]: ...


class GrantOptions:
    """Optional metadata for grant methods.

    endpoint: the Anvil VpcEndpoint the target reaches this service through.
    Anvil then adds ingress 443 on the endpoint's SG from the target's own SG,
    and egress 443 on the target's SG to the endpoint's SG (once per pair),
    and limits this grant's IAM statement to the endpoint with aws:SourceVpce.
    The endpoint's service must match the granted service (e.g. "sqs").
    """

    def __init__(self, justification: Optional[str] = None, endpoint: Any = None):
        self.justification = justification
        self.endpoint = endpoint


def merge_options(opts: Optional[GrantOptions] = None, justification: Optional[str] = None,
                  endpoint: Any = None) -> Optional[GrantOptions]:
    """Combine an optional GrantOptions with the keyword shortcuts grant methods accept."""
    j = justification if justification is not None else (opts.justification if opts else None)
    e = endpoint if endpoint is not None else (opts.endpoint if opts else None)
    if j is None and e is None:
        return None
    return GrantOptions(justification=j, endpoint=e)


def grant_resource_args(resource) -> Any:
    """Return the (args, kwargs) a component was constructed with, or None.

    Stashed by the construction wrapper (stack.py) next to the name.
    """
    return getattr(resource, "_anvil_ctor", None)


def ctor_input(resource, name: str) -> Any:
    """Read one top-level constructor input by its Python name, from either
    the keyword arguments or a positional args object/dict. None when unknown."""
    ctor = grant_resource_args(resource)
    if not ctor:
        return None
    args, kwargs = ctor
    if name in kwargs:
        return kwargs[name]
    if args:
        obj = args[0]
        if isinstance(obj, dict):
            return obj.get(name)
        return getattr(obj, name, None)
    return None


def input_field(obj, name: str) -> Any:
    """Read a field from a nested input given as an args class or a dict."""
    if obj is None:
        return None
    if isinstance(obj, dict):
        return obj.get(name)
    return getattr(obj, name, None)


# SG rule pairs already created in this program, keyed by
# "<endpoint>|<target>|<port>". Several grants from one target to the same
# endpoint share one pair: AWS rejects duplicate rules.
_wired_endpoint_pairs: Set[str] = set()


def connect_to_endpoint(endpoint_name: str, endpoint_sg_id, target, port: int) -> None:
    """Open one-way network access from a target to a VPC endpoint.

    Ingress on the endpoint SG from the target SG, and egress on the target SG
    to the endpoint SG, both on ``port``, created once per (endpoint, target,
    port). The target never carries the endpoint SG, so targets never trust
    each other.

    The rules have no parent: they read the endpoint's and the target's
    outputs, and a child of a component that reads that component's outputs
    waits on itself. The App's default-provider transformation gives them the
    app's AWS provider.
    """
    if not callable(getattr(target, "grant_security_group_id", None)):
        raise ValueError(
            f"Anvil grant: {target.grant_name()} cannot use a VPC endpoint because it has no "
            "security group. Only VPC-attached compute (e.g. a Lambda with vpc set) can be "
            "granted through an endpoint."
        )
    key = f"{endpoint_name}|{target.grant_name()}|{port}"
    if key in _wired_endpoint_pairs:
        return
    _wired_endpoint_pairs.add(key)

    target_name = target.grant_name()

    def _require(sg_id):
        if not sg_id:
            raise ValueError(
                f"Anvil grant: {target_name} is not in a VPC, so it cannot be granted through "
                f"endpoint {endpoint_name}. Set vpc on the target, or drop the endpoint option."
            )
        return sg_id

    target_sg_id = target.grant_security_group_id().apply(_require)
    tags = {"ManagedBy": "anvil"}

    aws.vpc.SecurityGroupIngressRule(
        f"{endpoint_name}-from-{target_name}-{port}",
        security_group_id=endpoint_sg_id,
        referenced_security_group_id=target_sg_id,
        ip_protocol="tcp",
        from_port=port,
        to_port=port,
        tags=tags,
    )
    aws.vpc.SecurityGroupEgressRule(
        f"{target_name}-to-{endpoint_name}-{port}",
        security_group_id=target_sg_id,
        referenced_security_group_id=endpoint_sg_id,
        ip_protocol="tcp",
        from_port=port,
        to_port=port,
        tags=tags,
    )


def grant_resource_name(resource) -> str:
    """Return the logical name a component was constructed with.

    The name is stashed on the instance by the construction wrapper (stack.py),
    so grant methods can build synchronous child-resource names without the
    generated SDK class declaring the attribute itself.
    """
    name = getattr(resource, "_anvil_name", None)
    if not isinstance(name, str):
        raise ValueError(
            "Anvil grant: could not resolve the resource name. Construct "
            "components via the anvil namespace (e.g. anvil.aws.Bucket(...)) "
            "so grants work."
        )
    return name


def create_grant(
    parent: pulumi.Resource,
    name: str,
    target: GrantTarget,
    actions: List[str],
    resource_arns: List[pulumi.Output[str]],
    opts: Optional[GrantOptions] = None,
    service: Optional[str] = None,
) -> None:
    """
    Creates a scoped IAM RolePolicy granting the specified actions on the
    specified resource ARNs to the target's execution role.
    """
    import json
    import re

    endpoint = opts.endpoint if opts else None
    if endpoint is not None:
        if not service:
            raise ValueError(f"Anvil grant: {name} does not support the endpoint option.")
        endpoint_name = grant_resource_name(endpoint)
        declared = ctor_input(endpoint, "service")
        if isinstance(declared, str) and declared != service:
            raise ValueError(
                f"Anvil grant: endpoint {endpoint_name} routes \"{declared}\", but this grant is "
                f"for \"{service}\". Pass an endpoint created with service=\"{service}\"."
            )
        connect_to_endpoint(endpoint_name, endpoint.security_group_id, target, 443)

    def _policy(values):
        arns, endpoint_id, endpoint_service = values
        if endpoint is not None and endpoint_service != service:
            raise ValueError(
                f"Anvil grant: {name} is for \"{service}\", but its endpoint routes "
                f"\"{endpoint_service}\"."
            )
        statement = {
            "Effect": "Allow",
            "Action": actions,
            "Resource": list(arns),
        }
        if endpoint_id:
            # Only works through this endpoint: credentials used from anywhere
            # else (e.g. leaked from the function's environment) are denied.
            statement["Condition"] = {"StringEquals": {"aws:SourceVpce": endpoint_id}}
        return json.dumps({"Version": "2012-10-17", "Statement": [statement]})

    policy_document = pulumi.Output.all(
        pulumi.Output.all(*resource_arns),
        endpoint.endpoint_id if endpoint is not None else None,
        endpoint.service if endpoint is not None else None,
    ).apply(_policy)

    role_name = target.grant_role_arn().apply(
        lambda arn: arn[arn.rfind("/") + 1:] if "/" in arn else arn
    )

    policy_name = name
    if opts and opts.justification:
        suffix = re.sub(r"[^a-z0-9]+", "-", opts.justification.lower())[:40]
        policy_name = f"{name}-{suffix}"

    aws.iam.RolePolicy(
        policy_name,
        role=role_name,
        policy=policy_document,
        opts=pulumi.ResourceOptions(parent=parent),
    )


# IAM action prefixes for VPC endpoint services whose names differ from them.
# Any other service's actions must use the service name as prefix.
_ENDPOINT_ACTION_PREFIXES = {
    "kinesis-streams": ["kinesis"],
    "ecr.api": ["ecr"],
    "ecr.dkr": ["ecr"],
    "monitoring": ["cloudwatch"],
}


def actions_outside_service(service: str, actions: List[str]) -> List[str]:
    allowed = _ENDPOINT_ACTION_PREFIXES.get(service, [service])
    return [a for a in actions if a.split(":")[0] not in allowed]


def _short_hash(s: str) -> str:
    """32-bit FNV-1a, hex. Same as the TS and Go runtimes."""
    h = 0x811C9DC5
    for byte in s.encode("utf-8"):
        h ^= byte
        h = (h * 0x01000193) & 0xFFFFFFFF
    return f"{h:08x}"


# Endpoint access policies created in this program, by resource name.
_endpoint_access_policies: Set[str] = set()


def grant_endpoint_access(target, endpoint, actions: Optional[List[str]] = None,
                          resources: Optional[List[Any]] = None) -> None:
    """Grant a VPC compute resource access to an AWS service through a VPC endpoint.

    Use it for services Anvil has no component for (Secrets Manager, KMS, SSM,
    STS, ECR, CloudWatch Logs, ...). For Anvil resources, prefer the resource's
    own grant with ``endpoint=``, which writes the actions for you.

    Always opens the network: ingress 443 on the endpoint's SG from the
    target's own SG, and egress 443 on the target's SG to the endpoint's SG
    (once per pair, shared with resource grants).

    With ``actions`` and ``resources``, also adds an IAM statement on the
    target's role limited to this endpoint with aws:SourceVpce. Without them,
    the target's IAM permissions for the service must come from elsewhere.

    Example::

        anvil.grant_endpoint_access(fn, secrets_endpoint,
            actions=["secretsmanager:GetSecretValue"],
            resources=[db_secret_arn])
    """
    endpoint_name = grant_resource_name(endpoint)
    actions = list(actions or [])
    resources = list(resources or [])
    if bool(actions) != bool(resources):
        raise ValueError(
            f"Anvil grant: grant_endpoint_access({target.grant_name()}, {endpoint_name}) needs "
            "both actions and resources, or neither."
        )

    connect_to_endpoint(endpoint_name, endpoint.security_group_id, target, 443)
    if not actions:
        return

    declared = ctor_input(endpoint, "service")
    if isinstance(declared, str):
        outside = actions_outside_service(declared, actions)
        if outside:
            raise ValueError(
                f"Anvil grant: endpoint {endpoint_name} routes \"{declared}\", so it cannot carry "
                f"{', '.join(outside)}."
            )

    name = f"{endpoint_name}-{target.grant_name()}-access-{_short_hash(','.join(sorted(actions)))}"
    if name in _endpoint_access_policies:
        raise ValueError(
            f"Anvil grant: {target.grant_name()} is already granted these actions through "
            f"{endpoint_name}. Put all the resources for the same actions in one "
            "grant_endpoint_access call."
        )
    _endpoint_access_policies.add(name)

    import json

    def _policy(values):
        arns, endpoint_id, service = values
        outside = actions_outside_service(service, actions)
        if outside:
            raise ValueError(
                f"Anvil grant: endpoint {endpoint_name} routes \"{service}\", so it cannot carry "
                f"{', '.join(outside)}."
            )
        return json.dumps({
            "Version": "2012-10-17",
            "Statement": [{
                "Effect": "Allow",
                "Action": actions,
                "Resource": list(arns),
                "Condition": {"StringEquals": {"aws:SourceVpce": endpoint_id}},
            }],
        })

    policy_document = pulumi.Output.all(
        pulumi.Output.all(*resources), endpoint.endpoint_id, endpoint.service
    ).apply(_policy)
    role_name = target.grant_role_arn().apply(
        lambda arn: arn[arn.rfind("/") + 1:] if "/" in arn else arn
    )
    # No parent: it reads the endpoint's and the target's outputs.
    aws.iam.RolePolicy(name, role=role_name, policy=policy_document)


def build_resource_arns(
    base_arn: pulumi.Output[str],
    paths: Optional[List[str]] = None,
) -> List[pulumi.Output[str]]:
    """Builds the list of ARNs for a grant based on a base ARN and optional path scoping."""
    arns: List[pulumi.Output[str]] = [base_arn]

    if not paths:
        arns.append(base_arn.apply(lambda arn: f"{arn}/*"))
    else:
        for p in paths:
            arns.append(base_arn.apply(lambda arn, _p=p: f"{arn}/{_p}"))

    return arns
