---
id: M2-13
title: "test: fuzz targets for Escape, deb control parser, pypi file-name parser, POM parser"
milestone: M2
lane: formats
size: S
priority: p2
labels: [area/formats, kind/test, good first issue]
depends: [M0-02, M2-01, M1-08]
---
**Goal**

The parsers that face untrusted input have fuzz targets and a seed corpus checked in, and CI
runs them for a short time.

**Scope**

- `FuzzEscape`, `FuzzUnescape`, `FuzzParseControl`, `FuzzParseAr`, `FuzzWheelFilename`,
  `FuzzPOM` (once M3-01 exists); `testdata/fuzz/` corpora; a `make fuzz` target (30 s each) and a
  weekly scheduled CI job (10 min each).

**Acceptance criteria**

- [ ] Each target runs clean for 10 minutes locally; any crash found is fixed in the same PR
      with a regression test.
