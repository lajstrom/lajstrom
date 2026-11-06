---
id: M3-03
title: "feat(format/maven): deploy path: PUT files into a session, commit on maven-metadata.xml"
milestone: M3
lane: formats
size: L
priority: p0
labels: [area/formats, kind/feature]
depends: [M3-01, M3-02]
---
**Goal**

`mvn deploy` (and Gradle `publish`) works against `/maven/<repo>/`.

**Scope**

- `PUT /maven/{repo}/{groupPath...}/{artifactId}/{version}/{file}`: open or reuse the session
  `(repo, g:a:v)`, add the file (artifact or pom); checksum files are verified against the
  already-added file's digest (or stored pending and verified when the file arrives) and never
  become layers; `.asc` signatures are layers typed `artifact` with `extension=asc`.
- `PUT …/{artifactId}/maven-metadata.xml` (and its checksums): commit the session; verify the
  metadata lists the version; respond 201/200; if the version already exists with identical files
  → 200 (idempotent redeploy); different bytes → 409 (`maven.allowRedeploy` per repo later).
- Ordering quirks: Maven PUTs checksums right after each file, metadata at the end; Gradle does
  the same with `sha256`/`sha512` too; both tested.

**Acceptance criteria**

- [ ] e2e: `mvn deploy` of a project with main jar, sources, javadoc, pom; then Gradle
      `publish` of another; sessions closed; one manifest per version with the right layers and
      annotations; redeploy same → 200; changed jar → 409.
- [ ] A session left without `maven-metadata.xml` expires per policy (test with a short timeout).
