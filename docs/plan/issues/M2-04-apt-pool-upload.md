---
id: M2-04
title: "feat(format/apt): pool downloads, PUT upload and oras path; apt-get install works"
milestone: M2
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature]
depends: [M2-02, M2-03, M1-02, M1-04]
---
**Goal**

The M2 demo: publish a `.deb` and install it with `apt-get`.

**Scope**

- Routes: `pool/{component}/{p}/{source}/{filename}` → file lookup by `(repo, filename)` →
  `ServeBlob` (307 or stream); `PUT /apt/{repo}/upload?suite=&component=` streaming the body
  into `ingest.Upload` with the deb plugin (`Build` sets the attrs from the query params).
- `oras push` path: the deb plugin's `Validate` is what the facade hook (M1-02) calls; document
  the exact `oras push` command with annotations and media types.
- Post-ingest hook: invalidate `index_cache` for `(suite, component, arch)` and `(suite,
  component, all-arches)` when arch is `all`; re-sign lazily on next GET.

**Acceptance criteria**

- [ ] e2e `apt.sh` (M2-10 wires it into CI): upload via PUT, upload a second package via `oras
      push`, Ubuntu container `apt-get update && apt-get install` both, `apt-get download`
      verifies the hashes, re-upload same bytes → 201, different bytes same version+arch → 409.
- [ ] docs: `docs/quickstart/apt.md`.
