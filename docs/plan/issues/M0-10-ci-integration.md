---
id: M0-10
title: "ci: integration job with registry and Postgres service containers; engine matrix"
milestone: M0
lane: platform
size: M
priority: p0
labels: [area/ci, kind/chore]
depends: [M0-09]
---
**Goal**

CI exercises the real registry and both database engines on every pull request, so provider,
index and ingest work is verified where it matters.

**Scope**

- `.github/workflows/ci.yml`: new job `integration` with services Zot, distribution, Postgres
  (same pinned images as compose), waits for health, runs
  `go test -race -tags integration ./...` with `LAJSTROM_TEST_REGISTRY`,
  `LAJSTROM_TEST_REGISTRY_2`, `LAJSTROM_TEST_POSTGRES_DSN`.
- Matrix: `db: [sqlite, postgres]` for the store suite.
- Go test cache and module cache keyed by `go.sum`.
- Required check added to the branch protection list by the lead (not by this PR; note in the
  description).

**Acceptance criteria**

- [ ] The job passes on a PR that touches nothing (no tests yet) and fails on a PR with a
      deliberately failing integration test (verified once, then reverted).
- [ ] Total CI time for a doc-only PR stays under 5 minutes (paths filter for the integration
      job: `**.go`, `go.*`, `deploy/**`, workflows).
- [ ] docs: CONTRIBUTING.md CI table updated.
