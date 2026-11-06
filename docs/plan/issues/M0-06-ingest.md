---
id: M0-06
title: "feat(ingest): ingest pipeline v1: validate → push blobs → put manifest → upsert index"
milestone: M0
lane: core
size: M
priority: p0
labels: [area/core, kind/feature]
depends: [M0-02, M0-03, M0-04, M0-05]
---
**Goal**

`internal/ingest.Pipeline` with the `Upload` path (frontend gives files, plugin builds the
manifest, pipeline pushes and indexes) and the `Ingest` path (manifest bytes already built by a
client, used by the facade in M1-02). One write path for everything.

**Spec and context**

`docs/architecture.md` flow A; `docs/spec/manifest-profile.md` §5; `docs/spec/layout.md` §2.

**Scope**

- `Upload`: `plugin.Build` → compute digests while streaming each file to the provider
  (`Push` with known size when available, temp spill never) → metadata layer → manifest via
  profile → `Ingest`.
- `Ingest`: `profile.Validate` → `plugin.Validate` (with a `BlobFetcher` backed by the provider)
  → all layers exist in the backend (`Exists`) → tag collision check against the index and the
  backend (resolve tag; same digest → idempotent success) → `PUT manifest` + tag → index upsert
  in one transaction → invalidate `index_cache` scopes the plugin names → audit event.
- Errors: `ErrInvalid` (400), `ErrConflict` (409, includes existing digest), `ErrRepoNotFound`.
- Ordering guarantee: index rows are written only after the registry accepted the manifest; if
  the index write fails, log and return 500 but leave the manifest (reindex will pick it up).

**Acceptance criteria**

- [ ] Integration test with the fake plugin against Zot: upload two files → one manifest with
      two layers + metadata layer, annotations right, index rows right, second identical upload
      is idempotent, different bytes same tag → `ErrConflict`.
- [ ] Unit tests with a fake provider for the ordering guarantee and each error.
- [ ] Memory: uploading a 512 MiB file does not allocate it (stream through `io.Pipe`/`TeeReader`).
- [ ] docs: flow A in architecture.md still matches; note any deviation.

**Out of scope**

Sessions (M3-02), signing hooks for apt (M2-03 adds a post-ingest hook; leave a hook point).
