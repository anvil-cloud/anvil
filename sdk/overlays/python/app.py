"""App entry point for Anvil infrastructure programs.

Wraps the Pulumi runtime so users never import ``pulumi`` directly. ``run()``
reads stage/project from config, creates providers with default tags, registers
default-provider injection, and invokes the user's callback with a Context.

Config classes (AppConfig, DefaultsConfig, provider configs) live in ``types.py``
and are re-exported via the package root. This module owns the runtime: Context,
App, and run().
"""

from __future__ import annotations

import json
import os
from typing import Dict, Optional

import pulumi

from .types import (
    AppConfig,
    AwsProviderConfig,
    ComplianceConfig,
    ComplianceCron,
    DefaultsConfig,
    GcpProviderConfig,
    StateConfig,
)
from .stack import Stack, set_active_stack


# ── Context ────────────────────────────────────────────────


class Context:
    """Passed to the App's ``run`` callback — the user's only interface to Anvil
    runtime information.
    """

    def __init__(
        self,
        stage: str,
        project: str,
        providers: Dict[str, pulumi.ProviderResource],
        stack: Stack,
    ) -> None:
        #: The current deployment stage (e.g. "dev", "staging", "prod").
        self.stage = stage
        #: The project name from anvil.yaml.
        self.project = project
        #: Named providers, keyed by config name (e.g. "aws", "aws.us", "gcp.eu").
        self.providers = providers
        self._stack = stack

    def export(self, name: str, value: pulumi.Input) -> None:
        """Export a stack output value without importing Pulumi."""
        pulumi.export(name, value)

    def provider(self, name: str) -> Optional[pulumi.ProviderResource]:
        """Return a named provider for explicit ``provider=`` wiring."""
        return self.providers.get(name)

    def ref(self, id: str):
        """Forward-reference a resource or block by its logical id, before or
        after it is declared. Attribute access yields the referenced resource's
        Output, so declaration order no longer matters::

            l = anvil.aws.Lambda("lam", environment={
                "DSQL_ENDPOINT": ctx.ref("db").endpoint,   # forward ref
            })
            db = anvil.aws.DSQL("db", schemas=[...])        # declared after — fine

        A reference to an id that is never declared raises after ``run()``
        completes — it will not silently resolve to nothing.
        """
        return self._stack.ref(id)


# ── Helpers ────────────────────────────────────────────────


def _cloud_of(key: str) -> str:
    """"aws" -> "aws", "aws.us" -> "aws", "gcp.eu" -> "gcp"."""
    dot = key.find(".")
    return key if dot == -1 else key[:dot]


def _build_merged_tags(
    stage: str, project: str, defaults: Optional[DefaultsConfig]
) -> Dict[str, str]:
    # stage and project are auto-injected; user tags override them.
    merged: Dict[str, str] = {"stage": stage, "project": project}
    if defaults and defaults.tags:
        merged.update(defaults.tags)
    return merged


def _make_aws_provider(
    name: str, cfg: AwsProviderConfig, merged_tags: Dict[str, str]
) -> pulumi.ProviderResource:
    import pulumi_aws as aws

    args: dict = {"default_tags": {"tags": merged_tags}}
    if cfg.region:
        args["region"] = cfg.region
    if cfg.profile:
        args["profile"] = cfg.profile
    if cfg.assume_role:
        args["assume_roles"] = [
            {
                "role_arn": cfg.assume_role.role_arn,
                "session_name": cfg.assume_role.session_name,
                "external_id": cfg.assume_role.external_id,
            }
        ]
    return aws.Provider(name, **args)


def _make_gcp_provider(
    name: str, cfg: GcpProviderConfig, merged_tags: Dict[str, str]
) -> pulumi.ProviderResource:
    import pulumi_gcp as gcp

    args: dict = {"default_labels": merged_tags}
    if cfg.project:
        args["project"] = cfg.project
    if cfg.region:
        args["region"] = cfg.region
    if cfg.zone:
        args["zone"] = cfg.zone
    if cfg.credentials:
        args["credentials"] = cfg.credentials
    return gcp.Provider(name, **args)


def _register_default_provider_injection(
    default_providers: Dict[str, pulumi.ProviderResource],
) -> None:
    """Inject the matching default provider into any resource that doesn't set one,
    based on the resource type's cloud prefix. Mirrors the TypeScript SDK.
    """

    def _transform(
        args: pulumi.ResourceTransformationArgs,
    ) -> Optional[pulumi.ResourceTransformationResult]:
        type_parts = args.type_.split(":")
        cloud: Optional[str] = None
        if type_parts[0] == "anvil" and len(type_parts) >= 3:
            cloud = type_parts[1]
        elif len(type_parts) >= 2:
            cloud = type_parts[0]

        if cloud and cloud in default_providers and not args.opts.provider:
            return pulumi.ResourceTransformationResult(
                props=args.props,
                opts=pulumi.ResourceOptions.merge(
                    args.opts,
                    pulumi.ResourceOptions(provider=default_providers[cloud]),
                ),
            )
        return None

    pulumi.runtime.register_stack_transformation(_transform)


def _write_state_settings(state: Optional[StateConfig]) -> None:
    """During the CLI's discovery run, write defaults.state where the CLI reads
    it before Pulumi runs (ANVIL_APP_SETTINGS_PATH). Values are validated by
    StateConfig. Must match cmd/anvil/cmd/state_settings.go."""
    path = os.environ.get("ANVIL_APP_SETTINGS_PATH")
    if os.environ.get("ANVIL_BUILD_MODE") != "true" or not path:
        return
    state = state or StateConfig()
    with open(path, "w") as f:
        json.dump(
            {
                "state": {
                    "retainVersions": state.retain_versions,
                    "compressState": state.compress_state,
                }
            },
            f,
            indent=2,
        )


# ── run / App ──────────────────────────────────────────────


def _run_program(app_config: AppConfig) -> None:
    """Execute an Anvil infrastructure program.

    Example::

        anvil.run(anvil.AppConfig(
            defaults=anvil.DefaultsConfig(tags={"team": "platform"}),
            aws_providers={"aws": anvil.AwsProviderConfig(region="ap-southeast-2")},
            run=main,
        ))
    """
    # ── Read config ────────────────────────────────────
    anvil_config = pulumi.Config("anvil")
    stage = anvil_config.require("stage")
    project = pulumi.get_project()

    defaults = app_config.defaults
    _write_state_settings(defaults.state if defaults else None)
    merged_tags = _build_merged_tags(stage, project, defaults)

    # ── Create providers ───────────────────────────────
    providers: Dict[str, pulumi.ProviderResource] = {}
    default_providers: Dict[str, pulumi.ProviderResource] = {}

    aws_providers = app_config.aws_providers or {}
    gcp_providers = app_config.gcp_providers or {}

    for key, cfg in aws_providers.items():
        provider = _make_aws_provider(f"anvil-provider-{key}", cfg, merged_tags)
        providers[key] = provider
        if "." not in key:
            default_providers["aws"] = provider

    for key, cfg in gcp_providers.items():
        provider = _make_gcp_provider(f"anvil-provider-{key}", cfg, merged_tags)
        providers[key] = provider
        if "." not in key:
            default_providers["gcp"] = provider

    # If no providers configured but default tags are set, create implicit
    # default providers to carry the tags (mirrors the TypeScript SDK).
    if not aws_providers and not gcp_providers and defaults and defaults.tags:
        import pulumi_aws as aws
        import pulumi_gcp as gcp

        aws_provider = aws.Provider(
            "anvil-provider-aws", default_tags={"tags": merged_tags}
        )
        providers["aws"] = aws_provider
        default_providers["aws"] = aws_provider

        gcp_provider = gcp.Provider(
            "anvil-provider-gcp", default_labels=merged_tags
        )
        providers["gcp"] = gcp_provider
        default_providers["gcp"] = gcp_provider

    if default_providers:
        _register_default_provider_injection(default_providers)

    # ── Create the forward-reference registry + Context ─
    stack = Stack()
    ctx = Context(stage, project, providers, stack)

    # ── Execute ────────────────────────────────────────
    # The active stack is set for the duration of run() so component construction
    # (and Block outputs) auto-register into it. validate() then turns any
    # never-declared forward reference into a clean error.
    set_active_stack(stack)
    try:
        if app_config.before_deploy:
            app_config.before_deploy(ctx)
        app_config.run(ctx)
        stack.validate()
        if app_config.after_deploy:
            app_config.after_deploy(ctx)
    except Exception as error:  # noqa: BLE001 - re-raised after optional hook
        if app_config.on_error:
            app_config.on_error(ctx, error)
        raise
    finally:
        set_active_stack(None)

    # ── Compliance ─────────────────────────────────────
    # Created outside the active stack so it isn't a ctx.ref target. The
    # default-provider transformation gives it the app's AWS provider.
    if app_config.compliance:
        _create_compliance_scanner(app_config.compliance, aws_providers)


def _create_compliance_scanner(
    compliance: ComplianceConfig, aws_providers: Dict[str, AwsProviderConfig]
) -> None:
    from .aws.compliance_scanner import ComplianceScanner

    schedule = compliance.schedule or "daily"
    is_cron = isinstance(schedule, ComplianceCron)
    regions = sorted({cfg.region for cfg in aws_providers.values() if cfg.region})
    ComplianceScanner(
        "anvil-compliance",
        frameworks=compliance.frameworks,
        schedule="cron" if is_cron else schedule,
        cron=schedule.cron if is_cron else None,
        timezone=schedule.timezone if is_cron else None,
        retention=compliance.retention,
        scan_on_deploy=compliance.scan_on_deploy,
        regions=regions,
    )


#: Public entry point. Aliased so App can call it without the parameter shadow.
run = _run_program


class App:
    """Convenience class mirroring the TypeScript ``new App({...})`` entry point.

    Equivalent to calling ``run(AppConfig(...))`` — the program executes
    immediately on construction.
    """

    def __init__(
        self,
        run,
        defaults: Optional[DefaultsConfig] = None,
        aws_providers: Optional[Dict[str, AwsProviderConfig]] = None,
        gcp_providers: Optional[Dict[str, GcpProviderConfig]] = None,
        before_deploy=None,
        after_deploy=None,
        on_error=None,
        compliance: Optional[ComplianceConfig] = None,
    ) -> None:
        _run_program(
            AppConfig(
                run=run,
                compliance=compliance,
                defaults=defaults,
                aws_providers=aws_providers,
                gcp_providers=gcp_providers,
                before_deploy=before_deploy,
                after_deploy=after_deploy,
                on_error=on_error,
            )
        )
