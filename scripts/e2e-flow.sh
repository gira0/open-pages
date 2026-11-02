#!/usr/bin/env bash
# Drives a running open-pages through the basic flow over HTTP: register, log in, create a
# site, upload a zip, and fetch the site on its own host. Used by scripts/e2e-kind.sh and
# usable against any instance whose requests reach the app with the right Host header.
#
#   scripts/e2e-flow.sh deploy|check
#
# deploy: create the account, log in, create the site "hello" and upload a page.
# check:  log in again (the account must exist) and fetch the page.
#
# Environment: E2E_URL (default http://127.0.0.1:18080), E2E_APP_HOST, E2E_CONTENT_DOMAIN
# (both required), E2E_DIR (scratch directory, default a new temp dir).
set -euo pipefail

url="${E2E_URL:-http://127.0.0.1:18080}"
app_host="${E2E_APP_HOST:?set E2E_APP_HOST}"
content_domain="${E2E_CONTENT_DOMAIN:?set E2E_CONTENT_DOMAIN}"
dir="${E2E_DIR:-$(mktemp -d)}"
cookies="$dir/cookies"
email="e2e@example.com"
password="e2e-password-0123456789"
marker="e2e-ok-$(cat "$dir/marker" 2>/dev/null || echo unset)"

api() { # api METHOD PATH [curl args...]
  local method="$1" path="$2"
  shift 2
  curl -fsS -X "$method" -H "Host: $app_host" -b "$cookies" -c "$cookies" "$url$path" "$@"
}

login() {
  api POST /v1/user/login -H 'Content-Type: application/json' \
    -d "{\"email\":\"$email\",\"password\":\"$password\"}" >/dev/null
}

fetch_site() {
  curl -fsS -H "Host: hello.$content_domain" "$url/"
}

case "${1:-}" in
deploy)
  mkdir -p "$dir"
  date +%s%N >"$dir/marker"
  marker="e2e-ok-$(cat "$dir/marker")"
  api POST /v1/user/register -H 'Content-Type: application/json' \
    -d "{\"email\":\"$email\",\"password\":\"$password\"}" >/dev/null
  login
  api POST /v1/auth/sites -H 'Content-Type: application/json' -d '{"name":"hello"}' >/dev/null
  python3 - "$dir/site.zip" "$marker" <<'PY'
import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "w") as z:
    z.writestr("index.html", "<h1>" + sys.argv[2] + "</h1>\n")
PY
  api POST /v1/auth/sites/hello/upload --data-binary "@$dir/site.zip" >/dev/null
  fetch_site | grep -q "$marker"
  echo "deployed and served: $marker"
  ;;
check)
  login
  fetch_site | grep -q "$marker"
  api GET /v1/auth/sites/hello | grep -q '"name":"hello"'
  echo "site and account survived: $marker"
  ;;
*)
  echo "usage: $0 deploy|check" >&2
  exit 2
  ;;
esac
