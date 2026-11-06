---
id: M1-11
title: "test(e2e): harness with real clients (oras, pip/twine) in CI"
milestone: M1
lane: platform
size: M
priority: p0
labels: [area/ci, kind/test]
depends: [M0-09, M0-10]
---
**Goal**

`make e2e` runs the real clients against a running Lajstrom + dev stack, locally and in CI; each
format adds one script. Friday demos are these scripts.

**Scope**

- `test/e2e/run.sh`: builds the binary, starts `lajstrom serve` against the compose stack (CI:
  service containers + the binary on the runner), waits for `/-/readyz`, runs every
  `test/e2e/<name>.sh`, collects logs on failure.
- Client containers: `ghcr.io/oras-project/oras`, `python:3.13` (pip, twine, uv), later
  `ubuntu:24.04` (M2-10) and `maven:3-eclipse-temurin-21` (M3-05); run with `--network host` or
  the compose network.
- `raw.sh` (from M0-07), `oras.sh`, `pypi.sh`; a tiny assertion library (`assert_eq`,
  `assert_status`).
- CI job `e2e` on PRs touching code; runtime budget 6 minutes.

**Acceptance criteria**

- [ ] `make e2e` passes locally on macOS and in CI; a broken endpoint makes it fail with the
      script name and the failing command.
- [ ] docs: `docs/testing.md` explains the levels and how to add a scenario.
