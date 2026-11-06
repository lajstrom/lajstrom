---
id: M2-03
title: "feat(keys): per-repo GPG keys, encrypted storage, key.asc, InRelease and Release.gpg signing"
milestone: M2
lane: core
size: M
priority: p0
labels: [area/core, area/formats, kind/feature]
depends: [M0-04, M2-02]
---
**Goal**

apt repositories are signed with a key Lajstrom generates and keeps per repo; clients pin it
with `signed-by`.

**Spec and context**

`docs/spec/index-schema.md` (`keys`); `docs/spec/layout.md` §3 (ProtonMail/go-crypto).

**Scope**

- `internal/keys`: master key from `keys.masterKeyEnv`/file (32 bytes, AES-GCM), generate an
  Ed25519 (or RSA 4096 if a config asks for legacy apt compatibility; default Ed25519 which apt
  ≥ 2.0 accepts) OpenPGP key per apt repo on creation, store encrypted, rotate (new key, old
  kept as `retired` and still served for a grace period).
- Signing service: clearsign (`InRelease`) and detached (`Release.gpg`) with the repo key;
  results cached in `index_cache` with `signed_at`.
- Routes: `dists/{suite}/InRelease`, `dists/{suite}/Release.gpg`, `/apt/{repo}/key.asc`.
- `lajstrom admin key export|rotate --repo apt/<name>`.

**Acceptance criteria**

- [ ] Integration: Ubuntu 22.04 and 24.04 containers verify `InRelease` with the exported key
      (`signed-by=`), no `[trusted=yes]`; Debian 12 as well.
- [ ] tests: encryption round-trip, rotation keeps old signatures verifiable, master key missing
      → clear startup error.
- [ ] docs: `docs/auth.md` §apt signing, key rotation runbook in operations.md.
