---
id: M1-07
title: "feat(index): scheduled reconcile loop"
milestone: M1
lane: core
size: S
priority: p1
labels: [area/index, kind/feature]
depends: [M1-06]
---
**Goal**

Writes that bypassed Lajstrom (ECR console, direct pushes) show up without an operator running
reindex.

**Scope**

- `reconcile.interval` config (default 1 h, `0` disables); runs the per-repo reindex with prune
  under a single-flight lock; jittered start; metrics and last-run status on `/-/readyz` details
  and in the repo API.

**Acceptance criteria**

- [ ] tests with a fake clock: runs on schedule, never overlaps, survives provider errors.
- [ ] docs: operations.md updated.
