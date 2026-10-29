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
  receive it. The server also ignores it on a content host.
- **Private sites work in browsers through per-site cookies.** A visitor to an
  `authenticated` or `restricted` site is sent through a login handoff on the app host and
  gets a cookie valid for that one site (host-only, `HttpOnly`, `SameSite=Lax`, `__Host-`
  prefixed with `cookie_secure`, at most 1 hour, a random token unrelated to the app
  session and stored hashed). Access is checked against the database on every request, so
  group and visibility changes apply at once, a cookie of one site is useless on another,
  and logging out of the app revokes every site cookie that came from that session. The
  one-time login code is short-lived, single use, bound to site, user and the browser that
  started the login (state cookie), and the return path cannot leave the site. See
  [Signing in to a site](auth.md#signing-in-to-a-site).
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

## Audit log

open-pages keeps a record of who changed what. Every successful change to a site (create,
update, deploy, rollback, delete), a group (create, delete, member added or removed) or an API
token (create, revoke) is written to the `audit_log` table, in the same transaction as the change where
there is one, and as a structured `audit` line in the log (so it reaches a log shipper or SIEM
even if the database is lost). An entry says when, which user (id and the email they had then),
with which credential (`session` or `token:<prefix>`), what, the name of the target, small
details such as the version or the old and new value of a changed field, and the client
address. Entries are snapshots: they stay after the user, site or group is deleted.

- **Not audited:** reads, failed or refused requests, sign-in and sign-out, account creation,
  group memberships synchronised from OIDC or SAML at sign-in, and housekeeping such as pruning
  old versions or expired tokens. Secrets (passwords, tokens, session cookies) never appear.
- **Who can read it:** a site's owner reads that site's history and every user reads their
  own actions ([API](api.md#get-v1authsitesnameaudit)). There is no administrator role, so
  nobody reads other people's entries through the API; operators read the database or the log.
- **Retention:** `[audit] retention_days` (default 365, `0` for ever). Older entries are deleted at
  startup and daily. Keep the log lines in your log system for longer if you need to.
- **Client address:** the address of the TCP peer. Behind a reverse proxy it is the proxy's
  address; forwarded headers are not trusted yet (X-Forwarded-For handling arrives with #72).
- **Not tamper-proof.** The log is an ordinary table: anyone with write access to `data.db`
  (or the host) can change or delete entries without a trace, and the application can too.
  Ship the log lines off the host to keep a copy that the host's administrators cannot rewrite.
  If an entry cannot be written the change still goes through; the failure is logged and the
  `audit` line is still emitted.

## What the deployment must provide

- **A separate content domain, ideally a separate registrable domain.** Use something like
  `pages.corp` for the app and `pages-content.corp` for the sites, the way GitHub uses
  `github.com` and `github.io`. Siblings under a shared parent (`app.pages.corp` and `sites.pages.corp`) are
  accepted, but then the app and the sites are same-site, so `SameSite` no longer separates
  them. That layout is safe only because of these protections, all of which need
  `cookie_secure = true` on HTTPS: the app rejects cross-origin browser `POST`, `PUT` and
  `DELETE` requests (Go's `http.CrossOriginProtection`, using `Sec-Fetch-Site` and `Origin`);
  JSON routes answer 415 unless the body is `application/json`, so a site cannot send a
  "simple" cross-site request with the session cookie; and the session cookie is named
  `__Host-auth_cookie`, so a site cannot overwrite it from a sibling host. Nothing on the
  parent domain may set cookies for it either (a `Domain=` cookie would reach both).
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

- **Public means public.** Anyone who can reach the content host can read a `public` site,
  and a malicious site can still do what any web page can do from its own origin (phish,
  mine, call other origins), without access to the app's session. Review who may create
  sites.
- **No rate limits**, no scanning of uploads, and no quota beyond the archive size limits in
  `[limits]`.

See [auth.md](auth.md) for accounts, tokens and visibility rules, and
[SECURITY.md](../SECURITY.md) for reporting a vulnerability.
