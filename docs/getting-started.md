# Getting started and operations

How to install, configure, run and look after an open-pages server. For the HTTP interface
see [api.md](api.md), for accounts and access rules [auth.md](auth.md), for the database
[schema.md](schema.md).

open-pages is a single Go binary. It serves a small JSON API, a test page, and the static
sites that people deploy to it, and it stores its state in one SQLite file plus a directory
of site files. It speaks plain HTTP; terminate TLS in a reverse proxy. The API and UI live on
one host (`app_host`) and every site on its own subdomain of a separate content domain; see
[URLs and hosts](#urls-and-hosts) and the [security model](security-model.md).

## Install

Pick one.

**Build from source.** Needs Go 1.26 or newer. No C toolchain: SQLite is pure Go.

```sh
go build -o open-pages ./cmd/open-pages
./open-pages -config configs/settings.ini
```

The UI templates and assets (`web/`) are embedded in the binary, so it can run from any
working directory and needs no internet access: the PatternFly CSS and fonts are vendored in
`web/static/patternfly/` by `scripts/vendor-patternfly.sh`.

**Release archive.** Pushing a `v*` tag publishes a GitHub release with `linux/amd64` and
`linux/arm64` archives (binary, `settings.ini`, `NOTICE` and the third-party license texts in
`licenses/`) and a `SHA256SUMS` file.

**Container image.** `ghcr.io/gira0/open-pages`, tagged with the version (stable releases
also get `major.minor` and `latest`). See [Containers](#containers).

**CLI only.** The same binary deploys sites from CI; `go install
github.com/gira0/open-pages/cmd/open-pages@latest` installs it. See
[Deploying with the CLI and CI](#deploying-with-the-cli-and-ci).

## Quick start

```sh
go build -o open-pages ./cmd/open-pages
./open-pages -config configs/settings.ini &

# create an account and log in
curl -X POST app.localhost:8080/v1/user/register -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"change-me-please"}'
curl -c cookies -X POST app.localhost:8080/v1/user/login -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"change-me-please"}'

# create a site, upload a zip of your files, open it (the shipped settings serve the API on
# app.localhost and sites on <site>.sites.localhost, which needs no DNS setup)
curl -b cookies -X POST app.localhost:8080/v1/auth/sites -H 'Content-Type: application/json' \
  -d '{"name":"hello"}'
(cd ./public && zip -qr ../site.zip .)
curl -b cookies -X POST --data-binary @site.zip app.localhost:8080/v1/auth/sites/hello/upload
curl http://hello.sites.localhost:8080/
```

`http://app.localhost:8080/index` serves a small test page with the same actions.

## Command line

```text
open-pages [-config settings.ini]                       run the server
open-pages deploy [-server URL] [-create=false] [-visibility V] <site> <dir>   deploy a directory
```

`-config` defaults to `settings.ini` in the working directory. The server stops gracefully
on `SIGINT` or `SIGTERM`, giving in-flight requests up to 30 seconds. There are no
environment variables for the server; configuration is the settings file only. (The
`deploy` subcommand reads `OPEN_PAGES_TOKEN` and `OPEN_PAGES_URL`.)

## Configuration reference

The settings file is INI. A missing file is a fatal error; a missing key uses the default.
The repository's [`configs/settings.ini`](../configs/settings.ini) documents the same keys and
[`deploy/settings.ini`](../deploy/settings.ini) holds the defaults baked into the container
image. Booleans written as anything other than a boolean silently fall back to the default,
except the security-relevant ones, `auth.local_login`, `oidc.enabled` and `saml.enabled`, where a bad
value fails startup. Integers that do not parse also fall back to the default.

### [server]

| Key | Default | Meaning |
|---|---|---|
| `host` | empty (all interfaces) | Interface to bind |
| `http_port` | `8080` | Port to listen on |
| `cookie_secure` | `false` | Set the `Secure` flag on the session cookie and name it `__Host-auth_cookie`, which sibling hosts cannot overwrite. Set to `true` whenever users reach the service over HTTPS, which production requires (see [security model](security-model.md)); with `false` the server logs a startup warning |
| `cors_origins` | empty | Comma-separated origins (`scheme://host[:port]`) allowed to call the API cross-origin with credentials; empty means same-origin only. Browser POST/PUT/DELETE from any other origin gets 403 |

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
| `max_concurrent_deploys` | `2` | Uploads extracted at the same time. Further uploads get 503 with `Retry-After` |
| `deploy_timeout_seconds` | `600` | Time budget for extracting one upload; extraction also stops when the client disconnects |

`max_extract_mb` counts decompressed bytes read from the archive, including entries that
are skipped (symlinks and other special entries). A zip with more than `max_extract_files`
entries is rejected before its directory is parsed, and one that declares more than
`max_extract_mb` in total is rejected before anything is written.

Archives breaking the extract limits, or with entries outside the site directory, are
rejected with 400. Make sure a reverse proxy in front allows bodies of at least
`max_upload_mb` (nginx: `client_max_body_size`).

### [sites]

| Key | Default | Meaning |
|---|---|---|
| `keep_versions` | `5` | Versions kept per site, including the live one; older ones are deleted after each deploy and can no longer be rolled back to. Values below 1 are treated as 1 |
| `app_host` | none, required | Bare host name of the API and UI (for example `pages.corp`). Lower-cased, one trailing dot removed; no scheme, port or path. See [URLs and hosts](#urls-and-hosts) |
| `content_domain` | none, required | Bare domain whose subdomains serve the sites: a site `blog` is at `https://blog.<content_domain>/` (for example `pages-content.corp`). Same format as `app_host`. Neither `app_host` nor `content_domain` may equal the other or be a subdomain of the other, at any depth |

`url_mode` and `base_domain` no longer exist; a settings file that still sets either fails
startup with a message pointing here.

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
| `local_login` | `true` | Serve `POST /v1/user/register` and `/v1/user/login`. `false` is for single sign-on only setups and requires `[oidc] enabled = true` or `[saml] enabled = true`, otherwise startup fails. Must be `true` or `false` |

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

### [saml]

Off unless `enabled = true`. When `enabled` is false the other keys are not looked at. The
flow, IdP setup and identity rules are in [auth.md](auth.md#saml-sign-in).

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Turn SAML sign-in on. Must be `true` or `false` |
| `idp_metadata_url` | none | `https` URL of the IdP's metadata (`http` for localhost). Fetched on first use and refreshed hourly. Set exactly one of this and `idp_metadata_file` |
| `idp_metadata_file` | none | Path of the IdP's metadata XML; read at startup (a missing file fails startup) and re-read hourly |
| `entity_id` | none, required | This service's entity ID, as registered at the IdP; assertions must name it as their audience |
| `acs_url` | none, required | Absolute URL of `/v1/auth/saml/acs` on this server, as registered at the IdP. `https` only (`http` for localhost) |
| `email_attribute` | `email` | Assertion attribute holding the email address. If it is missing, a NameID in the email-address format is used |
| `groups_attribute` | `groups` | Attribute holding group names; empty turns group mapping off |
| `nameid_attribute` | empty | When set, the user is identified by this attribute instead of the NameID |
| `allowed_email_domain` | empty | Only emails in this domain may sign in; a leading `@` is ignored; empty allows any |
| `key_file`, `cert_file` | empty | PEM RSA private key (ECDSA is not supported) and certificate used to sign AuthnRequests. Set both or neither; without them requests are unsigned. The certificate is published in the SP metadata |

### Startup checks

The server refuses to start, with the reason on stderr, when: the file cannot be read;
`sites.url_mode` or `sites.base_domain` is set; `sites.app_host` or `sites.content_domain`
is missing or not a bare host name (a scheme, port, path or space in it); `content_domain`
equals `app_host`, or either one is a subdomain of the other at any depth (a site under the
app host could set cookies for the app, and an app host under the content domain would be
a site host);
`log.level` or `log.format` is invalid; `metrics.token` is shorter than 16 characters;
`local_login`, `oidc.enabled` or `saml.enabled` is not a boolean; `local_login = false` without
OIDC or SAML; OIDC is enabled with a required key missing or a non-https URL; SAML is enabled
without `entity_id` or `acs_url`, with both or neither of the metadata keys, with a non-https
URL, with only one of `key_file` and `cert_file`, or with a key, certificate or metadata
file that cannot be read; or the data directories or
database cannot be created or migrated.

## URLs and hosts

Two kinds of host, never mixed:

| Host | Serves | Example |
|---|---|---|
| `app_host` | The API, the test page and its static assets, `/healthz`, and `/metrics` when enabled. Never site content, whatever the path | `https://pages.corp/` |
| `<site>.<content_domain>` | One site, read-only (`GET` and `HEAD`; any other method is `405` with `Allow: GET, HEAD`). Nothing of the API or UI. A label that is not a valid site name is a 404 | `https://blog.pages-content.corp/` |
| anything else | `404` for every request except `GET /healthz` (see below) | |

Root-absolute links (`<link href="/css/app.css">`) work in sites, because each site owns
the whole of its host.

**Choosing the domains.** Use a separate registrable domain for `content_domain` where you can
(the way GitHub uses `github.com` and `github.io`): browsers then treat the app and the sites
as different sites, not just different origins, and a cookie scoped to the parent domain can
never reach both. Two siblings under a shared parent (`app.pages.corp` and `sites.pages.corp`) are
accepted, but then the app and the sites are same-site. That layout relies on the app's
cross-origin request check, `application/json`-only JSON routes and the `__Host-` session
cookie, so it needs `cookie_secure = true` on HTTPS (see [security model](security-model.md)), and
nothing on the parent domain may set cookies for it. A content domain under the app host, or the reverse, is refused at startup.

**DNS and TLS.** Point `app_host` and the wildcard `*.<content_domain>` at the reverse
proxy, and give the proxy a certificate for `app_host` and a wildcard certificate for
`*.<content_domain>`. A wildcard covers one label only, which is exactly the shape of a
site host. The reverse proxy must pass the original `Host` header through to open-pages
(nginx: `proxy_set_header Host $host;`), because the server decides what to serve from it.
Host matching ignores case, a port and a trailing dot.

**Health probes.** `GET /healthz` (and `HEAD`) answers on any host that is not a site host, so
probes by IP address, `localhost` or an internal name keep working. On a site host the same
path is just a file of that site. `/metrics` is served on `app_host` only (with its token),
or on the dedicated metrics listener, which has no host rule.

**Sessions on the content domain.** The session cookie is host-only, so the browser never
sends it to a site host. Until per-site sign-in exists, `authenticated` and `restricted`
sites cannot be opened in a browser; API tokens work (see [auth.md](auth.md#known-limits)).
See the [security model](security-model.md) for what this protects.

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

The baked-in settings use `app_host = app.localhost` and `content_domain = sites.localhost`, which
only suit trying the image out on one machine. For a real deployment mount your own file
with your domains (and `cookie_secure = true` behind HTTPS).

To change settings, mount your own file over the baked-in one and keep `datapath` and
`tmppath` under `/data`:

```sh
docker run -p 8080:8080 -v open-pages-data:/data \
  -v "$PWD/my-settings.ini":/etc/open-pages/settings.ini:ro open-pages
```

A named volume is writable by uid 65532 out of the box. A bind-mounted data directory must
be `chown`ed to `65532` first. There are no environment variables for configuration.

`deploy/docker-compose.yml` is an example: it builds the image locally (use
`image: ghcr.io/gira0/open-pages:<version>` for a release), publishes port 8080, mounts the
`open-pages-data` volume at `/data`, and runs with `read_only: true`, `cap_drop: [ALL]` and
`no-new-privileges`. An optional commented line shows the settings override.

```sh
docker compose -f deploy/docker-compose.yml up --build
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
  turns that off. A site created this way has no group and is `public` unless
  `-visibility` says otherwise.
- `-visibility public|authenticated|restricted` creates the site with that visibility or
  changes it (with `PUT /v1/auth/sites/<site>`). A change that narrows the audience (say
  `public` to `restricted`) is made before the upload; one that widens it is made only
  after the upload succeeded, so a failed deploy never exposes the old version more widely.
  These are separate requests, so concurrent deploys to one site with different
  `-visibility` values can still interleave. Without the flag the visibility is left as it
  is.
  See [auth.md](auth.md#site-visibility) for what each value means.
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
