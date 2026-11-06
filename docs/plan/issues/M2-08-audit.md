---
id: M2-08
title: "feat(audit): audit log of writes and control-plane changes; GET /api/v1/audit"
milestone: M2
lane: core
size: S
priority: p1
labels: [area/core, area/api, kind/feature]
depends: [M0-04, M0-06]
---
**Goal**

Who pushed, deleted, created a token or changed a binding, with request id and IP.

**Scope**

- `internal/audit.Writer` called from ingest (push), delete paths, API mutations, token issuance
  (not every `/v2/token` call: once per principal per hour, to limit noise), admin CLI.
- `GET /api/v1/audit?repo=&principal=&action=&since=&until=&limit=` for admins; CSV export later.
- Async buffered writes with a bounded queue; drop with a metric if the DB is down rather than
  failing the request (document the choice).

**Acceptance criteria**

- [ ] tests: every mutation path produces exactly one event with the right fields; queue
      overflow behaviour.
- [ ] docs: operations.md §audit.
