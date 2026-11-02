# Running on OpenShift

open-pages runs under OpenShift's `restricted-v2` SecurityContextConstraint (SCC) with no
extra grants: no custom SCC, no `anyuid`, no `privileged`. This page covers what that means
and the OpenShift specific steps. The generic chart documentation, values and DNS/TLS
background are in [kubernetes.md](kubernetes.md); read it first.

## Why `restricted-v2` is enough

`restricted-v2` makes the pod run with:

- a UID chosen by OpenShift from the namespace's range (the annotation
  `openshift.io/sa.scc.uid-range`, for example `1000780000/10000`). Any `runAsUser` in the
  pod spec that falls outside that range is rejected, so the chart never sets one;
- GID `0` (root group) as the primary or a supplementary group, and an `fsGroup` taken from
  the namespace's `supplemental-groups` range if the chart sets none;
- `runAsNonRoot`, no privilege escalation, all capabilities dropped, a `RuntimeDefault`
  seccomp profile, and no host paths, host ports or privileged containers.

The chart and the image are built for exactly that:

- **Arbitrary UID, group 0.** The image has no `/etc/passwd` entry to look up. `/data` is
  owned by `65532:0` with mode `2775` (setgid, group write), and the app creates files with
  umask `002`, so whatever UID OpenShift assigns can write the volume as long as it has GID
  `0`, which `restricted-v2` guarantees. Old volumes written with other modes are repaired
  at startup (`groupWritableDirs`).
- **Read-only root file system.** The only writable paths are the data volume and the staging
  `emptyDir`.
- **No `runAsUser`, no `fsGroup` by default.** Leave `podSecurityContext.fsGroup` empty on
  OpenShift: the SCC assigns one, and a fixed value outside the allowed range is rejected.
- **No service account token** is mounted (`automountServiceAccountToken: false`); the app
  does not talk to the Kubernetes API.

You can check what the pod got:

```sh
oc -n open-pages get pod -l app.kubernetes.io/name=open-pages \
  -o jsonpath='{.items[0].metadata.annotations.openshift\.io/scc}{"\n"}'   # restricted-v2
oc -n open-pages get pod -l app.kubernetes.io/name=open-pages \
  -o jsonpath='{.items[0].spec.securityContext}{"\n"}'
```

(The image has no shell, so `oc rsh` and `oc exec ... sh` do not work; use `oc debug` with
another image against a copy of the pod spec when you need a shell.)

## Install

```sh
oc new-project open-pages
helm install open-pages deploy/helm/open-pages -n open-pages \
  --set config.appHost=pages.apps.example.com \
  --set config.contentDomain=pages-content.example.com \
  --set route.enabled=true \
  --set config.trustedProxies='{10.128.0.0/14}'
```

- `route.enabled=true` creates a Route for `appHost` and a wildcard Route
  (`wildcardPolicy: Subdomain`) for `*.<contentDomain>`, both with edge TLS and an HTTP
  redirect. Ingress objects are not needed.
- `config.trustedProxies` lists the CIDRs of the routers, so `X-Forwarded-Proto` and
  `X-Forwarded-For` are believed. Use the pod network CIDR your routers run in. Without it,
  the app sees plain HTTP from the router and the `Secure`/`__Host-` cookies and CSRF
  origin checks misbehave.
- The PVC uses the cluster's default StorageClass. It must be ReadWriteOnce block storage;
  do not put the data on NFS or another RWX backend (see
  [kubernetes.md](kubernetes.md#one-replica-one-rwo-volume-recreate)).

## The wildcard Route

The router admits wildcard Routes only if the IngressController allows them:

```sh
oc -n openshift-ingress-operator patch ingresscontroller/default --type=merge \
  -p '{"spec":{"routeAdmission":{"wildcardPolicy":"WildcardsAllowed"}}}'
oc -n open-pages get route
```

If a Route shows `Admitted: False`, read the reason with `oc get route <name> -o yaml`
(`status.ingress[].conditions`).

DNS: point `*.<contentDomain>` and `<appHost>` at the router (the `*.apps` wildcard already
does this for names under the cluster's apps domain). For a content domain outside the
cluster domain, create a wildcard A/CNAME record to the router's load balancer.

## TLS certificates

An edge Route without a certificate is served with the router's default certificate. That
certificate covers `*.apps.<cluster-domain>` one label deep only:

| Hosts | Default certificate |
|---|---|
| `pages.apps.example.com` | covered |
| `blog.pages-content.apps.example.com` (two labels) | **not covered** |
| a content domain outside the cluster domain | not covered |

So for sites, either use a content domain the default certificate matches, or serve the
content domain with its own certificate. The chart's Routes carry no certificate fields, so
the usual way is a dedicated IngressController whose `defaultCertificate` is a wildcard
certificate for `*.<contentDomain>` (a cert-manager `Certificate` with a DNS-01 issuer can
issue it, since HTTP-01 cannot issue wildcards), with the Routes admitted by that controller
through a route label selector. Patching `spec.tls` on the wildcard Route by hand also works
but the next `helm upgrade` resets it. The security model asks for the content domain to be a separate registrable domain
from the app host, which a name under the cluster domain is not.

## Monitoring

`serviceMonitor.enabled=true` adds a ServiceMonitor for OpenShift user-workload monitoring.
Enable it once for the cluster (`enableUserWorkload: true` in the
`cluster-monitoring-config` ConfigMap), label the namespace if your cluster requires it, and
keep the default `networkPolicy.metricsIngressFrom`, which allows the
`openshift-user-workload-monitoring` namespace.

## NetworkPolicy

`networkPolicy.enabled=true` is built for OpenShift: ingress from the routers
(`network.openshift.io/policy-group: ingress`) on the app port, from user-workload
monitoring on the metrics port, and egress to DNS and, when OIDC or SAML is on, the identity
provider. Plain NetworkPolicy has no host names, so restrict egress to the IdP with an
`EgressFirewall` (OVN-Kubernetes), or list the provider's CIDRs in
`networkPolicy.egress.allowedCIDRs`.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `unable to validate against any security context constraint` | A value sets `runAsUser`, `runAsGroup` or `fsGroup` outside the namespace range. Remove it. |
| `permission denied` creating files under `/data` | An old volume or a different image runs without GID `0`. Check the pod's `supplementalGroups`/`fsGroup`, and that the data directory is group `0`. |
| Wildcard Route `Admitted: False` | The IngressController does not allow wildcard Routes (above), or another Route claims the host. |
| Certificate errors on `<site>.<contentDomain>` | The default router certificate only covers one label under the apps domain (above). |
| Login works on the app host but not on a site | The app host and every site host must reach the same backend; check both Routes point at the same Service. |
| Pod stays `Pending` | No default StorageClass, or its volumes are not ReadWriteOnce block storage. |
