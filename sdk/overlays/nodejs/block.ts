import * as pulumi from '@pulumi/pulumi';
import { getActiveStack } from './stack';

/**
 * Block is an optional organisational grouping for Anvil resources.
 *
 * Resources created inside a Block constructor are automatically parented
 * to the Block via Pulumi's built-in auto-parenting. Blocks can expose
 * public properties (outputs) for cross-Block references.
 *
 * `stage` and `project` are automatically available via `this.stage` and
 * `this.project` — read from the same Pulumi config that App sets.
 *
 * Tagging is automatic — the App's provider injection applies `defaultTags`
 * to all resources, including those inside Blocks.
 *
 * Blocks are purely optional — flat top-level resources work identically.
 *
 * @example
 * ```ts
 * import * as anvil from "@anvil-cloud/sdk";
 *
 * class RulesEngine extends anvil.Block {
 *   public readonly bucketName: pulumi.Output<string>;
 *
 *   constructor(name: string, opts?: pulumi.ComponentResourceOptions) {
 *     super(name, {}, opts);
 *
 *     const bucket = new anvil.aws.Bucket("events", {
 *       dataClassification: "internal",
 *       hatch: {
 *         bucket: { bucket: `rules-${this.stage}-events` },
 *       },
 *     });
 *   }
 * }
 * ```
 */

export interface BlockArgs {
  [key: string]: any;
}

export class Block extends pulumi.ComponentResource {
  /** The current deployment stage (e.g. "dev", "staging", "prod", or OS username). */
  public readonly stage: string;

  /** The project name from anvil.yaml. */
  public readonly project: string;

  /**
   * The block's published outputs — its public surface for `ctx.ref(...)`.
   * Populated by `this.output(...)`. The object is registered with the active
   * Stack at construction time and mutated as outputs are published, so forward
   * references to a block's outputs resolve once `run()` completes.
   * @internal
   */
  private readonly __outputs: Record<string, pulumi.Output<any>> = {};

  /**
   * Publishes an output on this block, making it reachable via
   * `ctx.ref<ThisBlock>('blockName').<name>`. A block may only publish outputs
   * that originate from the resources it owns — it is a container for its own
   * resources, not a relay for others'.
   *
   * @param name  The public output name.
   * @param value The value to expose (typically a child resource's output).
   *
   * @example
   *   this.output('bucketName', events.bucketName);
   */
  protected output(name: string, value: pulumi.Input<any>): void {
    this.__outputs[name] = pulumi.output(value);
  }

  /**
   * Creates a new Block.
   *
   * @param name  The unique name of this Block within the stack.
   * @param args  Optional arguments (passed through for subclass use).
   * @param opts  Standard Pulumi ComponentResourceOptions (aliases, providers, etc.).
   */
  constructor(
    name: string,
    args?: BlockArgs,
    opts?: pulumi.ComponentResourceOptions
  ) {
    const typeName = new.target?.name ?? name;
    super(`anvil:block:${typeName}`, name, args ?? {}, opts);

    const anvilConfig = new pulumi.Config('anvil');
    this.stage = anvilConfig.require('stage');
    this.project = pulumi.getProject();

    // Register the block's output surface with the forward-reference registry.
    // The object is registered now (empty) and populated by this.output(...) as
    // the subclass constructor body runs; refs resolve after run() completes.
    getActiveStack()?.register(name, this.__outputs);
  }
}
