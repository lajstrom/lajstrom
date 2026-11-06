---
id: M3-09
title: "feat(core): per-repo policies: maxFileSize, blobRedirect, maven.sessionTimeout and expiry policy"
milestone: M3
lane: core
size: S
priority: p1
labels: [area/core, area/api, kind/feature]
depends: [M0-08, M3-02]
---
**Goal**

The handful of knobs the MVP formats need, stored in `repos.policy` and editable via `PATCH`.

**Scope**

- Typed policy struct with validation; defaults; `PATCH /api/v1/repos/{format}/{repo}` merges;
  enforcement points: ingest (maxFileSize, 413), ServeBlob (blobRedirect), sessions (timeout,
  seal/abort).

**Acceptance criteria**

- [ ] tests for validation and each enforcement point; docs in api.md and configuration.md.
