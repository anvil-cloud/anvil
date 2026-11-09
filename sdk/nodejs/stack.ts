// sdk/nodejs/stack.ts
// Hand-written. Backed up/restored during gen-sdk like app.ts, block.ts, grants.ts.
//
// Implements forward references for Anvil programs (Approach A — promise-backed
// refs). Resources construct eagerly, as they always have. A Stack registry maps
// each resource's logical id to the constructed object, and `ctx.ref('id')` hands
// back promise-backed Outputs that resolve once that id is registered — which may
// happen *later* in the same synchronous `run()`, enabling declaration in any order.
//
// Because each ref resolves to the resource's *real* Output, Pulumi's dependency
// tracking, ordering, and cycle detection all work unchanged. After `run()`
// returns, the App calls `validate()` to turn any never-declared reference into a
// clean static error instead of a hanging promise.
//
// See docs/design/forward-references.md for the full design.

import * as pulumi from '@pulumi/pulumi';

// ── Deferred ───────────────────────────────────────────────

interface Deferred<T> {
  readonly promise: Promise<T>;
  resolve(value: T): void;
}

function defer<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

// ── RefHandle type ─────────────────────────────────────────

/**
 * The compile-time shape of `ctx.ref<T>('id')`. Each of T's outputs is surfaced
 * as a (promise-backed) Output of the same value type, so
 * `ctx.ref<DSQL>('db').endpoint` is typed `Output<string>`. The generic is purely
 * for editor autocomplete — at runtime every property access returns an Output.
 */
export type RefHandle<T> = {
  readonly [K in keyof T]: T[K] extends pulumi.Output<infer U>
    ? pulumi.Output<U>
    : pulumi.Output<any>;
};

// ── Stack ──────────────────────────────────────────────────

/**
 * Stack is the per-program registry that backs forward references. One Stack
 * exists per App run. It is not part of the public API — users interact with it
 * only through `ctx.ref(...)`.
 */
export class Stack {
  /** Promise slot per logical id. Created on first ref or first register. */
  private readonly slots = new Map<string, Deferred<Record<string, any>>>();

  /** Ids that have actually been constructed (used for collision + validation). */
  private readonly registered = new Set<string>();

  /** Ids that some `ctx.ref(...)` asked for (used to report dangling refs). */
  private readonly requested = new Set<string>();

  private slot(id: string): Deferred<Record<string, any>> {
    let d = this.slots.get(id);
    if (!d) {
      d = defer<Record<string, any>>();
      this.slots.set(id, d);
    }
    return d;
  }

  /**
   * Records a constructed resource (or a block's published outputs) under its
   * logical id, resolving any forward references already waiting on it.
   *
   * @param id      The logical name passed to the resource/block constructor.
   * @param outputs The object whose properties are the resource's Outputs.
   * @internal
   */
  register(id: string, outputs: Record<string, any>): void {
    if (this.registered.has(id)) {
      throw new Error(
        `Anvil: duplicate resource id "${id}". Every resource and block in an ` +
          `App must have a unique name — forward references address resources by ` +
          `that name.`
      );
    }
    this.registered.add(id);
    this.slot(id).resolve(outputs);
  }

  /**
   * Returns a forward-reference handle for `id`. Property access yields a
   * promise-backed Output that resolves when `id` is registered.
   * @internal
   */
  ref<T = any>(id: string): RefHandle<T> {
    this.requested.add(id);
    const slot = this.slot(id);

    const handle = new Proxy(
      {},
      {
        get: (_t, prop) => {
          // Don't hijack thenable/symbol/internal access — only real outputs.
          if (typeof prop !== 'string') return undefined;
          if (prop === 'then' || prop === 'catch' || prop === 'finally') {
            return undefined;
          }
          if (prop.startsWith('__')) return undefined;

          // Build-mode discovery (ANVIL_BUILD_MODE=true) runs the program only to
          // collect the Lambda manifest; components are empty stubs and real
          // output values are irrelevant. That path serialises component inputs
          // synchronously during construction, so a deferred forward reference to
          // a not-yet-declared resource would deadlock it. Return a resolved
          // placeholder instead — discovery completes and ignores the value.
          if (process.env.ANVIL_BUILD_MODE === 'true') {
            return pulumi.output('');
          }

          return pulumi.output(
            slot.promise.then((outputs) => {
              const value = outputs[prop];
              if (value === undefined) {
                throw new Error(
                  `Anvil: "${id}" has no output "${prop}". Check the resource ` +
                    `type and that "${prop}" is a published output` +
                    ` (for a block, that it was passed to this.output(...)).`
                );
              }
              return value;
            })
          );
        },
      }
    );

    return handle as RefHandle<T>;
  }

  /**
   * Called by the App after `run()` returns. Throws a clean error for any
   * forward reference whose id was never declared — preventing a hung promise
   * from stalling the deployment.
   * @internal
   */
  validate(): void {
    const missing: string[] = [];
    for (const id of this.requested) {
      if (!this.registered.has(id)) missing.push(id);
    }
    if (missing.length === 0) return;

    const plural = missing.length > 1;
    throw new Error(
      `Anvil: forward reference${plural ? 's' : ''} to undeclared resource` +
        `${plural ? 's' : ''}: ${missing.map((m) => `"${m}"`).join(', ')}. ` +
        `Check the id matches the name passed to the resource (or block) ` +
        `constructor.`
    );
  }
}

// ── Active stack (one per program execution) ───────────────

let activeStack: Stack | undefined;

/** @internal Set by App around the user's `run()` callback. */
export function setActiveStack(stack: Stack | undefined): void {
  activeStack = stack;
}

/** @internal Read by the namespace wrapper and Block base class. */
export function getActiveStack(): Stack | undefined {
  return activeStack;
}

// ── Auto-registration (via utilities.lazyLoad) ─────────────

const wrapCache = new WeakMap<object, any>();

/**
 * Given a value resolved by `utilities.lazyLoad`, return a construction-wrapped
 * version if it's a component class, otherwise the value unchanged.
 *
 * This is the hook that powers `ctx.ref('id')`: constructing any component
 * auto-registers it into the active Stack under its logical name. Wrapping
 * happens here (inside lazyLoad) rather than around the namespace export so that
 * `anvil.aws.X` stays a real namespace — usable as a **type** as well as a value.
 *
 * Wrapped classes are cached by identity, so `===` and `instanceof` stay stable
 * and any future component works with zero extra wiring. Non-component values
 * (enums, helpers, providers, type-only exports) pass through untouched.
 */
export function maybeWrapComponent(value: any): any {
  if (!isComponentClass(value)) return value;
  let wrapped = wrapCache.get(value);
  if (!wrapped) {
    wrapped = wrapComponentClass(value);
    wrapCache.set(value, wrapped);
  }
  return wrapped;
}

function isComponentClass(value: any): boolean {
  return (
    typeof value === 'function' &&
    (value.__pulumiType !== undefined ||
      value.prototype instanceof pulumi.ComponentResource)
  );
}

function wrapComponentClass(Cls: any): any {
  return new Proxy(Cls, {
    construct(target, args, newTarget) {
      const instance = Reflect.construct(target, args, newTarget);
      const name = args[0];
      const opts = args[2];

      // Stash the logical name on the instance so grant methods can build
      // synchronous child-resource names (e.g. the IAM RolePolicy) without the
      // SDK class itself having to declare/assign it. Read via grantResourceName
      // in grants.ts. Set for every construction, including reconstruction.
      if (typeof name === 'string') {
        (instance as any).__anvilName = name;
        // The constructor args, so grants can read plain inputs synchronously
        // (e.g. a VpcEndpoint's service, or whether a DSQL cluster has a VPC
        // endpoint). Read via grantResourceArgs in grants.ts.
        (instance as any).__anvilArgs = args[1];
      }

      // Skip Pulumi's internal resource reconstruction (deserialization from a
      // URN), which passes { urn } as opts — only auto-register genuine
      // user-authored top-level constructions.
      const isReconstruction =
        opts && typeof opts === 'object' && 'urn' in opts;

      if (typeof name === 'string' && !isReconstruction) {
        getActiveStack()?.register(name, instance as Record<string, any>);
      }
      return instance;
    },
  });
}
