---
id: M3-10
title: "feat(facade): DELETE manifest and version: provider delete plus index removal, capability-gated"
milestone: M3
lane: core
size: S
priority: p1
labels: [area/facade, area/api, kind/feature]
depends: [M1-01, M2-07]
---
**Goal**

Operators can remove a bad version everywhere it is visible, with audit.

**Scope**

- `DELETE /v2/<name>/manifests/<digest|tag>` and `DELETE /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}[?variant=]`
  → provider `Delete` (if `Capabilities.DeleteManifest`, else 405 `UNSUPPORTED` with a message
  about the backend) → index rows removed → `index_cache` invalidated → audit. Also `DELETE` on
  the frontends where the protocol has it (raw).

**Acceptance criteria**

- [ ] Integration on Zot (supports delete) and distribution with delete disabled (405).
- [ ] apt index no longer lists the package after deletion (cache invalidation test).
