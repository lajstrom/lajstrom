---
id: M2-12
title: "feat(core): hardening: timeouts, provider retries with backoff, request limits, shutdown drain"
milestone: M2
lane: platform
size: M
priority: p1
labels: [area/core, kind/feature]
depends: [M0-03, M1-01]
---
**Goal**

Lajstrom behaves under a slow or flaky backend and under abusive clients.

**Scope**

- Server: read/write/idle timeouts, max header size, manifest body cap (M1-02), per-route
  concurrency limit for uploads, `MaxBytesReader` on API bodies.
- Provider: retry policy (idempotent operations only), circuit breaker per registry with
  readiness reporting, context deadlines per operation class.
- Shutdown: stop accepting, drain in-flight uploads up to `server.drainTimeout`, abort sessions
  cleanly.
- Rate limit on `/v2/token` and Basic auth failures per IP (token bucket in memory).

**Acceptance criteria**

- [ ] tests with a fake slow backend: retries happen only for idempotent ops; breaker opens and
      closes; drain completes an in-flight upload.
- [ ] docs: operations.md §limits.
