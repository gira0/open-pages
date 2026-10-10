# Getting started and operations

How to install, configure, run and look after an open-pages server. For the HTTP interface
see [api.md](api.md), for accounts and access rules [auth.md](auth.md), for the database
[schema.md](schema.md).

open-pages is a single Go binary. It serves a small JSON API, a test page, and the static
sites that people deploy to it, and it stores its state in one SQLite file plus a directory
of site files. It speaks plain HTTP; terminate TLS in a reverse proxy.

## Install

Pick one.

**Build from source.** Needs Go 1.26 or newer. No C toolchain: SQLite is pure Go.

```sh
go build -o open-pages ./cmd/open-pages
./open-pages -config settings.ini
```

The UI template is embedded in the binary, so it can run from any working directory.

**Release archive.** Pushing a `v*` tag publishes a GitHub release with `linux/amd64` and
`linux/arm64` archives (binary and `settings.ini`) and a `SHA256SUMS` file.

**Container image.** `ghcr.io/gira0/open-pages`, tagged with the version (stable releases
also get `major.minor` and `latest`). See [Containers](#containers).

**CLI only.** The same binary deploys sites from CI; `go install
github.com/gira0/open-pages/cmd/open-pages@latest` installs it. See
[Deploying with the CLI and CI](#deploying-with-the-cli-and-ci).

## Quick start

```sh
go build -o open-pages ./cmd/open-pages
./open-pages -config settings.ini &

# create an account and log in
curl -X POST localhost:8080/v1/user/register -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"change-me-please"}'
curl -c cookies -X POST localhost:8080/v1/user/login -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"change-me-please"}'

# create a site, upload a zip of your files, open it
curl -b cookies -X POST localhost:8080/v1/auth/sites -H 'Content-Type: application/json' \
  -d '{"name":"hello"}'
(cd ./public && zip -qr ../site.zip .)
curl -b cookies -X POST --data-binary @site.zip localhost:8080/v1/auth/sites/hello/upload
curl localhost:8080/hello/
```

`http://localhost:8080/index` serves a small test page with the same actions.

## Command line

```text
open-pages [-config settings.ini]                       run the server
open-pages deploy [-server URL] [-create=false] <site> <dir>   deploy a directory
```

`-config` defaults to `settings.ini` in the working directory. The server stops gracefully
on `SIGINT` or `SIGTERM`, giving in-flight requests up to 30 seconds. There are no
environment variables for the server; configuration is the settings file only. (The
`deploy` subcommand reads `OPEN_PAGES_TOKEN` and `OPEN_PAGES_URL`.)

## Configuration reference

The settings file is INI. A missing file is a fatal error; a missing key uses the default.
The repository's [`settings.ini`](../settings.ini) documents the same keys and
[`deploy/settings.ini`](../deploy/settings.ini) holds the defaults baked into the container
image. Booleans written as anything other than a boolean silently fall back to the default,
except the two security-relevant ones, `auth.local_login` and `oidc.enabled`, where a bad
value fails startup. Integers that do not parse also fall back to the default.

### Top level

| Key | Default | Meaning |
|---|---|---|
| `app_mode` | none | Present in the shipped files but **not read by the code**; it has no effect |

### [server]

| Key | Default | Meaning |
|---|---|---|
| `host` | empty (all interfaces) | Interface to bind |
| `http_port` | `8080` | Port to listen on |
| `cookie_secure` | `false` | Set the `Secure` flag on the session cookie. Set to `true` whenever users reach the service over HTTPS |
| `cors_origins` | empty | Comma-separated origins allowed to call the API cross-origin with credentials; empty means same-origin only |

### [paths]

| Key | Default | Meaning |
|---|---|---|
| `datapath` | `.` | Directory for `data.db` and the `op_data/` site files. Made absolute at startup |
| `tmppath` | `.` | Directory under which uploads are spooled (`<tmppath>/tmp`) before extraction. Extraction itself is staged inside `op_data/`. Made absolute at startup |

The `op_data/` and `tmp/` directories are created if missing (`op_data` mode 0755, `tmp`
0700).

### [limits]

| Key | Default | Meaning |
|---|---|---|
| `max_upload_mb` | `100` | Largest accepted upload (the archive as sent). Over it: 413 on `upload` |
| `max_extract_mb` | `500` | Largest total uncompressed size of an archive |
| `max_extract_files` | `10000` | Largest number of entries in an archive |

Archives breaking the extract limits, or with entries outside the site directory, are
rejected with 400. Make sure a reverse proxy in front allows bodies of at least
`max_upload_mb` (nginx: `client_max_body_size`).

### [sites]

| Key | Default | Meaning |
|---|---|---|
| `keep_versions` | `5` | Versions kept per site, including the live one; older ones are deleted after each deploy and can no longer be rolled back to. Values below 1 are treated as 1 |
| `url_mode` | `path` | `path` or `subdomain`; anything else fails startup. See [URL modes](#url-modes) |
| `base_domain` | empty | Bare host name of the API and UI (for example `pages.corp`). Required for `subdomain` mode, ignored in `path` mode. The only checks are that it is not empty and contains no `:` or `/`; other malformed values (spaces, bad labels) are accepted at startup and simply never match a request. It is lower-cased and a trailing dot removed |

### [log]

| Key | Default | Meaning |
|---|---|---|
| `level` | `info` | `debug`, `info`, `warn` or `error` (case-insensitive); anything else fails startup |
| `format` | `text` | `text` (key=value lines) or `json` (one object per line); anything else fails startup |

### [metrics]

| Key | Default | Meaning |
|---|---|---|
| `listen` | empty | Address of a dedicated listener that serves only `/metrics`, for example `127.0.0.1:9100`. Empty means none |
| `token` | empty | If set (at least 16 characters, else startup fails), `/metrics` is also served on the main listener, readable only with `Authorization: Bearer <token>`. If `listen` is set too, the token is required there as well |

With both empty, metrics are off. With only `listen` set, the dedicated listener is open
to anyone who can reach it, so bind it to loopback or a monitoring network.

### [auth]

| Key | Default | Meaning |
|---|---|---|
| `local_login` | `true` | Serve `POST /v1/user/register` and `/v1/user/login`. `false` is for OIDC-only setups and requires `[oidc] enabled = true`, otherwise startup fails. Must be `true` or `false` |

### [oidc]

Off unless `enabled = true`. When `enabled` is false the other keys are not looked at. The
sign-in flow, provider setup and identity rules are in [auth.md](auth.md#oidc-sign-in).

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Turn OIDC sign-in on. Must be `true` or `false` |
| `issuer` | none, required | Issuer URL exactly as the provider reports it; discovery is read from `<issuer>/.well-known/openid-configuration` on first use. `https` only (`http` for localhost) |
| `client_id` | none, required | Client id registered at the provider |
| `client_secret` | none, required | Client secret. Read from this file only, never logged; make the file readable only by the service user |
| `redirect_url` | none, required | Absolute URL of `/v1/auth/oidc/callback` on this server, as registered at the provider. `https` only (`http` for localhost) |
| `scopes` | `openid email profile` | Space- or comma-separated; `openid` is always added |
| `email_claim` | `email` | ID token claim holding the email address |
| `groups_claim` | `groups` | Claim holding group names; empty turns group mapping off |
| `allowed_email_domain` | empty | Only emails in this domain may sign in; a leading `@` is ignored; empty allows any |

### Startup checks

The server refuses to start, with the reason on stderr, when: the file cannot be read;
`url_mode` is invalid; `url_mode = subdomain` with an empty `base_domain` or one containing
`:` or `/` (no other hostname validation is done);
`log.level` or `log.format` is invalid; `metrics.token` is shorter than 16 characters;
`local_login` or `oidc.enabled` is not a boolean; `local_login = false` without OIDC; OIDC
is enabled with a required key missing or a non-https URL; or the data directories or
database cannot be created or migrated.

## URL modes

`[sites] url_mode` decides how a request names its site. The API, the test page and the
probes stay on the bare host in both modes. Site names are DNS labels in both, so switching
modes needs no data migration.

**Path mode** (`path`, default): `https://pages.corp/<site>/...`. Needs one DNS name and
one certificate. Caveats:

- Root-absolute links break. `<link href="/css/app.css">` asks for `/css/app.css`, which is
  not part of the site. Build sites with a base path (Hugo `baseURL`, Vite `base`, Jekyll
  `baseurl`) or use relative links.
- All sites share one origin with each other and with the API: shared cookies and local
  storage, and they can script each other. Do not host untrusted content in this mode.
- The reserved names (see [api.md](api.md#sites)) cannot be sites, so they cannot shadow
  API routes.

**Subdomain mode** (`subdomain`): `https://<site>.pages.corp/...` with
`base_domain = pages.corp`. Needs wildcard DNS (`*.pages.corp` and `pages.corp` both pointing
at the server) and a wildcard certificate. Root-absolute links work and every site has its
own origin. Behind a reverse proxy, pass the original `Host` header through. A request whose
host is `<label>.<base_domain>` is served as a site (a label that is not a valid site name
is a 404); any other host reaches the API and UI routes. Browsers cannot open non-public
sites in this mode; see [auth.md](auth.md#known-limits).

## Containers

The `Dockerfile` builds a static binary into a `scratch` image that runs as the non-root
user `65532:65532`. It listens on all interfaces, port 8080, and keeps everything in the
`/data` volume. Defaults come from `deploy/settings.ini` at
`/etc/open-pages/settings.ini` (`datapath = tmppath = /data`). The image has no `/tmp`, so
it sets `TMPDIR=/data/tmp`.

```sh
docker build -t open-pages .
docker run -p 8080:8080 -v open-pages-data:/data open-pages
```

To change settings, mount your own file over the baked-in one and keep `datapath` and
`tmppath` under `/data`:

```sh
docker run -p 8080:8080 -v open-pages-data:/data \
  -v "$PWD/settings.ini":/etc/open-pages/settings.ini:ro open-pages
```

A named volume is writable by uid 65532 out of the box. A bind-mounted data directory must
be `chown`ed to `65532` first. There are no environment variables for configuration.

`docker-compose.yml` is an example: it builds the image locally (use
`image: ghcr.io/gira0/open-pages:<version>` for a release), publishes port 8080, mounts the
`open-pages-data` volume at `/data`, and runs with `read_only: true`, `cap_drop: [ALL]` and
`no-new-privileges`. An optional commented line shows the settings override.

```sh
docker compose up --build
```

Because the image has no shell or `curl`, a container health check cannot run inside it;
probe `GET /healthz` from the orchestrator or load balancer instead.

## Health and metrics

### Health

`GET /healthz` needs no login. It returns `200` with `{"status":"ok", ...}` when the
database answers and both `op_data/` and the staging `tmp/` directory accept new files, and
`503` with `"status":"fail"` otherwise, with a per-check `ok` or `fail` for `database` and
`data_dir`. The reason for a failure is written to the log, not the response. The check gives
up after 2 seconds, and concurrent requests share one in-flight filesystem check, so a hung
mount cannot pile up goroutines. `GET /v1/ping` is a cheaper liveness probe that touches
nothing.

### Metrics

Prometheus text format, hand-written (no client library). Enable with `[metrics] listen`,
`[metrics] token`, or both (see the table above).

```sh
curl -H "Authorization: Bearer $METRICS_TOKEN" https://pages.corp/metrics
```

| Metric | Labels | Meaning |
|---|---|---|
| `openpages_http_requests_total` | `method`, `code` (`1xx` to `5xx`) | Requests served; unusual methods count as `OTHER` |
| `openpages_http_request_duration_seconds` | `le` | Histogram (bounds 0.005, 0.025, 0.1, 0.25, 1, 2.5, 10 seconds) with `_sum` and `_count` |
| `openpages_deploys_total` | `result` (`ok`, `rejected`, `failed`) | Archive uploads by outcome; `rejected` means the archive was refused |
| `openpages_errors_total` | none | Responses with a 5xx status |

Requests are never labelled by path or site, so the number of series is fixed. Counters
reset when the process restarts.

### Logging

Logs go to standard error in the `[log]` format. Every response carries an `X-Request-Id`
header, generated by the server (an incoming one is ignored). The request log line has
`request_id`, `method`, `route` (the matched pattern, not the raw URL), `status` and
`duration`; raw URLs and client addresses are deliberately not logged. Handler errors log
with the same `request_id`, so a client report can be matched to the log. `/healthz` and
`/metrics` requests are logged at `debug`; 5xx responses at `error`.

## Deploying with the CLI and CI

Create an API token while logged in (it is shown once; see [auth.md](auth.md#api-tokens)):

```sh
curl -b cookies -X POST https://pages.corp/v1/auth/tokens \
  -H 'Content-Type: application/json' -d '{"name":"ci","expires_in_days":90}'
```

Then deploy a directory:

```sh
export OPEN_PAGES_TOKEN=opt_...                  # environment only, never a flag
export OPEN_PAGES_URL=https://pages.corp         # or: -server https://pages.corp
open-pages deploy blog ./public                  # flags go before <site> <dir>
```

- The directory is zipped (files are read inside it only), uploaded to
  `/v1/auth/sites/<site>/upload`, and the new version id is printed. Symlinks and other
  special files are an error; an empty directory is an error.
- If the upload answers 404, the CLI creates the site and uploads again. `-create=false`
  turns that off. **A site created this way is `public` with no group.** To keep it private,
  create it first with the visibility you want, or change it afterwards with
  `PUT /v1/auth/sites/<site>`.
- The token must belong to the site's owner (otherwise 403).
- Plain `http` to a non-loopback host prints a warning, because the token travels in the
  clear. Redirects are never followed, so the token cannot be forwarded to another origin;
  use the final URL with `-server`.
- Exit code 0 on success, 1 on any error. Timeout 10 minutes.

CI examples ready to copy, not run by this repository's CI:
[`examples/github-actions.yml`](../examples/github-actions.yml) and
[`examples/gitlab-ci.yml`](../examples/gitlab-ci.yml). In short:

```yaml
# GitHub Actions step; the token is a repository secret, the URL a variable
- run: go install github.com/gira0/open-pages/cmd/open-pages@latest
- run: open-pages deploy blog ./public
  env:
    OPEN_PAGES_TOKEN: ${{ secrets.OPEN_PAGES_TOKEN }}
    OPEN_PAGES_URL: ${{ vars.OPEN_PAGES_URL }}
```

Without the CLI, any client can `POST` a zip to the upload route
([api.md](api.md#post-v1authsitesnameupload)).

## Data directory layout

Under `datapath`:

```text
data.db                          SQLite database
op_data/<site>/versions/<id>/    one extracted upload per deploy
op_data/<site>/current           symlink to versions/<id>, switched atomically
op_data/.staging-*               in-progress extractions (temporary)
```

and under `tmppath`, `tmp/` holds uploads being spooled (temporary; contains nothing worth
keeping). A deploy extracts into a staging directory, moves it into `versions/` and
renames the `current` link over the old one, so a request sees either the old or the new
version, never a mixture. Deleting a site removes `op_data/<site>/` and then its database
row.

## Backups

State is the database plus `op_data/`. `tmp/` and `.staging-*` need no backup.

The two must be captured together. The database says which sites exist and `op_data/`
holds their files, and a running server changes both: a deploy adds a version directory and
moves the `current` link, pruning deletes old versions, and deleting a site removes files
and then the row. Copying them one after the other while the server runs can give a site
row without files, or a `current` link whose target was pruned in between. So:

- **Stop the server for the whole backup**, then copy `data.db` (and any `data.db-journal`
  beside it, which belongs with it) and `op_data/` (`rsync -a` or `tar`, which keep the
  relative `current` symlinks). This is the supported method.
- Alternatively use a storage-level snapshot (LVM, ZFS, a cloud volume snapshot) that
  captures the volume holding both at one instant.
- Online tools such as `sqlite3 data.db ".backup ..."` give a consistent copy of the
  database only, not of `op_data/`. They are fine for an extra database copy, but are not a
  coordinated backup on their own.
- **Container.** With a named volume, stop the container, archive, start it again:

  ```sh
  docker compose stop
  docker run --rm -v open-pages-data:/data -v "$PWD":/backup alpine \
    tar czf /backup/open-pages-$(date +%F).tgz -C /data data.db op_data
  docker compose start
  ```

- **Before upgrades.** Migrations run at startup and are one-way
  ([schema.md](schema.md#migrations)); take a backup first.
- **Restore.** Stop the server, put `data.db` and `op_data/` back under `datapath`
  (owned by the service user, uid 65532 in the container), start it. A site row without
  files answers 404 until it is deployed again; files without a row are not served. A
  database restored from before an OIDC or token event also restores those sessions and
  tokens as they were then.
- **Secrets.** The settings file holds `client_secret` and `metrics.token`; back it up
  separately and keep it private. Sessions and API tokens are in the database, so the
  backup is as sensitive as the live data.
