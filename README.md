# open-pages

A self-hosted, GitHub Pages-like service for publishing static sites on internal networks.

> Status: early prototype. Users can register, log in, upload a site archive (or deploy from
> CI with an API token) and have it served. Per-site access control is not in yet: every deployed site is public.

## Requirements

- Go 1.26 or newer. No C toolchain is needed: SQLite is the pure-Go `modernc.org/sqlite`.

## Run

```sh
go build -o open-pages .
./open-pages -config settings.ini
```

Open http://localhost:8080/index for the test page. Settings are documented in
[`settings.ini`](settings.ini). The database (`data.db`) and extracted sites (`op_data/`)
are created under `datapath`.

## Container

A multi-stage `Dockerfile` builds a static binary into a minimal `scratch` image that runs as
a non-root user (uid 65532). The image listens on all interfaces, port 8080, and keeps the
database and sites in the `/data` volume.

```sh
docker compose up --build        # see docker-compose.yml
# or
docker build -t open-pages .
docker run -p 8080:8080 -v open-pages-data:/data open-pages
```

Defaults come from [`deploy/settings.ini`](deploy/settings.ini) inside the image. To change
them, mount your own file over `/etc/open-pages/settings.ini` (keep `datapath` and `tmppath`
under `/data`). The image sets `TMPDIR=/data/tmp` because it has no `/tmp`, so large
uploads are spooled on the volume. There are no environment variables: configuration is the settings file plus
the `-config` flag. A bind-mounted data directory must be writable by uid 65532.

Pushing a `v*` tag runs the release workflow: it publishes a GitHub release with
`linux/amd64` and `linux/arm64` archives (binary, `templates/`, `settings.ini`, and
`SHA256SUMS`) and pushes a multi-arch image to `ghcr.io/gira0/open-pages` tagged with the
version (stable releases also get `major.minor` and `latest`).

## API

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/v1/ping` | | Liveness: answers `pong` without touching anything |
| GET | `/healthz` | | Health: pings the database and checks that `op_data/` and the staging `tmp/` are writable; `200 {"status":"ok",...}` or `503 {"status":"fail",...}` with a per-check `ok`/`fail` (reasons go to the log) |
| GET | `/metrics` | token | Prometheus metrics; off unless configured, see [Operations](#operations) |
| POST | `/v1/user/register` | | Create a user (JSON or form: `email`, `password`) |
| POST | `/v1/user/login` | | Log in and receive a session cookie |
| GET | `/v1/auth/user` | ✓ | Current user: account data, groups, owned and viewable docs |
| POST | `/v1/auth/logout` | session | End the session |
| POST | `/v1/auth/tokens` | session | Create an API token: `name`, optional `expires_in_days` (1 to 3650, absent for no expiry); the token is returned once |
| GET | `/v1/auth/tokens` | session | List your tokens (name, prefix, created, expires; never the token) |
| DELETE | `/v1/auth/tokens/{id}` | session | Revoke one of your tokens |
| GET | `/v1/auth/groups` | ✓ | List the groups you belong to |
| POST | `/v1/auth/groups` | ✓ | Create a group (`name`); you become its owner and first member |
| GET | `/v1/auth/groups/{id}` | ✓ | The group and its members (id, email); members only |
| DELETE | `/v1/auth/groups/{id}` | ✓ | Delete the group and its memberships; owner only, and only while no site uses it |
| POST | `/v1/auth/groups/{id}/members` | ✓ | Add a member by `email`; owner only |
| DELETE | `/v1/auth/groups/{id}/members/{userid}` | ✓ | Remove a member; owner only, or yourself to leave |
| POST | `/v1/auth/sites` | ✓ | Create a site (`name`, optional `description` and `group` id); the name must be a DNS label |
| PUT | `/v1/auth/sites/{name}` | ✓ | Change `description` and/or `group` (JSON, absent fields are kept, `"group": 0` clears it); owner only |
| DELETE | `/v1/auth/sites/{name}` | ✓ | Delete the site, all its versions and its database row; owner only |
| POST | `/v1/auth/sites/{name}/upload` | ✓ | Deploy a `.zip`, `.tar.gz` or `.tar` (raw body) as a new version; owner only |
| POST | `/v1/auth/sites/{name}/formupload` | ✓ | Same, with the archive in the multipart field `file` |
| GET | `/v1/auth/sites/{name}/versions` | ✓ | List kept versions (newest first) and the `current` one; owner only |
| POST | `/v1/auth/sites/{name}/rollback` | ✓ | Make a kept version live: `{"version": "<id>"}`; owner only |

Auth "✓" means a session cookie or an API token (see below); "session" means a login
session only.

Whoever creates a site owns it. Only the owner can update, delete, redeploy, list versions
of or roll back a site: other users get 403, unknown sites 404. A `group` must be the id of
an existing group, otherwise the request fails with 400.

Groups: any logged-in user can create one and owns it. Only the owner can delete the group
or add and remove other members; any member can leave, and the owner can't be removed (delete
the group instead). Non-members get 403 on a group's details, unknown groups 404. A group
that is still the `group` of a site can't be deleted (409), so a site never silently loses
its group; move or delete the sites first. Group names are unique ignoring case (Unicode simple case folding, so `Ä` and `ä` clash). Groups
created before owners existed have no owner and can't be changed through the API.

Redeploying is just uploading again: each upload becomes a new version and goes live. Roll
back with the version id from the upload response or the versions list. Versions older
than `keep_versions` are deleted and can no longer be rolled back to.

Each upload is extracted into `op_data/<site>/versions/<id>/` and the
`op_data/<site>/current` symlink is switched to it with an atomic rename, so a site is never
half-deployed. The newest `keep_versions` versions (default 5, `[sites]` in `settings.ini`)
are kept; older ones are deleted.

Uploads are limited in size, file count and uncompressed size (see `[limits]` in
`settings.ini`). Entries that would land outside the site directory are rejected.

## API tokens and deploying from CI

An API token lets a pipeline call the API as you without a browser session. Create one while
logged in (the token is shown once, so store it right away), then send it as
`Authorization: Bearer <token>`:

```sh
curl -b cookies -X POST https://pages.corp/v1/auth/tokens \
  -H 'Content-Type: application/json' -d '{"name": "ci", "expires_in_days": 90}'
```

Tokens are random, stored only as a SHA-256 hash (plus a short prefix for the list), and can
be revoked at any time or set to expire. A token acts as its user with that user's full
rights, so keep it in the CI secret store; per-site scoping is not supported yet. Tokens
cannot create or revoke tokens or log out; those need a real login session. Each user can
hold at most 50 tokens.

The same binary deploys a directory:

```sh
export OPEN_PAGES_TOKEN=opt_...                  # read from the environment only
export OPEN_PAGES_URL=https://pages.corp         # or: -server https://pages.corp
open-pages deploy blog ./public                  # flags go before <site> <dir>
```

It zips the directory (files are read inside it only; symlinks and other special files are
rejected), uploads it to `/v1/auth/sites/blog/upload`, and creates the site first if it does
not exist (`-create=false` turns that off). Install it with
`go install github.com/gira0/open-pages@latest`. Example pipelines for
[GitHub Actions](examples/github-actions.yml) and [GitLab CI](examples/gitlab-ci.yml) are in
[`examples/`](examples/); they are examples only and are not run by this repository's CI.

## Serving sites

A deployed site is served from its live version (`op_data/<site>/current`). How a request
names its site is one setting in `settings.ini`:

```ini
[sites]
url_mode = path        # path (default) or subdomain
base_domain = pages.corp
```

The API and UI stay on the bare `base_domain` in both modes. Site names that could collide
with the API, the UI or service hosts are reserved in both modes and can't be created
(`v1`, `index`, `api`, `www`, `admin`, `ui`, `static`, `assets`, `health`, `metrics`, `login`
and a few similar ones; the full list is `reservedSiteNames` in `resolve.go`). Site names are DNS labels, so
switching modes needs no data migration.

**Path mode** (`url_mode = path`): `https://pages.corp/<site>/...`. It needs one DNS name and
one certificate and nothing else. The API and UI live on the same host, which is why those
names are reserved.

Caveats of path mode:

- A site lives under `/<site>/`, so **root-absolute links break**: `<link href="/css/app.css">`
  asks for `/css/app.css`, which is not part of the site. Build sites with a base path
  (Hugo `baseURL = "https://pages.corp/<site>/"`, Vite `base: "/<site>/"`, Jekyll `baseurl`, ...)
  or use relative links.
- All sites share one origin, so they share cookies and local storage and can script each
  other. Don't host untrusted content in this mode.

**Subdomain mode** (`url_mode = subdomain`): `https://<site>.pages.corp/...`. `base_domain` is
required and must be a bare host name without a port. It needs wildcard DNS (`*.pages.corp`
and `pages.corp` both pointing at this server) and a wildcard certificate. Root-absolute
links just work, and every site gets its own origin (own cookies and local storage), which
is the safer choice for content you don't trust. Requests for any host that is not
`base_domain` or a single-label subdomain of it get a 404. Behind a reverse proxy, pass the
original `Host` header through. The session cookie is host-only, so sites on subdomains
never see it.

What gets served (both modes):

- `index.html` for a directory (`/<site>/docs` redirects to `/<site>/docs/`); there are no
  directory listings.
- The site's own `404.html`, sent with status 404, or a plain 404 page.
- Content types from the file extension, `ETag` and `Last-Modified` with conditional and
  range requests, and `Cache-Control: no-cache` so browsers always revalidate (cheap with
  the ETag) and a new deploy or rollback shows up at once. `HEAD` works.
- Requests can't leave the site: `..` segments are cleaned, and the files are opened
  through `os.Root`, which also refuses symlinks inside a site that point outside it.

## Operations

**Logging.** `[log]` in `settings.ini` sets `level` (`debug`, `info`, `warn`, `error`) and
`format` (`text` or `json`, one object per line). Every response carries an `X-Request-Id`
header, generated by the server (an incoming `X-Request-Id` is ignored), and the request
log line and handler error lines include the same `request_id`, so a client report can be
matched to the log. Request lines record the request ID, method, matched route pattern,
status and duration; they do not record raw URLs or client addresses. Requests to
`/healthz` and `/metrics` are logged at `debug`, 5xx responses at `error`.

**Metrics.** Prometheus text format, hand-written to avoid a client library dependency.
Disabled by default; enable one or both of:

- `[metrics] listen = 127.0.0.1:9100` serves only `/metrics` on its own address; limit who
  can reach it with the bind address or firewall.
- `[metrics] token = <16+ characters>` serves `/metrics` on the main listener, readable only
  with `Authorization: Bearer <token>`. If `listen` is also set, the token is required there
  too.

| Metric | Labels | Meaning |
|---|---|---|
| `openpages_http_requests_total` | `method`, `code` (`2xx`...) | Requests served; unusual methods count as `OTHER` |
| `openpages_http_request_duration_seconds` | `le` | Histogram of request durations |
| `openpages_deploys_total` | `result` (`ok`, `rejected`, `failed`) | Archive uploads by outcome |
| `openpages_errors_total` | | Responses with a 5xx status |

Requests are never labelled by path or site, so the number of series stays fixed.
`healthz`, `readyz` and `metrics` are reserved site names in path mode.

## Development

```sh
go test -race ./...
golangci-lint run      # lint + format check, config in .golangci.yml
golangci-lint fmt      # apply gofmt/goimports
go mod tidy -diff      # go.mod/go.sum must be tidy
```

CI runs on every pull request: tidy check, golangci-lint (gofmt, goimports, vet,
staticcheck, gosec, …), race-enabled tests with a 70% coverage floor (report in the job
summary and as an artifact), govulncheck, and dependency review (blocking newly
introduced dependencies with known moderate-or-higher vulnerabilities). The build,
lint, tests, and vulnerability checks also run weekly and can be triggered manually.
govulncheck deliberately uses the latest scanner and vulnerability database.
Dependabot opens weekly grouped updates for Go modules and GitHub Actions, including
updates to SHA-pinned actions; automated security updates are enabled.
CodeQL scans the Go code and workflows on every pull request and weekly; results show
under the repository's Security tab.

The default-branch ruleset requires the CI and both CodeQL analysis jobs, blocks
CodeQL security alerts rated high or critical and error-level quality alerts, and
requires resolved review conversations and signed commits. Pull requests use squash
merges; no independent approval is required while the project has a solo maintainer.
Actions have read-only default permissions and cannot approve pull requests.
Secret scanning and push protection are enabled. See [SECURITY.md](SECURITY.md) for
supported versions and private vulnerability reporting.

## Code review

Copilot code review reads [`.github/skills/code-review/SKILL.md`](.github/skills/code-review/SKILL.md)
for this repo's conventions, security-sensitive areas and accepted patterns. Update it when a
review finding keeps recurring or a convention changes.
