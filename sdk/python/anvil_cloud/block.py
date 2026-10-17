"""Block base class for organisational grouping of Anvil resources."""

from typing import Any, Dict, Mapping, Optional

import pulumi

from .stack import get_active_stack


class Block(pulumi.ComponentResource):
    """An optional organisational grouping for Anvil resources.

    Resources created inside a Block constructor are automatically parented
    to the Block via Pulumi's built-in auto-parenting. Blocks can expose
    public properties (outputs) for cross-Block references.

    ``stage`` and ``project`` are automatically available via ``self.stage``
    and ``self.project`` — read from the same Pulumi config that App sets.

    Tagging is automatic — the App's provider injection applies default tags
    to all resources, including those inside Blocks.

    Blocks are purely optional — flat top-level resources work identically.

    Example::

        import anvil_cloud as anvil

        class RulesEngine(anvil.Block):
            def __init__(self, name: str, opts: pulumi.ResourceOptions | None = None):
                super().__init__(name, opts=opts)

                bucket = anvil.aws.Bucket("events",
                    data_classification="internal",
                    hatch={"bucket": {"bucket": f"rules-{self.stage}-events"}},
                )
    """

    stage: str
    """The current deployment stage (e.g. "dev", "staging", "prod", or OS username)."""

    project: str
    """The project name from anvil.yaml."""

    def __init__(
        self,
        name: str,
        args: Optional[Mapping[str, Any]] = None,
        opts: Optional[pulumi.ResourceOptions] = None,
    ) -> None:
        """Create a new Block.

        Args:
            name: The unique name of this Block within the stack.
            args: Optional arguments (passed through for subclass use).
            opts: Standard Pulumi ResourceOptions (aliases, providers, etc.).
        """
        type_name = type(self).__name__
        super().__init__(
            f"anvil:block:{type_name}",
            name,
            args or {},
            opts,
        )

        anvil_config = pulumi.Config("anvil")
        self.stage = anvil_config.require("stage")
        self.project = pulumi.get_project()

        # The block's published outputs — its public surface for ctx.ref(...).
        # Registered now (empty) and mutated by self.output(...) as the subclass
        # constructor body runs; forward references resolve after run() completes.
        self.__outputs: Dict[str, pulumi.Output] = {}
        stack = get_active_stack()
        if stack is not None:
            stack.register(name, self.__outputs)

    def output(self, name: str, value: pulumi.Input) -> None:
        """Publish an output on this block, making it reachable via
        ``ctx.ref(...).<name>``.

        A block may only publish outputs that originate from the resources it
        owns — it is a container for its own resources, not a relay for others'.

        Example::

            self.output("bucket_name", events.bucket_name)
        """
        self.__outputs[name] = pulumi.Output.from_input(value)
