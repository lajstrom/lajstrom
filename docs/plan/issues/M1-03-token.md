---
id: M1-03
title: "feat(facade): token endpoint and bearer validation (static users in M1, RBAC in M2)"
milestone: M1
lane: core
size: M
priority: p0
labels: [area/facade, area/auth, kind/feature]
depends: [M1-01]
---
**Goal**

The docker bearer flow: `GET /v2/` → 401 with `WWW-Authenticate`, `GET /v2/token` with Basic →
JWT, bearer checked on every `/v2` request with scopes enforced per repo and action.

**Spec and context**

`docs/spec/rbac.md` §4; distribution token spec.

**Scope**

- `internal/auth`: JWT issue/verify (ES256, key file from config, generated on first start if
  missing with a warning), `access` claims, 20-minute expiry, clock skew tolerance.
- M1 principal source: `auth.static` users from config (name, password hash, `admin: true`) and
  an `anonymousRead` flag; replaced by the control plane in M2-05/M2-07 behind the same
  `Authorizer` interface.
- Scope parsing (`repository:<name>:pull,push`), reduction to allowed actions, multiple `scope`
  params, the `service` param.
- Middleware on `/v2`: anonymous `pull` allowed when configured, else 401 with the challenge;
  insufficient scope → 403 `DENIED`.

**Acceptance criteria**

- [ ] `oras login localhost:8080 -u user -p pass` then push/pull works; wrong password → 401;
      a read-only user cannot push (403).
- [ ] `docker login` + `docker pull` through the facade of a manifest in an `oci` test repo
      works (bearer flow compatibility), even though image passthrough is post-MVP.
- [ ] tests: scope reduction table, expired token, wrong audience, key rotation (two keys).
- [ ] docs: `docs/auth.md` started.
