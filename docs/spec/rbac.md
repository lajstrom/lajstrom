# Spec: authentication, tokens and RBAC

- Status: normative for the MVP (2026-11-06); OIDC, groups and group bindings (§8) for M6 (#69).
  Owned by `internal/auth`.
- Related: [ADR 0007](../adr/0007-rbac-model.md), [http-routing.md](http-routing.md).

## 1. Principals

| Principal | Form | MVP |
| --- | --- | --- |
| local user | `user:<uid>` (below); password (argon2id) | yes |
| personal access token (PAT) | acts as its user, optionally narrowed by `scopes`; expiry mandatory (max 1 year, default 90 days); created with a password or by an OIDC login (§8), never with a token (§3) | yes |
| deploy token | `token:<id>`; bound to exactly one repo; scopes `read` and/or `write`; expiry optional | yes |
| anonymous | `anonymous`; only ever gets `read`, and only on repos with `anonymousRead: true` while the server-wide `auth.anonymousRead` (off by default) is on | yes |
| group | `group:<name>`; the groups claim of a user's latest OIDC login (§8) | M6 |
| OIDC user | `user:<uid>`, of the user its identity, the `iss` and `sub` of its ID token, maps to; made at its first login, without a password (§8) | M6 |

One credential works on every protocol: HTTP Basic on the frontends and on `/v2/token`
(`user:password`, `user:PAT`, or `deploy:<token>`), and the resulting bearer on `/v2`, where
Basic is taken too and evaluated on each request (§4). Outside
`/v2`, `Authorization: Bearer <secret>` with a personal access token or a deploy token counts
as its Basic form does: npm sends `_authToken` so, and yarn `npmAuthToken` (ADR 0012).
`npm login` is a way to get a personal access token: the user's password, checked and throttled
as in Basic, buys a new one with the default expiry (http-routing.md, npm); nobody signs up
that way. An OIDC login is another (§8): it ends in a personal access token, and no token of
the IdP is ever a credential of Lajstrom.
Passwords and token secrets are never logged; tokens are stored hashed (sha256) and shown once.
Failed password checks are throttled per account (by uid; a name that no user has, by the
name) and per client address, where Lajstrom knows the client: with `server.trustedProxies`
behind an edge, or with `server.tls`, when the peer is the client (#248); a throttled request is
`429` with `Retry-After`, whatever its password. Failed token secrets are not throttled
(docs/auth.md, #130). The token endpoint `/v2/token` is rate-limited per client address, where
Lajstrom knows it: 100 requests a second, then `429` with `Retry-After`.

### Users: the uid and the name

A user's identity is its **uid**, a UUID v7 (RFC 9562) in its canonical text form, made when the
user is created and never changed nor reused (#182). Everything that refers to a user refers to
the uid: the principal `user:<uid>` that bindings, publication sessions, the audit log, the `sub`
of a bearer token and the annotation `lajstrom.uploaded-by` carry, and the tokens, by the user's
row. A version 7 UUID begins with the time of its creation, which `createdAt` shows anyway.

The **name** is an attribute: what HTTP Basic carries before the colon and what people type. At
any moment it names one live user: it is unique among the users that are not deleted, it can be
changed (a rename), and a user that is deleted frees it. Where a person names a user (Basic, the
CLI, the API, `--principal user:alice`), the server resolves the name to the live user's uid at
the time of the request; answers and lists show both. A name of the form of a UUID is refused, so
that `user:<x>` is a uid when `x` is a UUID and a name otherwise.

**Deleting a user** disables it, revokes its tokens and removes its bindings, and keeps its row,
marked deleted: a uid in the audit log always resolves to who it was. A new user of the freed name
gets a new uid and inherits nothing. A user can be disabled instead, which is undone by enabling
it; deleting is not undone. The last enabled user with an admin binding is neither deleted nor
disabled (`409`). A rename changes nothing but the name: the bindings and tokens keep working, and
the audit log keeps the name of each event's time.

## 2. Resources and actions

Resource: a repo, identified by `<format>/<repo>`. Prefix-level resources (a path inside a repo)
are after the MVP. Two system-level resources, which only the `admin` role reaches: `system`
(users, bindings, registries, creating a repo, every user's tokens, the webhooks of any mask) and
`audit`.

| Action | Meaning |
| --- | --- |
| `read` | GET/HEAD on the frontend; `pull` scope on `/v2`; list in `/api/v1/repos` |
| `write` | PUT/POST on the frontend; `push` scope on `/v2`; create versions |
| `delete` | DELETE versions/manifests; `delete` scope on `/v2` |
| `manage` | change the repo (policy, anonymous read, deploy tokens) or delete it; reindex it and read its latest run (`GET …/reindex`); purge a proxy's upstream cache (§7); its webhooks, those whose mask is exactly the repo, their secrets, tests and delivery logs ([webhooks.md](webhooks.md)) |

On npm's routes (http-routing.md, npm), changing a package's dist-tags or deprecations is
`write`, the `DELETE` of a dist-tag included, and an unpublish is `delete`, a version that a
packument update leaves out included.

## 3. Roles and bindings

| Role | Actions |
| --- | --- |
| `reader` | read |
| `writer` | read, write |
| `maintainer` | read, write, delete, manage |
| `admin` | everything on every repo plus `system` and `audit` (system role; the mask is ignored) |

A `maintainer` binding on `*/*` has every action on every repo, and neither `system` nor
`audit`: those need the `admin` role.

A **binding** is `(principal, role, repo_mask)`. Masks: exact `apt/ubuntu-internal`, format-wide
`apt/*`, name-across-formats `*/payments`, everything `*/*`. Wildcards only as a whole segment.
A principal may have many bindings; the effective permission on a repo is the union.

A binding is refused with `400`, naming the field, when:

- its principal is neither a live user, `user:<name>` (stored as the user's `user:<uid>`) or
  `user:<uid>`, nor a group, `group:<name>` with a name of §8. A group need not have members:
  a binding may come before anyone logs in with the group, as before its repo. `anonymous`
  reads through a repo's `anonymousRead` and gets nothing else, and a deploy token
  `token:<id>` gets its repo and its scopes and nothing else, so neither has bindings;
- its role is not one of the four;
- its mask is not two segments `<format>/<repo>`, each `*` or a name: a format of this build,
  and a repo name by manifest-profile.md §4.1. The repo need not exist: a binding may come
  before its repo, and outlives it;
- its role is `admin` and its mask is not `*/*`: the mask of an admin binding is ignored, and
  `*/*` says so.

The same `(principal, role, repo_mask)` twice is `409`, and so is deleting an `admin` binding
of an enabled user when no enabled user would keep one: nobody could manage the server through
`/api/v1` after it. A group's `admin` binding does not count for this rule nor for the bootstrap
(§6): a group lapses without a change of the bindings (§8).
`lajstrom admin binding create` on the command line can always make one (§6).

Evaluation of `allowed(principal, action, format/repo)`. Steps 2 and 3 depend on the
credential, not on the bindings, and are checked first: a PAT's scopes narrow an `admin` too.

1. `admin` binding → allow.
2. Deploy token → allow iff the token's repo matches and `action` is in its scopes
   (`read`/`write`); never `delete`/`manage`.
3. PAT → compute as for its user, then intersect with the PAT's scopes if set.
4. User → any binding of the user, or of a group the user is in now (§8), whose mask matches
   the repo and whose role includes the action. A group is not a principal of a request: it
   reaches a repo only through its members.
5. Otherwise, if `action == read`, the repo has `anonymousRead`, and the server-wide switch
   `auth.anonymousRead` is on → allow (also for the `anonymous` principal).
6. Deny.

There is no mode without this evaluation: anonymous reads where both switches allow it and does
nothing else, on every server.

A change of the bindings, or of a repo (created, deleted, its `anonymousRead` changed), counts
from the next request on, on every replica, as a user disabled does. Each replica evaluates in
memory, from a snapshot of the bindings, of the repos' `anonymousRead` and of the users' groups
(§8); before it evaluates a request it reads the counter `auth_version` (index-schema.md), which
every such change moves in its own transaction, the groups of a login included, and reloads the
snapshot when the counter moved. A user's groups lapse at their time (§8) without a change: the
snapshot keeps that time, and the evaluation compares it with the clock. `auth.anonymousRead` is
configuration: it changes with a restart.

A denial on `<format>/<repo>` must not reveal whether the repo exists:

- the `anonymous` principal gets `401` with a challenge (the bearer challenge under `/v2/`,
  Basic elsewhere and on `/v2/token`), on a repo it may not read and on a repo that does not
  exist alike, so that clients send credentials;
- an authenticated principal with no access to the repo at all (not even `read`) gets `404`,
  the same answer as for a repo that does not exist;
- `403` is only for a principal that can read the repo but lacks the action (`write`, `delete`,
  `manage`): it already knows that the repo exists. A deploy token on its own repo gets `403`
  for an action outside its scopes, `read` included for a `write`-only one: it is bound to the
  repo and knows it.

Every other route that could tell whether a repo exists keeps the rule too, and how is part of
this spec, so that a review need not find it in the code:

- **Creating a repo** (`POST /api/v1/repos`) is `system` (§2). The `admin` role is checked
  before the body is read, so the `409` of a name that is taken, and the `400` of a body that
  does not parse, reach an admin only, who sees every repo anyway. Every other principal gets
  `403` (`anonymous` `401`) whatever the name: trying names by creating repos tells nothing.
- **Bindings, users and the audit log** are `system` and `audit`: admin only. A binding's mask
  need not name a repo that exists (above), so creating one tells nothing either.
- **A repo's deploy tokens** need `manage` on it, decided before the repo is looked up: the
  answers of this rule.
- **A repo's webhooks** (`/api/v1/repos/<format>/<repo>/webhooks…`, #77) need `manage` on it,
  decided before the repo is looked up, then the repo must exist (`404`), and a webhook id that
  is not of the repo's mask is `404`. **The webhooks of any mask** (`/api/v1/webhooks…`) need
  the admin role, checked before the body is read: a wildcard mask sends events of repos that
  do not exist yet. A webhook's mask, like a binding's, need not name a repo that exists, so
  creating one through `/api/v1/webhooks` tells nothing. A webhook's events tell its receiver
  what `read` on the repo shows and the actor of each write (webhooks.md §3).
- **A personal access token's scopes** are actions; they name no repo.
- **Creating a personal access token** (`POST /api/v1/users/<name>/tokens`) takes a password
  as the request's credential, besides the user's own or the `admin` role; an OIDC login mints
  one itself (§8), and is no credential of this route. A personal access token, with scopes or without, the admin's too, and a deploy token
  get `403` before the body is read, whoever's tokens they name: a stolen token must not mint
  tokens that outlive its revocation (decided on #249, 2026-11-10). A personal access token
  without scopes still lists and revokes its user's tokens; one with scopes and a deploy token
  get `403`. The command line (`lajstrom admin token create`) has no credential and is the
  operator's.
- **Lists** (`GET /api/v1/repos`, `/v2/_catalog`) show only the repos the principal may read.
- **`/v2` with a bearer**: the token names every repository asked for, with no action on one
  that does not exist or may not be read alike, and the facade asks the token before it looks
  the repo up (§4).

The rule is about the answers, not their durations: a denial on a repo that exists may take a
database read longer than the answer for one that does not. Making them equal is not in the
MVP.

## 4. OCI bearer tokens

`GET /v2/token?service=<host>&scope=repository:<format>/<repo>/<pkg>:pull,push&scope=…`

- `GET` only (the OAuth2 `POST` form is `405`). Authenticates with Basic (or none for
  anonymous). The service is the server's own: a `service` that names another is ignored.
- For each requested scope the facade keeps the actions the principal is allowed on
  `<format>/<repo>` (`pull`↔`read`, `push`↔`write`, `delete`↔`delete`); on the path of an `oci`
  group, `pull` at most, since nothing is pushed or deleted through it (§7). A scope with no allowed
  action stays in the token with `actions: []`, and so does the scope of a repo that does not
  exist: the client can read the token, and it names every repository asked for without telling
  which exist (§3). Asking for what one may not have is not an error of the token endpoint, as
  in the distribution token spec. More than 100 scopes in one request is `400`. The scope
  `registry:catalog:*`, which clients ask for to list `/v2/_catalog`, is kept as asked, but for
  a personal access token whose scopes do not read; any other scope that is not a repository
  is dropped.
- Issues a JWT signed with the current token key (`keys`; `ES256` by default, `EdDSA` with
  `keys.tokenAlgorithm`), `exp` 20 minutes after `iat`, verified with 60 s of clock skew;
  claims: `iss` and `aud` = the service, `sub` (the principal, `user:<uid>` for a user),
  `access: [{type, name, actions}]`, `iat`, `nbf`, `jti`. The answer is `token` and
  `access_token` (the same JWT), `expires_in` and `issued_at`; there is no `refresh_token`
  (clients re-authenticate with Basic). A token whose `sub` names a user by name, issued before
  the uid (#182), is refused like any token that does not verify: `401`, and the client gets
  another.
- The facade validates the bearer on every `/v2` request and enforces the actions, by the token
  before it looks the repo up, so that the answers keep §3: a repository the token does not name
  is `401` with `error="insufficient_scope"`, whether its repo exists or not; one it names with
  no action gets the answer of a repo that does not exist (`404 NAME_UNKNOWN`, `401` to
  `anonymous`); one it names without the action is `403 DENIED`, as the principal can read it.
  A cross-repo mount from a source the token names with no action is a plain upload session.
  The backend registry is never exposed to clients and trusts only Lajstrom (network policy or
  the backend's own credentials held by the provider).
- A token is trusted until its `exp`, and the facade does not evaluate the bindings again for
  a request it decides by the token. The bindings, a disabled user, a revoked or expired
  personal access or deploy token, and a repo's `anonymousRead` count when a token is issued:
  on `/v2` a change takes effect at the client's next `/v2/token`, at most 20 minutes later for
  a token issued before it. A repo deleted is unknown at once (`NAME_UNKNOWN`): the token names
  repositories by name. Two requests are evaluated when they come, as on every other route: one
  with Basic credentials instead of a bearer, and `/v2/_catalog` with a token that holds
  `registry:catalog:*`, which lists the repos the token's principal may read at that moment.
- To end every token now, `lajstrom admin key rotate --token --drop` (operations.md): within a
  second no replica accepts a token of the old key, and clients get new tokens with their
  credentials, under the bindings of that moment.

## 5. Audit

Every write (`push`, `delete`), every control-plane change, and every creation and revocation of
a personal access token or a deploy token is an `audit` row with the principal, action, repo,
subject, digest, client IP and request id. A change of the index or of the control plane writes
its row in its transaction. A write to the registry, which no transaction spans, has its row
before the write, `pending`, in a transaction of its own, and the row says how the write ended:
`ok`, in the transaction that records the write in the index; `failed`, with the error; or
`unknown`, when it is still pending after `ingest.claimTTL` and its writer stopped in between
([index-schema.md](index-schema.md#the-audit-row-of-a-registry-write),
[guarantees.md](guarantees.md) INV-14). The creation's row also
names the credential that created the token, `createdBy` in its details: `password`,
`token:<id>` for a token, or `oidc` for the token of an OIDC login (§8). A webhook's creation, change, deletion and the rotation of its secret are `webhook.create`,
`webhook.update`, `webhook.delete` and `webhook.secret` (#77), in the transaction of the change:
`repo` the webhook's mask, `subject` `webhook:<id>`, `details` `{"mask", "host", "events",
"active"}`, the host of its URL and never the URL nor the secret. A test delivery and the
deliveries are not audited: the delivery log has them (webhooks.md §4). Reads are not audited in the MVP (a per-repo option later). The
short-lived bearer tokens of `/v2/token` are not audited either (decided on #46, 2026-11-09): a
client gets one for every pull and push, and what a token allows is audited itself.

## 6. Bootstrap

No admin password is read from the environment or the configuration (#440). The first admin
is made in one of two ways, both with an `admin` binding on `*/*`:

- **The bootstrap token.** The operator makes it and gives it as the file of
  `auth.bootstrap.tokenFile`. While no enabled user has an admin binding, `serve` makes it valid
  on every replica at start; it never makes a token and never logs one. The index keeps its
  sha256 only (`bootstrap_token`, index-schema.md); it expires after `auth.bootstrap.ttl`. `POST /api/v1/bootstrap`
  (`lajstrom admin bootstrap`) exchanges it once for the first admin, in the transaction that
  marks it used, so that it makes one admin across replicas; the audit event is `user.create`
  by `anonymous` with the source `bootstrap`. Once an enabled user has an admin binding, the
  token is dead and the endpoint is `404`.
- **The database.** `lajstrom admin user create --name admin --admin` creates the user and the
  binding from the command line (no HTTP), with the password from standard input or a prompt.
  It needs the database, which is the authority, and works whatever admins exist, disabled or
  not: it is also the recovery of a lost admin, with `lajstrom admin user set-password`. Its
  audit events have the source `cli` and the `host` the command ran on.

The bindings of other users are made with
`lajstrom admin binding create|list|delete` from the command line (#45), which needs the database
and no credentials, or with `/api/v1/bindings` (#47). Everything else goes through `/api/v1`.

## 7. Proxy and group repos

A proxy and a group ([ADR 0013](../adr/0013-proxy-and-group-repos.md),
[proxy-and-group.md](proxy-and-group.md)) are repos: the resource is `<format>/<repo>`, masks
match them as they match a hosted repo, and the evaluation of §3 is unchanged. What each action
means on them:

| Action | proxy | group |
| --- | --- | --- |
| `read` | every read, a fetch on a miss included: the fetch is the server's, and the reader needs no `write` for it | every read through the group, of every member (below); on an `oci` group, of the paths its routes send to each member |
| `write` | the referrers of `/v2` (a signature or an attestation of a proxied artifact); the format's publish routes are `405` | nothing of its own: a write through the group is its hosted member's; through an `oci` group every write is `405` |
| `delete` | a held version, which the next miss fetches again | nothing of its own; through an `oci` group `405` |
| `manage` | as on a hosted repo, the keys of `upstream` but `name`, the purge of its cache, and a prefetch | as on a hosted repo, and `group.merge` |

**Read on the group is the whole permission.** For a read through a group, the members'
bindings, deploy tokens and `anonymousRead` are not asked: a principal that may read the group
reads the content of every member through it, the hosted member's included. Who may read a
group is therefore who may read its members' content, and `manage` on a group (its
`anonymousRead`, its deploy tokens) decides that: bind it as one would `manage` on every member.

**Through an `oci` group** ([ADR 0019](../adr/0019-oci-groups-one-member-per-repository-path.md),
proxy-and-group.md §12) read is the same whole permission: who may read the group reads the
paths its routes send to each member, and nothing else of a member, so a group can serve
`acme/public/*` of a hosted repo whose other paths stay private. A write through it needs `read`
on the group and is then `405 UNSUPPORTED`; one that cannot read the group gets the group's
denial. The token endpoint grants `pull` at most on a group's path (§4).

**A write through a group** of another format needs `read` on the group, then the action on the
group's hosted member, evaluated as if the request had come to the member's own URL. A principal that cannot
read the group gets the group's denial (§3); one that can read it and lacks the action on the
member gets `403` naming the member, which the group's resource already shows its readers. The
audit rows of the write (`push`, `delete`, `npm.dist-tag`, `npm.deprecate`) are the member's,
with `details.via` the group, `<format>/<group>`. A deploy token of a group has the scope `read`
only; a `write` scope is `400`, since it would grant nothing.

**The admin role turns a proxy into a hosted repo** (proxy-and-group.md §2.6): the repo then
takes publishes from whoever has `write` on it, which a proxy took from nobody, and that is a
decision of the same weight as choosing its upstream. The dist-tags and deprecations it writes to
the repo's journal are audit rows of the admin (`npm.dist-tag`, `npm.deprecate`).

**A prefetch** (proxy-and-group.md §5.1, §11.10) needs `manage`. A reader fetches the one version
it reads; a prefetch fetches every version of a list of names, or the platforms named of a list
of images, which spends the registry's space and the upstream's quota on the repo's behalf, and
that is the maintainer's choice. Its fetches are the audit rows of a miss's, `proxy.fetch` and
`proxy.reject`, with the caller as the principal; the prefetch itself writes no row of its own.

**The admin role** creates proxy and group repos, as every repo, and alone changes a proxy's
`upstream.name`, a group's `members` and an `oci` group's `paths`: each changes what every reader
of the repo gets, and a
member added by someone without `read` on it would publish it to the group's readers. The URL
of an upstream and its credentials are configuration; no action sets them.

**Audit** (§5), in addition:

| Action | When | Principal and fields |
| --- | --- | --- |
| `proxy.fetch` | a proxied ingest: the fetcher pushed an artifact | the principal of the request whose miss or prefetch caused it (a user, a token, `anonymous`), through a group too; `repo` the proxy, `subject` the version, `digest` the manifest's, `ip` the client's; `details` `{"upstream", "url", "checksum", "size"}` and `via`, the group, when there was one |
| `proxy.reject` | a fetch refused for its bytes: a checksum that differs, an artifact that `Validate` refuses, a file above a bound; or, before any download, a name the registry does not take (#599) | as `proxy.fetch`, without `digest`; `details.reason` (`checksum`, `invalid`, `size`, `name`) and, for a checksum, both values; `via` through a group, an `oci` one included |
| `repo.cache.purge` | `DELETE …/upstream-cache` (http-routing.md §6) | the caller; `details.name` when one name was purged |
| `repo.convert` | a proxy became a hosted repo (proxy-and-group.md §2.6) | the admin; `details` `{"from": "proxy", "to": "hosted", "upstream", "packages", "ops", "skipped"}`: the upstream it had, the packages it held, the journal ops planned and what was left out |

The principal of `proxy.fetch` is who brought the artifact in, not who may publish: it was
checked for `read` only. Requests for an upstream's metadata are reads and are not audited,
negative entries included.

## 8. OIDC: login, groups, group bindings (M6, #69)

One OpenID Connect provider, `auth.oidc` (configuration.md), logs people in; OIDC is off while
`auth.oidc.issuer` is empty. Lajstrom is its relying party, a confidential client whose redirect
URI is `<server.externalURL>/-/oidc/callback`. A login ends in a personal access token (§1),
never in a token of the IdP: the clients send what they send today, and no request but a login
asks the IdP anything. Token exchange for CI (a CI provider's OIDC token for a short-lived
token) and joining an existing user to an identity are not in this section (decisions on #69).

**The identity.** An identity is the pair (`iss`, `sub`) of a verified ID token
(index-schema.md, `identities`), and maps to one user. The first login of an identity creates
the user, `kind` `oidc`, without a password, named by the claim `auth.oidc.usernameClaim`
(default `preferred_username`), which must pass the rule of a user name (§1); a claim that is
missing or breaks the rule refuses the login, naming the claim. A name that a live user has
already refuses it too, naming the name: an identity is never joined to an existing user by
its name. Later logins find the user by the identity and keep its name, which a rename (§1)
may change. A disabled user is refused at login as on every request. A deleted user lets its
identity go: the next login creates a new user with a new uid, which inherits nothing; it is
disabling, not deleting, that keeps a person out.

**Groups.** The claim `auth.oidc.groupsClaim` (default `groups`; empty: no groups) is a list
of strings. Each that is 1 to 256 bytes of printable UTF-8, without a space at either end, is a
group, the principal `group:<name>`; any other is dropped and logged at debug level, as is the
claim when it is not a list. Each login replaces the groups of its identity, in the transaction
that moves `auth_version`, and they count until the login's `auth.oidc.tokenLifetime` has
passed (`identities.groups_expire_at`). So a group removed at the IdP grants nothing from the
user's next login on, on every replica, and no group grants beyond the lifetime of the token
its login minted. A group exists only through the identities that name it.

**Group bindings.** `group:<name>` takes every role and mask of §3. The permission of a user
is the union of its own bindings and the bindings of its current groups, and a personal access
token's scopes narrow that union as they narrow the user's own. A group's `admin` binding makes
its members admins; it is not "an admin" for the last-admin rule (§3) nor for the bootstrap
(§6).

**The login: authorization code flow with PKCE.** `GET /-/oidc/login` makes a login request
of 10 minutes (index-schema.md, `oidc_logins`): a `state` and a `nonce` of 256 random bits each
and a PKCE verifier (RFC 7636, `S256`). It answers `302` to the IdP's `authorization_endpoint`
with `response_type=code`, `client_id`, the redirect URI, the scopes of `auth.oidc.scopes`
(`openid` always), the state, the nonce and the code challenge, and sets a cookie of 256
random bits that binds the request to the browser: `lajstrom_oidc`, `HttpOnly`,
`SameSite=Lax`, path `/-/oidc/`, `Secure` unless `server.insecureHTTP`. The request keeps the
sha256 of the state and of the cookie, never either. `GET /-/oidc/callback?code=&state=` takes
the request of that state, pending, not expired and with that cookie, and marks it used in one
transaction, so that a state is redeemed once, on any replica. It then exchanges the code at
the `token_endpoint` with the verifier and the client's credentials (`client_secret_basic`),
and verifies the ID token:

- its signature, by the key of its `kid` in the IdP's JWKS, with an asymmetric algorithm that
  is the key's (`RS256`, `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`,
  `ES512`, `EdDSA`), never `none` nor an HMAC;
- `iss` equal to `auth.oidc.issuer`, which discovery (`<issuer>/.well-known/openid-configuration`)
  must answer, exactly, as its `issuer`;
- `aud` containing `auth.oidc.clientID`, and `azp`, when present, equal to it;
- `exp` present and passed by no more than 60 s, `iat` and `nbf` not more than 60 s ahead
  (the leeway of §4);
- `nonce` equal to the request's.

The JWKS is cached: a `kid` it does not have fetches it again, at most once every 30 s, and it
is fetched again hourly; while the IdP does not answer, the last set that loaded stays. Then,
in one transaction with the audit rows, the identity's user is found or created and its groups
replaced: `user.create` for a first login (`details.source` `oidc`), and `user.login`
(`details`: `issuer`, `subject`, `groups`). The browser's login mints a personal access token
of the user without scopes, named `oidc <time>`, which expires after `auth.oidc.tokenLifetime`
(24h by default, at most a year), with its `token.create` row (`createdBy` `oidc`), and answers
it once, on a page with `Cache-Control: no-store`. A refusal at the IdP (`error=`), a state that
is unknown, used, expired or of another browser, a code the IdP refuses, an ID token that does
not verify and a refused identity are `400` with a page that says to log in again and why in
Lajstrom's words, never the IdP's message; each is logged at info with its reason. No code,
state, verifier, cookie, IdP token or personal access token is logged, audited, or put in a URL
that Lajstrom renders.

**The device flow (RFC 8628), for `lajstrom login`.** `POST /-/oidc/device/code` makes a
device request of 10 minutes: a device code of 256 random bits, kept as its sha256, and a user
code of 8 letters of `BCDFGHJKLMNPQRSTVWXZ`, shown `XXXX-XXXX`. It answers `200` with
`device_code`, `user_code`, `verification_uri` (`<server.externalURL>/-/oidc/device`),
`verification_uri_complete` (with `?user_code=`), `expires_in` and `interval` (5). `GET
/-/oidc/device[?user_code=]` is a page where the person types or confirms the code (RFC 8628
§5.4); confirming it starts the code flow above for the device request, and the callback, once
the login verified, approves the device request for the user, mints nothing, and says to return
to the terminal. The CLI polls `POST /-/oidc/token` with
`grant_type=urn:ietf:params:oauth:grant-type:device_code` and the device code, and gets the
errors of RFC 8628 §3.5 as `400` with `{"error"}`: `authorization_pending` until the device is
approved, `slow_down` when it polls sooner than `interval` after its last poll,
`expired_token` after the 10 minutes, `access_denied` when the login was refused, and
`invalid_grant` for a device code that is unknown or redeemed. Once approved, the poll mints the
personal access token of the login as the browser's login does, with its `token.create` row, in
the transaction that marks the device request redeemed, and answers `200` with `access_token`,
`token_type` `Bearer`, `expires_in` and `user`, the user's name. A user code that is unknown,
expired or approved is a page that asks again; the pages and the start of a device request are
throttled per client address as the failed logins are (§1).

**What OIDC does not change.** Local users, their passwords, personal access tokens, deploy
tokens, anonymous read and the bearer tokens of §4 stay as they are. An OIDC user has no
password, so it cannot create a personal access token with the API (§3); it gets its tokens by
logging in, and may list and revoke them as any user.
