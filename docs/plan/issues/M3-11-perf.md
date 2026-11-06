---
id: M3-11
title: "perf: streaming audit and a load test of 1 GiB upload/download through the facade and raw"
milestone: M3
lane: platform
size: M
priority: p1
labels: [area/core, kind/test]
depends: [M1-01, M0-07]
---
**Goal**

Evidence that Lajstrom never buffers blobs and that throughput is bounded by the backend, not by
us.

**Scope**

- `test/load/` with a small Go program: parallel uploads/downloads of 100 MiB–1 GiB objects via
  `oras` and `curl`, measuring throughput and the server's RSS (`/-/metrics` process_resident_memory).
- Fix anything that buffers (`io.ReadAll`, `bytes.Buffer` on blob paths) found by grep and by the
  measurement.

**Acceptance criteria**

- [ ] RSS stays under 256 MiB during 8 parallel 1 GiB transfers on the dev stack; numbers and
      the method in `docs/operations.md` §performance.
