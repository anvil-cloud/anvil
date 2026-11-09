"""Forward references for Anvil programs (Approach A — deferred-lookup refs).

Hand-written. Backed up/restored during gen-sdk like app.py, block.py, types.py,
grants.py.

Resources construct eagerly, as they always have. A Stack registry maps each
resource's logical id to the constructed object, and ``ctx.ref('id')`` hands back
a handle whose attribute access yields a pulumi.Output that resolves once that id
is registered — which may happen *later* in the same synchronous ``run()``,
letting resources be declared in any order.

Because a ref resolves (at deploy time, after ``run()`` has fully executed) to the
resource's *real* Output, Pulumi's dependency tracking, ordering, and cycle
detection all work unchanged. After ``run()`` returns, the App calls ``validate()``
to turn any never-declared reference into a clean error instead of a silent miss.

See docs/design/forward-references.md for the full design.
"""

from __future__ import annotations

import asyncio
import os
from typing import Any, Dict, Optional, Set

import pulumi


# ── Stack ──────────────────────────────────────────────────


class Stack:
    """Per-program registry that backs forward references.

    One Stack exists per ``run()``. It is not part of the public API — users
    interact with it only through ``ctx.ref(...)``.
    """

    def __init__(self) -> None:
        # Logical id -> constructed object (or a block's published outputs dict).
        self._registered: Dict[str, Any] = {}
        # Ids that some ctx.ref(...) asked for (used to report dangling refs).
        self._requested: Set[str] = set()
        # Per-id futures that a deferred ref awaits and register() completes.
        # Using a future (rather than reading the dict directly) makes resolution
        # robust to scheduling order: a ref coroutine scheduled before its
        # resource is constructed simply waits, instead of seeing an empty dict.
        self._slots: Dict[str, "asyncio.Future"] = {}

    def _slot(self, id: str) -> "asyncio.Future":
        fut = self._slots.get(id)
        if fut is None:
            fut = asyncio.get_event_loop().create_future()
            self._slots[id] = fut
            # If the resource was already registered (backward reference),
            # resolve the freshly created slot immediately.
            if id in self._registered:
                fut.set_result(self._registered[id])
        return fut

    def register(self, id: str, outputs: Any) -> None:
        """Record a constructed resource (or block output surface) under ``id``.

        :param id: The logical name passed to the resource/block constructor.
        :param outputs: The object whose attributes/keys are the resource's Outputs.
        """
        if id in self._registered:
            raise Exception(
                f'Anvil: duplicate resource id "{id}". Every resource and block in '
                f"an App must have a unique name — forward references address "
                f"resources by that name."
            )
        self._registered[id] = outputs
        # Resolve any forward-reference slot already waiting on this id.
        fut = self._slots.get(id)
        if fut is not None and not fut.done():
            fut.set_result(outputs)

    def ref(self, id: str) -> Any:
        """Return a forward-reference handle for ``id``.

        Attribute access on the handle yields a pulumi.Output that resolves to the
        referenced resource's output once ``id`` is registered.
        """
        self._requested.add(id)
        return _RefHandle(self, id)

    def validate(self) -> None:
        """Raise if any forward reference named an id that was never declared.

        Called by the App after ``run()`` returns, so a typo'd reference fails
        loudly instead of silently resolving to nothing.
        """
        missing = [i for i in self._requested if i not in self._registered]
        if not missing:
            return
        plural = len(missing) > 1
        names = ", ".join(f'"{m}"' for m in missing)
        raise Exception(
            f'Anvil: forward reference{"s" if plural else ""} to undeclared '
            f'resource{"s" if plural else ""}: {names}. Check the id matches the '
            f"name passed to the resource (or block) constructor."
        )

    def _resolve(self, id: str, attr: str) -> "pulumi.Output[Any]":
        """Build a deferred Output for ``id``'s ``attr``.

        The object is looked up at resolution time (deploy), by which point
        ``run()`` has completed and the registry is fully populated. ``apply``
        flattens the picked Output and propagates its dependencies, so provenance
        is preserved.
        """
        # Build-mode discovery (ANVIL_BUILD_MODE=true) runs the program only to
        # collect the Lambda manifest; components are empty stubs and real output
        # values are irrelevant. Crucially, that path serialises component inputs
        # synchronously during construction, so a deferred forward reference to a
        # not-yet-declared resource would deadlock it. Return a resolved
        # placeholder instead — discovery completes and ignores the empty values.
        if os.environ.get("ANVIL_BUILD_MODE") == "true":
            return pulumi.Output.from_input("")

        slot = self._slot(id)

        async def _await_obj() -> Any:
            # Waits until the resource is registered (or returns immediately if it
            # already is). validate() guards against ids that are never declared.
            return await slot

        def _pick(obj: Any) -> Any:
            value = obj.get(attr) if isinstance(obj, dict) else getattr(obj, attr, None)
            if value is None:
                raise Exception(
                    f'Anvil: "{id}" has no output "{attr}". Check the resource type '
                    f'and that "{attr}" is a published output (for a block, that it '
                    f"was passed to self.output(...))."
                )
            return value

        # from_input(coroutine) schedules the lookup; apply(_pick) flattens the
        # resulting real Output with full dependency propagation.
        return pulumi.Output.from_input(_await_obj()).apply(_pick)


class _RefHandle:
    """Handle returned by ``ctx.ref(id)``. Each attribute is a deferred Output."""

    def __init__(self, stack: "Stack", id: str) -> None:
        # Stored via object.__setattr__ so normal lookup finds them and they don't
        # route through __getattr__ (which only fires for missing attributes).
        object.__setattr__(self, "_stack", stack)
        object.__setattr__(self, "_id", id)

    def __getattr__(self, attr: str) -> "pulumi.Output[Any]":
        # Leave dunder/internal access alone — only real outputs become Outputs.
        if attr.startswith("_"):
            raise AttributeError(attr)
        stack: Stack = object.__getattribute__(self, "_stack")
        id: str = object.__getattribute__(self, "_id")
        return stack._resolve(id, attr)


# ── Active stack (one per program execution) ───────────────

_active_stack: Optional["Stack"] = None


def set_active_stack(stack: Optional["Stack"]) -> None:
    """Set by the App around the user's ``run()`` callback. Internal."""
    global _active_stack
    _active_stack = stack


def get_active_stack() -> Optional["Stack"]:
    """Read by the namespace wrapper and Block base class. Internal."""
    return _active_stack


# ── Namespace auto-registration ────────────────────────────


def _is_component_class(value: Any) -> bool:
    return isinstance(value, type) and issubclass(value, pulumi.ComponentResource)


def _wrap_component_class(cls: type) -> type:
    """Return a subclass of ``cls`` that auto-registers itself on construction."""

    class _Wrapped(cls):  # type: ignore[valid-type, misc]
        def __init__(self, name, *args, **kwargs):
            super().__init__(name, *args, **kwargs)
            # Stash the logical name so grant methods can build synchronous
            # child-resource names via grants.grant_resource_name(self), without
            # the generated class declaring it. Read by *_grants.py companions.
            if isinstance(name, str):
                self._anvil_name = name
                # The constructor args, so grants can read plain inputs
                # synchronously (e.g. a VpcEndpoint's service). Read via
                # grants.ctor_input.
                self._anvil_ctor = (args, kwargs)
            stack = get_active_stack()
            if stack is None or not isinstance(name, str):
                return
            # Skip Pulumi's internal resource reconstruction (which passes an
            # opts with a urn). User-authored constructions have no urn.
            opts = kwargs.get("opts")
            if opts is not None and getattr(opts, "urn", None):
                return
            stack.register(name, self)

    _Wrapped.__name__ = cls.__name__
    _Wrapped.__qualname__ = getattr(cls, "__qualname__", cls.__name__)
    _Wrapped.__module__ = cls.__module__
    _Wrapped.__doc__ = cls.__doc__
    return _Wrapped


class _WrappedNamespace:
    """Proxies a component module (e.g. ``anvil_cloud.aws``) so that constructing
    any component auto-registers it into the active Stack under its logical name.

    The ``new anvil.aws.DSQL('id', ...)`` syntax is unchanged. Component classes
    are wrapped lazily on first access and cached; non-component attributes
    (enums, args classes, submodules) pass through untouched.
    """

    def __init__(self, module: Any) -> None:
        object.__setattr__(self, "_module", module)
        object.__setattr__(self, "_cache", {})

    def __getattr__(self, name: str) -> Any:
        module = object.__getattribute__(self, "_module")
        value = getattr(module, name)
        if not _is_component_class(value):
            return value
        cache = object.__getattribute__(self, "_cache")
        wrapped = cache.get(name)
        if wrapped is None:
            wrapped = _wrap_component_class(value)
            cache[name] = wrapped
        return wrapped


def wrap_namespace(module: Any) -> Any:
    """Wrap a component namespace module for forward-reference auto-registration."""
    return _WrappedNamespace(module)
