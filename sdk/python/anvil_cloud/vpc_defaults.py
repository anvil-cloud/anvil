# sdk/python/anvil_cloud/vpc_defaults.py
# Hand-written. Copied in from sdk/overlays/python during gen-sdk.
#
# App-wide defaults for the security groups Anvil creates.
import copy
from typing import Any, Optional

import pulumi


class VpcDefaultsConfig:
    """App-wide defaults for VPC networking (DefaultsConfig.vpc).

    allow_all_outbound: put back the rule AWS adds to a new security group (all
    traffic to 0.0.0.0/0) on every security group Anvil creates: Lambda, VPC
    endpoint and DSQL endpoint. Off by default. A resource's own
    allow_all_outbound wins over this. `anvil preview` warns for every group
    it applies to.
    """

    def __init__(self, allow_all_outbound: Optional[bool] = None):
        self.allow_all_outbound = allow_all_outbound


def _with_vpc_default(vpc: Any) -> Any:
    """Fill in allow_all_outbound on a vpc block (dict, args class or Output)."""
    if vpc is None:
        return None
    if isinstance(vpc, pulumi.Output):
        return vpc.apply(_with_vpc_default)
    if isinstance(vpc, dict):
        if vpc.get("allow_all_outbound") is not None:
            return vpc
        return {**vpc, "allow_all_outbound": True}
    if getattr(vpc, "allow_all_outbound", None) is not None:
        return vpc
    filled = copy.copy(vpc)
    pulumi.set(filled, "allow_all_outbound", True)
    return filled


def register_vpc_defaults(vpc: Optional[VpcDefaultsConfig]) -> None:
    """Apply defaults.vpc to every Anvil component that owns a security group,
    unless it sets the value itself."""
    if not vpc or not vpc.allow_all_outbound:
        return

    def _transform(args: pulumi.ResourceTransformationArgs):
        props = args.props
        if args.type_ == "anvil:aws:VpcEndpoint":
            if props.get("allow_all_outbound") is not None:
                return None
            return pulumi.ResourceTransformationResult(
                props={**props, "allow_all_outbound": True}, opts=args.opts
            )
        if args.type_ in ("anvil:aws:Lambda", "anvil:aws:DSQL"):
            if props.get("vpc") is None:
                return None
            return pulumi.ResourceTransformationResult(
                props={**props, "vpc": _with_vpc_default(props["vpc"])}, opts=args.opts
            )
        return None

    pulumi.runtime.register_stack_transformation(_transform)
