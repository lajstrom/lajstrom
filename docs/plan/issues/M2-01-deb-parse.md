---
id: M2-01
title: "feat(format/deb): parse .deb (ar, control.tar.*), md5sums, profile mapping"
milestone: M2
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature]
depends: [M0-02, M0-16]
---
**Goal**

The pure half of the apt plugin: read a `.deb`, produce the profile entry and the metadata layer,
detect disagreement with annotations.

**Spec and context**

Debian Policy §5 (control files), `deb(5)`; `docs/spec/manifest-profile.md` (deb rows).

**Scope**

- `internal/format/deb/parse.go`: `ar` reader (stdlib has none; write a minimal one, ~100
  lines), `debian-binary` check, `control.tar.{gz,xz,zst}` → `control` (RFC 822-style fields,
  multi-line `Description`), `md5sums`, size, sha256/sha1/md5 of the whole file (apt's `Packages`
  needs all three).
- Entry: name = `Package`, version = `Version`, variant = `Architecture`; attrs
  `lajstrom.deb.arch|suite|component` (suite/component from upload params, required);
  `Section`, `Priority`, `Depends`, … go to the metadata layer; the `Filename` apt will see is
  computed (`pool/<component>/<p>/<source>/<file>`).
- `Validate`: annotations ↔ control (name, version, arch); file name matches
  `<name>_<version-escaped>_<arch>.deb` (epoch stripped, `:` → `%3a` as dpkg does).

**Acceptance criteria**

- [ ] Tests with `testutil.Deb` for each compression, epochs, `all` arch, multi-line description,
      unusual field order; corrupted `ar` → `ErrInvalid`.
- [ ] Fuzz target on the `ar` + control parser.
- [ ] No new dependency except `ulikunitz/xz` and `klauspost/compress` (zstd).
