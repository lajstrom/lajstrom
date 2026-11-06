---
id: M0-17
title: "feat(core): observability baseline: JSON logs, request id, metrics, readiness"
milestone: M0
lane: platform
size: S
priority: p1
labels: [area/core, kind/feature]
depends: [M0-01]
---
**Goal**

From the first week every request is traceable and the server says when it is ready.

**Scope**

- `slog` JSON handler by default (text in dev via config), request id from `X-Request-Id` or
  generated, included in every log line of the request and returned in the response.
- `/-/metrics`: Prometheus with http request duration/size histograms by route pattern and
  status, provider call duration by operation and registry, DB query duration.
- `/-/readyz`: DB ping and each registry's `/v2/` within the last 60 s (background probe).
- Never log `Authorization`, cookies, token values (middleware redaction test).

**Acceptance criteria**

- [ ] tests: request id propagation, redaction, readiness flips when the registry probe fails.
- [ ] docs: `docs/operations.md` started (logs, metrics, readiness).
