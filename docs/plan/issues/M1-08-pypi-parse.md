---
id: M1-08
title: "feat(format/pypi): parse wheel and sdist metadata, normalise names, profile mapping"
milestone: M1
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature]
depends: [M0-02, M0-16]
---
**Goal**

The pure half of the pypi plugin: given a distribution file, produce the manifest profile entry
(name, version, variant, annotations, metadata layer) and detect disagreement.

**Spec and context**

PEP 503 (normalisation), PEP 427 (wheel file name and tags), PEP 625 (sdist name), PEP 566
(core metadata as JSON), `docs/spec/manifest-profile.md` (pypi rows).

**Scope**

- `internal/format/pypi/parse.go`: wheel file name → (name, version, build, python/abi/platform
  tags); `METADATA` from the `.dist-info` in the zip; sdist `PKG-INFO` from the tarball; PEP 503
  normalisation; variant = the compressed tag set string (`py3-none-any`,
  `cp311-cp311-manylinux_2_17_x86_64`) or `sdist`.
- Metadata layer content: core metadata as JSON (PEP 566 shape) plus `filename`, `sha256`,
  `packagetype`, `requires_python`.
- `Validate`: annotations ↔ file name ↔ METADATA agree (name normalised, version exact).

**Acceptance criteria**

- [ ] Tests with `testutil.Wheel`/`Sdist` fixtures and a table of real-world wheel file names
      (pure, abi3, manylinux/musllinux multi-tag, macOS universal2, Windows, build numbers).
- [ ] Validation errors for: name mismatch, version mismatch, wheel without METADATA, sdist
      without PKG-INFO, non-PEP-440 version (accepted but flagged `lajstrom.pypi.nonstandard=true`).
- [ ] Fuzz target for the file-name parser.
