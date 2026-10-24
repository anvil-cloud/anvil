import * as pulumi from '@pulumi/pulumi';
import * as aws from '@pulumi/aws';
import * as gcp from '@pulumi/gcp';
import * as fs from 'fs';
import { Stack, setActiveStack, RefHandle } from './stack';
import { ComplianceScanner } from './aws/complianceScanner';

// ── Types ──────────────────────────────────────────────────

/**
 * Context passed to the App's run callback.
 * This is the user's only interface to Anvil runtime information.
 */
export interface Context {
  /** The current deployment stage (e.g. "dev", "staging", "prod", or OS username). */
  readonly stage: string;

  /** The project name from anvil.yaml. */
  readonly project: string;

  /** Named providers, keyed by their config name (e.g. "aws", "aws.us", "gcp.eu"). */
  readonly providers: Record<string, pulumi.ProviderResource>;

  /** Export a stack output value without importing Pulumi. */
  export(name: string, value: pulumi.Input<any>): void;

  /**
   * Forward-reference a resource or block by its logical id, before or after it
   * is declared. Property access yields the referenced resource's Output, so
   * declaration order no longer matters.
   *
   * The generic gives editor autocomplete on the referenced type's outputs:
   *
   * ```ts
   * const l = new anvil.aws.Lambda('lam', {
   *   environment: { DSQL_ENDPOINT: ctx.ref<anvil.aws.DSQL>('db').endpoint },
   * });
   * const db = new anvil.aws.DSQL('db', { ... }); // declared after — fine
   * ```
   *
   * A reference to an id that is never declared is a static error thrown after
   * `run()` completes — it will not hang the deployment.
   */
  ref<T = any>(id: string): RefHandle<T>;
}

/** AWS provider configuration. */
export interface AwsProviderConfig {
  region?: string;
  profile?: string;
  assumeRole?: {
    roleArn: string;
    sessionName?: string;
    externalId?: string;
  };
}

/** GCP provider configuration. */
export interface GcpProviderConfig {
  project?: string;
  region?: string;
  zone?: string;
  credentials?: string;
}

/** Default options applied to all resources. */
export interface DefaultsConfig {
  /**
   * Tags merged into every taggable resource via the cloud provider's
   * native defaultTags (AWS) or defaultLabels (GCP).
   *
   * `stage` and `project` are auto-injected. User tags override auto-injected ones.
   * Per-resource tags override default tags.
   */
  tags?: Record<string, string>;

  /** The stage's Pulumi state bucket. */
  state?: StateConfig;
}

/** Settings for the stage's Pulumi state bucket. */
export interface StateConfig {
  /**
   * How many deploys' state the bucket keeps, counting the latest: 2 keeps
   * the latest deploy's state and the one before, 3 also keeps the deploy
   * before that. Older deploys' state is deleted after each deploy. Default 3,
   * min 2, max 101.
   */
  retainVersions?: number;
  /** Write state gzipped. Default true. Switching either way reads existing state. */
  compressState?: boolean;
}

/**
 * A compliance framework. Either a friendly name, which always points at the
 * newest version Anvil pins (e.g. 'cis' → CIS 7.0), or a raw Prowler AWS
 * compliance ID, which stays fixed (e.g. 'cis_6.0_aws', 'ens_rd2022_aws').
 * Unknown values fail at deploy with a "did you mean" suggestion.
 */
export type ComplianceFramework =
  | 'soc2'
  | 'iso27001'
  | 'cis'
  | 'nist-800-53'
  | 'nist-800-171'
  | 'nist-csf'
  | 'pci'
  | 'hipaa'
  | 'fsbp'
  | 'gdpr'
  | 'nis2'
  | 'dora'
  | 'fedramp-low'
  | 'fedramp-moderate'
  | 'cmmc'
  | 'essential-eight'
  | 'well-architected'
  | 'c5'
  | 'csa-ccm'
  | (string & {});

/**
 * When to scan. Presets run at a fixed time derived from the project and
 * stage, so apps in one account don't all scan at once.
 */
export type ComplianceSchedule =
  | 'daily'
  | 'weekly'
  | 'none'
  | {
      /** Six AWS cron fields, e.g. "0 3 * * ? *". At most once an hour. */
      cron: string;
      /** IANA time zone, e.g. "Australia/Sydney". Default: UTC. */
      timezone?: string;
    };

/** How long scan results are kept. */
export type ComplianceRetention = '30d' | '90d' | '180d' | '1y' | '2y' | '7y';

/** Compliance scanning with Prowler. See `anvil compliance --help`. */
export interface ComplianceConfig {
  /** Frameworks to scan against. Required — there is no default. */
  frameworks: ComplianceFramework[];
  /** Default: "daily". */
  schedule?: ComplianceSchedule;
  /** Default: "1y". */
  retention?: ComplianceRetention;
  /** Start a scan after each successful deploy. Default: false. */
  scanOnDeploy?: boolean;
}

/**
 * Configuration for the App class.
 */
export interface AppConfig {
  /** The infrastructure definition callback. Required. */
  run: (ctx: Context) => void;

  /** Default resource options applied to all resources. */
  defaults?: DefaultsConfig;

  /**
   * Named provider configurations.
   * Keys follow the pattern "cloud" or "cloud.name":
   *   "aws"    → default AWS provider
   *   "aws.us" → named AWS provider for US region
   *   "gcp"    → default GCP provider
   *   "gcp.eu" → named GCP provider for EU
   */
  providers?: Record<string, AwsProviderConfig | GcpProviderConfig>;

  /**
   * Scheduled compliance scans of this app's deployed resources. The first
   * deploy with this set creates a scanner shared by every Anvil app in the
   * account and region (you'll be asked to confirm).
   *
   * @example
   * compliance: { frameworks: ['soc2', 'iso27001', 'cis'], schedule: 'daily' }
   */
  compliance?: ComplianceConfig;

  /**
   * Called before the infrastructure program runs.
   * @future Not yet implemented — reserved for forward compatibility.
   */
  beforeDeploy?: (ctx: Context) => void;

  /**
   * Called after the infrastructure program completes successfully.
   * @future CLI-side hook — not yet implemented.
   */
  afterDeploy?: (ctx: Context) => void;

  /**
   * Called if the infrastructure program throws an error.
   * @future CLI-side hook — not yet implemented.
   */
  onError?: (ctx: Context, error: Error) => void;
}

// ── Helpers ────────────────────────────────────────────────

/**
 * Determine which cloud a provider key belongs to.
 * "aws" → "aws", "aws.us" → "aws", "gcp.eu" → "gcp"
 */
function getCloud(key: string): string {
  const dot = key.indexOf('.');
  return dot === -1 ? key : key.substring(0, dot);
}

/**
 * Validates defaults.state and, during the CLI's discovery run, writes it where
 * the CLI reads it before Pulumi runs (ANVIL_APP_SETTINGS_PATH). Must match
 * cmd/anvil/cmd/state_settings.go.
 */
function writeStateSettings(state: StateConfig | undefined): void {
  const retainVersions = state?.retainVersions ?? 3;
  const compressState = state?.compressState ?? true;
  if (!Number.isInteger(retainVersions) || retainVersions < 2 || retainVersions > 101) {
    throw new Error(
      `defaults.state.retainVersions must be a whole number from 2 to 101 (got ${retainVersions}). ` +
        '1 would keep no previous state to recover from.'
    );
  }
  if (typeof compressState !== 'boolean') {
    throw new Error('defaults.state.compressState must be true or false.');
  }
  const path = process.env.ANVIL_APP_SETTINGS_PATH;
  if (process.env.ANVIL_BUILD_MODE === 'true' && path) {
    fs.writeFileSync(path, JSON.stringify({ state: { retainVersions, compressState } }, null, 2));
  }
}

// ── App ────────────────────────────────────────────────────

/**
 * App is the entry point for an Anvil infrastructure program.
 *
 * It wraps Pulumi's runtime so users never import `@pulumi/pulumi` directly.
 * The constructor reads stage/project from Pulumi config, creates providers
 * with default tags, and invokes the user's `run` callback with a Context.
 *
 * @example
 * ```typescript
 * import { App } from "@anvil-cloud/sdk";
 * import * as anvil from "@anvil-cloud/sdk";
 *
 * export default new App({
 *   defaults: {
 *     tags: { team: "platform" },
 *   },
 *   providers: {
 *     "aws": { region: "ap-southeast-2" },
 *   },
 *   run(ctx) {
 *     // Inherits soc2 + iso27001 automatically
 *     const bucket = new anvil.aws.Bucket("my-data", {
 *       dataClassification: "sensitive",
 *     });
 *     ctx.export("bucketName", bucket.bucketName);
 *   },
 * });
 * ```
 */
export class App {
  /** Stack outputs collected via ctx.export(). */
  [key: string]: any;

  constructor(config: AppConfig) {
    writeStateSettings(config.defaults?.state);

    // ── Read config from Pulumi ────────────────────────
    const anvilConfig = new pulumi.Config('anvil');
    const stage = anvilConfig.require('stage');
    const project = pulumi.getProject();

    // ── Build default tags ─────────────────────────────
    const autoTags: Record<string, string> = { stage, project };
    const userTags = config.defaults?.tags ?? {};
    const mergedTags: Record<string, string> = { ...autoTags, ...userTags };

    // ── Create providers ───────────────────────────────
    const providers: Record<string, pulumi.ProviderResource> = {};
    const defaultProviders: Record<string, pulumi.ProviderResource> = {};

    if (config.providers) {
      for (const [key, providerConfig] of Object.entries(config.providers)) {
        const cloud = getCloud(key);
        const isDefault = !key.includes('.');
        const providerName = `anvil-provider-${key}`;

        let provider: pulumi.ProviderResource;

        switch (cloud) {
          case 'aws': {
            const awsConfig = providerConfig as AwsProviderConfig;
            const awsArgs: Record<string, any> = {
              defaultTags: { tags: mergedTags },
            };

            if (awsConfig.region) awsArgs.region = awsConfig.region;
            if (awsConfig.profile) awsArgs.profile = awsConfig.profile;
            if (awsConfig.assumeRole) {
              awsArgs.assumeRoles = [
                {
                  roleArn: awsConfig.assumeRole.roleArn,
                  sessionName: awsConfig.assumeRole.sessionName,
                  externalId: awsConfig.assumeRole.externalId,
                },
              ];
            }

            provider = new aws.Provider(providerName, awsArgs);
            break;
          }

          case 'gcp': {
            const gcpConfig = providerConfig as GcpProviderConfig;
            const gcpArgs: Record<string, any> = {
              defaultLabels: mergedTags,
            };

            if (gcpConfig.project) gcpArgs.project = gcpConfig.project;
            if (gcpConfig.region) gcpArgs.region = gcpConfig.region;
            if (gcpConfig.zone) gcpArgs.zone = gcpConfig.zone;
            if (gcpConfig.credentials)
              gcpArgs.credentials = gcpConfig.credentials;

            provider = new gcp.Provider(providerName, gcpArgs);
            break;
          }

          default:
            throw new Error(
              `Unknown cloud provider "${cloud}" in providers config key "${key}".\n` +
                `  Supported prefixes: "aws", "gcp".`
            );
        }

        providers[key] = provider;
        if (isDefault) {
          defaultProviders[cloud] = provider;
        }
      }
    }

    // If no providers configured but defaults.tags is set,
    // create implicit default providers to carry the tags.
    if (!config.providers && config.defaults?.tags) {
      const awsProvider = new aws.Provider('anvil-provider-aws', {
        defaultTags: { tags: mergedTags },
      });
      providers['aws'] = awsProvider;
      defaultProviders['aws'] = awsProvider;

      const gcpProvider = new gcp.Provider('anvil-provider-gcp', {
        defaultLabels: mergedTags,
      });
      providers['gcp'] = gcpProvider;
      defaultProviders['gcp'] = gcpProvider;
    }

    // ── Register default provider injection ────────────
    if (Object.keys(defaultProviders).length > 0) {
      pulumi.runtime.registerStackTransformation(
        (
          args: pulumi.ResourceTransformationArgs
        ): pulumi.ResourceTransformationResult | undefined => {
          const typeParts = args.type.split(':');
          let cloud: string | undefined;

          if (typeParts[0] === 'anvil' && typeParts.length >= 3) {
            cloud = typeParts[1];
          } else if (typeParts.length >= 2) {
            cloud = typeParts[0];
          }

          if (cloud && defaultProviders[cloud] && !args.opts.provider) {
            return {
              props: args.props,
              opts: pulumi.mergeOptions(args.opts, {
                provider: defaultProviders[cloud],
              }),
            };
          }

          return undefined;
        }
      );
    }

    // ── Collect exports reference ──────────────────────
    // We store exports on the App instance itself.
    // Since the entry point does `export default new App(...)`,
    // Pulumi picks up all enumerable properties as stack outputs.
    const self = this;

    // ── Create the forward-reference registry ──────────
    // Resources construct eagerly; the Stack records them by id so `ctx.ref`
    // can resolve references declared in any order. See ./stack.ts.
    const stack = new Stack();

    // ── Create Context ─────────────────────────────────
    const ctx: Context = {
      stage,
      project,
      providers,
      export(name: string, value: pulumi.Input<any>) {
        self[name] = value;
      },
      ref<T = any>(id: string): RefHandle<T> {
        return stack.ref<T>(id);
      },
    };

    // ── Execute ────────────────────────────────────────
    // The active stack is set for the duration of `run()` so that component
    // construction (and Block outputs) auto-register into it. After `run()`
    // returns, validate() turns any never-declared forward reference into a
    // clean error instead of a hung promise.
    setActiveStack(stack);
    try {
      config.run(ctx);
      stack.validate();
    } finally {
      setActiveStack(undefined);
    }

    // ── Compliance ─────────────────────────────────────
    // Created outside the active stack so it isn't a ctx.ref target. The
    // default-provider transformation gives it the app's AWS provider.
    if (config.compliance) {
      createComplianceScanner(config.compliance, config.providers);
    }
  }
}

/** Every region the app's AWS providers deploy to. */
function awsRegions(
  providers: Record<string, AwsProviderConfig | GcpProviderConfig> | undefined
): string[] {
  const regions = new Set<string>();
  for (const [key, cfg] of Object.entries(providers ?? {})) {
    const region = (cfg as AwsProviderConfig).region;
    if (getCloud(key) === 'aws' && region) regions.add(region);
  }
  return [...regions];
}

function createComplianceScanner(
  compliance: ComplianceConfig,
  providers: Record<string, AwsProviderConfig | GcpProviderConfig> | undefined
): void {
  const schedule = compliance.schedule ?? 'daily';
  new ComplianceScanner('anvil-compliance', {
    frameworks: compliance.frameworks,
    schedule: typeof schedule === 'string' ? schedule : 'cron',
    cron: typeof schedule === 'string' ? undefined : schedule.cron,
    timezone: typeof schedule === 'string' ? undefined : schedule.timezone,
    retention: compliance.retention,
    scanOnDeploy: compliance.scanOnDeploy,
    regions: awsRegions(providers),
  });
}
