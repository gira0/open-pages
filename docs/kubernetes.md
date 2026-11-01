# Running on Kubernetes and OpenShift

The Helm chart in [`deploy/helm/open-pages/`](../deploy/helm/open-pages/) runs open-pages as
one pod with a persistent volume. It meets the Kubernetes `restricted` Pod Security Standard
and OpenShift's `restricted-v2` SCC without extra grants: non-root, any UID with group 0,
no privilege escalation, all capabilities dropped, `RuntimeDefault` seccomp, read-only root
filesystem, no `hostPath`, no host ports, no init containers. `runAsUser` is not set, so
OpenShift can assign one from the namespace range. See
[Container image](getting-started.md#container-image) for how the image supports that.

Every value is commented in [`values.yaml`](../deploy/helm/open-pages/values.yaml) and checked
by `values.schema.json`.

## Install

You need a cluster with a default StorageClass for block storage, an ingress controller or an
OpenShift router, and two DNS names (below). There is no published chart repository yet, so
install from a checkout:

```sh
helm install open-pages deploy/helm/open-pages \
  --namespace open-pages --create-namespace \
  --set config.appHost=pages.corp \
  --set config.contentDomain=pages-content.corp \
  --set ingress.enabled=true \
  --set ingress.tls.appSecretName=pages-tls \
  --set ingress.tls.wildcardSecretName=pages-content-wildcard-tls
```

`config.appHost` and `config.contentDomain` are required; rendering fails with a message
naming the missing value. The chart does not create DNS records, certificates or the first
user: register through the API or UI afterwards (see [auth.md](auth.md)). The image tag
defaults to the chart's `appVersion`; set `image.tag`, or `image.digest` (which wins) to pin
`sha256:...`. **Until open-pages v0.1.0 is released, `ghcr.io/gira0/open-pages:0.1.0` does not
exist**, so a default install ends in `ImagePullBackOff`: set `image.tag` (or `image.digest`)
to an image you have built or that exists. For an air-gapped mirror set `global.imageRegistry`, and `imagePullSecrets`.

## One replica, one RWO volume, Recreate

The chart always runs one replica and refuses anything else (`replicaCount` must be 1, in the
schema and in the templates). The state is a SQLite database and the site files on one volume.
SQLite relies on file locking and shared memory that behave correctly only on a local
filesystem or block device, so:

- The PVC must be `ReadWriteOnce` (the chart refuses an access mode list without it). Do not
  put it on NFS, CephFS or another ReadWriteMany filesystem.
- A second pod writing the same database would corrupt it, so there is no HPA and the
  Deployment strategy is `Recreate`: the old pod stops and releases the volume before the new
  one starts. An upgrade therefore has a short outage.
- No `PodDisruptionBudget` is shipped. With one replica a budget with `minAvailable: 1` blocks
  every node drain, and `maxUnavailable: 1` protects nothing. Plan node maintenance as a
  restart.

Back up the volume as described in [Backups](getting-started.md#backups) (a `VolumeSnapshot`
works on CSI drivers that support it). Use `persistence.existingClaim` to reuse a claim.

Uploads are staged on a separate `emptyDir` (`stagingVolume`, 1Gi by default, mounted at
`/staging`): `[paths] tmppath` and `TMPDIR` point there, so a large upload does not fill the
PVC. Set `stagingVolume.sizeLimit` above `config.limits.maxUploadMb` times
`config.limits.maxConcurrentDeploys`. Disable it to stage on the PVC.

## Volume ownership: fsGroup

The image keeps `/data` writable for group 0, and the pod keeps group 0.

- On OpenShift leave `podSecurityContext.fsGroup` empty. `restricted-v2` assigns the UID and
  `fsGroup` itself and the volume becomes writable.
- On plain Kubernetes many CSI drivers create a root-owned volume that the pod cannot write.
  If the pod fails with permission errors on `/data`, set
  `--set podSecurityContext.fsGroup=65532`. The chart then also sets
  `fsGroupChangePolicy: OnRootMismatch`. It is empty by default because a fixed `fsGroup` is
  rejected by `restricted-v2`.

## Probes, shutdown and resources

Liveness uses `/livez`, which does no I/O. Readiness uses `/readyz` (database answers and the
data directories accept files). A `startupProbe` on `/readyz` is on by default
(`probes.startup.enabled`, 5 s x 60). At start the app walks the whole data volume to repair
group permissions before it listens, which can take minutes with many sites on slow storage;
without the startup probe, liveness would kill the pod after about 30 seconds. On shutdown the app fails `/readyz`, waits `config.drainDelay`
(5 seconds), then gives requests `config.shutdownTimeout` (30) to finish.
`terminationGracePeriodSeconds` (60) must be larger than the sum, which the chart checks.

Default requests are 100m CPU and 256Mi memory with a 768Mi memory limit; zip extraction can
use several hundred MiB. Use `extraEnv` to add `GOMEMLIMIT` if you want the Go runtime to stay
below the limit.

## DNS and TLS

The security model needs two names, on separate registrable domains where you can
([security-model.md](security-model.md)):

| Name | Serves |
|---|---|
| `config.appHost` (`pages.corp`) | API, UI, probes |
| `*.` + `config.contentDomain` (`*.pages-content.corp`) | one host per site |

Site hosts are exactly one label under the content domain (`blog.pages-content.corp`), so a
wildcard Ingress host, Route or certificate for `*.<contentDomain>` covers them. It does not
cover a deeper name such as `a.b.pages-content.corp`; site names are single labels, so the app
never serves one.

Create a DNS A/CNAME record for `appHost` and a **wildcard** record `*.<contentDomain>` to the
ingress controller or router address. Both hosts must reach the same Service, which the
chart's Ingress, Routes and HTTPRoute all do (the sign-in handoff for non-public sites needs
it). The TLS terminates at the edge, so `config.cookieSecure` stays `true`.

**Certificates.** Sites need a wildcard certificate for `*.<contentDomain>`. With cert-manager
use an issuer with a **DNS-01** solver: HTTP-01 cannot issue wildcard certificates. Create a
`Certificate` for `*.pages-content.corp` into the Secret named in
`ingress.tls.wildcardSecretName`, and another for `pages.corp` (HTTP-01 is fine there) into
`ingress.tls.appSecretName`. Or supply pre-provisioned certificates in those Secrets.

**Forwarded headers.** Set `config.trustedProxies` to the CIDRs of the ingress controller or
router pods, so `X-Forwarded-For` and `X-Forwarded-Proto` are believed. Without it client
addresses in logs are the proxy's.

**Upload size and timeouts.** The edge must accept the largest upload and keep the connection
open for it. For ingress-nginx set `nginx.ingress.kubernetes.io/proxy-body-size` to at least
`config.limits.maxUploadMb` (for example `100m`) and `proxy-read-timeout`/`proxy-send-timeout`
(seconds, for example `"600"`) in `ingress.annotations`. The app itself sets no write timeout.
On OpenShift the Route annotation `haproxy.router.openshift.io/timeout` comes from
`route.timeout` (default `10m`).

### OpenShift

```sh
helm install open-pages deploy/helm/open-pages -n open-pages \
  --set config.appHost=pages.apps.example.com \
  --set config.contentDomain=pages-content.apps.example.com \
  --set route.enabled=true
```

`route.enabled` creates a Route for `appHost` and a wildcard Route (`wildcardPolicy:
Subdomain`, host `wildcard.<contentDomain>`), both with edge TLS and redirect from HTTP. The
router only admits wildcard routes if the IngressController has
`ROUTER_ALLOW_WILDCARD_ROUTES=true`:

```sh
oc -n openshift-ingress-operator patch ingresscontroller/default --type=merge \
  -p '{"spec":{"routeAdmission":{"wildcardPolicy":"WildcardsAllowed"}}}'
```

Without a Route certificate the router serves its default certificate, which on a standard
cluster covers `*.apps.<cluster-domain>` only. Names directly under that domain, such as
`pages.apps.example.com`, work. A content domain such as `pages-content.apps.example.com`
puts sites two labels deep (`blog.pages-content.apps.example.com`), which the default
wildcard certificate does not cover: use a domain the default certificate matches, or a
custom IngressController (and certificate) for the content domain. The security model also
asks for separate registrable domains, which a name under the cluster domain is not.

### Gateway API

`httpRoute.enabled` renders an `HTTPRoute` with both hostnames; set `httpRoute.parentRefs`
to your Gateway and make its listener accept both names.

## Secrets

OIDC client secret, metrics token and the SAML signing key are never written to the ConfigMap.
They are mounted as files under `/etc/open-pages/secrets` and handed to the app with
`OPEN_PAGES_OIDC_CLIENT_SECRET_FILE`, `OPEN_PAGES_METRICS_TOKEN_FILE`, and
`OPEN_PAGES_SAML_KEY_FILE` / `OPEN_PAGES_SAML_CERT_FILE` (for SAML the setting itself is a
path, see [Environment variables](getting-started.md#environment-variables)). Two ways:

- Set `secrets.oidcClientSecret`, `secrets.metricsToken`, `secrets.samlKey`, `secrets.samlCert`
  and the chart creates a Secret. These values are kept in Helm's release records, so use
  this for trials.
- Set `secrets.existingSecret` to a Secret you manage with External Secrets, Sealed Secrets or
  Vault. Key names default to `oidc-client-secret`, `metrics-token`, `saml.key` and
  `saml.crt`; override them in `secrets.keys`. The chart cannot see changes inside such a
  Secret: restart the Deployment (`kubectl rollout restart`) after rotating it.

OIDC and SAML are turned on with `config.oidc.enabled` and `config.saml.enabled`; see
[auth.md](auth.md) for the provider side and the redirect URLs. The chart only supports SAML
metadata from a URL (`config.saml.idpMetadataUrl`); the app's `idp_metadata_file` setting is
not wired up because the chart has no way to mount extra files. For an air-gapped cluster,
serve the metadata from an internal HTTPS URL.

Values written into `settings.ini` must not contain line breaks, a leading `#` or `;`, or a
space followed by `#` or `;` (the ini parser would treat them as comments or new keys). The
values schema rejects them.

## Metrics

Set `config.metrics.listen` to `:9100` to serve `/metrics` on its own port. The chart then
adds a ClusterIP Service `<release>-open-pages-metrics`, which the app Service and Ingress do
not expose. `serviceMonitor.enabled` adds a ServiceMonitor (Prometheus Operator, OpenShift
user-workload monitoring). With `config.metrics.tokenFromSecret` the listener also requires a
bearer token and the ServiceMonitor sends it.

## NetworkPolicy

`networkPolicy.enabled` creates a policy that denies everything else:

- Ingress to the app port from `networkPolicy.appIngressFrom`. The default selects namespaces
  labelled `network.openshift.io/policy-group: ingress` (the OpenShift routers); for
  ingress-nginx use its namespace label instead. Metrics port from `metricsIngressFrom`.
- Egress to DNS (ports 53 and 5353), and to `egress.allowedCIDRs` on `egress.allowedPorts`
  (443) for OIDC and SAML identity providers. NetworkPolicy has no FQDN rules, so if the
  provider's address changes, use a wide CIDR or an OpenShift `EgressFirewall` or a
  `CiliumNetworkPolicy` instead and leave the list empty.

**The defaults only fit OpenShift.** `appIngressFrom` selects the OpenShift router namespaces
and `metricsIngressFrom` selects `openshift-user-workload-monitoring`. On plain Kubernetes,
enabling the policy without overriding them cuts off your ingress controller: set
`networkPolicy.appIngressFrom` (and `metricsIngressFrom`) first.

It needs a CNI that enforces NetworkPolicy and is off by default.

## Upgrade and uninstall

`helm upgrade` with the same values. The pod rolls whenever the ConfigMap or the chart-created
Secret changes (checksum annotations) and the Recreate strategy gives a brief outage. The PVC
is deleted with the release unless it is an `existingClaim`; keep a backup first.

## Checks

CI runs `helm lint --strict`, renders the files in `deploy/helm/open-pages/ci/`, and validates
the output with `kubeconform -strict`, using the datree CRD catalog for Route, ServiceMonitor
and HTTPRoute. It also renders each file's `settings.ini` and loads it with the app's own
config loader (`TestChartSettings`), so a renamed key or a changed value format fails CI.
