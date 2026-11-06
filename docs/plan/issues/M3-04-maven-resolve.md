---
id: M3-04
title: "feat(format/maven): resolve path: artifacts, checksums, maven-metadata.xml rendering"
milestone: M3
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature]
depends: [M3-01, M0-05]
---
**Goal**

`mvn dependency:get` and Gradle resolve from `/maven/<repo>/`.

**Scope**

- `GET …/{version}/{file}` → `ServeBlob`; `GET …/{file}.sha1|.md5|.sha256|.sha512` from the
  metadata layer (computed at ingest), `text/plain`.
- `GET …/{artifactId}/maven-metadata.xml` rendered from the index (`groupId`, `artifactId`,
  `versioning/latest`, `release`, `versions` sorted by Maven version order, `lastUpdated`),
  cached in `index_cache`, with its checksums.
- `HEAD` support (Maven checks existence); 404 shapes Maven tolerates.

**Acceptance criteria**

- [ ] e2e: `mvn dependency:get -DremoteRepositories=… -Dartifact=g:a:v` into a clean `~/.m2`;
      Gradle resolution with `maven { url }`; version ranges `[1.0,2.0)` resolve via
      `maven-metadata.xml`.
- [ ] Golden test for `maven-metadata.xml` ordering with qualifiers (`1.0-rc1 < 1.0`).
- [ ] docs: `docs/quickstart/maven.md` (settings.xml, Gradle, deploy tokens).
