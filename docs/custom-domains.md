# Custom domains

A site is normally served at `https://<site>.<content_domain>/`. A **custom domain** gives a
site a host name of its own as well, for example `https://handbook.corp.example/`, so the
address can be short, memorable and stable even if the site is renamed or moved.

open-pages does **not** terminate TLS for custom domains and does not talk to a certificate
authority. A reverse proxy in front of it (nginx, Traefik, an Ingress controller, the
OpenShift router) holds the certificate, routes the host to open-pages and passes the `Host`
header through unchanged. open-pages maps that `Host` header to a site. This is the same
division of labour as for `app_host` and `*.<content_domain>` (see
[URLs and hosts](getting-started.md#urls-and-hosts)), so there is nothing new to operate
except one route and one certificate per domain.

The feature is **off by default**.

## How it works

1. An operator enables it and lists the domains users may use (`custom_domain_suffixes`).
2. The owner of a site attaches a domain to it, with the API, `open-pages domain` or the
   *Custom domains* section of the manage page.
3. DNS for that name points at the reverse proxy, which has a certificate for it and routes
   it to open-pages.
4. A request whose `Host` is that domain is answered exactly like a request to the site's own
   host: `GET` and `HEAD` only, the site's files, the same visibility rules, the same
   `/__open-pages/` prefix. Nothing of the API or UI is reachable there.

Attaching a domain only changes what open-pages answers to. It does not create DNS records or
certificates, and a domain that the proxy does not route to open-pages is simply unreachable.

## Configuration

In `[sites]` of `settings.ini` (or `OPEN_PAGES_SITES_*` variables):

| Key | Default | Meaning |
|---|---|---|
| `custom_domains` | `false` | Turn the feature on. The three routes below only exist when it is `true` |
| `custom_domain_suffixes` | none, required when on | Comma-separated domains, for example `corp.example, docs.example`. A custom domain must be a **strict subdomain** of one of them: `handbook.corp.example` is allowed, `corp.example` itself and `evilcorp.example` are not. A suffix may not be, or lie under, `app_host` or `content_domain` |
| `custom_domains_per_site` | `5` | Most domains one site may have (1 to 100) |

```ini
[sites]
app_host = pages.corp.example
content_domain = pages-content.corp.example
custom_domains = true
custom_domain_suffixes = corp.example
```

The suffix list is the control that matters. Without it any user could claim a name such as
`mail.corp.example` or `sso.corp.example`, and whenever a catch-all proxy rule or a wildcard
DNS record sent that name to open-pages, the site would answer for it. Choose suffixes whose
names you are happy for any site owner to use, and prefer a dedicated zone such as
`docs.corp.example` over a company-wide one.

## Rules for a domain

A domain is lower-cased and a trailing dot is removed. It is refused with `400` unless it

- has at least two DNS labels, at most 253 characters in all, each label 1 to 63 lowercase
  letters, digits or hyphens that neither start nor end with a hyphen;
- has no port, path or underscore, is not an IP address (the last label must contain a letter)
  and has no `xn--` label (IDNA A-labels are shown as Unicode by browsers, a homograph risk);
- is not `app_host` or `content_domain`, or anything under them. Those hosts are the app
  and the site hosts; a custom domain never overlaps them;
- is a strict subdomain of one of `custom_domain_suffixes`.

A domain belongs to **one site**. Adding a domain another site has answers `409`; adding one the
site already has answers `200` and changes nothing. Deleting a site removes its domains. Only
the **owner** of a site may add or remove its domains (group members may not), and every
change is written to the [audit log](security-model.md#audit-log) as `site.domain_add` and
`site.domain_remove`.

## Managing domains

API (see [api.md](api.md#get-v1authsitesnamedomains)):

```sh
curl -H "Authorization: Bearer $OPEN_PAGES_TOKEN" https://pages.corp.example/v1/auth/sites/handbook/domains
curl -H "Authorization: Bearer $OPEN_PAGES_TOKEN" -X PUT    https://pages.corp.example/v1/auth/sites/handbook/domains/handbook.corp.example
curl -H "Authorization: Bearer $OPEN_PAGES_TOKEN" -X DELETE https://pages.corp.example/v1/auth/sites/handbook/domains/handbook.corp.example
```

CLI (token in `OPEN_PAGES_TOKEN`, server in `OPEN_PAGES_URL` or `-server`):

```sh
open-pages domain list   handbook
open-pages domain add    handbook handbook.corp.example
open-pages domain remove handbook handbook.corp.example
```

The site's manage page shows the same list with an add form and *Remove* buttons.

## Security

- **Each domain is its own origin.** Site cookies are host-only (`__Host-` with
  `cookie_secure = true`), so a login on `handbook.corp.example` is not sent to the site's
  other host, to another custom domain, to a sibling site or to the app. The app session
  cookie never reaches any of them.
- **Browser login on non-public sites** works as for the site's own host: the visitor is sent
  to `app_host`, signs in there, and the one-time code comes back to the host they started on.
  `GET /v1/auth/site-login` accepts a `host` parameter, but only a domain registered for that
  very site is honoured, so it cannot be used to send a login code to another host.
- **The `Host` header decides.** open-pages never reads `X-Forwarded-Host`. A proxy that
  rewrites `Host` to its upstream name breaks the feature, and one that forwards a
  client-supplied `Host` for names it should not serve exposes whatever those names map to.
  Route only the hosts you intend to.
- **Same content, several hosts.** A site is served on its own host and on each custom domain
  at once. Access is checked per request on every host, so changing visibility, group or
  membership takes effect on all of them immediately.
- **Hijack on removal.** A removed or deleted domain stops being served at once, but DNS and
  the proxy route usually remain. Remove those too, or the name may later be claimed by another
  site (it will then serve that site's content under a certificate you issued).
- Put `Strict-Transport-Security` on the proxy for custom domains if the name is only ever
  served over HTTPS. open-pages sets no HSTS header itself.

## Reverse proxy examples

In every case the proxy must pass the original `Host`, speak HTTP to open-pages, set
`X-Forwarded-Proto: https` if `trusted_proxies` lists it (otherwise set
`cookie_secure = true`, which makes redirects use `https`) and allow request bodies up to
`max_upload_mb` for the app host (custom domains only serve `GET` and `HEAD`).

### nginx

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name handbook.corp.example docs.corp.example;

    ssl_certificate     /etc/ssl/corp/handbook.crt;   # a certificate covering both names
    ssl_certificate_key /etc/ssl/corp/handbook.key;

    location / {
        proxy_pass http://open-pages:8080;
        proxy_set_header Host $host;                   # required: this is what selects the site
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}

server {
    listen 80;
    server_name handbook.corp.example docs.corp.example;
    return 301 https://$host$request_uri;
}
```

One `server` block per domain, or one for a whole zone with a wildcard certificate
(`server_name *.docs.corp.example;`) if you give everyone names under a dedicated suffix.

### Traefik

File provider (dynamic configuration). Traefik passes `Host` through by default.

```yaml
http:
  routers:
    open-pages-custom:
      rule: "HostRegexp(`^[a-z0-9-]+\\.docs\\.corp\\.example$`)"   # a dedicated zone
      entryPoints: [websecure]
      service: open-pages
      tls:
        certResolver: corp          # or a certificate store holding *.docs.corp.example
  services:
    open-pages:
      loadBalancer:
        servers:
          - url: "http://open-pages:8080"
```

For a handful of domains, list them explicitly: ``rule: "Host(`handbook.corp.example`) || Host(`wiki.corp.example`)"``.

### Kubernetes Ingress

The Helm chart's own Ingress covers `appHost` and `*.<contentDomain>` only. Add one Ingress
per custom domain (or one for several names) in the release namespace:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: open-pages-handbook
  annotations:
    cert-manager.io/cluster-issuer: corp-ca       # if you use cert-manager
spec:
  ingressClassName: nginx
  tls:
    - hosts: [handbook.corp.example]
      secretName: handbook-tls
  rules:
    - host: handbook.corp.example
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: open-pages          # the chart's Service: <release>-open-pages
                port:
                  name: http
```

Enable the feature in the chart with `config.sites.customDomains` (see
[kubernetes.md](kubernetes.md#custom-domains)).

### OpenShift

The router matches the `Host` header and passes it through. Create one **Route per domain**
(wildcard Routes only help for a zone you control, and need the router to allow wildcard
policy):

```yaml
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: open-pages-handbook
spec:
  host: handbook.corp.example
  to:
    kind: Service
    name: open-pages              # the chart's Service
  port:
    targetPort: http
  tls:
    termination: edge
    insecureEdgeTerminationPolicy: Redirect
    # certificate, key and caCertificate for handbook.corp.example, or omit them to use
    # the router's default certificate when the name is under the cluster's apps domain
```

Or with `oc`:

```sh
oc create route edge open-pages-handbook --service=open-pages --hostname=handbook.corp.example \
  --cert=handbook.crt --key=handbook.key --insecure-policy=Redirect
```

Unless the route's `host` is under the cluster's apps domain, DNS for the name must point at
the router, and the cluster's router settings must admit the name (a cluster administrator
controls this). Without `certificate` and `key` the router serves its default certificate,
which only covers names under the cluster's apps domain, so give the Route its own certificate
for any other name. See also [openshift.md](openshift.md).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `404` from the proxy or a certificate warning | The proxy has no route or certificate for the name; open-pages is not involved yet |
| `404 page not found` from open-pages | The domain is not attached to a site, `custom_domains` is off, or the proxy rewrote `Host` (check what open-pages receives: `curl -H 'Host: handbook.corp.example' http://open-pages:8080/`) |
| API returns `404` on `/domains` | `custom_domains = true` is not set; the routes do not exist otherwise |
| `400 domain must be a subdomain of one of: ...` | The name is outside `custom_domain_suffixes` |
| Login loops on a non-public site | The proxy does not send `X-Forwarded-Proto: https` (and `cookie_secure` is `false`), so the redirect back uses `http`; or `Host` is rewritten, so the state cookie and the redeemed code land on different hosts |
