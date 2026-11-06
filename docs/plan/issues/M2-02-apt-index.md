---
id: M2-02
title: "feat(format/apt): Packages and Release rendering, index_cache, by-hash"
milestone: M2
lane: formats
size: L
priority: p0
labels: [area/formats, kind/feature]
depends: [M2-01, M0-04, M0-05]
---
**Goal**

`apt-get update` sees a correct repository: `Release` with the hashes of every `Packages` file,
indexes per suite/component/arch generated from the index DB and cached.

**Spec and context**

Debian Repository Format (wiki.debian.org/DebianRepository/Format); `docs/spec/http-routing.md`
§3 apt.

**Scope**

- `RenderIndex` for scope `(suite, component, arch)`: `Packages` (fields from the metadata layer,
  `Filename`, `Size`, `MD5sum`, `SHA1`, `SHA256`), `.gz` and `.xz` variants; `all` packages
  included in every arch index; `Contents-<arch>` not in scope.
- `Release` for the suite: `Origin`, `Label`, `Suite`, `Codename`, `Date`, `Architectures`,
  `Components`, `Acquire-By-Hash: yes`, `MD5Sum`/`SHA1`/`SHA256` sections listing every index
  file and size. Signing is M2-03; this issue renders the unsigned `Release`.
- `index_cache` rows per (repo, scope, kind) with the content and sha256; invalidation on ingest
  and delete; `by-hash/SHA256/<hash>` served from the cache; lazy render on first GET.
- Routes: `dists/{suite}/Release`, `dists/{suite}/{component}/binary-{arch}/Packages[.gz|.xz]`,
  `…/by-hash/SHA256/{hash}`.
- Suites and architectures discovered from versions (no config needed); empty suite → 404.

**Acceptance criteria**

- [ ] Golden-file tests for `Packages` and `Release` with a fixture set (two suites, two
      components, amd64 + arm64 + all).
- [ ] Integration: Ubuntu container runs `apt-get update` with `[trusted=yes]` (signing comes in
      M2-03) and `apt-cache policy <pkg>` shows our version.
- [ ] Cache: a second GET does not hit the DB for the content (test counts queries).
