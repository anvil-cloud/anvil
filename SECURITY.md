# Security Policy

Anvil exists to make cloud infrastructure secure by default, so we treat security reports as a priority.

## Reporting a vulnerability

**Please don't open a public issue for security problems.**

Report privately through GitHub instead:
[**Report a vulnerability**](https://github.com/anvil-cloud/anvil/security/advisories/new)

Include what you can of:

- The affected component, SDK, or CLI command, and the Anvil version (`anvil version`)
- Steps to reproduce, or a minimal Anvil program that shows the problem
- What an attacker could do with it

We'll acknowledge your report within 3 business days and keep you updated as we investigate. Once a fix is released, we'll publish a GitHub Security Advisory and credit you, unless you'd rather stay anonymous.

## What counts as a vulnerability

Anvil's promise is its defaults, so a component that deploys something less secure than its documentation says is a vulnerability, not just a bug. For example:

- A component that leaves a resource public, unencrypted, or without TLS when the docs say it's locked down
- A grant (`grantRead`, `grantConnect`, …) that gives more IAM permissions than it documents
- Secrets or credentials exposed in state, logs, build output, or the deployed site
- Origin protection, WAF, or authorizer settings that can be bypassed
- Issues in the `anvil` CLI, the provider, the TypeScript, Python, or Go SDKs, or the install script

Out of scope:

- Configuration you explicitly opted into through arguments, `transform`, or overrides
- Vulnerabilities in AWS, GCP, or Pulumi themselves. Please report those to the vendor.
- Missing hardening that isn't covered by a component's documented defaults. Open a feature request for these.

## Supported versions

Anvil is pre-1.0. Security fixes go into the latest release only, so please upgrade to the newest version before reporting.

| Version | Supported |
| --- | --- |
| Latest 0.1.x release | ✅ |
| Older releases | ❌ |
