---
id: M3-07
title: "docs: install, deploy (compose and Kubernetes manifests), configuration reference, maven quickstart, backup and reindex runbook"
milestone: M3
lane: platform
size: M
priority: p0
labels: [area/docs, kind/docs]
depends: [M3-06, M3-04, M1-06]
---
**Goal**

Someone outside the team installs v0.1.0 against their own Zot or ECR and publishes four kinds
of packages from the docs alone.

**Scope**

- `docs/install.md`, `docs/deploy/compose.md` (production-ish compose with Postgres and an
  external registry), `docs/deploy/kubernetes.md` (plain manifests: Deployment, Service,
  Secret for keys, ConfigMap; Helm chart is post-MVP), `docs/configuration.md` complete,
  `docs/quickstart/maven.md`, `docs/operations.md` (backup of the control plane, reindex, key
  rotation, metrics), `docs/registries/` reviewed.
- README: replace "Status" with the one-paragraph pitch, the install one-liner and the
  per-format table of what works.

**Acceptance criteria**

- [ ] A person who did not build it follows install + one quickstart per format on a fresh VM
      and records the outcome in the PR.
