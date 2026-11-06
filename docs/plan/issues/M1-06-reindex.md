---
id: M1-06
title: "feat(index): lajstrom reindex: full rebuild, per-repo, report of foreign manifests"
milestone: M1
lane: core
size: L
priority: p0
labels: [area/index, kind/feature]
depends: [M0-02, M0-03, M0-04]
---
**Goal**

The proof of ADR 0001: drop the derived tables, run `lajstrom reindex`, get the same index back.
Also the way to attach an existing registry prefix and to repair drift.

**Spec and context**

`docs/architecture.md` flow E; `docs/spec/index-schema.md` (derived tables, invariants).

**Scope**

- `internal/index/reindex`: for each repo (or `--repo format/name`), list OCI repositories under
  the repo's path via the provider (catalog or `oci_repos`), list tags, fetch manifests (cache by
  digest; concurrency limit per registry), `profile.Validate` + plugin `Validate` with a fetcher
  → upsert `packages`/`versions`/`files`; at the end delete rows whose manifest was not seen
  (`--prune`, default on for full runs), drop `index_cache` for touched repos, refresh `oci_repos`.
- Manifests under a managed path without `lajstrom.schema` are reported, not indexed
  (`--report` prints them; also exposed in the API later).
- `POST /api/v1/repos/{format}/{repo}/reindex` triggers it asynchronously; status in the response
  of a follow-up GET (`reindex: {state, started, done, errors}`).
- Progress logging and metrics (manifests/s, errors).

**Acceptance criteria**

- [ ] Integration test: ingest a mixed set through the pipeline (raw + fake plugin), snapshot
      derived tables, `DropDerived`, reindex, compare: identical rows (modulo ids and
      `indexed_at`).
- [ ] Drift test: push a manifest with `oras` straight to the backend (bypassing Lajstrom),
      reindex, it is indexed; delete a manifest in the backend, reindex with prune, its rows go.
- [ ] Foreign manifest test: a Helm chart pushed under a managed path is reported, not indexed.
- [ ] 10k manifests reindex in under 2 minutes against local Zot (document the number).
- [ ] docs: `docs/operations.md` §reindex and §disaster recovery runbook.
