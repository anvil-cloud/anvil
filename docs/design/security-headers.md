# Security headers on site components

**Status:** Implemented for every site component through the shared hosting
layer (`internal/awssite/hosting.go`, `security_headers.go`). Unit-tested;
**not yet deployed to AWS**.

## What and why

Every site component adds browser security headers to **every response** —
pages, API responses and static assets — through a CloudFront response headers
policy. **On by default.**

| Header | Value | Protects against | Why it's a default |
|---|---|---|---|
| `Strict-Transport-Security` | `max-age=31536000` (1 year) | Downgrade to HTTP on hostile networks (public Wi-Fi) | After one visit, browsers always use HTTPS for the domain. Safe on CloudFront, which is always HTTPS |
| `X-Frame-Options` | `SAMEORIGIN` | Clickjacking — another site framing yours and tricking users into clicks | Only breaks sites meant to be embedded on other domains, which can opt out |
| `X-Content-Type-Options` | `nosniff` | Browsers guessing a file is a script and running it | Anvil serves correct content types for web files |
| `Referrer-Policy` | `strict-origin-when-cross-origin` | Full URLs (tokens, IDs in query strings) leaking to other sites | Already the browser default; setting it makes it explicit and consistent |
| `Permissions-Policy` | `camera=(), microphone=(), geolocation=(), usb=(), browsing-topics=()` | Injected scripts or embedded third parties using powerful browser features the site never intended to use | Most sites never use these; sites that do opt in per feature |
| `Cross-Origin-Opener-Policy` | `same-origin-allow-popups` | Other sites getting a handle on the site's window — tab-nabbing and cross-window data leaks | `allow-popups` keeps OAuth sign-in and payment popups working |

**Why `payment` isn't in the default Permissions-Policy:** `payment=()` blocks
the browser Payment Request API, which Apple Pay / Google Pay buttons use
(e.g. Stripe's Payment Request Button). Payments are common enough that the
default mustn't break them. Sites that never take payments can add it:
`permissionsPolicy: 'camera=(), microphone=(), geolocation=(), usb=(), browsing-topics=(), payment=()'`.

**Why `same-origin-allow-popups`, not `same-origin`:** strict `same-origin`
also severs the link to popups the site opens, which breaks "Sign in with
Google/Apple" and payment popups that report back to the page.
`same-origin-allow-popups` still isolates the site from windows *other* sites
open, which is the protection that matters.

**Why default-on:** missing security headers are one of the most common
pentest and SOC 2 audit findings, they fail scanner grades
(securityheaders.com, Mozilla Observatory), and the risk to existing sites is
small. Hosting platforms set them by default (Vercel and Netlify add HSTS;
Rails and Django add frame options, nosniff and a referrer policy); SST and raw
CDK/Pulumi don't. Anvil's secure defaults put it with the platforms.

**How secure the implementation is:**

- **The app always wins** (`override: false` on every header) — Anvil can't
  weaken a stricter policy the app sets itself.
- **Values are validated before deploy.** `permissionsPolicy` must be printable
  ASCII on one line (no CR/LF, so a value can't smuggle in extra headers) and
  within CloudFront's 1,783-character limit; enum inputs reject unknown values.
- **Applied at the edge to every response** — including WAF blocks, errors and
  static assets — so no response goes out without them.
- **Nothing weakens by default:** the opt-outs (`frameOptions: 'none'`,
  `permissionsPolicy: 'none'`, `crossOriginOpenerPolicy: 'none'`,
  `enabled: false`) all have to be set explicitly.

**How much it helps:** moderately. It fully closes clickjacking and
HTTPS-downgrade attacks, and hardens a couple of edge cases (mis-typed files,
referrer leaks). It's defence in depth — it limits damage when something else
goes wrong — not a replacement for app security.

### Content-Security-Policy is bring-your-own

Anvil sends **no** Content-Security-Policy — not a partial "baseline" one, and
there's no CSP report collector. CSP is the app's responsibility, configured in
the framework (SvelteKit: `kit.csp`, which generates per-request nonces for its
own scripts).

Why: a useful CSP depends on what each app loads, its nonces must come from the
server rendering the page (a fixed CloudFront header can't match them), and
Anvil doesn't change app behaviour. A script-free baseline (`object-src`,
`base-uri`, `frame-ancestors`) and a managed report collector were considered
and left out — the baseline adds little beyond the headers already sent, and
both would blur the line that CSP belongs to the app.

If an app wants CloudFront to send a fixed CSP anyway, `hatch.responseHeadersPolicy`
can add one.

### The app always wins

Every header uses CloudFront's `override: false`: if the app sets a header
itself, CloudFront keeps the app's value. Apps with their own policy are never
overruled.

## API

```ts
new anvil.aws.SvelteKitSite('web', {
  path: 'web',
  // omitted → all four headers, SAMEORIGIN, HSTS without includeSubDomains/preload
  securityHeaders: {
    frameOptions: 'deny',                               // 'sameorigin' (default) | 'deny' | 'none'
    hsts: { includeSubDomains: true, preload: true },   // opt-in, hard to undo
    permissionsPolicy: 'camera=(self), microphone=(self)', // allow what your app uses; 'none' = no header
    crossOriginOpenerPolicy: 'same-origin',             // 'same-origin-allow-popups' (default) | 'same-origin' | 'none'
  },
  // securityHeaders: { enabled: false }               // turn the policy off
});
```

| Input | Default | Notes |
|---|---|---|
| `securityHeaders.enabled` | `true` | |
| `securityHeaders.frameOptions` | `sameorigin` | `none` removes `X-Frame-Options` so the site can be embedded anywhere |
| `securityHeaders.hsts.includeSubDomains` | `false` | Applies HSTS to every subdomain — affects anything served over HTTP on them |
| `securityHeaders.hsts.preload` | `false` | Requires `includeSubDomains`. Submitting to the browser preload list is a separate, manual step |
| `securityHeaders.permissionsPolicy` | `camera=(), microphone=(), geolocation=(), usb=(), browsing-topics=()` | Full header value; replaces the default. `none` sends no header |
| `securityHeaders.crossOriginOpenerPolicy` | `same-origin-allow-popups` | `same-origin` (strict) or `none` (no header) |
| `hatch.responseHeadersPolicy` | — | Escape hatch: extra headers, a CloudFront-set CSP, etc. |

Off by default for `includeSubDomains`/`preload` because they're hard to undo
(browsers remember them for a year) and affect domains beyond the site.

## Shared by every framework

Security headers live in the **shared hosting layer**, alongside server
protection, WAF attachment and origin protection:

- Inputs: `protection`, `waf`, `originProtection`, `securityHeaders` — names in
  `sites.HostingInputNames`, types in `provider/sites/types.go`.
- Implementation: `awssite.NewHosting(...)` → `CreateFunctionURL` → `ApplyTo`
  (CloudFront args) → `GrantAccess`. A framework component calls these four and
  gets everything.
- Each site component's Args declares the four fields itself — **not** via an
  embedded struct: Pulumi's component input decoding
  (`ConstructInputs.CopyTo`) only fills top-level tagged fields, so embedded
  inputs would silently never be set. A per-component test
  (`sites.MissingHostingInputs`) fails if a framework forgets one.

## Effects

**Visitors:** nothing visible. Pages look and work the same.

**Developers:**

| Situation | What happens |
|---|---|
| Local dev | `vite dev` has no CloudFront, so it doesn't send these headers — a framing or file-type issue only shows after deploy |
| Embedding the site on another domain | Blocked by `SAMEORIGIN`; browser console: *Refused to display '…' in a frame because it set 'X-Frame-Options' to 'sameorigin'*. Fix: `frameOptions: 'none'` |
| A file served with the wrong content type | With `nosniff`, a script or stylesheet with the wrong type won't load |
| Plain HTTP on the domain | After a visit, browsers force HTTPS for a year (HSTS) |
| Changing a header | A CloudFront update — takes a few minutes to propagate |

## Messages Anvil emits

| Level | When | Message (source) |
|---|---|---|
| **Error** | invalid `frameOptions` | `invalid securityHeaders.frameOptions "<x>": must be "sameorigin", "deny" or "none"` (`security_headers.go`) |
| **Error** | `preload` without `includeSubDomains` | `securityHeaders.hsts.preload requires hsts.includeSubDomains: browsers only accept HSTS preload for a whole domain including its subdomains` (`security_headers.go`) |
| **Error** | invalid `crossOriginOpenerPolicy` | `invalid securityHeaders.crossOriginOpenerPolicy "<x>": must be "same-origin-allow-popups", "same-origin" or "none"` (`security_headers.go`) |
| **Error** | `permissionsPolicy` with a control character / newline | `securityHeaders.permissionsPolicy contains an invalid character <c>: header values must be printable ASCII on one line` (`security_headers.go`) |
| **Error** | `permissionsPolicy` too long | `securityHeaders.permissionsPolicy is <n> characters; CloudFront allows at most 1783` (`security_headers.go`) |

## Audits and compliance

| Who checks | Checks these headers? |
|---|---|
| AWS Security Hub / AWS Config (CloudFront controls) | **No** — no control covers response headers. Security Hub checks HTTPS, WAF, TLS policy, OAC, logging |
| Cloud posture tools (Prowler, Wiz, …) | Generally no — infrastructure configuration |
| Web vulnerability scanners (OWASP ZAP, Burp, Qualys, Tenable, Detectify) | **Yes** — each missing header is a low/informational finding, including Permissions-Policy and cross-origin isolation |
| Pentests, bug bounties | **Yes** — routinely reported as low-severity findings |
| securityheaders.com, Mozilla Observatory | **Yes** — they grade these headers |
| Enterprise security questionnaires | Often ask directly (HSTS, CSP, X-Frame-Options) |

- **OWASP ASVS** has an HTTP security headers section (HSTS, nosniff, frame
  protection, Referrer-Policy, CSP); Anvil's defaults cover all of it except CSP.
- **PCI DSS 4.0** requires detecting unauthorised changes to scripts and HTTP
  headers on payment pages (6.4.3, 11.6.1) — CSP in the app matters for card
  payment pages.
- **SOC 2 / ISO 27001** don't name headers; auditors rely on pentest and
  scanner reports, which is where missing headers show up.

So the headers clear the *web scanner and pentest* findings; they don't change a
Security Hub score. Separate Security Hub findings on site components (e.g.
CloudFront.5 access logging) are tracked under future work.

## For the user docs

### ⚠️ Warnings

1. **Sites using the camera, microphone, geolocation or USB** must allow them:
   e.g. `permissionsPolicy: 'camera=(self), microphone=(self)'` (your value
   replaces the whole default). Otherwise the browser refuses the feature
   without asking the user.
2. **Sites embedded on other domains break** with the default `SAMEORIGIN`.
   Set `securityHeaders: { frameOptions: 'none' }` for widget-style sites.
3. **The headers only exist on the deployed site**, not in `vite dev`. Test
   embeds and unusual file types against a deployed stage.
4. **HSTS is sticky for a year.** Once browsers have seen it, the domain can't
   go back to plain HTTP. `includeSubDomains` and `preload` extend that to
   every subdomain and are hard to undo — only enable them deliberately.
5. **`crossOriginOpenerPolicy: 'same-origin'` breaks sign-in and payment
   popups** that report back to the page — keep the default unless you know
   you don't use them.
6. **Content-Security-Policy is bring-your-own** — Anvil doesn't send one.
   For XSS protection, configure it in your framework (SvelteKit: `kit.csp`);
   use its report-only mode first and a CSP reporting service of your choice.

### 💡 Good to know

- On by default for every site component; no configuration needed.
- Headers your app sets itself take priority.
- Applied to every response, including static assets.
- No performance cost — CloudFront adds them at the edge.
- Header scanners (securityheaders.com, Mozilla Observatory) will still flag
  the missing Content-Security-Policy until the app sets one.

## Future work

- CloudFront access logging by default (Security Hub CloudFront.5).
- A deploy message listing the headers applied, to surface the dev/prod difference.

## Sources

- [AWS: CloudFront response headers policies](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/adding-response-headers.html)
- [Vercel: HSTS defaults](https://vercel.com/docs/cdn-security/encryption)
- [Netlify forum: HSTS defaults](https://answers.netlify.com/t/security-headers-adding-includesubdomains-and-preload-to-strict-transport-security-header-to-sites-with-default-domain-name/19706)
- [MDN: Strict-Transport-Security](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Strict-Transport-Security)
- [MDN: X-Frame-Options](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Frame-Options)
- [MDN: Permissions-Policy](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Permissions-Policy)
- [MDN: Cross-Origin-Opener-Policy](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cross-Origin-Opener-Policy)
- [AWS Security Hub: CloudFront controls](https://docs.aws.amazon.com/securityhub/latest/userguide/cloudfront-controls.html)
- [SvelteKit: Content Security Policy (`kit.csp`)](https://svelte.dev/docs/kit/configuration#csp)
