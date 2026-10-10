---
name: code-review
description: Review pull requests for open-pages, a self-hosted GitHub Pages-like static hosting service. Use for every PR review in this repository.
---

# Reviewing open-pages

open-pages hosts static sites on internal networks. Users upload archives, and the service
serves them. Review for correctness and security first; style is handled by `golangci-lint`.

## Stack (flag deviations)

- Go standard library `net/http` only. No Gin, chi or other web frameworks.
- SQLite through pure-Go `modernc.org/sqlite`. Flag cgo, a C toolchain, or any new dependency
  that needs one. Flag new dependencies that the standard library already covers.
- The server is configured only by the settings file plus `-config`; flag new server environment
  variables. The `open-pages deploy` CLI intentionally reads `OPEN_PAGES_TOKEN` and
  `OPEN_PAGES_URL`.

## Security-sensitive areas (look hardest here)

- **Archive extraction and upload** (`internal/extract/extract.go`, `cmd/open-pages/upload.go`): path traversal (`..`, absolute
  paths), symlinks and hard links, zip bombs (size and entry-count limits), partial extraction
  left behind on failure.
- **Auth** (`cmd/open-pages/auth.go`, `cmd/open-pages/tokens.go`): token and session handling, constant-time comparison,
  tokens never logged or returned twice, a malformed `Authorization` header must never fall
  back to the session cookie.
- **Site names** (`internal/names/names.go`, `cmd/open-pages/sites.go`): must be hostname-valid and must not be a reserved name.
- **Serving and URL resolution** (`cmd/open-pages/serve.go`, `cmd/open-pages/resolve.go`): check both `url_mode = path` and
  `url_mode = subdomain`; no escaping the site root; behaviour on unknown or reserved hosts.
- **Log injection**: do not log raw unvalidated user input (request paths, hosts, headers,
  client addresses). CodeQL flags this; log IDs, matched route patterns and status instead.
  Values already validated to a bounded form, such as a site name (a DNS label), are fine.
- **Redirects and outbound HTTP** (`cmd/open-pages/cli.go`): never forward credentials across redirects.

## Data and deploys

- Deploys are atomic and versioned: publish, switch `current` and prune happen in one critical
  section (`deployMu`). Check any new path that touches `versions/` or `current`.
- Deploy, update, delete and rollback must re-check that the site id and owner still match the
  database row inside the lock. Managing a site (update, delete, rollback, deploy) is
  owner-only; a group only grants viewing, so flag group members gaining write access.
- Partial updates write only the fields present in the request.
- Keep a snapshot read (versions plus current) coherent.

## Repo rules

- Commits are signed and PRs are squash-merged. Flag anything that assumes otherwise.
- GitHub Actions are pinned to a commit SHA, not a tag.
- Behaviour changes need tests; bug fixes need a regression test.
- Docs (`README.md`, `settings.ini`) must match behaviour. Check that documented guarantees
  (for example request IDs on every handler log line) are really true.

## Recurring findings (catch them up front)

Past Copilot reviews kept raising these:

1. Concurrency between deploy, delete, update and prune on the same site.
2. Docs or comments that promise more than the code does.
3. Config values parsed too leniently (accept only the documented values).
4. Credentials leaking: forwarded across redirects, or falling back between auth schemes.
   (CORS `Access-Control-Expose-Headers: X-Request-Id` is required so browsers can read the
   request ID; do not flag it.)
5. Blocking calls that ignore their context or timeout (filesystem, network).
6. UI or templates left stale after an API or route change.

## Do not flag (accepted)

- The `docs` table name is intentionally kept for now.
- No route aliases or schema migrations before real releases.
- Pruning a version while a reader that already resolved it finishes is accepted (readers hold
  an `os.Root`; the worst case is one 404 during a deploy).
- Every site is public until per-site access control lands (phase 2).
- Roadmap lives in GitHub issues (see #31), not in markdown files.
