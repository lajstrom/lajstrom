---
id: M2-06
title: "feat(auth): personal access tokens and deploy tokens"
milestone: M2
lane: core
size: M
priority: p0
labels: [area/auth, kind/feature]
depends: [M2-05]
---
**Goal**

CI jobs get a credential scoped to one repo; people get tokens instead of passwords in
`~/.pypirc` and `settings.xml`.

**Spec and context**

`docs/spec/rbac.md` §1; `docs/spec/index-schema.md` (`tokens`).

**Scope**

- Token format `ljs_<kind>_<random 32 bytes base62>`; stored sha256; shown once; `expires_at`
  mandatory for PATs (default 90 d, max 365 d), optional for deploy tokens; `last_used_at` updated
  at most once a minute; `revoked_at`.
- PAT acts as its user, optional `scopes` narrowing (`read`, `write`); deploy token is a
  principal bound to one repo with scopes `read`/`write`.
- Authenticator: Basic with `user:ljs_pat_…` or `deploy:ljs_dep_…` (and `anything:ljs_dep_…`
  for clients that require a user name); bearer `Authorization: Bearer ljs_pat_…` accepted on
  `/api/v1`.
- API: `GET/POST /api/v1/users/{name}/tokens`, `DELETE …/{id}`,
  `GET/POST /api/v1/repos/{format}/{repo}/deploy-tokens`, `DELETE …/{id}`; CLI equivalents.

**Acceptance criteria**

- [ ] tests: expiry, revocation, scope narrowing, deploy token on the wrong repo → 403, the
      token value never appears in logs (grep the test log output).
- [ ] e2e: `twine upload` with a deploy token; `apt` with `user:token` in `auth.conf`.
