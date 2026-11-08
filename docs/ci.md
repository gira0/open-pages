# Continuous integration

Workflows live in `.github/workflows/`. All actions are pinned by commit SHA, and the tools
the jobs install are pinned to exact versions.

## Jobs in `ci.yml`

| Job | Checks |
|---|---|
| `lint` | `go mod tidy`, golangci-lint (gofmt, goimports, vet, staticcheck, gosec) |
| `test` | `go test -race` with a 70% coverage floor |
| `vulncheck` | govulncheck (pinned) |
| `dependency-review` | New vulnerable dependencies in a PR |
| `workflow-lint` | actionlint (with shellcheck on inline `run:` blocks) and shellcheck on `scripts/*.sh` |
| `dockerfile-lint` | hadolint on the Dockerfile |
| `config-scan` | Trivy misconfiguration scan of the Dockerfile, compose file and the rendered Helm chart |
| `openapi` | Redocly lint of `api/openapi.yaml` |
| `image` | Image build, Trivy vulnerability scan, read-only smoke test |
| `helm` | Chart lint, render, kubeconform, settings load test |
| `kind-e2e` | Install the chart on a kind cluster and run the end-to-end flow |
| `s3-conformance` | S3 backend tests against Garage |

## Required checks (branch protection)

The `protect-default` ruleset is changed in the repository settings, not by a workflow. It
should require: `lint`, `test`, `vulncheck`, `dependency-review`, both CodeQL `Analyze` jobs,
and, added with the jobs above, `workflow-lint`, `dockerfile-lint`, `config-scan`, `openapi`,
`image`, `helm`, `kind-e2e` and `s3-conformance`.

## Trivy image scan behavior

The Trivy image scan blocks on `main`, tags, and pull requests that change the Dockerfile,
`go.mod`/`go.sum` or the workflows. On other pull requests it only prints findings, so a new
upstream CVE cannot turn unrelated PRs red. A CVE in the image therefore never blocks an
ordinary PR even when `image` is required; it surfaces on the next push to `main` and in the
weekly scheduled run.
