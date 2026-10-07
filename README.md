# open-pages

A self-hosted, GitHub Pages-like service for publishing static sites on internal networks.

> Status: early prototype. Users can register, log in and upload a site archive, which is
> extracted on the server. Serving the uploaded sites is the next milestone.

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
| GET | `/v1/auth/user` | ✓ | Current user id |
| POST | `/v1/auth/logout` | ✓ | End the session |
| POST | `/v1/auth/sites` | ✓ | Create a site (`name`, `description`); the name must be a DNS label |
| POST | `/v1/auth/sites/{name}/upload` | ✓ | Deploy a `.zip`, `.tar.gz` or `.tar` (raw body) as a new version; owner only |
| POST | `/v1/auth/sites/{name}/formupload` | ✓ | Same, with the archive in the multipart field `file` |

Each upload is extracted into `op_data/<site>/versions/<id>/` and the
`op_data/<site>/current` symlink is switched to it with an atomic rename, so a site is never
half-deployed. The newest `keep_versions` versions (default 5, `[sites]` in `settings.ini`)
are kept; older ones are deleted.

Uploads are limited in size, file count and uncompressed size (see `[limits]` in
`settings.ini`). Entries that would land outside the site directory are rejected.

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
