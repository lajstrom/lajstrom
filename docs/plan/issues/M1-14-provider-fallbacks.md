---
id: M1-14
title: "feat(provider): fallbacks: referrers via tag schema, repo list via oci_repos when no catalog"
milestone: M1
lane: core
size: M
priority: p1
labels: [area/provider, kind/feature]
depends: [M0-03, M1-12]
---
**Goal**

Backends without the referrers API or without a catalog still work (GHCR, some ECR cases, Harbor
configurations).

**Scope**

- Referrers: when `Capabilities.Referrers` is false, list and push referrers with the
  `sha256-<digest>` tag schema (oras-go does part of this); make `oras discover`/`cosign verify`
  work on such a backend.
- Catalog: when `Capabilities.Catalog` is false, `ListRepositories` reads `oci_repos` (kept up to
  date by ingest) and reconcile verifies each by `tags/list`; `lajstrom reindex --attach
  <prefix>` accepts an explicit list of repositories from a file for the first attach.

**Acceptance criteria**

- [ ] Integration against distribution with referrers disabled (config) and catalog disabled
      (config switch): cosign and reindex still pass.
- [ ] tests for both paths.
