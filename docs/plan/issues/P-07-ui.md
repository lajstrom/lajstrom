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

**Acceptance criteria** (fixed 2026-11-06, before scheduling)

- [ ] One static bundle per release, built with relative asset paths, published as a tar.gz and as
      an nginx image; the same bundle works at the root of an object-storage origin (S3 behind a
      CDN) and under `/ui/` behind the edge of Lajstrom.
- [ ] Runtime configuration from `config.json` next to the bundle, versioned schema
      (`"schema": 1`): `apiBaseUrl` (empty means the same origin), `branding` (white label: title,
      logo, colours, support link; the Lajstrom version stays in the about dialog for support),
      later `oidc`. The nginx image generates the file from environment variables or takes a
      mounted one; `config.json` is served with `Cache-Control: no-cache`, hashed assets are
      immutable.
- [ ] Start-up never shows a blank page: a missing `config.json` means defaults; a malformed one or
      an unknown schema renders an error page that names the problem; an unreachable
      `GET {apiBaseUrl}/-/version` renders an error page with the URL and the status; a version
      skew against the `/api/v1` contract the bundle was built for shows a warning and goes on.
- [ ] Server side (lajstrom/lajstrom): CORS allowlist `server.corsOrigins` on `/-/version` and
      `/api/v1`; browsers use bearer tokens only, never cookies; short-lived login tokens from
      `/api/v1/login` rather than pasted PATs.
- [ ] Read-only screens: repos, packages, versions, files, referrers, audit; search; per-format
      usage snippets.

**Status**

Post-MVP. Not scheduled; discussed at the Friday review once v0.1.0 is out. Comments and design
notes welcome; no implementation PRs before it gets a milestone.
