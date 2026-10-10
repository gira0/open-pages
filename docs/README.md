# open-pages documentation

open-pages is a self-hosted service for publishing static sites on an internal network. It
is a pre-release project: these pages describe what the code does today, including its
known limits.

| Page | Read it to |
|---|---|
| [getting-started.md](getting-started.md) | Install, configure (every settings key), run in a container, monitor, deploy from the CLI or CI, back up |
| [api.md](api.md) | Look up a route: method, auth, request and response, status codes, `curl` example |
| [auth.md](auth.md) | Understand local accounts, sessions, API tokens, OIDC sign-in, groups and per-site visibility |
| [schema.md](schema.md) | See every database table and column, the indexes, and how migrations work |

Other files in the repository:

- [`settings.ini`](../settings.ini): commented example configuration.
- [`deploy/settings.ini`](../deploy/settings.ini): defaults baked into the container image.
- [`examples/`](../examples/): GitHub Actions and GitLab CI deploy pipelines.
- [`SECURITY.md`](../SECURITY.md): supported versions and vulnerability reporting.

Where to start:

- Running a server: [getting-started.md](getting-started.md), then
  [auth.md](auth.md#known-limits) for what to expect from non-public sites.
- Deploying a site: [getting-started.md](getting-started.md#deploying-with-the-cli-and-ci).
- Writing a client or script: [api.md](api.md).
- Changing the code: [schema.md](schema.md#migrations) for the schema rules, and the
  repository README for the development checks.
