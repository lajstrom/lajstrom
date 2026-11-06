---
id: M2-10
title: "test(e2e): apt-get update/install scenario on Ubuntu and Debian in CI"
milestone: M2
lane: platform
size: M
priority: p0
labels: [area/ci, kind/test]
depends: [M2-04, M1-11]
---
**Goal**

The M2 demo runs on every pull request.

**Scope**

- `test/e2e/apt.sh` wired into the harness; client containers `ubuntu:22.04`, `ubuntu:24.04`,
  `debian:12`; the repo key fetched from `/apt/<repo>/key.asc` into `/etc/apt/keyrings`;
  `apt-get update`, `install`, `download`, `apt-cache policy`; with and without `by-hash`.
- Auth variants: anonymous read on, then off with `auth.conf.d` credentials.

**Acceptance criteria**

- [ ] Passes in CI in under 3 minutes (cache the client images).
- [ ] A broken `Release` hash fails the test with apt's own error visible in the log.
