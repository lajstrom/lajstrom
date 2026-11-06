---
id: M1-04
title: "feat(facade): blob GET → 307 to the registry's presigned URL when supported, else stream"
milestone: M1
lane: core
size: S
priority: p1
labels: [area/facade, kind/feature]
depends: [M1-01, M0-03]
---
**Goal**

Large downloads bypass Lajstrom when the backend can hand out a direct URL (ECR, S3-backed
distribution/Zot/Harbor); otherwise we stream.

**Scope**

- Facade and frontends share one `ServeBlob(w, r, repo, digest, opts)` helper: capability check
  → `BlobRedirectURL` → 307 with `Location`; else stream with `Content-Length`, `Docker-Content-Digest`,
  `ETag`, `Accept-Ranges`, `Range` passthrough.
- Per-repo policy `blobRedirect: auto|never` (never = always stream, for clients that cannot
  follow redirects to another host).

**Acceptance criteria**

- [ ] Integration on distribution with the S3 driver (MinIO in the compose profile from M0-11):
      GET returns 307 and the client download verifies the digest; on filesystem storage it
      streams.
- [ ] `Range` requests work in stream mode (test with `curl -r`).
- [ ] tests for the policy switch.
