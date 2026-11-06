---
id: M1-15
title: "docs: raw and pypi quickstarts, configuration reference"
milestone: M1
lane: platform
size: S
priority: p1
labels: [area/docs, kind/docs]
depends: [M0-07, M1-09, M1-10]
---
**Goal**

A newcomer can run the dev stack, create a repo, publish and consume raw files and Python
packages from the docs alone.

**Scope**

- `docs/quickstart/raw.md`, `docs/quickstart/pypi.md` (pip, uv, poetry, twine), `docs/configuration.md`
  generated or hand-kept from `internal/config` (every key, default, env override).
- Each quickstart is executed by the e2e script of the same name, so docs and tests do not drift
  (the script sources the commands from a fenced block, or the doc links the script).

**Acceptance criteria**

- [ ] Someone who did not write the code follows the docs on a clean machine and succeeds
      (note who tried in the PR).
