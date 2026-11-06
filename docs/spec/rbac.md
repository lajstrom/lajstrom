# Spec: authentication, tokens and RBAC

- Status: normative for the MVP (2026-11-06). Owned by `internal/auth`.
- Related: [ADR 0007](../adr/0007-rbac-model.md), [http-routing.md](http-routing.md).

## 1. Principals

| Principal | Form | MVP |
| --- | --- | --- |
| local user | `user:<name>`; password (argon2id) | yes |
| personal access token (PAT) | acts as its user, optionally narrowed by `scopes`; expiry mandatory (max 1 year, default 90 days) | yes |
| deploy token | `token:<id>`; bound to exactly one repo; scopes `read` and/or `write`; expiry optional | yes |
| anonymous | `anonymous`; only ever gets `read`, and only on repos with `anonymousRead: true` | yes |
| group | `group:<name>`; from OIDC claims | after the MVP |
| OIDC user | `user:<subject or preferred_username>` | after the MVP |

One credential works on every protocol: HTTP Basic on the frontends and on `/v2/token`
(`user:password`, `user:PAT`, or `deploy:<token>`), and the resulting bearer on `/v2`.
Passwords and token secrets are never logged; tokens are stored hashed (sha256) and shown once.

## 2. Resources and actions

Resource: a repo, identified by `<format>/<repo>`. Prefix-level resources (a path inside a repo)
are after the MVP. Two system-level resources: `system` (users, bindings, registries) and
`audit`.

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

A **binding** is `(principal, role, repo_mask)`. Masks: exact `apt/ubuntu-internal`, format-wide
`apt/*`, name-across-formats `*/payments`, everything `*/*`. Wildcards only as a whole segment.
A principal may have many bindings; the effective permission on a repo is the union.

Evaluation of `allowed(principal, action, format/repo)`:

1. `admin` binding → allow.
2. Deploy token → allow iff the token's repo matches and `action` is in its scopes
   (`read`/`write`); never `delete`/`manage`.
3. PAT → compute as for its user, then intersect with the PAT's scopes if set.
4. User/group → any binding whose mask matches the repo and whose role includes the action.
5. Otherwise, if `action == read` and the repo has `anonymousRead` → allow (also for the
   `anonymous` principal).
6. Deny.

A denial on `<format>/<repo>` must not reveal whether the repo exists:

- the `anonymous` principal gets `401` with a Basic challenge, on a repo it may not read and on
  a repo that does not exist alike, so that clients send credentials;
- an authenticated principal with no access to the repo at all (not even `read`) gets `404`,
  the same answer as for a repo that does not exist;
- `403` is only for a principal that can read the repo but lacks the action (`write`, `delete`,
  `manage`): it already knows that the repo exists.

## 4. OCI bearer tokens

`GET /v2/token?service=<host>&scope=repository:<format>/<repo>/<pkg>:pull,push&scope=…`

- Authenticates with Basic (or none for anonymous).
- For each requested scope the facade keeps the actions the principal is allowed on
  `<format>/<repo>` (`pull`↔`read`, `push`↔`write`, `delete`↔`delete`); scopes with no allowed
  action are dropped, as the distribution token spec expects.
- Issues a JWT (`ES256`, key from `keys`), `exp` 20 minutes, claims: `sub` (principal),
  `access: [{type: repository, name, actions}]`, `aud` = service, `jti`. Also returns
  `refresh_token` = none (clients re-authenticate with Basic).
- The facade validates the bearer on every `/v2` request and enforces the actions; the backend
  registry is never exposed to clients and trusts only Lajstrom (network policy or the backend's
  own credentials held by the provider).

## 5. Audit

Every write (`push`, `delete`), every control-plane change and every token issuance is an
`audit` row with the principal, action, repo, subject, digest, client IP and request id. Reads
are not audited in the MVP (a per-repo option later).

## 6. Bootstrap

`lajstrom admin user create --name admin --admin` creates the first user and an `admin` binding
from the command line (no HTTP), prompting for the password or reading
`LAJSTROM_ADMIN_PASSWORD`. Everything after that goes through `/api/v1`.
