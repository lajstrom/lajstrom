# Spec: authentication, tokens and RBAC

- Status: normative for the MVP (2026-11-06). Owned by `internal/auth`.
- Related: [ADR 0007](../adr/0007-rbac-model.md), [http-routing.md](http-routing.md).

## 1. Principals

| Principal | Form | MVP |
| --- | --- | --- |
| local user | `user:<uid>` (below); password (argon2id) | yes |
| personal access token (PAT) | acts as its user, optionally narrowed by `scopes`; expiry mandatory (max 1 year, default 90 days) | yes |
| deploy token | `token:<id>`; bound to exactly one repo; scopes `read` and/or `write`; expiry optional | yes |
| anonymous | `anonymous`; only ever gets `read`, and only on repos with `anonymousRead: true` | yes |
| group | `group:<name>`; from OIDC claims | after the MVP |
| OIDC user | `user:<uid>`, of the user its IdP's `sub` maps to | after the MVP |

One credential works on every protocol: HTTP Basic on the frontends and on `/v2/token`
(`user:password`, `user:PAT`, or `deploy:<token>`), and the resulting bearer on `/v2`.
Passwords and token secrets are never logged; tokens are stored hashed (sha256) and shown once.

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
(users, bindings, registries, creating a repo, every user's tokens) and `audit`.

| Action | Meaning |
| --- | --- |
| `read` | GET/HEAD on the frontend; `pull` scope on `/v2`; list in `/api/v1/repos` |
| `write` | PUT/POST on the frontend; `push` scope on `/v2`; create versions |
| `delete` | DELETE versions/manifests; `delete` scope on `/v2` |
| `manage` | change the repo (policy, anonymous read, deploy tokens), reindex it |

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

- its principal is not a live user, `user:<name>` (stored as the user's `user:<uid>`) or
  `user:<uid>`. `group:<name>` comes with OIDC, after the MVP. `anonymous` reads through a repo's `anonymousRead` and gets nothing else, and a
  deploy token `token:<id>` gets its repo and its scopes and nothing else, so neither has
  bindings;
- its role is not one of the four;
- its mask is not two segments `<format>/<repo>`, each `*` or a name: a format of this build,
  and a repo name by manifest-profile.md §4.1. The repo need not exist: a binding may come
  before its repo, and outlives it;
- its role is `admin` and its mask is not `*/*`: the mask of an admin binding is ignored, and
  `*/*` says so.

The same `(principal, role, repo_mask)` twice is `409`, and so is deleting the last `admin`
binding of an enabled user: nobody could manage the server through `/api/v1` after it.
`lajstrom admin binding create` on the command line can always make one (§6).

Evaluation of `allowed(principal, action, format/repo)`:

1. `admin` binding → allow.
2. Deploy token → allow iff the token's repo matches and `action` is in its scopes
   (`read`/`write`); never `delete`/`manage`.
3. PAT → compute as for its user, then intersect with the PAT's scopes if set.
4. User/group → any binding whose mask matches the repo and whose role includes the action.
5. Otherwise, if `action == read`, the repo has `anonymousRead`, and the server-wide switch
   `auth.anonymousRead` is on → allow (also for the `anonymous` principal).
6. Deny.

There is no mode without this evaluation: anonymous reads where both switches allow it and does
nothing else, on every server.

A change of the bindings, or of a repo (created, deleted, its `anonymousRead` changed), counts
from the next request on, on every replica, as a user disabled does. Each replica evaluates in
memory, from a snapshot of the bindings and of the repos' `anonymousRead`; before it evaluates a
request it reads the counter `auth_version` (index-schema.md), which every such change moves in
its own transaction, and reloads the snapshot when the counter moved. `auth.anonymousRead` is
configuration: it changes with a restart.

A denial on `<format>/<repo>` must not reveal whether the repo exists:

- the `anonymous` principal gets `401` with a Basic challenge, on a repo it may not read and on
  a repo that does not exist alike, so that clients send credentials;
- an authenticated principal with no access to the repo at all (not even `read`) gets `404`,
  the same answer as for a repo that does not exist;
- `403` is only for a principal that can read the repo but lacks the action (`write`, `delete`,
  `manage`): it already knows that the repo exists.

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
- **A personal access token's scopes** are actions; they name no repo.
- **Lists** (`GET /api/v1/repos`, `/v2/_catalog`) show only the repos the principal may read.
- **`/v2` with a bearer**: the token names every repository asked for, with no action on one
  that does not exist or may not be read alike, and the facade asks the token before it looks
  the repo up (§4).

The rule is about the answers, not their durations: a denial on a repo that exists may take a
database read longer than the answer for one that does not. Making them equal is not in the
MVP.

## 4. OCI bearer tokens

`GET /v2/token?service=<host>&scope=repository:<format>/<repo>/<pkg>:pull,push&scope=…`

- Authenticates with Basic (or none for anonymous).
- For each requested scope the facade keeps the actions the principal is allowed on
  `<format>/<repo>` (`pull`↔`read`, `push`↔`write`, `delete`↔`delete`). A scope with no allowed
  action stays in the token with `actions: []`, and so does the scope of a repo that does not
  exist: the client can read the token, and it names every repository asked for without telling
  which exist (§3). Asking for what one may not have is not an error of the token endpoint, as
  in the distribution token spec.
- Issues a JWT (`ES256`, key from `keys`), `exp` 20 minutes, claims: `sub` (the principal,
  `user:<uid>` for a user), `access: [{type: repository, name, actions}]`, `aud` = service,
  `jti`. Also returns `refresh_token` = none (clients re-authenticate with Basic). A token whose
  `sub` names a user by name, issued before the uid (#182), is refused like any token that does
  not verify: `401`, and the client gets another.
- The facade validates the bearer on every `/v2` request and enforces the actions, by the token
  before it looks the repo up, so that the answers keep §3: a repository the token does not name
  is `401` with `error="insufficient_scope"`, whether its repo exists or not; one it names with
  no action gets the answer of a repo that does not exist (`404 NAME_UNKNOWN`, `401` to
  `anonymous`); one it names without the action is `403 DENIED`, as the principal can read it.
  A cross-repo mount from a source the token names with no action is a plain upload session.
  The backend registry is never exposed to clients and trusts only Lajstrom (network policy or
  the backend's own credentials held by the provider).
- A token is trusted until its `exp`, and the facade does not evaluate the bindings again on
  `/v2`. The bindings, a disabled user, a revoked or expired personal access or deploy token,
  and a repo's `anonymousRead` count when a token is issued: on `/v2` a change takes effect at
  the client's next `/v2/token`, at most 20 minutes later for a token issued before it. A repo
  deleted is unknown at once (`NAME_UNKNOWN`): the token names repositories by name.
- To end every token now, `lajstrom admin key rotate --token --drop` (operations.md): within a
  second no replica accepts a token of the old key, and clients get new tokens with their
  credentials, under the bindings of that moment.

## 5. Audit

Every write (`push`, `delete`), every control-plane change and every token issuance is an
`audit` row with the principal, action, repo, subject, digest, client IP and request id. Reads
are not audited in the MVP (a per-repo option later).

## 6. Bootstrap

`lajstrom admin user create --name admin --admin` creates the first user and an `admin` binding
from the command line (no HTTP), prompting for the password or reading
`LAJSTROM_ADMIN_PASSWORD`. The bindings of other users are made with
`lajstrom admin binding create|list|delete` from the command line (#45), which needs the database
and no credentials, or with `/api/v1/bindings` (#47). Everything else goes through `/api/v1`.
