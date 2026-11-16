# Spec: authentication, tokens and RBAC

- Status: normative for the MVP (2026-11-06); OIDC, groups and group bindings (§8) for M6 (#69);
  service accounts (§1) after the MVP (#1020); inactive users (§1) after the MVP (#1283). Owned
  by `internal/auth`.
- Related: [ADR 0007](../adr/0007-rbac-model.md), [ADR 0021](../adr/0021-service-accounts.md),
  [ADR 0027](../adr/0027-inactive-users-disabled-at-authentication.md),
  [http-routing.md](http-routing.md).

## 1. Principals

| Principal | Form | MVP |
| --- | --- | --- |
| local user | `user:<uid>` (below); password (argon2id) | yes |
| personal access token (PAT) | acts as its user, optionally narrowed by `scopes`; expiry mandatory (max 1 year, default 90 days), but a service account's (below); created with a password or by an OIDC login (§8), never with a token (§3) | yes |
| deploy token | `token:<id>`; bound to exactly one repo, or to a mask of §3 (`oci/*`, `*/talos`), below; scopes `read`, `write` and `attach`, one or more; expiry optional, within `auth.deployTokens.maxTTL` when it is set (below, #1286) | yes |
| anonymous | `anonymous`; only ever gets `read`, and only on repos with `anonymousRead: true` while the server-wide `auth.anonymousRead` (off by default) is on | yes |
| client certificate | `cert:spiffe://<trust domain>/<path>` or `cert:<bundle>/<name>`; a certificate verified by a configured CA, on `/v2/token` only (§9) | #1019 |
| Kubernetes service account | `k8s:<cluster>/<namespace>/<service account>`; a service-account token of a configured cluster, on `/v2/token` only (§9) | #1019 |
| CI job | `ci:<issuer>/<project>`; a token of minutes, `ljs_ci_…`, minted at `/-/oidc/token` for the OIDC token of a configured CI platform under the trust policies of the configuration, which grant its rights (§11) | #937 |
| group | `group:<name>`; the groups claim of a user's latest OIDC login (§8) | M6 |
| OIDC user | `user:<uid>`, of the user its identity, the `iss` and `sub` of its ID token, maps to; made at its first login, without a password (§8) | M6 |
| service account | `user:<uid>` of a user of kind `service` (below); no password, no OIDC login; authenticates with its personal access tokens only; a required owner | #1020 |

One credential works on every protocol: HTTP Basic on the frontends and on `/v2/token`
(`user:password`, `user:PAT`, `deploy:<token>`, or a CI token under any user name, §11), and the
resulting bearer on `/v2`, where Basic is taken too and evaluated on each request (§4). Outside
`/v2`, `Authorization: Bearer <secret>` with a personal access token, a deploy token or a CI
token counts as its Basic form does: npm sends `_authToken` so, and yarn `npmAuthToken` (ADR 0012), and
Terraform the token of its `credentials` block.
The archive URL that the download of a terraform module answers carries a bearer token of §4
in its query, made for the principal of the download, which downloads that version and nothing
else: the archive route takes it in place of credentials, since Terraform fetches the archive
without the credentials it sends the registry (http-routing.md, terraform; ADR 0023). It is no
credential anywhere else.
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

**A refused credential is a line at info** (#1284), `authentication refused`, one per request,
beside its access line, with:

- `credential`, the kind of what was sent: `password`, `pat`, `deploy`, `ci`, `certificate`,
  `kubernetes`, `bootstrap` (the token of §6), or `bearer` (§4);
- `account`, the principal it named when Lajstrom knows one: `user:<uid>` of the user its name or
  its token resolves to, `token:<id>` of a deploy token, else `unknown`; and `token`, the id of a
  token that was found (revoked, expired);
- `reason`, from a fixed list (`unknown user`, `disabled`, `no password`, `wrong password`,
  `unknown token`, `revoked`, `expired`, …), never a value of the request;
- `address`, the client address (ADR 0011), and `requestId`.

Never the user name as it was typed, which may be a password typed into the wrong field or a
token pasted there (threat model L-3); never the secret, a part of it but the prefix that tells
its kind, or its hash. A request that a throttle refuses is its `throttled` line instead: its
credential was not checked. A bearer of §4 that only expired stays at debug, since every client
of `/v2` gets a new one every 20 minutes as a matter of course; one that does not verify (its
signature, its key, its issuer or audience) is at info. The refusals of an OIDC login (§8) and
of a token exchange (§11) keep their own lines. Passwords are bounded by the throttles; token
secrets are not, and a flood of them is one more line per access line. A throttle that trips is
an audit row, `login.block` (§5); a single failure is not.

A **deploy token over a mask** (#911) reaches every repo its mask matches, in the grammar and by
the matching of a binding's mask (§3): a fleet of nodes sends one credential per registry host,
and reads several repos of it. The mask names no repo: it matches the repos created after the
token too, from their creation on, and a repo deleted does not take the token with it. Only the
admin role creates one, with its password and with no token (§3, the routes; decided on #911,
2026-11-14), and on the command line the operator: `lajstrom admin token create --mask` writes
the database as every `lajstrom admin` command of §6 does, with no HTTP and no password, for
whoever holds the database's credentials is the operator. Everything else is a deploy token's:
its scopes, its expiry and its bound (below), how it authenticates, its revocation, its audit,
its hash, its secret shown once.

**The lifetime of a deploy token** (#1286, decided 2026-11-16) is bounded by
`auth.deployTokens.maxTTL` (configuration.md), a duration, `0` by default: no bound, and a deploy
token created without an expiry never expires. With a bound, a repo's token and a token over a
mask alike, made through the API or on the command line: one created without an expiry expires at
the bound, and one asked to expire later is `400` naming the field (`expiresAt`, `expiresIn`, or
`--expires` on the command line), the bound and the latest expiry it allows. The command line
reads the bound of the configuration it is given. The bound counts when a token is created, as a
service account's does (below): a token created before it was set or lowered keeps its expiry, or
none, until it is revoked, and raising it or setting `0` lengthens no token. A live deploy token
beyond the bound in force, one without an expiry or whose `expiresAt` is more than the bound after
its `createdAt`, is answered with `beyondMaxTTL: true` in every view of a token (http-routing.md
§4), worked out when it is answered and stored nowhere, and `serve` logs at warn at start how many
there are, when there are any. The bound is configuration: a change takes a restart, and each
replica applies its own. It bounds no personal access token, a service account's included (below),
and no CI token (§11): each has its own.

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
disabled (`409`); a service account does not count (below). A rename changes nothing but the
name: the bindings and tokens keep working, and the audit log keeps the name of each event's
time.

### Service accounts

A **service account** (#1020, [ADR 0021](../adr/0021-service-accounts.md)) is a user of kind
`service`, for a system rather than a person. It has no password and no OIDC identity, and
nothing logs it in: it authenticates with its personal access tokens only, as Basic
`<name>:<token>` or `Bearer <token>`. Everything else is a user's: its uid, its name among the
names of the users, its bindings, a rename, disabling, enabling, deleting, and the principal
`user:<uid>` of its requests. Setting its password is `400`.

Its **owner** is required: a live user that is not a service account, given as `user:<name>` or
`user:<uid>` and kept as `user:<uid>`, or a group, `group:<name>` with a name of §8, which need
not have members. The owner answers for the account and gets no right on it, nor on its tokens,
by owning it. The admin role creates a service account and changes its owner. What disabling a
user owner does is `auth.serviceAccounts.onOwnerDisabled`: with `flag`, the default, nothing for
its service accounts, which the users API marks `ownerDisabled`; with `disable`, the transaction
that disables the owner disables its enabled service accounts too, each with an audit event of its
own. Enabling the owner again enables none of them: each is enabled by itself. Deleting a user
that owns a live service account is `409`, naming them, whatever the setting, so that the owner of
a service account is a live user or a group.

Its tokens are personal access tokens, whose scopes narrow its bindings as a person's do.
Creating one takes the admin role and the admin's password, or the command line (§3): the
account has no password and never mints its own. Their lifetime is bounded by
`auth.serviceAccounts.maxTokenTTL` instead of a person's year, 365 days by default: a token
created without an expiry expires at the bound, and one asked to expire later is `400`. `0` bounds
nothing, and a token created without an expiry then never expires. A bound lowered later counts
for the tokens created after it; a live token beyond it is answered with `beyondMaxTTL: true` and
counted at start, as a deploy token beyond `auth.deployTokens.maxTTL` is (above).

A service account may have any binding, `admin` included. Its `admin` binding does not count for
the last-admin rule (above, §3) nor for the bootstrap (§6), as a group's does not: without a
password it cannot make a token once its own expire. That rule never refuses disabling or
deleting it, nor deleting its `admin` binding.

### Inactive users

A user's **last activity** (#1283,
[ADR 0027](../adr/0027-inactive-users-disabled-at-authentication.md)), `users.last_seen_at`
([index-schema.md](index-schema.md#a-users-last-activity)), is the time of its last successful
authentication: its password (HTTP Basic, `/v2/token`, `npm login`, a web UI's
`POST /api/v1/login`), a personal access token of its own (in Basic, as a bearer, a web UI's token with its proof, §10), or an
OIDC login (§8: the callback that verified the ID token, for a browser, a web UI or a device). A
service account authenticates with its tokens only, so their use is its activity. Nothing else
is: the bearer of §4 and the archive token of a terraform download are issued for an
authentication, which counts, and are no credential of the user; an admin's change of the user
(a password set, a token made for it, a binding, a rename) is not the user's. A deploy token, a
CI token (§11) and the principals of §9 are no user, and nothing here concerns them.

An authentication reads the user's row for every credential anyway (§3). After the credential
has verified and the check below has passed, it records the activity as a request records a
token's use (#1277, `tokens.last_used_at`): when the `last_seen_at` it read is NULL or a minute
old or more, with a write that only moves it forward and moves neither the row's revision nor
`auth_version`. An OIDC login records it in its transaction. So the record is less than a minute
behind, costs no read, and is at most one write a minute per active user across the replicas. A
record that fails is logged at debug and refuses nothing, and the next request writes again.

**Disabling an inactive user.** `auth.disableInactiveUsersAfter` (configuration.md) is `0` by
default, which disables nobody; PCI DSS 8.2.6 is `2160h`, 90 days. A user is **inactive** when it
is live and enabled, and the setting has passed since its last activity, or since its creation
when it has none. No task looks for inactive users (decided on #1283, 2026-11-16): the
authentication of one disables it. Once its credential has verified (a password by argon2id, a
token by its hash, neither revoked nor expired, the ID token of an OIDC login), the credential is
refused, and the user is disabled in a transaction that reads its row again and changes it only
while it is still enabled and inactive: one `user.disable` row (§5). So from the moment the
setting has passed, no credential of the user is taken on any replica, and its row says
`disabled` from the first one tried. A credential that does not verify disables nobody: trying
names and passwords disables no one and tells nothing.

The refusal is a disabled user's: `401` with the challenge and without the reason, and for a
password a failed login of the account and the address (§1). The page of an OIDC login says that
the account was disabled for inactivity and that an admin can enable it, and the login writes no
`user.login` row. The refusal is logged at info with the user's principal, its last activity and
the setting. With `auth.serviceAccounts.onOwnerDisabled` `disable`, the user's service accounts
are disabled with it, as with every disabling (above).

**The last enabled admin** (the rule above) is never disabled so: its credential is taken and
its activity recorded, which makes it active again, and a warning is logged with its principal
and its last activity. A service account's and a group's `admin` bindings do not count for the
rule, so a service account with one, and a user who is an admin through a group only, are
disabled as any user.

**Enabling** a user restarts its clock: the transaction of its `user.enable` sets
`last_seen_at` to that time, so that the person is not disabled again by the next request.
Creating a user sets nothing: a user that never authenticates is inactive once the setting has
passed since its creation.

**What is shown.** The users API (http-routing.md §4) and `lajstrom admin user list` answer
`lastSeenAt` for every user, `null` before any record, and, while the setting is not `0`,
`inactive: true` for an inactive user: before an authentication disables it, and for the last
enabled admin, whom none disables. The access report (#1279) reads the two. With the setting not
`0`, `serve` logs at warn at start how many users are inactive, when there are any, the last
enabled admin among them if it is, so that an operator who sets or lowers it sees whom it reaches
before their next authentication disables them (as with `beyondMaxTTL` above). The setting counts from
the start of the `serve` that has it: a lowered one makes the users beyond it inactive at once,
and a raised one makes active again those it no longer reaches, unless they are disabled
already.

**Upgrades.** A user of a release before migration 0034 has the last activity the migration
gives it (index-schema.md, A user's last activity): the time of the migration for a user with a password, whose
password logins left no record before it, and the latest use of its tokens and of its OIDC
logins for a user without one, a service account or an OIDC user. So the upgrade alone makes no
user with a password inactive; a service account or an OIDC user whose records are older than
the setting is inactive at once. A snapshot of before 0034 is restored alike (index-schema.md,
Snapshots of the control plane).

## 2. Resources and actions

Resource: a repo, identified by `<format>/<repo>`. Prefix-level resources (a path inside a repo)
are after the MVP. Three system-level resources, which only the `admin` role reaches: `system`
(users, bindings, registries, creating a repo, every user's tokens, every token at once
(`GET /api/v1/tokens`), the deploy tokens over a mask, the webhooks of any mask, the CI trust policies of §11), `audit`, and
`policy` (the policy rules of any mask other than one repo, and the list of every rule;
[policy.md](policy.md), #101).

| Action | Meaning |
| --- | --- |
| `read` | GET/HEAD on the frontend; `pull` scope on `/v2`; list in `/api/v1/repos`; search in `/api/v1/search` |
| `write` | PUT/POST on the frontend; `push` scope on `/v2`; create versions |
| `attach` | attach to a version: the `PUT` of a referrer on `/v2` (manifest-profile.md §6: a signature, an SBOM, a scan report, provenance, a verdict, any manifest with a `subject`) and the blob uploads it needs; the `push` scope on `/v2` for a principal without `write`, which the token carries as `attach` (§4) (#89) |
| `delete` | DELETE versions/manifests; `delete` scope on `/v2` |
| `manage` | change the repo (policy, anonymous read, deploy tokens) or delete it; reindex it and read its latest run (`GET …/reindex`); read its open publication sessions (`GET …/sessions`); purge a proxy's upstream cache (§7); its webhooks, those whose mask is exactly the repo, their secrets, tests and delivery logs ([webhooks.md](webhooks.md)); its policy: the rules whose mask is exactly the repo, and the deny of a version and its removal, which the server signs ([policy.md](policy.md) §7, #101) |

`write` implies `attach`: a role, a binding, a personal access token's scopes or a deploy
token's scopes that have `write` have `attach` too, so a token made before `attach` existed keeps
attaching (decided on #89, 2026-11-16). `attach` alone pushes no version: a manifest that is no
referrer needs `write`. Attachments are written through `/v2` only; the management API lists them
(`read`) and has no route that writes one (#89, option G). Deleting an attachment is `delete`.

On npm's routes (http-routing.md, npm), changing a package's dist-tags or deprecations is
`write`, the `DELETE` of a dist-tag included, and an unpublish is `delete`, a version that a
packument update leaves out included. A verdict pushed through `/v2` as a referrer needs `attach`,
as every referrer does, and `write` holds it (#89, option C; ADR 0008, Amendment I). Yanking and unyanking a pypi version through the
management API (http-routing.md §4, #81) is `write` too: like a deprecation it is an entry of the
package's journal and deletes nothing, and the version's files stay served.

## 3. Roles and bindings

| Role | Actions |
| --- | --- |
| `reader` | read |
| `attacher` | read, attach |
| `writer` | read, write |
| `maintainer` | read, write, delete, manage |
| `admin` | everything on every repo plus `system` and `audit` (system role; the mask is ignored) |

A `maintainer` binding on `*/*` has every action on every repo, and neither `system` nor
`audit`: those need the `admin` role. `writer`, `maintainer` and `admin` attach through `write`;
`attacher` is the role of a scanner or a signing job that reads a repo and attaches to its
versions, and publishes nothing (#89).

A **binding** is `(principal, role, repo_mask)`. Masks: exact `apt/ubuntu-internal`, format-wide
`apt/*`, name-across-formats `*/payments`, everything `*/*`. Wildcards only as a whole segment.
A principal may have many bindings; the effective permission on a repo is the union.

A binding is refused with `400`, naming the field, when:

- its principal is neither a live user, `user:<name>` (stored as the user's `user:<uid>`) or
  `user:<uid>`, nor a group, `group:<name>` with a name of §8, nor a `cert:` or `k8s:`
  principal or pattern of §9. A `ci:` principal (§11) has no bindings: its rights are its
  trust policies'. A group need not have members:
  a binding may come before anyone logs in with the group, as before its repo. `anonymous`
  reads through a repo's `anonymousRead` and gets nothing else, and a deploy token
  `token:<id>` gets its repo, or the repos of its mask, and its scopes and nothing else, so
  neither has bindings;
- its principal is a `cert:` or `k8s:` principal or pattern (§9) and its role is `admin`;
- its role is not one of the five;
- its mask is not two segments `<format>/<repo>`, each `*` or a name: a format of this build,
  and a repo name by manifest-profile.md §4.1. The repo need not exist: a binding may come
  before its repo, and outlives it;
- its role is `admin` and its mask is not `*/*`: the mask of an admin binding is ignored, and
  `*/*` says so.

The same `(principal, role, repo_mask)` twice is `409`, and so is deleting an `admin` binding
of an enabled user when no enabled user would keep one: nobody could manage the server through
`/api/v1` after it. A group's `admin` binding does not count for this rule nor for the bootstrap
(§6): a group lapses without a change of the bindings (§8). Neither does a service account's
(§1), which cannot make a token once its own expire.
`lajstrom admin binding create` on the command line can always make one (§6).

Evaluation of `allowed(principal, action, format/repo)`. Steps 2 and 3 depend on the
credential, not on the bindings, and are checked first: a PAT's scopes narrow an `admin` too.

1. `admin` binding → allow.
2. Deploy token → allow iff the repo is the token's repo, or its mask matches the repo as a
   binding's mask matches (whole segments; never `system` nor `audit`), and `action` is in its
   scopes (`read`/`write`/`attach`, `write` holding `attach`); never `delete`/`manage`.
   CI token (§11) → allow iff a grant of its trust policies has a mask that matches the repo as
   a binding's does and a role that includes `action`; never `system` nor `audit`.
3. PAT → compute as for its user, then intersect with the PAT's scopes if set.
4. User → any binding of the user, or of a group the user is in now (§8), whose mask matches
   the repo and whose role includes the action. A group is not a principal of a request: it
   reaches a repo only through its members.
   A `cert:` or `k8s:` principal (§9) → any binding of it, or of a pattern of §9 that matches
   it, whose mask matches the repo and whose role includes the action.
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
- `403` is only for a principal that can read the repo but lacks the action (`write`, `attach`,
  `delete`, `manage`): it already knows that the repo exists. A principal with `attach` and
  without `write` gets `403 DENIED` for a manifest `PUT` of `/v2` that is no referrer, once the
  body is parsed (http-routing.md §2). A deploy token on its own repo gets `403`
  for an action outside its scopes, `read` included for a `write`-only one: it is bound to the
  repo and knows it. A deploy token over a mask is bound so to every repo its mask matches and
  gets the same `403` on each; its scopes, `read` or `write`, let it find which of them exist
  anyway. A repo outside its mask is `404`, as for any principal with no access at all. A CI
  token (§11) is bound so to every repo a mask of its grants matches: `403` for an action its
  roles lack there, `404` on a repo no mask of its grants matches.

Every other route that could tell whether a repo exists keeps the rule too, and how is part of
this spec, so that a review need not find it in the code:

- **Creating a repo** (`POST /api/v1/repos`) is `system` (§2). The `admin` role is checked
  before the body is read, so the `409` of a name that is taken, and the `400` of a body that
  does not parse, reach an admin only, who sees every repo anyway. Every other principal gets
  `403` (`anonymous` `401`) whatever the name: trying names by creating repos tells nothing.
- **Bindings, users and the audit log** are `system` and `audit`: admin only. A binding's mask
  need not name a repo that exists (above), so creating one tells nothing either.
- **A repo's deploy tokens** need `manage` on it, decided before the repo is looked up: the
  answers of this rule. Its list holds its own tokens, not those over a mask that matches it.
  A CI token (§11) creates none, whatever its grants: `403` before the body is read.
- **A repo's open publication sessions** (`GET …/sessions`, http-routing.md §4) need `manage` on
  it, decided before the repo is looked up: the answers of this rule. Their files are not
  published yet, and `read` does not show them.
- **Every token and the registries** (`GET /api/v1/tokens`, `GET /api/v1/registries`) are
  `system` (§2): the `admin` role, checked before anything else, so that every other principal
  gets `403` (`anonymous` `401`), a `maintainer` on `*/*` included. A filter on a repo that does
  not exist answers what one on a repo without tokens answers, to the admin only.
- **A repo's `permissions`** (`GET /api/v1/repos`, http-routing.md §4) are the answers of the
  evaluation above for the request on a repo it may read: they tell nothing of another repo.
- **The deploy tokens over a mask** (`/api/v1/deploy-tokens`, http-routing.md §4) are `system`
  (§2). The `admin` role is checked before the body is read, so the `400` of a mask that breaks
  the grammar and the `409` of a name that is taken reach an admin only; every other principal
  gets `403` (`anonymous` `401`), a `maintainer` on `*/*` included. **Creating one takes the
  admin's password** as the request's credential, as creating a personal access token does
  (below): a personal access token, with scopes or without, the admin's too, and a deploy token
  get `403` before the body is read, naming the rule; an admin without a password, an OIDC user
  (§8), makes one on the command line. A stolen token must not mint a token that needs no expiry
  and outlives its revocation (decided on #911, 2026-11-14). Listing and revoking them take the
  admin role with any credential that reaches it. A mask names no repo, so the routes tell
  nothing of which repos exist.
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
  one itself (§8), and is no credential of this route. A service account has no password, so
  only the admin role creates its tokens here (§1). A personal access token, with scopes or without, the admin's too, a deploy token
  and a CI token (§11) get `403` before the body is read, whoever's tokens they name: a stolen token must not mint
  tokens that outlive its revocation (decided on #249, 2026-11-10). A personal access token
  without scopes still lists and revokes its user's tokens; one with scopes and a deploy token
  get `403`. The command line (`lajstrom admin token create`) has no credential and is the
  operator's.
- **Lists** (`GET /api/v1/repos`, `/v2/_catalog`) show only the repos the principal may read.
- **Search** (`GET /api/v1/search`, #956) runs over the repos the principal may read, as the list
  shows them: a repo it may not read adds no hit, a digest or a name that only such a repo holds
  is not found, and the answer is the same whether such a repo exists or not. `anonymous`
  searches the repos it may read, and gets `200`, with no hits when there are none.
- **`/v2` with a bearer**: the token names every repository asked for, with no action on one
  that does not exist or may not be read alike, and the facade asks the token before it looks
  the repo up (§4).

The rule is about the answers, not their durations: a denial on a repo that exists may take a
database read longer than the answer for one that does not. Making them equal is not in the
MVP.

## 4. OCI bearer tokens

`GET /v2/token?service=<host>&scope=repository:<format>/<repo>/<pkg>:pull,push&scope=…`, and
`POST /v2/token` with the same fields in the OAuth2 form.

- `GET` authenticates with Basic (or none for anonymous). `POST` takes the OAuth2 form of the
  password grant, whose `username` and `password` are checked as Basic credentials are, with
  the same throttles, and needs them (http-routing.md §2, #908); both issue the same token.
  The service is the server's own: a `service` that names another is ignored.
- For each requested scope the facade keeps the actions the principal is allowed on
  `<format>/<repo>` (`pull`↔`read`, `push`↔`write`, `delete`↔`delete`); on the path of an `oci`
  group, `pull` at most, since nothing is pushed or deleted through it (§7). A `push` that the
  principal may not have as `write` and may as `attach` is granted as the action `attach`, which
  no client asks for: the access claim names it, and the facade takes with it the blob uploads
  and the `PUT` of a referrer, and nothing else that `push` allows (#89). A scope with no allowed
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
  (clients authenticate again, with Basic or the form). A token whose `sub` names a user by
  name, issued before the uid (#182), is refused like any token that does not verify: `401`,
  and the client gets another.
- The facade validates the bearer on every `/v2` request and enforces the actions, by the token
  before it looks the repo up, so that the answers keep §3: a repository the token does not name
  is `401` with `error="insufficient_scope"`, whether its repo exists or not; one it names with
  no action gets the answer of a repo that does not exist (`404 NAME_UNKNOWN`, `401` to
  `anonymous`); one it names without the action is `403 DENIED`, as the principal can read it.
  A cross-repo mount from a source the token names with no action is a plain upload session.
  The backend registry is never exposed to clients and trusts only Lajstrom (network policy or
  the backend's own credentials held by the provider).
- A token is trusted until its `exp`, and the facade does not evaluate the bindings again for
  a request it decides by the token. The bindings, a disabled or inactive user (§1), a revoked
  or expired personal access or deploy token, and a repo's `anonymousRead` count when a token is
  issued:
  on `/v2` a change takes effect at the client's next `/v2/token`, at most 20 minutes later for
  a token issued before it. A repo deleted is unknown at once (`NAME_UNKNOWN`): the token names
  repositories by name. Two requests are evaluated when they come, as on every other route: one
  with Basic credentials instead of a bearer, and `/v2/_catalog` with a token that holds
  `registry:catalog:*`, which lists the repos the token's principal may read at that moment.
- To end every token now, `lajstrom admin key rotate --token --drop` (operations.md): within a
  second no replica accepts a token of the old key, and clients get new tokens with their
  credentials, under the bindings of that moment.
- The download of a terraform module (http-routing.md, terraform) issues a token too, for its
  principal once it may read the repo, with the claims above and one access entry,
  `{"type": "terraform-archive", "name": "terraform/<repo>/<package path>:<tag>", "actions":
  ["pull"]}`: the version's OCI reference. The archive route of that version takes it, by its
  signature, `exp`, service and that entry, and evaluates no binding; `/v2` grants nothing for
  its type, as for any type that is not `repository`. `--drop` ends it with the others.

## 5. Audit

Every write (`push`, `delete`), every control-plane change, and every creation and revocation of
a personal access token or a deploy token is an `audit` row with the principal, action, repo,
subject, digest, client IP and request id. The subject of a token's row is its owner: the
user's `user:<uid>`, a repo's `<format>/<repo>`, which is the row's `repo` too, or the mask of a
deploy token over a mask, with no `repo`. A change of the index or of the control plane writes
its row in its transaction. A write to the registry, which no transaction spans, has its row
before the write, `pending`, in a transaction of its own, and the row says how the write ended:
`ok`, in the transaction that records the write in the index; `failed`, with the error; or
`unknown`, when it is still pending after `ingest.claimTTL` and its writer stopped in between
([index-schema.md](index-schema.md#the-audit-row-of-a-registry-write),
[guarantees.md](guarantees.md) INV-14). The creation's row also
names the credential that created the token, `createdBy` in its details: `password`,
`token:<id>` for a token, or `oidc` for the token of an OIDC login (§8); and, for a deploy token
or a service account's token created under a bound of §1, `maxTTL`, the seconds of that bound,
so that the log shows the bound of each creation (#1286); a creation refused writes no row. The
token of a web UI's sign-in refused as idle (§10) is revoked by the server: a `token.revoke` row by
`anonymous`, with `source` `idle` and `reason` `idle`. A service account's requests have the
principal `user:<uid>` and the name of every user, and the answers of the audit log add
`principalKind` `service` to its rows; its `user.create` row has `kind` `service` and
`owner` in its details, a change of its owner is `user.owner`, with `from` and `owner`, and a
service account that its owner's disabling disables (§1) is a `user.disable` of its own, with the
owner as `owner` in its details. A user that its inactivity disables (§1) is a `user.disable` by
`anonymous`, with the `source` `inactivity`, the address and the request id of the request whose
credential found it inactive, and in its details `reason` `inactive`, its `lastSeenAt` (absent
when it had none) and the setting as `after` (`2160h`); a service account disabled with it has
`owner` in its details, as above. A webhook's creation, change, deletion and the rotation of its secret are `webhook.create`,
`webhook.update`, `webhook.delete` and `webhook.secret` (#77), in the transaction of the change:
`repo` the webhook's mask, `subject` `webhook:<id>`, `details` `{"mask", "urlHost", "events",
"active"}`, the host of its URL and never the URL nor the secret, with the origin's keys of the
other rows (`source`, `requestId`, and `host`, the machine of a command-line change). A policy rule's creation and deletion are `policy.rule.create` and
`policy.rule.delete` (#101), in the transaction of the change: `repo` the rule's mask, `subject`
`rule:<id>`, `details` the rule's fields. The rows of a verdict that ingest writes or deletes are
`policy.verdict.create` and `policy.verdict.delete`, in their transaction: `repo` the repo,
`subject` `<name> <version>[ <variant>]` (an `oci` repo's path), `digest` the judged manifest's,
`details` `{"verdict", "reason", "source", "scope", "validUntil", "attestation"}`. A request that
policy refuses is `policy.refuse`, in a transaction of its own ([policy.md](policy.md) §6.6): the
one read that is audited, so that a refusal can be traced to its rule or verdict. A test delivery and the
deliveries are not audited: the delivery log has them (webhooks.md §4). Reads are not audited in the MVP (a per-repo option later). The
short-lived bearer tokens of `/v2/token` are not audited either (decided on #46, 2026-11-09): a
client gets one for every pull and push, and what a token allows is audited itself. A CI token's
exchange is: `token.exchange`, a row per token minted and, rate-limited, per refusal; and so is
every change of a CI trust policy through the API, `ci.policy.create`, `ci.policy.update`,
`ci.policy.renew`, `ci.policy.approve` and `ci.policy.delete`, and the mark of one whose expiry
passed, `ci.policy.expire` (§11).

**The origin of a row** (#1284) is in its `details`, under the same keys whatever the action:
`source` (`api`, `cli`, `config`, `bootstrap`, `npm`, `expiry`, …), `requestId` for a change over
HTTP, `host`, the machine of a change of the command line or the replica of a change of
`serve`'s start, and, on a row of the command line, `osUser`, `actor` and `changeReason` (§6).
Every row with `source: cli` has `host` and `osUser`, the rows of `lajstrom admin key` included.
`changeReason` is the key of a person's reason for a change, whatever its source: a reason given
over the API (#1285) goes there too. It is not `reason`, which rows already hold with other
meanings: a rule's text (`policy.rule.create`), why something was refused (`proxy.reject`,
`token.exchange`), a verdict's reason.

**A throttle of failed logins that trips** (§1) is `login.block` (#1284), by `anonymous`, in a
transaction of its own, written by the failure that fills the budget of a window: the 10th
failed password of an account in its minute, the 30th of a client address, the 10th wrong
bootstrap token of an address (§6). So it is written at most once per throttle, key and window
on a replica. `subject` is the account, `user:<uid>`, or `unknown` for a name that no user has
(never the name), or, for a throttle of an address, the address (an IPv6 address as its /64);
`ip` is the address of the failure; `details` `{"limit", "credential", "failures", "window",
"until", "requestId"}`: the throttle (`account`, `address` or `bootstrap`), the kind of
credential (`password`, `bootstrap`), the failures counted, the window, and when the subject's
credentials are checked again (RFC 3339). A replica writes at most 60 of them a minute, whatever
the keys: beyond, a trip is counted, not written, and the next row it writes carries
`suppressed`, the trips left out since its last row; the `throttled` lines and
`lajstrom_auth_throttled_total` have every one. A row that cannot be written is logged at error
and changes no answer. An account lockout (#1280) is meant to write this action too, with a
`limit` of its own, so that one query finds every account and address that was blocked.

**A change of the configuration** is `config.change` (#1284), by `anonymous`, `source: config`,
written by `serve` at its start when the configuration it runs with is not the last one recorded
on the database:

- **What counts** is the effective configuration, after the defaults, the file and the
  environment ([configuration.md](../configuration.md#changes-of-the-configuration)): every key
  but `database.*` (its DSN may hold a password, and the record is in that database), `log.*`
  (a knob of one run, which flags override), `server.listen` and `server.metricsListen` (an
  address of each replica, such as a pod's IP) and `repos` (their effect has its own rows,
  `repo.create` with `source: config`). A key added later counts unless it is said here.
- **No secret.** A key that names where a secret is counts by that name only, the path or the
  variable, never by the secret nor by a hash of it, which would let a weak password be guessed
  offline: `server.tls.keyFile`, `auth.bootstrap.tokenFile`, `auth.oidc.clientSecretFile`,
  `keys.masterKeyEnv`, `keys.masterKeyFile`, `registries[].auth.passwordEnv`,
  `upstreams[].auth.passwordEnv`, `upstreams[].auth.passwordFile` and
  `upstreams[].auth.tokenFile`. A key that names a file of public material counts by its path and
  the sha256 of the content the start read, so that a CA added to a bundle's file is a change:
  `server.tls.certFile`, the `caFile` of `auth.clientCertificates.bundles[]`,
  `auth.kubernetes.clusters[]` and `auth.ci.issuers[]`, `attachments.trust[].publicKeyFile` and
  `attachments.keyless.trustedRoot`.
- **Keys.** An item of a list of named items is written with its name,
  `auth.clientCertificates.bundles[corp].caFile`; a list of values or a map is one value,
  `server.trustedProxies`, `auth.ci.policies[deploy].claims`. The **fingerprint** is
  `sha256:<hex>` of the canonical JSON of the keys and their values, in key order.
- **One row per change, not per replica.** After its migrations and the repos of the file, and
  before it listens, `serve` reads `config_state` (index-schema.md). When it has no row, or a
  fingerprint other than the replica's, `serve` replaces the row, compare-and-set on the
  fingerprint it read, and appends `config.change` in the same transaction. Of replicas that
  start together with one new configuration, one writes; the others find theirs recorded and
  write nothing. One that loses to another configuration reads the new row and compares again.
  A start that cannot record its change stops, as a failed migration does: no configuration
  takes effect without its row.
- **The row:** `repo` empty, `subject` the new fingerprint, `details` `{"source", "host",
  "release", "previous", "initial", "changes", "truncated"}`: the replica's host name, the
  binary's version, the fingerprint before; `initial: true` for the first record of a database,
  without `previous`, whose changes are every key; the changes in key order, each `{"key",
  "from", "to"}`, without `from` for a key added and without `to` for one removed; and, when the
  changes would take the details past 64 KiB, the number of those left out, `config_state`
  holding them all.

A release that adds a key or changes a default changes the configuration: the first start of it
writes the row, whose `release` says why. Replicas that run with different configurations write a
row at each start that changes the record, which is how it shows that they differ.
`lajstrom admin` and `lajstrom reindex` record no configuration: their rows say what they did.

## 6. Bootstrap

No admin password is read from the environment or the configuration (#440). The first admin
is made in one of two ways, both with an `admin` binding on `*/*`:

- **The bootstrap token.** The operator makes it and gives it as the file of
  `auth.bootstrap.tokenFile`. While no enabled user has an admin binding (a service account's
  does not count, §1), `serve` makes it valid
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
  audit events have the source `cli` and the `host` the command ran on, and say who ran it as
  far as Lajstrom can tell (below).

**Who ran a command** (#1284). Whoever holds the database's credentials is the operator, so a
command against the database authenticates nobody: its rows stay by `anonymous`. Every row that a
command of `lajstrom` writes against the database (`lajstrom admin`, `lajstrom reindex`) has
`source: cli`, `host`, the host name of the machine, and `osUser`, the operating-system user the
command runs as (`user.Current`): its name, or `uid:<n>` where the system knows no name for it
(a container's arbitrary uid). In a container (`kubectl exec`, a Job) neither names a person: the
person is in the operator's records, the Kubernetes audit log of the exec or of the Job's
creation, or the bastion's, which the `host` (the pod's name) and the time lead to. Two optional
values say more, each from a flag or, without it, from the environment:

- `--actor` (`LAJSTROM_ACTOR`): who runs it, as the operator names people (an e-mail address, a
  login of the IdP), in `details.actor`. A label the person asserts, and nothing more: never
  resolved to a user, never the row's principal, granting nothing. 1 to 128 bytes of UTF-8
  without control characters, and not in the form of a principal of §1 (`user:…`, `token:…`,
  `group:…`, `cert:…`, `k8s:…`, `ci:…`, `anonymous`), so that no reader takes it for one.
- `--reason` (`LAJSTROM_REASON`): why, a ticket or a sentence, in `details.changeReason` (§5). 1
  to 1024 bytes of UTF-8 without control characters.

A value outside its bounds is a usage error, exit status `2`, before anything is written; an empty
variable counts as unset, and a flag wins over its variable. The same values go into every row the
command writes. Each command that writes rows against the database takes both flags; the
commands that go over HTTP (`admin bootstrap`, `admin retention`, `admin repo prefetch`), whose rows
the server writes for the credential they send, and those that write no row do not. On
`lajstrom admin policy rule create`, `--reason` is the rule's, which clients see; the reason of
the change comes from `LAJSTROM_REASON` there.

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
| `write` | the referrers of `/v2` (a signature or an attestation of a proxied artifact); the format's publish routes are `405` | nothing of its own: a write through the group is its hosted member's; through an `oci` group, and an apt group of several members (proxy-and-group.md §8.4), every write is `405` |
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
only; a `write` scope is `400`, since it would grant nothing. A deploy token over a mask may have
`write` whatever its mask matches: on a group, it gets what `write` gets any principal there, a
write through it to the hosted member when its mask matches the member too.

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
| `proxy.fetch` | a proxied ingest: the fetcher pushed an artifact | the principal of the request whose miss or prefetch caused it (a user, a token, `anonymous`), through a group too; `repo` the proxy, `subject` the version, `digest` the manifest's, `ip` the client's; `details` `{"upstream", "url", "checksum", "size"}`, `checksum` `""` for a file fetched unchecked (manifest-profile.md §3.1), and `via`, the group, when there was one |
| `proxy.reject` | a fetch refused for its bytes: a checksum that differs, an artifact that `Validate` refuses, a file above a bound; or, before any download, a name the registry does not take (#599, #662) | as `proxy.fetch`, without `digest`; `details.reason` (`checksum`, `invalid`, `size`, `name`) and, for a checksum, both values; `via` through a group, an `oci` one included |
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
token) is §11, and joining an existing user to an identity is not in this section (decisions on
#69).

**The identity.** An identity is the pair (`iss`, `sub`) of a verified ID token
(index-schema.md, `identities`), and maps to one user. The first login of an identity creates
the user, `kind` `oidc`, without a password, named by the claim `auth.oidc.usernameClaim`
(default `preferred_username`), which must pass the rule of a user name (§1); a claim that is
missing or breaks the rule refuses the login, naming the claim. A name that a live user has
already refuses it too, naming the name: an identity is never joined to an existing user by
its name. Later logins find the user by the identity and keep its name, which a rename (§1)
may change. A disabled user is refused at login as on every request, and an inactive one is
disabled and refused (§1). A deleted user lets its
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
in one transaction with the audit rows, the identity's user is found or created, its groups
replaced and its last activity recorded (§1): `user.create` for a first login (`details.source`
`oidc`), and `user.login` (`details`: `issuer`, `subject`, `groups`). The browser's login mints
a personal access token of the user without scopes, named `oidc <time>`, which expires after
`auth.oidc.tokenLifetime` (24h by default, at most a year), with its `token.create` row (`createdBy` `oidc`), and answers
it once, on a page with `Cache-Control: no-store`; a login that a web UI started hands the UI a
code instead (§10). A refusal at the IdP (`error=`), a state that
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

## 9. Node and workload identity (after the MVP, #1019)

Two credentials prove who a client is without a secret of Lajstrom's: a client certificate that
a configured CA signed, which containerd and Talos present for a node, and a Kubernetes
service-account token for image pulls (KEP-4412), which the kubelet hands its credential
provider for a pod ([ADR 0025](../adr/0025-node-and-workload-identity.md)). Each is a credential
of `/v2/token` only; no other route reads it. Each gives a principal of its own:

| Principal | Form |
| --- | --- |
| client certificate | `cert:spiffe://<trust domain>/<path>`, the SPIFFE ID of a certificate of a `spiffe` bundle; `cert:<bundle>/<name>`, of a `cn` or `dns` bundle, `<name>` the certificate's CN or first DNS SAN |
| Kubernetes service account | `k8s:<cluster>/<namespace>/<service account>` |

A bundle's or a cluster's name is the name of its entry in the configuration (configuration.md,
`auth`): lowercase letters and digits with single hyphens between them, at most 63 bytes, and
never `spiffe`. Neither principal has a row, a password, a token of Lajstrom or an owner: it
exists through its credential, as a group exists through the identities that name it. Neither is
a user, a service account (§1) or a deploy token, and none of them maps onto another; they share
the bindings of §3 and nothing else.

**Bindings.** A `cert:` or `k8s:` principal takes the bindings of §3 with the roles `reader`,
`writer` and `maintainer`; `admin` is `400`. A binding's principal may also be a **pattern**: a
principal of these kinds whose last segment is `*`, which matches every principal of its kind
that begins with the pattern without its `*` and goes on past it, at any depth.
`cert:spiffe://cluster-a/*` matches every SPIFFE ID of the trust domain `cluster-a`,
`cert:spiffe://cluster-a/node/*` those below `/node/`, `cert:corp/*` every certificate of the
bundle `corp`, `k8s:prod/*` every service account of the cluster `prod`, and `k8s:prod/ci/*`
those of its namespace `ci`. A binding's principal of these kinds is refused with `400` when it
breaks this grammar:

- `cert:spiffe://<trust domain>/<path>`: the trust domain of lowercase letters, digits, `.`, `-`
  and `_`; the path one or more segments of letters, digits, `.`, `-` and `_`, none `.` or `..`,
  or a last segment `*`;
- `cert:<bundle>/<name>`: the name 1 to 255 bytes of printable ASCII without a space and without
  `*`, or a last segment `*` after `<bundle>/` or after a `/` of the name;
- `k8s:<cluster>/<namespace>/<service account>`: the namespace a DNS label (RFC 1123, at most 63
  bytes), the service account a DNS subdomain (at most 253 bytes), or `k8s:<cluster>/*`, or
  `k8s:<cluster>/<namespace>/*`.

A `*` anywhere else is `400`, and the principal of a request never has one (below). A binding
need not name a bundle or a cluster that is configured, as it need not name a repo that exists.

### Client certificates

**Bundles.** `auth.clientCertificates.bundles` lists the CAs Lajstrom takes client certificates
from, each with a `name`, a `caFile` of PEM certificates, and an `identity`: `spiffe` (the
default), with the bundle's `trustDomain`, one bundle per trust domain; `cn`; or `dns`. The files
are read at start: a changed bundle takes a restart.

**Where the certificate comes from.** From one place for a request, by its peer:

- **The connection**, when Lajstrom terminates TLS (`server.tls`) and the peer is not in
  `server.trustedProxies`. With a bundle configured, the listener asks every client for a
  certificate, with the bundles' CAs as the acceptable ones, and verifies one that is sent: a
  client without one goes on as before, and one whose certificate no bundle's CA signed fails the
  handshake. The certificate of a trusted proxy, whatever it is, is never its client's.
- **The edge's header**, `auth.clientCertificates.forwardedHeader`, when the peer is in
  `server.trustedProxies`; from any other peer the header is not read. `X-Forwarded-Client-Cert`
  is read as Envoy writes it: elements separated by `,`, keys and values by `;` and `=`, values
  in `"…"` where they need it; the last element counts, and its `Cert`, a URL-escaped PEM
  certificate, with its `Chain`, URL-escaped PEM intermediates, when present. Any other header
  holds a URL-escaped PEM certificate followed by any intermediates (nginx's
  `$ssl_client_escaped_cert`, ingress-nginx's `ssl-client-cert`), or base64 DER certificates
  separated by `,` (Traefik, HAProxy). The edge must set the header itself and drop one that a
  client sent.

**Verification.** The bundles are tried in their order. The first whose CAs verify the chain,
the leaf with the intermediates that came with it, at the server's clock, for client
authentication (an extended key usage `clientAuth`, `any`, or none), decides; a leaf that is a
CA is refused. The certificate's name is then:

- under `spiffe`: the one URI SAN of the leaf, which must be its only URI SAN and a SPIFFE ID of
  the bundle's trust domain with a path (X509-SVID); the principal is
  `cert:<the SPIFFE ID>`;
- under `cn`: the leaf's subject CN; under `dns`: its first DNS SAN; the principal is
  `cert:<bundle>/<name>`.

A name must be 1 to 255 bytes of printable ASCII without a space and without `*`. A certificate
that a bundle verifies and that has no name by it is refused; it is not tried against the next
bundle.

**On `/v2/token`.** A `GET` without an `Authorization` header and with a certificate that
verifies is issued the token of the certificate's principal (§4); Basic credentials, when they
are sent, are the credential instead, and the certificate is not read. A `POST` takes the form's
credential only. A certificate that does not verify, which only the edge's header can bring, or
that has no name by its bundle, is `401` with the Basic challenge, logged at debug with the
reason and the bundle, never the certificate.

**Revocation.** Neither a CRL nor OCSP is read: a certificate counts until its `notAfter`. A binding
removed counts from the next `/v2/token` on (§4); a bundle removed, at the next start.

### Kubernetes service-account tokens

**Clusters.** `auth.kubernetes.clusters` lists the clusters whose service-account tokens
Lajstrom takes, each with a `name`, the `issuer` (the exact `iss` of its tokens; https, http only
with `server.insecureHTTP`), the `audience` (by default `server.externalURL`) and a `caFile` for
an issuer whose TLS certificate a private CA signed. No two clusters have the same issuer.

**The token on `/v2/token`.** The password of Basic credentials (`GET`) or of the form (`POST`)
is a cluster's token when it has the form of a JWT (three base64url parts separated by `.`) and
the `iss` of its payload, read before it is verified, is that cluster's issuer; the user name is
not read. Any other password is checked as §1 says. The token must verify:

- its signature, by the key of its `kid` in the issuer's JWKS, with an algorithm of §8, never
  `none` nor an HMAC;
- `iss` the cluster's issuer, which its discovery document
  (`<issuer>/.well-known/openid-configuration`) must name exactly as its `issuer`, beside the
  `jwks_uri`;
- `aud` containing the cluster's audience;
- `exp` present and passed by no more than 60 s, `iat` and `nbf` not more than 60 s ahead;
- the claim `kubernetes.io` with `namespace` and `serviceaccount.name`, a DNS label and a DNS
  subdomain, and `sub` equal to `system:serviceaccount:<namespace>:<name>`.

Its principal is `k8s:<cluster>/<namespace>/<name>`. The discovery document is read again
hourly, and the JWKS is cached as the OIDC login's (§8): a `kid` it does not have fetches it
again at most once every 30 s, it is fetched again hourly, and while the issuer does not answer
the last document and set that loaded stay. Nothing is read at start. A token that does not
verify is `401` with the Basic challenge and counts as no failed login (§1), as a token secret
does not; an issuer whose keys never loaded is `503` with `Retry-After`. Lajstrom does not ask
whether the pod or the service account of a token still exists: the token counts until its
`exp`.

### Tokens and audit

The bearer of a `cert:` or `k8s:` principal names it as its `sub`, and is issued as any (§4):
the bindings count when it is issued, and it is trusted for its 20 minutes. Such bearers are not
audited (§5), and each one issued is logged at debug level with its principal and its bundle or
cluster. The writes they allow are audit rows with the principal, which
`lajstrom.uploaded-by` carries too, and a change of a binding of these kinds is a
`binding.create` or `binding.delete` row as any.

## 10. Sign-in of a web UI (#70)

A web UI ([ADR 0022](../adr/0022-web-ui-sign-in-with-short-lived-tokens.md)) holds a personal
access token in the browser, bound to a key of the browser, and sends it with a proof of that key;
it never gets a cookie for `/api/v1`. `POST /api/v1/login` (http-routing.md §4) gives it one, for
a password or for an OIDC login. The token is a personal access token of the user, without
scopes, as one that the user makes, and everything of §1 and §3 holds for it: it is listed with
the user's tokens, revoked with `DELETE …/tokens/<id>` (the UI's sign-out, with the token
itself), ended by disabling or deleting the user, and creates no token. It expires after
`auth.login.tokenLifetime` (12h by default, at most 24h); there is no refresh token. With
`auth.login.idleTimeout`, it also ends once unused for that long (Idle, below).

**Bound to a key (DPoP, RFC 9449; decided on #1170, 2026-11-16).** Every login takes a DPoP
proof, a JWT that the UI signs with a key of its own, an ES256 (P-256) key that the browser keeps
and no script reads out; the token is bound to that key, `tokens.dpop_jkt` (index-schema.md), its
JWK SHA-256 thumbprint (RFC 7638), which the token's views answer as `dpopJkt`. The token
authenticates only on `/api/`, as `Authorization: DPoP <token>` with a new proof of that key for
the request's method and URI and the token's hash, each proof used once on any replica
(http-routing.md §4). It is refused everywhere else, as a bearer, in Basic credentials, on the
frontends, on `/v2` and on `/v2/token`: no client of a package format and nobody who copies it
off the browser without the key can use it. The `token.create` row of a bound token has
`dpopJkt` in its details. A token the person makes with `POST …/tokens` or the command line is
bound to no key, and a bound token is never sent as a bearer.

**A password.** The user name and the password of the body are checked as Basic credentials are
(§1): argon2id every time, the throttles of failed logins per account and per address, a token
in place of the password refused as a wrong password, every refusal `401` without its reason.
The token is named `login <time> <4 hex>`, and its `token.create` row has `createdBy`
`password` and the source `api`, as npm's login has.

**An OIDC login.** The UI makes a PKCE verifier (RFC 7636), keeps it, and sends the person to
`/-/oidc/login` with `return_to`, a page of the UI, `code_challenge`, the verifier's S256
challenge, and `dpop_jkt`, the thumbprint of its DPoP key (RFC 9449 §10). `return_to` must be an
absolute URL on the origin of `server.externalURL` or of `server.corsOrigins`, without a
fragment: the callback redirects only to the UI's own pages. The login request keeps the three
(index-schema.md, `oidc_logins`). The login runs as in §8, with its refusals and its
`user.login` row; at its end the callback mints no token and shows no page: it makes a code of
256 random bits, keeps its sha256 and the user on the login request, which becomes `approved`,
and answers `302` to `<return_to>#code=<code>`. A fragment reaches no server and no `Referer`.
`POST /api/v1/login` with `{"code", "codeVerifier"}` takes the login request of that code,
`approved` and within the 10 minutes of the login, compares the S256 of the verifier with the
challenge and the thumbprint of the proof's key with `dpop_jkt`, in constant time. On a match it
marks the request `redeemed`, once, on any replica, and mints the token as §8 does for the
browser, named `oidc <time> <4 hex>`, bound to the key, with its `token.create` row (`createdBy`
`oidc`), expiring after the shorter of `auth.login.tokenLifetime` and `auth.oidc.tokenLifetime`;
the groups of the login count for `auth.oidc.tokenLifetime`, as for every login. A wrong verifier
or key marks the request `denied`, so a code is tried once: a code read from the history or from
the page is of no use without the key that started the login. No code, verifier or challenge is
logged, audited, or put in a URL but the one redirect.

**The sign-in at the IdP** (#937). The callback of a web UI's login keeps the ID token's
`auth_time` and `amr` (RFC 8176; its strings of at most 64 bytes, at most 16 of them) on the login
request, and the token of its code keeps them (index-schema.md, `tokens.auth_time` and
`tokens.amr`), as does its `token.create` row (`authTime`, `amr`): a request with that token and a
proof of its key carries when and how the person authenticated, which a step that needs a fresh
sign-in with a second factor reads (§11). A login with `prompt=login` (http-routing.md §7) asks
the IdP to authenticate the person again, `prompt=login` and `max_age=600` on its authorization
request; its token is a token of a sign-in as any other, of the same lifetime, and its sign-in is
fresh for the 10 minutes after `auth_time`. A token of a password, of a login of §8 and of an ID
token without `auth_time` keeps none.

**Idle (decided on #1277, 2026-11-16).** With `auth.login.idleTimeout` (configuration.md; `0`,
the default, is off; `15m` for PCI DSS 8.2.8), a token bound to a key whose last use is more than
that before a request is refused, and the person signs in again. It holds for the tokens of
`POST /api/v1/login` only, those with `dpop_jkt`: the sessions of a web UI. A token the person
makes, the token of an OIDC login of §8 (its token page, `lajstrom login`), a deploy token, a
service account's token and a CI token are the credentials of a client, which authenticates every
request and holds no session: a job that runs once a day would be refused, and their expiry bounds
them. Every request that the token authenticates is a use, whatever it asks, so a request that a UI
makes on a timer keeps its sign-in alive: a UI makes none with the token while the person does
nothing (lajstrom-ui), and asks `GET /api/v1/info` without it.

- **The last use** is `tokens.last_used_at`, or `created_at` for a token not used yet. A request
  with a token of any kind writes it when the value read with the token, which every request reads, is a minute old or
  more, or null; the write only moves it forward. So it is written at most once a minute per
  token on all replicas together (but for replicas that read the same value at once), and is less
  than a minute behind the last use. A token is refused after between `idleTimeout` less a minute
  and `idleTimeout` without use, never later, in the clocks of the replicas (guarantees.md, clocks
  agree). A write that fails is logged and refuses nothing; the next request writes again.
- **The refusal** comes after the proof of the token's key verified and after the token passed
  the checks of revocation, expiry and its user: `401 UNAUTHENTICATED` with
  `WWW-Authenticate: DPoP error="invalid_token", error_description="idle", algs="ES256"`
  (http-routing.md §4). A UI tells it from a token revoked or expired, whose challenge has no
  `error_description`; whoever sends the token without its key never learns it.
- **The refusal revokes the token**, once on any replica, with its `token.revoke` row (§5), by
  `anonymous`, `source` `idle`, the request's id, and in its details `reason` `idle`, `lastUsedAt`
  and `idleTimeout` (seconds) beside `id`, `kind` and `name`. An idle sign-in so ends for good: a
  later change of `idleTimeout` does not bring it back, and the sign-out of a UI whose own timer
  ran out (`DELETE …/tokens/<id>`, with the token) revokes it all the same. A token never sent
  again, a tab closed, stays unrevoked until it expires, refused as idle by every replica.
- **Its lifetime and a fresh sign-in.** `idleTimeout` is `0`, or from `5m` to less than
  `auth.login.tokenLifetime`; the token ends at the earlier of its expiry and its idle bound. A
  use moves no `auth_time`: a step that needs a fresh sign-in (§11) still needs one of the last 10
  minutes, and the token of a login with `prompt=login` is bound as any other. `GET /api/v1/info`
  answers the value (`login.idleTimeout`, http-routing.md §4), so that a UI warns the person
  before the server refuses.
- A replica of a release before it refuses nothing as idle: the bound counts once every replica
  knows it.

## 11. Token exchange for CI (after the MVP, #937)

A CI job trades the OIDC token its platform issues it, GitHub Actions' or GitLab CI's, for a
token of Lajstrom's of minutes, the **CI token**, at `POST /-/oidc/token` (RFC 8693,
http-routing.md §7). Trust policies in the configuration decide which tokens are taken and what
the CI token may do ([ADR 0026](../adr/0026-token-exchange-for-ci.md)). Nothing is stored per
job but the audit row of the exchange.

**Issuers.** `auth.ci.issuers` (configuration.md, `auth`) lists the platforms whose tokens are
taken, each with a `name`, a `platform` (`github` or `gitlab`), the `issuer` (the exact `iss` of
its tokens; https, http only with `server.insecureHTTP`), the `audience` (by default
`server.externalURL` without a trailing `/`) and a `caFile` for an issuer whose TLS certificate a
private CA signed. A name follows the rule of §9's names; no two issuers have the same name or
the same issuer.

**Trust policies.** A policy comes from the configuration, `auth.ci.policies`, or from the API,
`/api/v1/ci/policies` (http-routing.md §4) and `lajstrom admin ci-policy`; each has one owner
(ADR 0015) and is described below. A policy has a `name` (the rule of §9's names, unique among
both), an `issuer` (the name of an issuer of `auth.ci.issuers`), `claims` and `grants`.
`claims` maps a claim to a value or a list of values: a token matches the policy when, for every
claim of the map, the token has the claim and its value matches one of the values. `grants` is a
list of one or more `{role, mask}`: a role `reader`, `writer` or `maintainer` (`admin` is
refused), and a mask of §3, two segments `<format>/<repo>`, each `*` or a name: a format name
(http-routing.md §1) and a repo name by manifest-profile.md §4.1. A format that this build does
not have matches no repo. The claims a policy may name, by platform:

| Kind | GitHub | GitLab |
| --- | --- | --- |
| id | `repository_id`, `repository_owner_id`, `actor_id` | `project_id`, `namespace_id`, `user_id` |
| name | `repository`, `repository_owner`, `actor` | `project_path`, `namespace_path`, `user_login` |
| pattern | `ref`, `environment`, `workflow_ref`, `job_workflow_ref` | `ref`, `ref_path`, `environment`, `ci_config_ref_uri` |
| value | `ref_type`, `ref_protected`, `event_name`, `runner_environment`, `repository_visibility` | `ref_type`, `ref_protected`, `environment_protected`, `pipeline_source`, `runner_environment`, `deployment_tier`, `project_visibility` |

A claim of the token is compared as a string: a string as it is, a number or a boolean as its
JSON text (`123`, `true`); any other value matches nothing. An id is decimal digits. A value of a
pattern may hold `*`, which matches any run of characters but `/`, and `**`, which matches any
run; no other character is special, and a value without `*` matches itself only. A value of
any other claim matches itself only, and holds no `*`. The configuration is refused at start, and
a policy of the API with `400` naming the field, when a policy:

- names a claim that is not in the table for its issuer's platform, or a value that is empty,
  made only of `*`, not decimal for an id, or holds `*` in a claim that is not a pattern;
- names its project by no id: neither `repository_id` nor `repository_owner_id` on GitHub,
  neither `project_id` nor `namespace_id` on GitLab. A name beside an id is a condition too;
- grants `writer` or `maintainer` and has no condition on `ref`, `ref_path` or `environment`;
- grants `writer` or `maintainer` and its `event_name` (GitHub) or `pipeline_source` (GitLab)
  names only events of a pull request (below);
- grants `admin`, or a mask that breaks the grammar above, or nothing.

**Policies of the API.** The CI trust policies are a resource of `system` (§2): the routes and the
command line take the admin role, checked before the body is read. A policy made through them is
a row of `ci_policies` (index-schema.md), owned by the API; one of the configuration is shown
through the API with `source` `config`, and is changed only in the configuration: a `PUT`, a
`DELETE`, a renewal or an approval of it is `409`, and so is a `POST` with the name of either. The
same rules as the configuration's apply, and its issuer must be an issuer of the configuration. A
policy of the API that grants `reader` only is made, changed and deleted by an admin and counts at
once; one that grants `writer` or `maintainer` takes the steps below. A row that breaks a rule, has
the name of a policy of the configuration, or writes without an expiry or outside
`auth.ci.apiWriteRepos` (restored from a snapshot, or the configuration changed since) is left out
of the policies the replica evaluates, logged at warn. Every create, change, renewal, approval,
expiry and delete moves `auth_version` and the policy's revision in its transaction, and writes
its row (Audit, below) and its event (webhooks.md §1). Each replica reads the policies of the
index again when `auth_version` moved, which it reads before each exchange and each request with a
CI token, and when the earliest expiry among them passed: a change counts from the next request
on, on every replica, without a restart.

**Policies of the API that write** (decided on #937, 2026-11-16). A policy of the API that grants
`writer` or `maintainer`, made, changed so that it still grants one, or renewed, takes these
steps; deleting one, or changing it to grant `reader` only, takes none:

1. **A fresh sign-in with a second factor.** The request's credential is the token of a web UI's
   sign-in through OIDC (§10), with a proof of its key, whose ID token's `auth_time` is at most 10
   minutes before the request (and at most 60 s after it), and whose `amr` (RFC 8176) shows a
   second factor: `mfa`, or methods of two kinds of factor, knowledge (`pwd`, `pin`, `kba`),
   possession (`hwk`, `swk`, `otp`, `sms`, `tel`, `sc`, `pop`) and inherence (`fpt`, `face`,
   `iris`, `retina`, `vbm`). A web UI gets one with a login with `prompt=login` (http-routing.md
   §7), which asks the IdP to authenticate the person again. A password's sign-in, a token the
   person made, a deploy token, a service account's token, a client certificate, a Kubernetes
   token, a CI token and the command line never are one: `403` saying what is missing.
2. **A second admin.** With `auth.ci.apiWriteApproval: second-admin`, the default, the policy
   waits, `state` `pending`, and grants nothing until an admin other than the one who made the
   change (`updatedBy`) approves it: `POST …/<name>/approve` with a fresh sign-in of step 1 and
   `If-Match`, the ETag of the revision read (`400` without one or with `*`, `412` for another),
   so that an approval names what it approves. With `none`, for an install with one admin, the
   change counts at once, and the other steps hold. An install with one admin may instead declare
   its policies that write in the configuration, where the review in git is the second pair of
   eyes.
3. **Bounded repos.** The mask of each grant that writes is within a mask of
   `auth.ci.apiWriteRepos` (configuration.md): every repo it matches, one of those matches
   (`pypi/app` and `pypi/*` within `pypi/*`, `*/*` within `*/*` only). Empty, the default, no
   policy of the API writes: a policy that writes elsewhere is declared in the configuration. Else
   `403` naming the grant; an approval checks it again.
4. **An expiry.** The policy has `expiresAt`, at most 180 days after the change and by default
   180 days after it; one beyond, or past, is `400`. `POST …/<name>/renew` gives it another, under
   steps 1 to 3: the renewal of a policy that counts waits beside it (`renewal`), the policy
   keeping its expiry until the renewal is approved; that of an expired policy waits pending; that
   of a pending one is `409` (a change gives it its expiry). From its `expiresAt` on a policy
   grants nothing, whether a replica marked it yet or not. Each replica looks once a minute for
   the policies of the API whose expiry passed and marks each `expired` in a transaction that
   changes it only at the revision it read, so that of the replicas one marks it, with its
   `ci.policy.expire` row and its event. An expired policy is renewed or deleted.
5. **Events.** Every create, approval, change, renewal, expiry and delete of a policy of the API,
   one that reads included, is an event of the webhooks (webhooks.md §1, `ci.policy.*`).

A policy of the API has a `state`: `active`, which counts; `pending`, which waits for an approval
and grants nothing; `expired`, which grants nothing. What waits, a pending policy or the renewal
of one that counts, is approved once; an approval when nothing waits, or after the expiry, is
`409`. A change of a policy that writes waits for its approval again, whatever it changed. The
command line, `lajstrom admin ci-policy`, is no sign-in: it makes the policies that read, lists
every policy, and deletes any.

**A pull request never writes.** A policy that grants `writer` or `maintainer` never matches a
token of a pull request, whatever its conditions: on GitHub one whose `event_name` is
`pull_request`, `pull_request_target`, `pull_request_review` or `pull_request_review_comment`; on
GitLab one whose `pipeline_source` is `merge_request_event` or `external_pull_request_event`.
Such a job may run the code of a fork with the identity of the base project. A policy that grants
`reader` only matches them as its conditions say.

**The exchange.** The subject token must be a JWT whose `iss`, read before it is verified, is the
`issuer` of an issuer of `auth.ci.issuers`, and must verify as a Kubernetes token does (§9): its
signature by the key of its `kid` in the issuer's JWKS, with an algorithm of §8, never `none` nor
an HMAC; `iss` the issuer, which its discovery document must name exactly; `aud` containing the
issuer's audience; `exp` present and passed by no more than 60 s, `iat` and `nbf` not more than
60 s ahead. The discovery document and the JWKS are cached as §9 says, and nothing is read at
start. Then its project is read: GitHub's `repository` or GitLab's `project_path`, one or more
segments of letters, digits, `.`, `-` and `_` separated by `/` (two on GitHub), none `.` or `..`,
at most 255 bytes. Every policy of the issuer is matched; the token is taken when at least one
matches, and refused otherwise. A taken token is answered with a CI token, minted then, and the
`token.exchange` row of its exchange.

**The CI token.** Its principal is `ci:<issuer>/<project>`, `<issuer>` the name of the issuer in
the configuration: `ci:github/acme/app`, `ci:gitlab/acme/platform/app`. The principal has no
row, no password, no owner and no bindings: it exists through its token. The token is `ljs_ci_`
followed by a JWT signed with the token key of §4 (its algorithm and `kid`), header `typ`
`ljs-ci+jwt`, with the claims `iss` the service, `aud` `lajstrom:ci`, `sub` the principal, `jti`
16 random bytes in base64url (the token's id), `iat`, `nbf`, `exp` `auth.ci.tokenLifetime` later
(15 minutes by default, from 1 to 20), and `policies`, the names of the policies that matched.
It is a credential wherever a deploy token is (§1): the password of Basic under any user name, on
every route and on `/v2/token` (`GET`, and the `POST` form); `Authorization: Bearer` outside
`/v2`. It must verify: the prefix, the signature by a token key that verifies (§4), the `typ`,
`aud` `lajstrom:ci`, `exp` required, 60 s of leeway, `sub` a `ci:` principal. A token that does
not verify is `401` with the challenge, and counts as no failed login.

**What it may do.** The grants of the policies the token names, as the replica that serves the
request has them: step 2 of §3. A policy removed from the configuration grants nothing from that
replica's restart on, one of the API deleted, pending again or past its expiry from the next
request on, and a token none of whose policies the replica has is `401`. It never reaches `system` nor `audit`, and creates no personal access token and no
deploy token (§3). The bearer `/v2/token` issues on it names the `ci:` principal as its `sub` and
is issued as any (§4): its access is reduced by the grants when it is issued, and it is trusted
for its 20 minutes. It carries the CI token's id as the claim `ci`.

**Revocation.** A CI token is not revoked alone. It ends at its `exp`; a policy of the API deleted,
changed to wait for an approval, or past its expiry, ends it at its next request, one of the
configuration removed at the restart of each replica;
`lajstrom admin key rotate --token --drop` ends it with every bearer (§4).

**Audit.** A token minted is a `token.exchange` row (§5), in a transaction of its own, by the
`ci:` principal: `subject` the principal, no `repo`, and `details` with `source` `ci`, `outcome` `minted`,
`issuer` (the name), `policies`, `token` (the `jti`), `expiresAt`, and `claims`, the claims of the token
that the table names for its platform, with `run_id`, `run_attempt` and `sha` on GitHub and
`pipeline_id`, `job_id` and `sha` on GitLab, each a string of at most 256 bytes; never the subject
token, the CI token, nor `user_email`. The rows that ingest and the facade write for a request of
a CI token (its pushes, deletes and a proxy's fetches) are rows of its principal whose
`details.ciToken` is the CI token's id, also through a bearer that names one (`ci`), and
`lajstrom.uploaded-by` names the principal.

A refusal of the subject token (`400 invalid_grant`) is a `token.exchange` row too, with
`outcome` `refused` and the `reason`: by the `ci:` principal when the token verified and its
project keeps the grammar, by `anonymous` otherwise; `details` with `issuer` when the token's
`iss` names one, and, for a token that verified only, `claims` limited to the ids, the project,
`ref`, `ref_type`, `environment` and the event (`event_name`, `pipeline_source`). The rows of
refusals are rate-limited on each replica by a key, `<issuer>/<project id>` (`repository_id`,
`project_id`) for a token that verified and the client address otherwise: one row a minute per
key, whose `details.suppressed` counts the refusals of its key left without a row since the row
before. At most 10 000 keys are kept, and a refusal of a key beyond them writes no row. Each
refusal is logged at info as well, with its reason, the issuer and the project when the token
verified, never the token.

A create, change, renewal, approval or delete of a policy through the API is `ci.policy.create`,
`ci.policy.update`, `ci.policy.renew`, `ci.policy.approve` or `ci.policy.delete`, in its
transaction, by the admin, `subject` the policy's name, no `repo`, and `details` with the policy as
the change left it (as it was, for a delete): `issuer`, `claims`, `grants`, `state`, and
`expiresAt` and `renewalExpiresAt` when it has them; the `previous` policy for a change, a renewal
and an approval; `signIn`, `{token, authTime, amr}`, for a request with the sign-in of a web UI
(§10); and the origin's keys (`source` `api` or `cli`, `requestId`, `host`). The mark of a policy
whose expiry passed is `ci.policy.expire`, by `anonymous`, with `source` `expiry`.
