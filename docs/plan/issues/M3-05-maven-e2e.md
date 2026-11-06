---
id: M3-05
title: "test(e2e): mvn deploy and dependency:get, Gradle publish and resolve, in CI"
milestone: M3
lane: platform
size: M
priority: p0
labels: [area/ci, kind/test]
depends: [M3-03, M3-04, M1-11]
---
**Goal**

The M3 demo on every pull request.

**Scope**

- `test/e2e/maven.sh` with `maven:3-eclipse-temurin-21` and `gradle:8-jdk21` containers, a
  generated project (`mvn archetype` is slow; keep a tiny project in `test/e2e/fixtures/maven`),
  `settings.xml` with a deploy token; deploy, resolve, redeploy, 409 case, session expiry case.

**Acceptance criteria**

- [ ] Passes in CI in under 4 minutes (cache `~/.m2` of the client container across runs).
