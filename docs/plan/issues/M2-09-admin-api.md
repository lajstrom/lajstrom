---
id: M2-09
title: "feat(api): /api/v1 users, tokens, bindings, whoami; admin CLI subcommands"
milestone: M2
lane: core
size: M
priority: p0
labels: [area/api, kind/feature]
depends: [M2-05, M2-06, M2-07]
---
**Goal**

Everything an operator needs for the MVP without a UI: manage users, tokens and bindings over
HTTP and from the CLI, with the error envelope and auth of the spec.

**Spec and context**

`docs/spec/http-routing.md` §4.

**Scope**

- Endpoints for users, PATs, deploy tokens, bindings, `whoami`, audit (M2-08) with validation,
  pagination (`limit`/`cursor`), and `manage`/admin checks.
- `lajstrom admin` subcommands as a thin client of the same handlers (in-process, no HTTP).
- OpenAPI 3.1 document generated or hand-written at `docs/api/openapi.yaml`, served at
  `/api/v1/openapi.yaml`.

**Acceptance criteria**

- [ ] `httptest` tests for every endpoint: happy path, validation, authz.
- [ ] The OpenAPI document validates (`make api-lint` with a pinned validator) and matches the
      handlers (contract test for paths and methods).
- [ ] docs: `docs/api.md` complete for the MVP.
