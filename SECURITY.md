# Security policy

## Supported versions

open-pages is an early prototype, not a production-ready service. Security fixes
target the latest code on `main`; older commits and releases are not supported.
Do not expose the prototype to untrusted networks or use it for sensitive data.

## Security model

How hosted content is isolated from the application (separate content domain, one origin
per site, host-only session cookie) and what a deployment must provide is described in
[docs/security-model.md](docs/security-model.md).

## Reporting a vulnerability

Use GitHub's private vulnerability reporting:
https://github.com/gira0/open-pages/security/advisories/new

Include the affected commit, reproduction steps, impact, and any proposed fix.
Do not publish exploit details or credentials in a public issue. There is no
guaranteed response time while the project is in early development.
