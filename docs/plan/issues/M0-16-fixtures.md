---
id: M0-16
title: "test(testutil): in-process fixture builders: .deb, wheel, sdist, jar + POM"
milestone: M0
lane: formats
size: M
priority: p0
labels: [area/formats, kind/test, good first issue]
depends: []
---
**Goal**

Every format plugin needs realistic packages in its tests, and we do not vendor binaries from
distributions (license cleanliness, repo size). `internal/testutil` builds them in memory.

**Scope**

- `testutil.Deb(opts)`: a valid `ar` archive with `debian-binary`, `control.tar.gz` (and `.xz`,
  `.zst` variants) containing `control` and `md5sums`, and `data.tar.gz` with a couple of files.
  Options: name, version, arch, depends, description.
- `testutil.Wheel(opts)`: a zip with `<name>-<ver>.dist-info/{METADATA,WHEEL,RECORD}` and a
  module file; correct wheel file name from the tags. `testutil.Sdist(opts)`: `tar.gz` with
  `PKG-INFO`.
- `testutil.Jar(opts)` + `testutil.POM(opts)`: a jar (zip with `META-INF/MANIFEST.MF`) and a
  minimal POM with groupId/artifactId/version/packaging and optional dependencies.
- Each returns `[]byte` and the expected parsed metadata, so plugin tests can assert equality.

**Acceptance criteria**

- [ ] `dpkg-deb --info` accepts the generated `.deb` (verified in an Ubuntu container, documented
      in the test file; not required in CI).
- [ ] `pip install` of the generated wheel works in a Python container (same note).
- [ ] `mvn` resolves the generated jar+POM from a file repo (same note).
- [ ] Unit tests for each builder: archive structure, sizes, checksums.
