---
id: M0-07
title: "feat(format/raw): raw hosted repo: PUT, GET, HEAD, DELETE, listing"
milestone: M0
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature]
depends: [M0-05, M0-06, M0-08]
---
**Goal**

The first format end to end, and the M0 demo: `curl -T` a file in, `curl` it out, see the
manifest in Zot.

**Spec and context**

`docs/spec/http-routing.md` §3 raw; `docs/spec/manifest-profile.md` (raw rows).

**Scope**

- `internal/format/raw`: `EscapeName` (per path component), `Build` (one file layer + metadata
  layer with path, contentType, size, sha256), `Validate` (annotations ↔ metadata ↔ layer),
  routes `GET/HEAD/PUT/DELETE /raw/{repo}/{path...}` and the JSON listing on `/` and directory
  prefixes.
- Version = first 16 hex of the content sha256; GET resolves the newest version for the path
  (by `created_at`), `?digest=` pins.
- GET uses `Provider.BlobRedirectURL` → 307 when available (Zot: check), else streams with
  `Content-Length`, `Content-Type`, `ETag` (digest), `Last-Modified`; supports `Range` by
  passing it to the backend when streaming.

**Acceptance criteria**

- [ ] e2e script (`test/e2e/raw.sh`, used by `make e2e` once M1-11 lands; until then run by
      hand): PUT, GET equals the file, HEAD headers, PUT same bytes → 201 idempotent, PUT new
      bytes → new version and GET returns the new bytes, `?digest=` returns the old, listing shows
      both paths, DELETE removes.
- [ ] `oras pull` of the manifest from the backend writes the original file name.
- [ ] Unit tests for path escaping (`Docs/Readme.MD`, spaces, unicode), listing JSON.
- [ ] docs: `docs/quickstart/raw.md` with the curl commands.

**Out of scope**

HTML directory listing, auth (M2; until then everything is open on the dev stack).
