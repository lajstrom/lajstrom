---
id: M2-07
title: "feat(auth): RBAC: roles, bindings with masks, enforcement in frontends and facade scopes, anonymous read"
milestone: M2
lane: core
size: L
priority: p0
labels: [area/auth, kind/feature, contract]
depends: [M2-05, M2-06, M1-03]
---
**Goal**

The evaluator of `docs/spec/rbac.md` §3, used by every frontend route and by the token endpoint.

**Scope**

- `bindings` CRUD; `Authorizer.Allowed(principal, action, format, repo) bool` as an in-memory
  evaluator over a cached bindings snapshot (reloaded on change via a version counter in the
  store); mask matching with whole-segment wildcards.
- Enforcement: a `Require(action)` middleware for plugin routes (plugins declare the action per
  route: GET/HEAD → read, PUT/POST → write, DELETE → delete); `/api/v1` requires `manage` or
  admin; `/v2/token` scope reduction uses the same evaluator.
- `anonymous_read` per repo; `anonymous` principal; 404 vs 403 behaviour of the spec.
- Default bindings on bootstrap: the first admin.

**Acceptance criteria**

- [ ] Table test of the evaluation algorithm (every rule of rbac.md §3, including PAT narrowing
      and deploy tokens).
- [ ] e2e `rbac.sh`: reader cannot push (frontend and `oras`), writer can, maintainer can
      delete, deploy token is confined, anonymous read toggles, unknown repo is 404 for everyone.
- [ ] Performance: 100k evaluations/s on a laptop with 1k bindings (benchmark in the PR).
- [ ] docs: `docs/auth.md` complete for the MVP.
