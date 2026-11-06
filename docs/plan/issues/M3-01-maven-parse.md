---
id: M3-01
title: "feat(format/maven): coordinates from path, POM parsing, profile mapping, checksums"
milestone: M3
lane: formats
size: M
priority: p0
labels: [area/formats, kind/feature]
depends: [M0-02, M0-16]
---
**Goal**

The pure half of the maven plugin: understand the Maven 2 layout and POMs, produce the profile
entry for a (groupId, artifactId, version) with all its files.

**Spec and context**

Maven repository layout; POM 4.0.0 reference; `docs/spec/manifest-profile.md` (maven rows).

**Scope**

- `internal/format/maven/parse.go`: path → coordinates (`com/example/lib-core/1.2.0/lib-core-1.2.0-sources.jar`
  → g=com.example, a=lib-core, v=1.2.0, classifier=sources, extension=jar); checksum file
  detection (`.sha1`, `.md5`, `.sha256`, `.sha512`, `.asc` signatures as files); `maven-metadata.xml`
  detection (artifact-level vs version-level for snapshots, which we reject in the MVP).
- POM parsing (stdlib `encoding/xml`): coordinates with parent inheritance for version/groupId,
  `packaging`, dependencies (for the metadata layer); property interpolation only for
  `${project.*}`.
- Entry: name `g:a`, version `v`, no variant; file layers typed `artifact` (annotations
  `extension`, `classifier`) and `pom`; metadata layer with files + checksums + POM essentials.
- `Validate`: POM coordinates ↔ path ↔ annotations; every artifact file name follows
  `<a>-<v>[-<classifier>].<ext>`.

**Acceptance criteria**

- [ ] Tests with `testutil.Jar`/`POM` and a table of real-world paths (classifiers, dotted
      artifactIds, `tar.gz` extension, `pom` packaging only, parent-inherited version).
- [ ] POMs with `${revision}` (CI-friendly versions) are rejected with a clear message in the MVP.
