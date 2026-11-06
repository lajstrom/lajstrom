---
id: M1-12
title: "docs(registries): consolidate spike results; wire capabilities per backend"
milestone: M1
lane: platform
size: S
priority: p1
labels: [area/docs, area/provider, kind/docs]
depends: [M0-11, M0-12, M0-13, M0-14, M0-15]
---
**Goal**

One page that says which backends are supported at which level, from measurements.

**Scope**

- `docs/registries/README.md`: the matrix with verified values, support tiers (tested in CI /
  tested manually / untested), known limitations per backend, the config snippet for each.
- Provider: default `Capabilities` per backend `type`/detected vendor header, overridable in
  config; update `docs/architecture.md` §5 to point here instead of carrying the table.

**Acceptance criteria**

- [ ] Every "verify" cell from the architecture page is resolved or explicitly marked unknown
      with the reason.
- [ ] tests: capability defaults per backend type.
