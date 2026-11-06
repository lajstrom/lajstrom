---
id: M3-02
title: "feat(ingest): publication sessions: open, add, commit, abort, expire"
milestone: M3
lane: core
size: L
priority: p0
labels: [area/core, kind/feature, contract]
depends: [M0-06, M0-04]
---
**Goal**

Formats whose upload protocol is many requests (maven now, docker multi-arch stitching later)
accumulate files in a session and commit once, producing one manifest (flow B).

**Spec and context**

`docs/architecture.md` flow B; `docs/spec/index-schema.md` (`sessions`);
`docs/spec/http-routing.md` §3 maven (commit semantics, seal/abort policy).

**Scope**

- `ingest.Sessions`: `Open(repo, key, principal)` (idempotent for the same key+principal;
  another principal on an open session → 409), `Add(session, file)` streams the blob to the
  backend immediately (so the session holds descriptors, not bytes), `Commit(session)` → plugin
  `Build` from the descriptors → `Ingest`; `Abort`; `Expire` worker with per-repo policy `seal`
  (commit what is there) or `abort`, default abort, timeout default 30 min.
- State machine and transitions recorded in `sessions.state`; files in `sessions.files` json;
  blobs of aborted sessions are left to the registry's GC (document).
- Single-flight on commit; crash safety: a session is either committed (manifest exists) or not.

**Acceptance criteria**

- [ ] Unit tests of the state machine (every transition, idempotent re-add of the same file by
      digest, commit with zero files → error).
- [ ] Integration: open, add 5 files, commit → one manifest with 5 layers; abort → nothing
      indexed; expiry with `seal` and with `abort`.
- [ ] docs: layout.md §2 Sessions shape updated.
