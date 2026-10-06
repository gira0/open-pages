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
| POST | `/v1/auth/docs/create` | ✓ | Create a doc record (`name`, `description`) |
| POST | `/v1/auth/docs/upload` | ✓ | Upload a `.zip`, `.tar.gz` or `.tar` as the raw body |
| POST | `/v1/auth/docs/formupload` | ✓ | Upload an archive in the multipart field `file` |

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
summary and as an artifact), and govulncheck. Dependabot opens weekly grouped updates for
Go modules and GitHub Actions.
