# Security model

open-pages hosts content that other people wrote, JavaScript included. This page says what
keeps that content away from the application, what a deployment has to provide for that to
hold, and what is not covered yet.

## Guarantees

- **User content never shares an origin with the app.** The API, the UI and the session
  cookie live on `app_host`. Hosted sites are served only from `<site>.<content_domain>`.
  The app host never serves site content, whatever the path, and a site host never serves
  the API, the UI or static assets. There is no path-based mode and no setting that turns
  this off. Startup refuses a `content_domain` equal to `app_host`, and any layout where one is a
  subdomain of the other, at any depth.
- **Each site is its own origin.** Every site has its own host, so its scripts, cookies and
  local storage are separate from every other site's and from the app's. A site cannot read
  or script another site. Cookies are the one exception, because browsers scope them by
  domain, not origin: script on one site can set a cookie with `Domain=<content_domain>`
  that the browser sends to every sibling site (and, unless `content_domain` is a public
  suffix, nothing stops that). Sites and tools that keep security-relevant state in cookies
  must use host-only cookies with the `__Host-` prefix, and must not trust a cookie a
  sibling site could have set.
- **The app session never reaches content hosts.** The session cookie is set without a
  `Domain` attribute (host-only), so a browser sends it to `app_host` and nowhere else,
  and it is `HttpOnly` and `SameSite=Lax`, so script on a site host can neither read it nor
  receive it.
- **Requests are routed by host, and unknown hosts reach nothing.** The `Host` header
  (case-insensitive, port and one trailing dot ignored) decides what a request is. A host
  that is neither `app_host` nor `<valid site name>.<content_domain>` gets a `404`, so a
  forged `Host` header never reaches the API. The only route that answers on an arbitrary
  host is `GET /healthz`, for probes by IP address.
- **Content hosts are read-only.** Only `GET` and `HEAD` are accepted there; everything else
  is `405`. CORS headers are never added to site responses, and the UI's
  `Content-Security-Policy` is not applied to them (their authors decide what they load).
- **Files stay inside the site.** Paths are resolved with `os.Root`, so `..` and symlinks
  that point outside the site are refused.

## What the deployment must provide

- **A separate content domain, ideally a separate registrable domain.** Use something like
  `pages.corp` for the app and `pages-content.corp` for the sites, the way GitHub uses
  `github.com` and `github.io`. Siblings under a shared parent (`app.pages.corp` and `sites.pages.corp`) are
  accepted, but then nothing on the parent domain may set cookies for it (a `Domain=` cookie
  on the parent would reach both), and the browser's same-site checks no longer separate
  the app from the sites.
- **Wildcard DNS and a wildcard TLS certificate** for `*.<content_domain>`, plus DNS and a
  certificate for `app_host`. See [URLs and hosts](getting-started.md#urls-and-hosts).
- **HTTPS everywhere, with `cookie_secure = true`.** open-pages itself speaks plain HTTP and
  expects a TLS-terminating reverse proxy. The server does not enforce HTTPS: it is the
  operator's job to serve only HTTPS and to set `[server] cookie_secure = true` so the session
  cookie carries the `Secure` flag. Without TLS, sessions and API tokens cross the network in
  the clear. Consider `Strict-Transport-Security` at the proxy.
- **The original `Host` header passed through** by the reverse proxy.
- **No cookie or CORS exceptions that bridge the domains.** Leave `cors_origins` empty
  unless you know the origin, and never list a site host there: it would let that site's
  scripts call the API with credentials.

## What is not covered yet

- **There is no per-site sign-in.** A site that is not `public` is checked against the
  session cookie or an API token (`Authorization: Bearer`) on the content host. Because the
  session cookie is host-only and never reaches a content host, a browser cannot open an
  `authenticated` or `restricted` site by signing in to the app: it gets the same `404` as
  for a site that does not exist. Command-line clients can use an API token. Browser
  sign-in for sites is planned separately.
- **Public means public.** Anyone who can reach the content host can read a `public` site,
  and a malicious site can still do what any web page can do from its own origin (phish,
  mine, call other origins), without access to the app's session. Review who may create
  sites.
- **No rate limits**, no scanning of uploads, and no quota beyond the archive size limits in
  `[limits]`.

See [auth.md](auth.md) for accounts, tokens and visibility rules, and
[SECURITY.md](../SECURITY.md) for reporting a vulnerability.
