---
id: P-01
title: "feat(format/rpm): rpm plugin: header parsing, repodata rendering, GPG-signed repomd"
milestone: none
lane: none
size: L
priority: p2
labels: [area/formats, kind/feature, post-mvp]
depends: []
---
**Goal**

Yum/DNF repositories: parse RPM headers (lead, signature, header sections), render `repodata/*.xml.gz` and `repomd.xml`, sign `repomd.xml.asc` with the repo key, serve `Packages/` by digest. Variant = arch. Upload via `oras push` and `PUT`.

**Status**

Post-MVP. Not scheduled; discussed at the Friday review once v0.1.0 is out. Comments and design
notes welcome; no implementation PRs before it gets a milestone.
