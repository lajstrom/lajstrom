---
id: M0-08
title: "feat(api): repo management v1: /api/v1/repos, config bootstrap, admin repo CLI"
milestone: M0
lane: core
size: M
priority: p0
labels: [area/api, kind/feature]
depends: [M0-01, M0-04]
---
**Goal**

Repos exist before anything can be pushed (no auto-create, ADR 0004). Create and list them via
the API, via the `repos:` section of the config, and via `lajstrom admin repo`.

**Spec and context**

`docs/spec/http-routing.md` §4; `docs/spec/index-schema.md` (`repos`); ADR 0004.

**Scope**

- `internal/api`: `GET/POST /api/v1/repos`, `GET/PATCH/DELETE /api/v1/repos/{format}/{repo}`,
  `POST …/reindex` (stub until M1-06), JSON error envelope of §4.
- Validation: name grammar, format registered, registry name exists in config, `kind: hosted`.
- Config bootstrap: `repos:` merged into the control plane on start (create if missing; never
  delete).
- `lajstrom admin repo create|list|delete` talking to the DB directly (no HTTP).
- Auth: none in M0 (open), a TODO marker where M2-07 plugs in.

**Acceptance criteria**

- [ ] API tests with `httptest` for every endpoint and each validation error.
- [ ] Bootstrap test: start with two repos in config, one already exists → one created.
- [ ] `DELETE` refuses when the repo has versions unless `?force=true` (versions removed from
      the index; registry untouched; audit row).
- [ ] docs: `docs/api.md` started with the repos resource.
