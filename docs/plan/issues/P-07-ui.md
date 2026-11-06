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

The UI is a separate deliverable: its own repository,
[lajstrom/lajstrom-ui](https://github.com/lajstrom/lajstrom-ui) (scaffold only so far: Apache-2.0,
DCO and CLA checks, pull request template), built as a static bundle that runs from any origin:
an nginx container behind the same edge as Lajstrom (`ui` stays a reserved first URL segment,
`docs/spec/http-routing.md` §1, so that the edge can route `/ui/` to it), or object storage
behind a CDN (S3). The Go server never serves `/ui/`.

What cross-origin use needs on the API side, all part of this issue: a CORS allowlist
(`server.corsOrigins`); bearer-only auth for browsers (no cookies, hence no CSRF); a runtime
`config.json` next to the bundle with the API base URL; the UI reads `/-/version` and warns on a
version skew against the `/api/v1` contract it was built for.

**Status**

Post-MVP. Not scheduled; discussed at the Friday review once v0.1.0 is out. Comments and design
notes welcome; no implementation PRs before it gets a milestone.
