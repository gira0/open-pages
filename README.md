# open-pages

A self-hosted, GitHub Pages-like service for publishing static sites on internal networks.

> Status: early prototype. Users can register, log in, upload a site archive and have it
> served. Per-site access control is not in yet: every deployed site is public.

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

## API

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/v1/ping` | | Health check |
| POST | `/v1/user/register` | | Create a user (JSON or form: `email`, `password`) |
| POST | `/v1/user/login` | | Log in and receive a session cookie |
| GET | `/v1/auth/user` | ✓ | Current user: account data, groups, owned and viewable docs |
| POST | `/v1/auth/logout` | ✓ | End the session |
| POST | `/v1/auth/sites` | ✓ | Create a site (`name`, optional `description` and `group` id); the name must be a DNS label |
| PUT | `/v1/auth/sites/{name}` | ✓ | Change `description` and/or `group` (JSON, absent fields are kept, `"group": 0` clears it); owner only |
| DELETE | `/v1/auth/sites/{name}` | ✓ | Delete the site, all its versions and its database row; owner only |
| POST | `/v1/auth/sites/{name}/upload` | ✓ | Deploy a `.zip`, `.tar.gz` or `.tar` (raw body) as a new version; owner only |
| POST | `/v1/auth/sites/{name}/formupload` | ✓ | Same, with the archive in the multipart field `file` |
| GET | `/v1/auth/sites/{name}/versions` | ✓ | List kept versions (newest first) and the `current` one; owner only |
| POST | `/v1/auth/sites/{name}/rollback` | ✓ | Make a kept version live: `{"version": "<id>"}`; owner only |

Whoever creates a site owns it. Only the owner can update, delete, redeploy, list versions
of or roll back a site: other users get 403, unknown sites 404. A `group` must be the id of
an existing group, otherwise the request fails with 400.

Redeploying is just uploading again: each upload becomes a new version and goes live. Roll
back with the version id from the upload response or the versions list. Versions older
than `keep_versions` are deleted and can no longer be rolled back to.

Each upload is extracted into `op_data/<site>/versions/<id>/` and the
`op_data/<site>/current` symlink is switched to it with an atomic rename, so a site is never
half-deployed. The newest `keep_versions` versions (default 5, `[sites]` in `settings.ini`)
are kept; older ones are deleted.

Uploads are limited in size, file count and uncompressed size (see `[limits]` in
`settings.ini`). Entries that would land outside the site directory are rejected.

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
