# open-pages

A self-hosted, GitHub Pages-like service for publishing static sites on internal networks.
People log in (local accounts or the company identity provider over OIDC), create a site,
and upload a `.zip`, `.tar.gz` or `.tar` archive, from a browser, `curl` or CI. Each upload
becomes a new version that goes live atomically and can be rolled back. A site is public,
visible to any logged-in user, or restricted to its owner and a group.

> Status: pre-release. The documentation describes what exists today, including known limits.

## Quick start

Needs Go 1.26 or newer; there is no C toolchain requirement (SQLite is pure Go).

```sh
go build -o open-pages .
./open-pages -config settings.ini     # run from the directory that contains templates/
```

Then open http://localhost:8080/index, or use the API:

```sh
curl -X POST localhost:8080/v1/user/register -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"change-me-please"}'
curl -c cookies -X POST localhost:8080/v1/user/login -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"change-me-please"}'
curl -b cookies -X POST localhost:8080/v1/auth/sites -H 'Content-Type: application/json' \
  -d '{"name":"hello"}'
curl -b cookies -X POST --data-binary @site.zip localhost:8080/v1/auth/sites/hello/upload
curl localhost:8080/hello/
```

As a container:

```sh
docker compose up --build        # or: docker run -p 8080:8080 -v open-pages-data:/data open-pages
```

Deploy a directory from CI with the same binary:

```sh
OPEN_PAGES_TOKEN=opt_... open-pages deploy -server https://pages.corp blog ./public
```

## Documentation

All reference material is in [`docs/`](docs/README.md):

- [Getting started and operations](docs/getting-started.md): install, every settings key,
  URL modes, containers, health and metrics, CLI and CI deploys, backups.
- [API reference](docs/api.md): every route with auth, request and response shapes, status
  codes and examples.
- [Access control and authentication](docs/auth.md): accounts, sessions, API tokens, OIDC,
  groups, site visibility, known limits.
- [Database schema](docs/schema.md): tables, columns, indexes, migrations.

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
