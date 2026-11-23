# Site Protection — who can call a site's server Lambda

**Status:** Implemented for `SvelteKitSite` (`protection`, `waf: { arn }` and
`originProtection` inputs). `edge-oac` **verified end-to-end on AWS** (see
*Verification*); origin protection verified locally only (against a mocked
KeyValueStore). The `Waf` component that `waf.arn` attaches is documented in
[waf.md](waf.md).

## The problem

A `SvelteKitSite` serves SSR from a Lambda behind CloudFront:

```
browser → CloudFront (+ WAF) → Lambda Function URL → SvelteKit
```

The Lambda is reached through a **Function URL** — a real HTTPS endpoint
(`https://<32 random chars>.lambda-url.<region>.on.aws`). With
`AuthType: NONE`, anyone who learns the URL can call the server **directly**,
skipping CloudFront and everything attached to it:

- **WAF** — managed rules, rate limiting, IP reputation
- **Header trust** — `X-Forwarded-For` and `CloudFront-Viewer-*` become spoofable
- **Cost / capacity** — every direct hit is a billed, uncached invocation
  (denial-of-wallet, and account concurrency exhaustion takes the real site down)
- **Geo restrictions**

The URL is unguessable but **not secret** — it leaks through error pages,
`Host`-derived redirects, logs, SSRF, or anyone with state/account read access,
and stays valid until the function is recreated. Obscurity doesn't recover from
a leak.

## The decision

Anvil follows **AWS's documented approach** for locking a Function URL behind
CloudFront:

- [Restrict access to an AWS Lambda function URL origin (CloudFront Developer Guide)](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-restricting-access-to-lambda.html)
  — lock the Function URL with `AWS_IAM` and CloudFront Origin Access Control.
  For `PUT`/`POST`, the request must carry `x-amz-content-sha256` because
  "Lambda doesn't support unsigned payloads".
- [Protecting an AWS Lambda function URL with Amazon CloudFront and Lambda@Edge (AWS Compute Blog)](https://aws.amazon.com/blogs/compute/protecting-an-aws-lambda-function-url-with-amazon-cloudfront-and-lambdaedge/)
  — use a Lambda@Edge origin-request function to handle request bodies at the
  edge.

```ts
new anvil.aws.SvelteKitSite('web', { path: './web' });                       // "none" — public

new anvil.aws.SvelteKitSite('web', { path: './web', waf: { arn: waf.arn } }); // "edge-oac" — locked automatically
```

Three modes:

| Mode | Function URL | Requests with a body |
|---|---|---|
| `none` | public | work |
| `oac` | IAM-locked to this distribution | rejected unless they already carry `x-amz-content-sha256` — for read-only sites |
| `edge-oac` | IAM-locked to this distribution | **work** — a Lambda@Edge function adds the header; nothing is needed in the app |

Rules:

- **An explicit `protection` always wins.**
- **Unset:** `none`, or **`edge-oac` while a WAF is attached or origin
  protection is enabled** — with an info message saying so.
- **Explicit `none` with a WAF or origin protection** is respected, with a
  warning that the WAF/proxy can be bypassed.

| Configuration | Result | Message |
|---|---|---|
| nothing set | `none` | — |
| `protection: 'oac'` or `'edge-oac'` | that mode | — |
| `waf: { arn }` | `edge-oac` | **Info**: default applied |
| `waf: { arn }` + `protection: 'oac'` | `oac` | — |
| `waf: { arn }` + `protection: 'none'` | `none` | **Warning**: WAF can be bypassed |
| `originProtection: true` | `edge-oac` | **Info**: default applied; **Info**: configure the proxy header |
| `originProtection: true` + `protection: 'none'` | `none` | **Warning**: proxy can be bypassed |

### Why public by default

- **It just works.** A fresh site accepts webhooks, OAuth `form_post` callbacks,
  forms, mobile/CLI clients — nothing to learn on day one.
- **Without a WAF there's little to bypass.** App auth (sessions, authorization
  checks) runs either way; going direct gives an attacker nothing they can't do
  through CloudFront. The residual risks are header spoofing and cost.
- **Industry parity.** SST and cdk-nextjs both default to a public Function URL.

### Why a WAF or origin protection defaults to `edge-oac`

- A WAF or proxy in front of a public Function URL can be bypassed by calling
  the URL directly, so the sensible default once one is attached is a locked
  server.
- `edge-oac` is the locked mode that needs **nothing from the app**: the header
  is added at the edge, so forms, `fetch`, webhooks and callbacks keep working.
  Anvil never modifies the app's code or pages.
- The user stays in control: an explicit `protection` always wins, including
  `none`.
- This matches cdk-nextjs, which automatically adds a Lambda@Edge signer when
  its Function URL is set to `AWS_IAM`.

### Why the header is added at the edge

Options considered for adding `x-amz-content-sha256`:

| Option | Who adds the header | Verdict |
|---|---|---|
| **Lambda@Edge (`edge-oac`)** | Edge function hashes the body | **Chosen** — AWS's documented approach; app untouched |
| The app's own code | Every call site / a fetch wrapper | Rejected — the app shouldn't have to add anything |
| Anvil-injected browser script | Anvil rewrites pages to load a signer | Rejected — modifies the user's app |
| Secret header instead of IAM | CloudFront custom origin header, checked in-process | Rejected — not IAM: the URL stays public and every direct hit still invokes (and bills) the Lambda |

### Security of `oac` / `edge-oac`

`x-amz-content-sha256` is **not a credential** — it's a body hash anyone can
compute. What locks the origin is the SigV4 signature only CloudFront can
produce:

- Function URL `AuthType: AWS_IAM`, no `principal: "*"` permission
- Lambda permissions granted to `cloudfront.amazonaws.com` **scoped by
  `SourceArn` to this site's distribution** (no other distribution — including
  another account's — can use them)
- Lambda OAC with `signingBehavior: always`

So a direct call to the Function URL is rejected by the Lambda service with 403
**before the function runs** (no invocation, no bill), with or without the
header. Requests through CloudFront pass the WAF like any user. The edge signer
runs after the WAF, so it never lets anything past it. Bonus: Lambda verifies
the body matches the signed hash, so it can't be altered between CloudFront and
Lambda.

**Wording for docs/marketing:** "private origin, locked to CloudFront by IAM" or
"identity-based origin access" — **not** "zero trust". It applies one zero-trust
principle (authenticate every request, ignore network location) to one hop;
end users are still anonymous at the edge and Lambda trusts what CloudFront
forwards.

## Origin protection (`originProtection: true`)

For sites behind a CDN/proxy — Cloudflare, Fastly, Akamai, nginx, anything that
can add a request header. It locks the second hop the same way `protection`
locks the third:

```
visitor → proxy (adds x-origin-secret) → CloudFront → Lambda
                                          │            └─ IAM/OAC (edge-oac by default)
                                          └─ CloudFront Function: no valid header → 403
```

```ts
const site = new anvil.aws.SvelteKitSite('web', {
  path: './web',
  domain: 'example.com',
  originProtection: true,   // protection defaults to "edge-oac"
});
export const originSecret = site.originSecret;   // Pulumi secret — paste into the proxy's header rule
```

What Anvil creates:

| Resource | Purpose |
|---|---|
| CloudFront KeyValueStore + key `origin-secret` | Holds the secret (64 hex chars, 256 bits). Generated on first deploy; the key ignores later value changes, so it is **stable across deploys** |
| CloudFront Function (`cloudfront-js-2.0`, viewer-request) | Reads the secret from the store, compares in constant time, **strips the header** on success, returns 403 otherwise. Shared with the apex/www redirect when that's on ([edge/viewer-request.js](../../provider/internal/awssite/edge/viewer-request.js)): the guard runs first, then the redirect, because CloudFront allows one viewer-request function per behavior |
| Cloudflare Transform Rule (only with `dns.provider: cloudflare`, proxied) | Anvil adds the header itself: one rule per site, managed through Cloudflare's per-rule API so hand-made rules are untouched. Uses one of the zone's Transform Rules (Free 10). See [Site domains and DNS](site-domains.md#origin-protection) |
| Function association on **every** cache behavior | Static asset paths can't bypass the check |

Design choices:

- **A boolean, not an object.** There's nothing to configure: the header name is
  fixed (`x-origin-secret`) and the secret is generated.
- **Generic, not per-provider.** The mechanism only needs a proxy that can set a
  request header; per-provider setup lives in the docs.
- **On the site, not in the `Waf` component.** Origin locking decides *who may
  reach CloudFront* — an access rule like `protection`, not traffic filtering.
  As a CloudFront Function it needs no AWS WAF (≈$0.10/M requests vs $5+/month),
  which matters because proxies like Cloudflare already include a WAF; and it
  composes with an Anvil `Waf` when both are wanted.
- **Secret in a KeyValueStore, not in the function code**, so reading the
  function doesn't reveal it. The store also makes zero-downtime rotation
  possible later (accept a second key during the switch).
- **The header is stripped** before the origin so the app never receives the
  secret and can't log it by accident.
- **Protection defaults to `edge-oac`.** A proxy in front means both hops should
  be locked, otherwise the Function URL is a way around the proxy.

Security: the CloudFront lock is a **shared secret** — strong against guessing,
but anyone who obtains the value (proxy dashboard access, stack state access,
KeyValueStore read access) can reach CloudFront without the proxy. They still
can't reach Lambda without CloudFront (IAM), and still face any AWS WAF and the
app's auth. Viewer mTLS (CloudFront, Nov 2025) is the stronger alternative and is
listed under future work.

## How it works

### Resources per mode

| Resource | `none` | `oac` | `edge-oac` |
|---|---|---|---|
| Function URL auth | `NONE` | `AWS_IAM` | `AWS_IAM` |
| `lambda:InvokeFunctionUrl` permission | `*`, `FunctionUrlAuthType=NONE` | CloudFront, `SourceArn`=distribution | same |
| `lambda:InvokeFunction` permission (`InvokedViaFunctionUrl`) | `*` | CloudFront, `SourceArn`=distribution | same |
| Lambda OAC on the CloudFront origin | — | ✓ | ✓ |
| Lambda@Edge signer (us-east-1) + IAM role, origin-request with `includeBody` on the server behavior | — | — | ✓ |

Lambda requires **both** permissions for every Function URL created since
October 2025 — including `NONE` ones. The code previously created neither (a
latent 403 bug for new public sites); `none` mode now creates them explicitly.
Permission resource names differ per mode (`-public` / `-cloudfront`) so
switching modes replaces them cleanly.

The existing `AllViewerExceptHostHeader` origin request policy is required:
SigV4 is computed against the Lambda URL's own `Host`.

### The edge signer

`edge/edge-signer.js`, deployed as Lambda@Edge (Node.js 22, x86_64, 128 MB, 5 s
timeout, published version) and associated as **origin-request** with
`includeBody: true` on the server (default) cache behavior only — S3 behaviors
serve static GETs. For `POST`/`PUT`/`PATCH` it hashes the raw body bytes and sets
`x-amz-content-sha256`; CloudFront's OAC then signs the request. A body over
Lambda@Edge's 1 MB limit arrives truncated, so the function returns **413** with
a clear message instead of a mismatched hash. Same pattern as SST's
`oac-with-edge-signing` and cdk-nextjs's `sign-fn-url`.

It also **percent-encodes query parameter names** on every request. Verified on
a deployed site: an IAM-locked Function URL rejects any parameter *name*
containing a raw special character (`/ : @ ! * ' ( ) , ; + ~ [ ]`, …) with
`400 InvalidQueryStringException` — values are unaffected. That breaks
SvelteKit form actions (`?/save`) and bracket-style params (`filter[name]=x`).
Names are re-encoded so only `A–Z a–z 0–9 - _ .` stay raw (`+` → `%20` to keep
its "space" meaning); standard URL parsing in any framework decodes them back to
the same names.

### Files

- `provider/internal/awssite/protection.go` — mode constants + `ResolveProtection` (tested)
- `provider/internal/awssite/site_lambda.go` — `CreateSiteFunctionURL`, `GrantSiteFunctionURLAccess`
- `provider/internal/awssite/cloudfront.go` — Lambda OAC, edge signer association, viewer-request function on every behavior
- `provider/internal/awssite/edge_signer.go` + `edge/edge-signer.js` — Lambda@Edge signer
- `provider/internal/awssite/origin_protection.go` + `edge/origin-guard.js` — KeyValueStore secret + CloudFront Function
- `provider/internal/awssite/hosting.go` — shared hosting layer used by every site component
- `provider/sites/types.go` — shared hosting input types + `MissingHostingInputs`
- `provider/aws/sveltekitsite/sveltekitsite.go` — wiring
- `scripts/generate-site-schemas/main.go` — `SiteProtection` enum

### Shared by every site component

Protection, WAF attachment, origin protection and security headers form one
**shared hosting layer** that every framework component uses
(`internal/awssite/hosting.go`):

```go
hosting, _ := awssite.NewHosting(ctx, site, name, awssite.HostingInputs{...}) // before the server Lambda
fnURL, _ := hosting.CreateFunctionURL(lambdaFn)                                // after the Lambda
hosting.ApplyTo(&cfArgs)                                                       // before the distribution
hosting.GrantAccess(lambdaFn, distribution.Arn)                                // after the distribution
```

- **Inputs** `protection`, `waf`, `originProtection`, `securityHeaders` —
  names in `sites.HostingInputNames`, types in `provider/sites/types.go`.
- **Declared on each component's Args, not embedded.** Pulumi's component input
  decoding (`ConstructInputs.CopyTo`) only fills top-level tagged fields, so
  inputs inside an embedded struct would silently never be set. Each site
  component has a test calling `sites.MissingHostingInputs` that fails if one is
  missing.
- **Static SPA (no server):** no Lambda, so `protection` doesn't apply; WAF,
  origin protection and security headers still attach to CloudFront.

Security headers are documented in [security-headers.md](security-headers.md).

## Verification

`edge-oac` deployed to AWS (`ap-southeast-2`) with a SvelteKit 3 / adapter-node 6
test app (`testWaf`, 2026-10-07):

| Check | Result |
|---|---|
| Direct call to the Function URL (GET and POST, unsigned) | ✅ `403 AccessDeniedException` — locked |
| Page through CloudFront | ✅ 200 |
| `fetch` POST JSON through CloudFront | ✅ 200, `x-amz-content-sha256` present at the app (edge signer ran, IAM accepted) |
| 1.5 MB POST | ✅ 413 from the edge signer with its message |
| Form action, `use:enhance` (`?/save`) | ✅ works |
| Form action, native no-JS form (`?/save`) | ✅ works — after the query-name fix below |
| `fetch` POST `multipart/form-data`, `fetch` PUT with no `Content-Type` to a `+server.ts` endpoint | ⚠️ 403 `Cross-site … form submissions are forbidden` — **SvelteKit's CSRF check, not site protection** (see *Known issues*) |

Found during verification and fixed: the IAM-locked Function URL rejected
SvelteKit's `?/save` action URLs with `400 InvalidQueryStringException`. Any raw
special character in a query parameter *name* is rejected (values are fine);
the edge signer now percent-encodes names (see *The edge signer*).

## Known issues

- **SvelteKit CSRF 403 on some `fetch` requests (all protection modes).**
  `fetch` requests to `+server.ts` endpoints with a form-like body
  (`multipart/form-data`, `application/x-www-form-urlencoded`) or no
  `Content-Type` get SvelteKit's `403 Cross-site … form submissions are
  forbidden`. JSON requests and form actions work. The request passes CloudFront,
  the edge signer and IAM and is rejected inside SvelteKit, whose CSRF check
  compares the request's `Origin` with its own origin. Likely cause: CloudFront
  forwards the Function URL's host instead of the viewer's (required for SigV4),
  and adapter-node 6 (SvelteKit 3) no longer reads the `ORIGIN` env var, so the
  app's view of its own origin is wrong. Not caused by site protection; the fix
  belongs in `SvelteKitSite` (pass the viewer host/origin through to
  adapter-node). Under investigation.

## Messages Anvil emits

Exact text lives in code; keep this table in sync.

| Level | When | Message (source) |
|---|---|---|
| **Error** | `protection` isn't a known value | `invalid protection "<x>": must be "none", "oac" or "edge-oac"` (`protection.go`) |
| **Info** | WAF and/or origin protection on, `protection` unset | `Because <reason>, protection is set to "edge-oac": the server Function URL is locked to CloudFront, and a Lambda@Edge function adds the x-amz-content-sha256 header to requests with a body. Request bodies over 1 MB are rejected. Set protection explicitly to choose another mode.` (`protection.go`) |
| **Warning** | WAF and/or origin protection on, `protection: "none"` | `protection is "none" while <reason>: the server Function URL is public, so anyone who learns it can go around CloudFront and bypass it.` (`protection.go`) |
| **Info** | `originProtection: true` | `Origin protection enabled: CloudFront only accepts requests carrying the x-origin-secret header. Configure your CDN/proxy to send it on every request, with the originSecret output as the value — until then the site returns 403.` (`sveltekitsite.go`) |
| **400 `InvalidQueryStringException`** (runtime, from Lambda) | `oac` only, query parameter name with a raw special character (e.g. `?/save`) — `edge-oac` encodes these |
| **413** (runtime) | `edge-oac`, request body over 1 MB | `{"error":"Request body exceeds the 1 MB Lambda@Edge limit. Upload large files directly to S3 with a presigned URL."}` (`edge/edge-signer.js`) |

`<reason>` is one of: `a WAF is attached`, `origin protection is enabled`,
`a WAF is attached and origin protection is enabled`.

## For the user docs

Link both AWS references from the user docs wherever `protection` is explained:
[CloudFront: Restrict access to a Lambda function URL origin](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-restricting-access-to-lambda.html)
and [AWS Compute Blog: Protecting a Lambda function URL with CloudFront and Lambda@Edge](https://aws.amazon.com/blogs/compute/protecting-an-aws-lambda-function-url-with-amazon-cloudfront-and-lambdaedge/).

### ⚠️ Warnings — `edge-oac` (the default with a WAF or origin protection)

1. **Attaching a WAF or enabling origin protection locks the server.** With
   `protection` unset, the deploy switches to `edge-oac` and Anvil prints an
   info message. Set `protection` explicitly to choose otherwise.
2. **Request bodies over 1 MB are rejected (413).** That's the Lambda@Edge body
   limit. Upload large files directly to S3 with a presigned URL.
3. **Small added cost and latency.** The edge function runs on every request to
   the server (not static assets): about $0.60 per million requests plus a few
   milliseconds.
4. **Destroying or switching away takes longer.** AWS removes Lambda@Edge
   replicas from every edge location after the distribution stops using them,
   which can take several minutes.
5. **The viewer's `Authorization` header is replaced** by CloudFront's SigV4
   signature. Apps that send `Authorization: Bearer …` to their *own* SvelteKit
   server lose the token — send it in another header (e.g. `x-auth-token`).
   Cookie sessions (Auth.js, Lucia, Better Auth, SvelteKit `cookies`) are
   unaffected.
6. **Switching modes on an existing site can cause a few minutes of 403s**
   while CloudFront propagates the change (the Function URL's auth type flips
   immediately; CloudFront edges catch up over minutes). Deploy outside peak
   traffic. New sites aren't affected.
7. **Apps that read the raw query string** (instead of parsed parameters)
   see encoded names under `edge-oac` — e.g. `%2Fsave` instead of `/save`.
   Parsed values (`URLSearchParams`, `url.searchParams`, framework query
   helpers) are unchanged.
8. **Don't rewrite request bodies in a proxy or Cloudflare Worker** in front of
   the site — the hash is computed at CloudFront, so it stays correct, but a
   body changed *after* hashing would fail.
9. **WAFs attached outside Anvil are overwritten.** A WebACL added to the
   distribution in the console or another stack is removed on the next deploy.
   Attach WAFs via `waf.arn`.

### ⚠️ Warnings — `oac`

1. **Requests with a body are rejected (403)** unless the client already sends
   `x-amz-content-sha256`. Forms, `fetch` POSTs and webhooks to the site fail.
   Use `oac` only for read-only sites; otherwise use `edge-oac`.
2. **Query parameter names with special characters are rejected (400
   `InvalidQueryStringException`)** — e.g. SvelteKit's `?/action` and
   `filter[name]=x`. `edge-oac` encodes them at the edge; `oac` has no edge
   function to do it.
3. Warnings 5, 6 and 9 from `edge-oac` apply too.

### ⚠️ Warnings — `none` (the default without a WAF or origin protection)

1. **The server Function URL can be called directly.** It bypasses CloudFront:
   no WAF, no rate limit, no caching, no geo restrictions.
2. **Don't trust forwarded headers for security decisions** — `X-Forwarded-For`,
   `CloudFront-Viewer-Address`, `CloudFront-Viewer-Country` can be forged on
   direct requests (IP rate limits, geo logic, audit logs).
3. **Direct traffic is billed** and can exhaust account concurrency.
4. **Security scanners flag public Function URLs** (Prowler, Security Hub-style
   checks); auditors may ask about it.
5. **With a WAF or origin protection, `none` makes them bypassable.** It's
   allowed when set explicitly, with a deploy warning.

### ⚠️ Warnings — origin protection (`originProtection: true`)

1. **Configure the proxy before pointing DNS at it.** Until the proxy sends
   `x-origin-secret`, every request gets a 403. Order: deploy → copy the
   `originSecret` output into the proxy's header rule → switch DNS to the proxy.
2. **Every request must go through the proxy.** Direct requests to the
   CloudFront domain (or the custom domain resolving to CloudFront) are
   rejected — including your own health checks and uptime monitors.
3. **Treat the secret like a password.** Anyone who can see it (proxy dashboard
   users, stack state readers, CloudFront KeyValueStore readers) can reach
   CloudFront without the proxy. Don't paste it into tickets or chat.
4. **Protection defaults to `edge-oac`** — its warnings apply.
5. **Existing sites upgrading from the old `originProtection: { provider }`**:
   the old WAF-based setup and its Secrets Manager secret are replaced and the
   secret value changes once — update the proxy's header rule after the deploy.
6. **No rotation yet.** If the secret leaks, the current way to rotate is to
   replace the KeyValueStore key (e.g. `pulumi up --replace` on it) and update
   the proxy, which causes 403s in between.

### 💡 Good to know

- **SvelteKit form actions work with `edge-oac`** — both `use:enhance` and
  native no-JS forms, verified on AWS. (Some `fetch` requests to `+server.ts`
  endpoints currently hit SvelteKit's own CSRF 403 in every mode — see *Known
  issues*.)
- **With `edge-oac`, nothing changes in your app** — forms, `use:enhance`,
  `fetch`, axios, webhooks and OAuth callbacks all work as before.
- **GET/HEAD requests never need the header** — pages, `load` data, API GETs.
- **Static assets** (`/_app/*`) come from S3 through their own OAC and don't go
  through the edge signer.
- **`HttpApi` is a separate endpoint** and isn't affected by site protection.
- **The hash header isn't a secret** and doesn't grant access; locking comes
  from IAM.
- **Direct calls to a locked Function URL cost nothing** — Lambda rejects them
  before invocation.
- **Works behind Cloudflare (or any proxy).** `originProtection: true` locks
  CloudFront to the proxy and `edge-oac` locks the Lambda to CloudFront, so the
  proxy is the only way in.
- **Proxy setup is one header rule, not a WAF change.** Cloudflare with
  `dns.provider: 'cloudflare'`: Anvil adds the rule (token needs Zone >
  Transform Rules > Edit). Cloudflare otherwise: Rules →
  Transform Rules → Modify Request Header → *Set static* (free plan). Fastly:
  Headers (Request, Set) or VCL `set req.http.x-origin-secret`. Akamai: Modify
  Outgoing Request Header. nginx: `proxy_set_header x-origin-secret …;`.
  Caddy: `header_up x-origin-secret …`. Use SSL mode **Full (strict)** on
  Cloudflare; without a custom domain the proxy must send the
  `*.cloudfront.net` hostname as `Host`.
- **Behind a proxy you may not need an AWS WAF.** Cloudflare includes a WAF on
  every plan (free: managed ruleset + 1 rate-limit rule; Pro: full managed +
  OWASP rulesets). `originProtection` doesn't require an Anvil `Waf`; add one
  only for defence in depth.
- **The origin secret is stable across deploys** and never reaches the app (the
  edge function strips the header).
- **HttpApi can't take a WAF** (an API Gateway v2 limit) — protect webhook
  routes there by verifying the sender's signature (Stripe signing secret,
  GitHub HMAC).

### Recommended patterns

- Hobby / marketing site → leave `protection` unset (`none`).
- Production app with a WAF → `waf: { arn: waf.arn }` (gets `edge-oac`).
- Behind Cloudflare or another proxy → `originProtection: true` (gets `edge-oac`).
- Production app that will have a WAF later → set `protection: "edge-oac"` from
  the start, so attaching the WAF changes nothing else.
- Large uploads → presigned S3 URLs, not request bodies through the site.

## Future work

- **Origin secret rotation without downtime** — accept a second KeyValueStore
  key during the switch (the edge function already loops over `SECRET_KEYS`).
- **Origin protection via viewer mTLS** (CloudFront, Nov 2025) as a stronger
  option than the shared secret, for proxies that present client certificates
  (e.g. Cloudflare Authenticated Origin Pulls with your own CA).
- **Reserved concurrency default** for `none` mode, to cap denial-of-wallet.
  Needs care: new accounts have low concurrency limits and must keep 10
  unreserved.
- **Print the protection mode on every deploy** (e.g. `protection: none (public Function URL)`).

## Sources

- [AWS: Restrict access to a Lambda function URL origin (OAC)](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-restricting-access-to-lambda.html)
- [AWS Compute Blog: Protecting a Lambda function URL with CloudFront and Lambda@Edge](https://aws.amazon.com/blogs/compute/protecting-an-aws-lambda-function-url-with-amazon-cloudfront-and-lambdaedge/)
- [AWS: Control access to Lambda function URLs](https://docs.aws.amazon.com/lambda/latest/dg/urls-auth.html)
- [SST `ssr-site.ts` (`protection` modes)](https://github.com/sst/sst/blob/dev/platform/src/components/aws/ssr-site.ts)
- [cdk-nextjs `NextjsDistribution.ts` (Lambda@Edge signer for `AWS_IAM`)](https://github.com/jetbridge/cdk-nextjs/blob/main/src/NextjsDistribution.ts)
- [NIST SP 800-207 Zero Trust Architecture](https://csrc.nist.gov/pubs/sp/800/207/final)
