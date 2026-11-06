---
id: M3-08
title: "chore(release): v0.1.0: changelog, tag, release notes, announcement draft"
milestone: M3
lane: core
size: S
priority: p0
labels: [area/release, kind/chore]
depends: [M3-05, M3-06, M3-07, M2-10, M1-11]
assignee: gecube
---
**Goal**

Ship.

**Scope**

- Thursday 2026-12-03: feature freeze; `v0.1.0-rc.1` from `main`; the e2e suite and a manual
  pass of every quickstart on the rc artifacts.
- Friday 2026-12-04: `CHANGELOG.md` (Keep a Changelog, generated then edited), tag `v0.1.0`,
  release notes (what works, what does not, registries tested, known limitations), lajstrom.dev
  landing updated with the install one-liner, announcement draft for the lead to post (Hacker
  News "Show HN", r/devops, the OCI and oras communities, LinkedIn).
- Milestone M3 closed; leftovers moved to a new `M4` milestone with a date.

**Acceptance criteria**

- [ ] `v0.1.0` release page lists the artifacts, the signatures verify, the docs link works.
- [ ] `docs/roadmap.md` updated with the outcome and the next month's preview.
