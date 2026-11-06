---
id: M3-06
title: "build(release): goreleaser: binaries, container images (ghcr + docker hub), SBOM, cosign, Homebrew tap, release workflow"
milestone: M3
lane: platform
size: L
priority: p0
labels: [area/release, kind/chore]
depends: [M0-01]
---
**Goal**

A tag `vX.Y.Z` produces everything an operator needs to install Lajstrom, signed and with an
SBOM, without a human on a laptop.

**Scope**

- `.goreleaser.yaml`: linux/darwin × amd64/arm64 static binaries (`CGO_ENABLED=0`), version/
  commit/date ldflags (`/-/version`), archives + checksums, SBOM with syft, cosign keyless
  signing of checksums and images, Homebrew formula pushed to `lajstrom/homebrew-tap`.
- Container image: distroless static, non-root, `/lajstrom` entrypoint, multi-arch manifest,
  pushed to `ghcr.io/lajstrom/lajstrom` and `docker.io/lajstrom/lajstrom` (secrets as repo
  secrets; the lead configures them), `org.opencontainers.image.*` labels, `latest` only on
  stable tags.
- `.github/workflows/release.yml` on `v*` tags with `id-token: write` for keyless signing;
  draft release with generated notes from Conventional Commits (`git-cliff` or goreleaser's
  changelog groups).
- Snapshot builds on `main` (`goreleaser --snapshot`) as CI artifacts for testing.

**Acceptance criteria**

- [ ] A pre-release tag `v0.1.0-rc.1` produces all artifacts; `cosign verify` of the image and
      checksums passes; `brew install lajstrom/tap/lajstrom` works on macOS arm64;
      `docker run ghcr.io/lajstrom/lajstrom:v0.1.0-rc.1 version` prints the version.
- [ ] docs: `docs/install.md` (binary, brew, container), `docs/release.md` (how to cut a release).
