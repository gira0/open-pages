#!/usr/bin/env bash
# End-to-end test of the Helm chart on a kind cluster. Expects a kind cluster that already
# holds the image docker.io/library/open-pages:e2e (CI builds it and runs "kind load"), and
# kubectl, helm, curl and python3 on the PATH.
#
# It installs the chart into a namespace that enforces the Pod Security "restricted" level,
# runs the pod as an arbitrary UID with group 0 (as OpenShift's restricted-v2 SCC does),
# deploys a site through the API and fetches it on its wildcard host, restarts the pod and
# checks that the data survived and that the old pod shut down cleanly.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
ns=open-pages-e2e
release=open-pages
export E2E_APP_HOST=pages.e2e.test
export E2E_CONTENT_DOMAIN=sites.e2e.test
export E2E_URL=http://127.0.0.1:18080
E2E_DIR="$(mktemp -d)"
export E2E_DIR
uid=1000780000

pf_pid=""
log_pid=""
cleanup() {
  if [ -n "$pf_pid" ]; then kill "$pf_pid" 2>/dev/null || true; fi
  if [ -n "$log_pid" ]; then kill "$log_pid" 2>/dev/null || true; fi
}
trap cleanup EXIT

dump() {
  echo "::group::cluster state"
  kubectl -n "$ns" get all,pvc,events -o wide || true
  kubectl -n "$ns" describe pods || true
  kubectl -n "$ns" logs -l app.kubernetes.io/name=open-pages --tail=100 || true
  echo "::endgroup::"
}
trap dump ERR

port_forward() {
  if [ -n "$pf_pid" ]; then kill "$pf_pid" 2>/dev/null || true; fi
  kubectl -n "$ns" port-forward "deploy/$release" 18080:8080 >"$E2E_DIR/pf.log" 2>&1 &
  pf_pid=$!
  for _ in $(seq 1 30); do
    if curl -fsS -H "Host: $E2E_APP_HOST" "$E2E_URL/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "port-forward never became ready" >&2
  cat "$E2E_DIR/pf.log" >&2
  return 1
}

kubectl create namespace "$ns"
kubectl label namespace "$ns" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=latest

# runAsUser/runAsGroup mimic what restricted-v2 injects. fsGroup stays unset, as on OpenShift.
helm install "$release" "$here/../deploy/helm/open-pages" --namespace "$ns" \
  --set image.registry=docker.io --set image.repository=library/open-pages \
  --set image.tag=e2e --set image.pullPolicy=Never \
  --set config.appHost="$E2E_APP_HOST" --set config.contentDomain="$E2E_CONTENT_DOMAIN" \
  --set config.cookieSecure=false \
  --set podSecurityContext.runAsUser="$uid" --set podSecurityContext.runAsGroup=0 \
  --wait --timeout 4m

got_uid="$(kubectl -n "$ns" get pod -l app.kubernetes.io/name=open-pages \
  -o jsonpath='{.items[0].spec.securityContext.runAsUser}')"
if [ "$got_uid" != "$uid" ]; then
  echo "pod runs as UID '$got_uid', want $uid" >&2
  exit 1
fi

port_forward
"$here/e2e-flow.sh" deploy

# Follow the old pod's log so its shutdown messages are kept after it is gone.
old_pod="$(kubectl -n "$ns" get pod -l app.kubernetes.io/name=open-pages -o jsonpath='{.items[0].metadata.name}')"
kubectl -n "$ns" logs -f "$old_pod" >"$E2E_DIR/old.log" 2>&1 &
log_pid=$!

# Recreate strategy: the old pod stops (SIGTERM, graceful shutdown) before the new one starts
# on the same volume.
kubectl -n "$ns" rollout restart "deploy/$release"
kubectl -n "$ns" rollout status "deploy/$release" --timeout=3m
kubectl -n "$ns" wait --for=delete "pod/$old_pod" --timeout=2m || true
wait "$log_pid" 2>/dev/null || true
log_pid=""

if ! grep -q 'shutting down' "$E2E_DIR/old.log"; then
  echo "old pod did not log a graceful shutdown" >&2
  cat "$E2E_DIR/old.log" >&2
  exit 1
fi
if grep -q 'level=ERROR' "$E2E_DIR/old.log"; then
  echo "old pod logged errors" >&2
  cat "$E2E_DIR/old.log" >&2
  exit 1
fi

port_forward
"$here/e2e-flow.sh" check

kubectl -n "$ns" get pvc
echo "kind e2e passed"
