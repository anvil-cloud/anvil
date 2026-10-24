"""
Re-exports of core Pulumi types so users never need to
import pulumi directly for standard operations.

Example::

    import anvil_cloud as anvil

    class MyComponent(anvil.ComponentResource):
        def __init__(self, name: str, opts: anvil.ResourceOptions | None = None):
            super().__init__("my:component:MyComponent", name, {}, opts)

    value: anvil.Output[str] = anvil.Output.from_input("hello")
"""

from typing import Any, Callable, Dict, List, Optional, Union

# ── Resource base classes ──────────────────────────────────
# Note: Python Pulumi has no ``ComponentResourceOptions`` (that is a TypeScript
# concept) — component resources use ``ResourceOptions``.
from pulumi import (
    ComponentResource,
    CustomResource,
    ResourceOptions,
    ProviderResource,
)

# ── Input/Output types ─────────────────────────────────────
from pulumi import (
    Output,
    Input,
    Inputs,
)

# ── Utility functions ──────────────────────────────────────
# Note: Python Pulumi has no top-level ``secret`` — it is ``Output.secret(...)``.
from pulumi import (
    export,
    get_project,
    get_stack,
    Config,
    output,
)

#: Mark a value secret. Aliased to the Output staticmethod for API parity with
#: the TypeScript SDK's ``anvil.secret``.
secret = Output.secret

# ── Escape hatch ───────────────────────────────────────────
# For anything not re-exported above, users can access the
# full Pulumi namespace without adding a separate dependency.
import pulumi


# ── Config Classes ─────────────────────────────────────────
# Typed configuration for App, providing intellisense and validation.


class StateConfig:
    """Settings for the stage's Pulumi state bucket.

    ``retain_versions`` is how many deploys' state the bucket keeps, counting the
    latest: 2 keeps the latest deploy's state and the one before, 3 also keeps
    the deploy before that. Older deploys' state is deleted after each deploy.
    Default 3, min 2, max 101.

    ``compress_state`` writes state gzipped. Default True. Switching either way
    reads existing state.
    """

    def __init__(
        self,
        retain_versions: int = 3,
        compress_state: bool = True,
    ):
        # bool is an int subclass; reject it so True isn't read as 1.
        if (
            not isinstance(retain_versions, int)
            or isinstance(retain_versions, bool)
            or not 2 <= retain_versions <= 101
        ):
            raise ValueError(
                "defaults.state.retain_versions must be a whole number "
                f"from 2 to 101 (got {retain_versions!r}). 1 would keep no "
                "previous state to recover from."
            )
        if not isinstance(compress_state, bool):
            raise ValueError(
                "defaults.state.compress_state must be True or False."
            )
        self.retain_versions = retain_versions
        self.compress_state = compress_state


class DefaultsConfig:
    """Default configuration applied to all resources."""

    def __init__(
        self,
        tags: Optional[Dict[str, str]] = None,
        state: Optional[StateConfig] = None,
    ):
        self.tags = tags or {}
        self.state = state or StateConfig()


class AssumeRoleConfig:
    """Configuration for assuming an IAM role."""

    def __init__(
        self,
        role_arn: str,
        session_name: Optional[str] = None,
        external_id: Optional[str] = None,
    ):
        self.role_arn = role_arn
        self.session_name = session_name
        self.external_id = external_id


class AwsProviderConfig:
    """Configuration for an AWS provider."""

    def __init__(
        self,
        region: Optional[str] = None,
        profile: Optional[str] = None,
        assume_role: Optional[AssumeRoleConfig] = None,
    ):
        self.region = region
        self.profile = profile
        self.assume_role = assume_role


class GcpProviderConfig:
    """Configuration for a GCP provider."""

    def __init__(
        self,
        project: Optional[str] = None,
        region: Optional[str] = None,
        zone: Optional[str] = None,
        credentials: Optional[str] = None,
    ):
        self.project = project
        self.region = region
        self.zone = zone
        self.credentials = credentials


class ComplianceCron:
    """A custom compliance scan schedule.

    ``cron`` is six AWS cron fields, e.g. ``"0 3 * * ? *"`` (at most once an
    hour). ``timezone`` is an IANA name, e.g. ``"Australia/Sydney"`` (default UTC).
    """

    def __init__(self, cron: str, timezone: Optional[str] = None):
        self.cron = cron
        self.timezone = timezone


class ComplianceConfig:
    """Scheduled compliance scans (Prowler) of this app's deployed resources.

    The first deploy with this set creates a scanner shared by every Anvil app
    in the account and region (you'll be asked to confirm).

    Example::

        compliance=anvil.ComplianceConfig(
            frameworks=["soc2", "iso27001", "cis"],
            schedule="daily",
        )

    Args:
        frameworks: Required. Friendly names (always the newest version Anvil
            pins): soc2, iso27001, cis, nist-800-53, nist-800-171, nist-csf,
            pci, hipaa, fsbp, gdpr, nis2, dora, fedramp-low,
            fedramp-moderate, cmmc, essential-eight, well-architected, c5,
            csa-ccm. Or any raw Prowler AWS compliance ID, which stays fixed
            (e.g. "cis_6.0_aws").
        schedule: "daily" (default), "weekly", "none", or a ComplianceCron.
            Presets run at a fixed time derived from the project and stage.
        retention: How long results are kept: 30d, 90d, 180d, 1y (default),
            2y, 7y.
        scan_on_deploy: Start a scan after each successful deploy.
    """

    def __init__(
        self,
        frameworks: List[str],
        schedule: Union[str, ComplianceCron] = "daily",
        retention: Optional[str] = None,
        scan_on_deploy: bool = False,
    ):
        self.frameworks = frameworks
        self.schedule = schedule
        self.retention = retention
        self.scan_on_deploy = scan_on_deploy


class AppConfig:
    """
    Typed configuration for anvil.run().

    Example::

        anvil.run(anvil.AppConfig(
            defaults=anvil.DefaultsConfig(
                tags={"team": "platform"},
            ),
            aws_providers={
                "aws": anvil.AwsProviderConfig(region="ap-southeast-2"),
            },
            run=main,
        ))
    """

    def __init__(
        self,
        run: Callable[["Context"], None],
        defaults: Optional[DefaultsConfig] = None,
        aws_providers: Optional[Dict[str, AwsProviderConfig]] = None,
        gcp_providers: Optional[Dict[str, GcpProviderConfig]] = None,
        before_deploy: Optional[Callable[["Context"], None]] = None,
        after_deploy: Optional[Callable[["Context"], None]] = None,
        on_error: Optional[Callable[["Context", Exception], None]] = None,
        compliance: Optional[ComplianceConfig] = None,
    ):
        self.run = run
        self.compliance = compliance
        self.defaults = defaults
        self.aws_providers = aws_providers
        self.gcp_providers = gcp_providers
        self.before_deploy = before_deploy
        self.after_deploy = after_deploy
        self.on_error = on_error