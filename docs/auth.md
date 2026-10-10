# Access control and authentication

How people and pipelines prove who they are, how groups work, and exactly who can see each
site. Route details (bodies, status codes) are in [api.md](api.md).

## Accounts

An account is a row in the `user` table (see [schema.md](schema.md#user)). The email is the
account name; there is no separate username. Two kinds exist:

- **Local accounts**, created with `POST /v1/user/register`. The email must be a bare
  address, the password 8 to 72 bytes. The password is stored as a bcrypt hash. Registration
  is open to anyone who can reach the server; there is no approval step or email
  verification. Local email addresses are unique case-sensitively.
- **OIDC accounts**, created at first sign-in through the identity provider (see below).
  They have no password, so local login always fails for them.

Setting `[auth] local_login = false` removes the register and login routes, for setups where
everyone signs in through OIDC. It requires `[oidc] enabled = true` or the server refuses to
start. Existing sessions and API tokens keep working. The setting accepts only `true` or
`false`; anything else fails startup.

There is no API to delete users, change a password, or change an email.

## Sessions

`POST /v1/user/login` (and a successful OIDC callback) creates a session: a random 256-bit
token stored in the `session` table with an expiry 7 days ahead, sent to the browser as the
`auth_cookie` cookie.

- The cookie is `HttpOnly`, `SameSite=Lax`, `Path=/`. It has the `Secure` flag only when
  `[server] cookie_secure = true`; set that whenever the service is reached over HTTPS.
- Sessions do not slide: the expiry is fixed at login. Expired rows are deleted at the next
  login by anyone; until then they are ignored but remain in the table.
- `POST /v1/auth/logout` deletes the session and clears the cookie. A request that arrives
  with a cookie the server does not accept gets the cookie cleared in the response.
- Session-only routes (token management, logout) reject API tokens.

## API tokens

For CI pipelines and scripts. A token authenticates as the user who created it and has all
of that user's rights. There are no scopes and no per-site tokens.

- Create one while logged in with a session: `POST /v1/auth/tokens` with a name and an
  optional `expires_in_days` (1 to 3650; absent or 0 means no expiry). The `opt_...` value
  is in the response and cannot be retrieved again.
- Only a SHA-256 hash and a display prefix (`opt_` plus 8 characters) are stored. At most 50
  tokens per user; expired ones are deleted when the owner creates another and do not count.
- Send it as `Authorization: Bearer opt_...` on any "any" route in [api.md](api.md), and
  when fetching non-public sites.
- A token cannot create, list or revoke tokens, and cannot log out. Revoke one with
  `DELETE /v1/auth/tokens/{id}` from a session; it stops working immediately.
- If a request has an `Authorization: Bearer` header, only the token is considered, even if
  it is invalid or empty and a valid session cookie is also present.
- The `Authorization` header is also what the `open-pages deploy` command sends; see
  [getting-started.md](getting-started.md#deploying-with-the-cli-and-ci).

The metrics token in `settings.ini` is unrelated to API tokens and only opens `/metrics`.

## OIDC sign-in

Optional OpenID Connect sign-in with the company identity provider (tested against the
standard flow of providers such as Keycloak, Entra ID, ADFS). Off by default; without an
`[oidc]` section the server behaves as before. LDAP is not supported; it is deferred.

### Settings

```ini
[oidc]
enabled = true
issuer = https://sso.example.com/realms/corp
client_id = open-pages
client_secret = ...
redirect_url = https://pages.example.com/v1/auth/oidc/callback
scopes = openid email profile
email_claim = email
groups_claim = groups
allowed_email_domain = corp.example
```

Every key is described in [getting-started.md](getting-started.md#oidc). The essentials:

- `issuer`, `client_id`, `client_secret` and `redirect_url` are required when enabled;
  startup fails otherwise.
- `issuer` and `redirect_url` must be `https` (plain `http` is accepted for `localhost`,
  `127.0.0.1` and `::1` only).
- `issuer` must be exactly what the provider reports in its discovery document at
  `<issuer>/.well-known/openid-configuration`. Discovery is fetched on the first sign-in
  attempt, so the server starts even if the provider is down; sign-in then answers 502.
- `redirect_url` must be the absolute URL of `/v1/auth/oidc/callback` on this server, and
  must be registered at the provider exactly like that.
- The client secret is read from the settings file only (there is no environment
  override) and is never logged. Make the file readable only by the service user.

### Provider setup

Keycloak: create an OpenID Connect client with "Client authentication" on, the standard
flow enabled, and the callback URL as a valid redirect URI. Copy the secret from the
Credentials tab. To send groups, add a "Group Membership" mapper to the client's dedicated
scope with "Full group path" off and "Add to ID token" on.

Other providers: register a web application with the same redirect URL, the authorization
code flow and client-secret authentication. For Entra ID use the issuer
`https://login.microsoftonline.com/<tenant>/v2.0` and add a groups claim in the app
registration's token configuration.

### The flow

1. A browser opens `GET /v1/auth/oidc/login` (optionally with `?return_to=/some/path`). The
   server stores a pending attempt, sets the `oidc_state` cookie and redirects to the
   provider using the authorization code flow with PKCE (S256), `state` and `nonce`.
   Attempts expire after 10 minutes; at most 10000 are kept in memory.
2. The provider redirects back to `/v1/auth/oidc/callback`. The server checks that the
   `state` matches the cookie and a pending attempt (single use), exchanges the code at
   the token endpoint with client-secret basic authentication, and verifies the ID token:
   RS256 or ES256 signature against the provider's JWKS (any other algorithm, `none`
   included, is refused), issuer, audience (plus `azp` when there are several audiences),
   expiry, not-before (one minute of clock skew allowed) and nonce.
3. It finds or creates the local user, maps groups, and starts the same 7-day session as a
   local login. The response is a `303` to `return_to`, or `200 {"status":"successful login"}`
   when none was given.

JWT verification is implemented in the standard library; there is no JWT or OAuth
dependency.

### How an identity becomes a user

- An OIDC user is identified by issuer plus subject (`oidc_issuer`, `oidc_subject`), never by
  email. The email is read from `email_claim` at first sign-in and then kept; later changes
  at the provider do not update it.
- The email must parse as a bare address and `email_verified`, if present, must not be
  `false`. If `allowed_email_domain` is set, the email must be in that domain
  (case-insensitive). Failures answer 403.
- If the email already belongs to any other account (local or another identity, compared
  ignoring case), sign-in is refused with 409. Accounts are never linked or taken over by
  matching email. There is no admin-initiated account linking; an operator has to remove or
  rename the conflicting account in the database first.

### Group mapping

`groups_claim` (default `groups`) names the ID token claim holding group names, either a
list of strings or a single string. Setting it empty turns mapping off.

- Names are matched to **existing** open-pages groups with the same name, ignoring case.
  Names that match no group are ignored. Groups are never created from a token.
- At every sign-in the user is added to the matching groups, and removed from groups that an
  earlier sign-in added (`user_group.oidc = 1`) but the token no longer lists.
- Memberships added by hand through the API are never removed by a sync. If a group owner
  removes a provider-managed member, the next sign-in adds them back. If an owner adds an
  existing provider-managed member by hand, the membership becomes a manual one.
- At most 500 names per token are read.

## Groups

A group is a named set of users. Names are 1 to 64 characters, no control characters, and
unique ignoring case (Unicode simple case folding, so `Ä` and `ä` clash). The stored name
keeps its original spelling.

| Action | Who |
|---|---|
| Create a group | any logged-in user; becomes owner and first member |
| List own groups | any logged-in user |
| See a group and its members | members |
| Add a member by email | owner |
| Remove a member | owner; or a member removing themselves |
| Delete the group | owner, and only while no site uses it (otherwise 409) |

The owner cannot be removed from their group; delete the group instead. A group that
predates owners (`owner` is NULL in the database) cannot be changed through the API until an
operator sets an owner. There is no ownership transfer.

Members are added by the email of an existing account, so a person has to register or sign in
once first. Deleting a group deletes its memberships.

## Site visibility

Every site has a `visibility`, set when it is created and changeable by its owner (default
`public`). It controls who can **view** the site content and its metadata. Changing,
deleting, uploading to, listing versions of and rolling back a site is owner-only whatever
the visibility.

| Level | Served to | Listed in `GET /v1/sites` | In `docs.viewable` of `GET /v1/auth/user` |
|---|---|---|---|
| `public` | everyone, no login | yes | only if owned or shared through a group |
| `authenticated` | any logged-in user (session or API token) | no | yes, for every user |
| `restricted` | the owner, and the members of the site's group | no | owner and group members |

Exact rules (`canView` in `sites.go`):

1. `public`: allowed, for anonymous visitors too.
2. Otherwise an anonymous request is refused.
3. `authenticated`: allowed for any valid user.
4. `restricted`: allowed for the site owner. A site with no group is therefore private to
   its owner.
5. `restricted` with a group: allowed for a member of that group, **but only while the site
   owner is still a member of that group too**. A site cannot be shared with a group its
   owner has no standing in.

Consequences:

- Putting a site into a group requires the site owner to be a member (403 otherwise; 400 if
  the group does not exist). If the owner later leaves or is removed from the group, the
  other members lose access at once, and the owner keeps theirs.
- A group that is still the `group` of any site cannot be deleted.
- A site's group is stored regardless of visibility, but only `restricted` sites use it for
  access. `public` and `authenticated` ignore it.
- Credentials: both the session cookie and an API token (`Authorization: Bearer`) are
  honoured when serving.

What a refused viewer sees:

- **Serving the site.** Anyone who may not view a non-public site, anonymous visitors
  included, gets the same plain `404 page not found` as for a site that does not exist
  (`Cache-Control: no-store`), so the response does not reveal that the site exists. Content
  served for non-public sites, including its 404 pages and redirects, is sent with
  `Cache-Control: private, no-cache` and `Vary: Cookie, Authorization`.
- **JSON API.** `GET /v1/auth/sites/{name}` returns `403 {"error":"you may not view this
  site"}` to a logged-in user who may not view it (the management routes tell logged-in
  users which sites exist), and 404 for an unknown site.

### Site ownership

The creator of a site owns it; there is no transfer. The owner can set the description,
group (or clear it with `0`) and visibility, delete the site, upload new versions, list
versions and roll back.

## Known limits

- **Subdomain mode and browsers.** In `url_mode = subdomain` the session cookie is host-only
  (no `Domain` attribute) and never reaches `<site>.<base_domain>`. A browser therefore
  cannot open `authenticated` or `restricted` sites there, and signing in does not change
  that. API tokens work (`curl -H "Authorization: Bearer ..."`). Use path mode if
  non-public sites must open in a browser.
- **No IP-based visibility.** There is no "intranet" level that depends on the client
  address. Restrict by network at the reverse proxy.
- **Path mode shares an origin.** All sites in path mode share cookies and local storage with
  the API and with each other. Do not host untrusted content in path mode.
- **OIDC maps only to existing groups.** Groups are never created from tokens.
- **LDAP is not supported.** It is deferred.
- **No admin-initiated account linking.** An OIDC identity whose email matches an existing
  account is refused with 409.
- **API tokens are not scoped.** A token has all the rights of its user.
- **No user management API.** No password change or reset, no user deletion, no email
  change, no ownership transfer for sites or groups.
- **Open registration.** With local login enabled anyone who can reach the server can
  create an account; limit reachability at the network level, or use OIDC-only mode.
