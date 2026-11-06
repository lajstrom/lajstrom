---
id: P-02
title: "feat(format/npm): npm plugin: packument rendering, publish, dist-tags, scoped packages"
milestone: none
lane: none
size: L
priority: p2
labels: [area/formats, kind/feature, post-mvp]
depends: []
---
**Goal**

npm registry API subset for `npm install` and `npm publish`: packument with `versions`, `dist.tarball` URLs, `dist-tags` (mutable pointers stored in the index, derived from a `latest` rule or annotations), scoped names `@scope/name` → path `scope/name`, `npm login` token flow.

**Status**

Post-MVP. Not scheduled; discussed at the Friday review once v0.1.0 is out. Comments and design
notes welcome; no implementation PRs before it gets a milestone.
