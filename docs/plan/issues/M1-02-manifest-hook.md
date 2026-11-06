---
id: M1-02
title: "feat(facade): PUT manifest hook → ingest; error mapping"
milestone: M1
lane: core
size: M
priority: p0
labels: [area/facade, kind/feature]
depends: [M1-01, M0-06]
---
**Goal**

`PUT /v2/<name>/manifests/<tag>` is the single upload API for every format (flow A). The
manifest is validated by the profile and the plugin, then stored and indexed by the pipeline.

**Scope**

- Parse the body (size cap from config, default 4 MiB), require `Content-Type` of the OCI
  manifest; refuse image indexes on non-`oci` repos (`MANIFEST_INVALID`).
- Call `ingest.Ingest` with the repo, tag/digest reference, principal; map `ErrInvalid` → 400
  `MANIFEST_INVALID` with the validation message in `detail`, `ErrConflict` → 409 `TAG_INVALID`
  with the existing digest, blob missing → 400 `MANIFEST_BLOB_UNKNOWN`.
- PUT by digest (no tag) is accepted only if the manifest's annotations yield a tag; the facade
  tags it (so `oras push --format` tools that push by digest still index).
- Respond 201 with `Location` and `Docker-Content-Digest`.

**Acceptance criteria**

- [ ] Integration: `oras push` with the raw profile annotations indexes the file; GET on
      `/raw/<repo>/<path>` returns it. Push again with other bytes and the same tag → 409 with the
      digest in the body. Push with a missing annotation → 400 and the message names the key.
- [ ] `oras push` without `--artifact-type` (defaults to an unknown type) → 400 with a hint.
- [ ] tests: error mapping table; size cap.
- [ ] docs: quickstart/oras.md shows the required `--annotation` flags for raw.
