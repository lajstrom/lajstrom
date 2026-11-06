---
id: P-07
title: "feat(ui): read-only web UI: repos, packages, versions, files, referrers, audit"
milestone: none
lane: none
size: L
priority: p2
labels: [area/core, kind/feature, post-mvp]
depends: []
---
**Goal**

SPA served at `/ui`, backed by `/api/v1`; search; per-format usage snippets; later: management screens.

The code lives in its own repository, [lajstrom/lajstrom-ui](https://github.com/lajstrom/lajstrom-ui)
(scaffold only so far: Apache-2.0, DCO and CLA checks, pull request template). The server embeds
the built bundle at release time and serves it under `/ui/`; `ui` is a reserved first URL segment
(`docs/spec/http-routing.md` §1).

**Status**

Post-MVP. Not scheduled; discussed at the Friday review once v0.1.0 is out. Comments and design
notes welcome; no implementation PRs before it gets a milestone.
