---
id: M1-05
title: "feat(facade): referrers passthrough with tag-schema fallback; cosign sign/verify e2e"
milestone: M1
lane: core
size: M
priority: p1
labels: [area/facade, kind/feature]
depends: [M1-01, M1-02]
---
**Goal**

Supply-chain tools work unchanged: `cosign sign` attaches a signature to our manifest through the
facade, `cosign verify` reads it back, and the UI/API can list referrers.

**Scope**

- `GET /v2/<name>/referrers/<digest>` via the provider (API or tag schema per capability),
  `artifactType` filter param, `OCI-Filters-Applied` header.
- Manifest PUT hook: a manifest with `subject` and a non-Lajstrom artifactType (cosign,
  notation, SBOM media types) is a referrer: validate the subject exists, pass through to the
  backend without indexing beyond a `referrers` note on the version (optional column, derived).
- Tag-schema writes (`sha256-<digest>.sig`) accepted on managed repos as referrers.

**Acceptance criteria**

- [ ] e2e: `cosign sign --key …` and `cosign verify` on `localhost:8080/raw/assets/hello:1`
      against Zot (referrers API) and distribution (tag schema).
- [ ] `oras discover` lists the signature.
- [ ] tests: referrer detection table (cosign, notation, SPDX, CycloneDX, unknown with subject).
