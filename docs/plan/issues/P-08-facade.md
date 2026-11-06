---
id: P-08
title: "feat(facade): docker image passthrough with RBAC and audit; multi-arch stitching by hook"
milestone: none
lane: none
size: L
priority: p2
labels: [area/facade, kind/feature, post-mvp]
depends: []
---
**Goal**

Architecture §7. `oci` repos pass images through unchanged; sessions collect per-platform manifests and materialise the index; policy for platform sets and timeouts.

**Status**

Post-MVP. Not scheduled; discussed at the Friday review once v0.1.0 is out. Comments and design
notes welcome; no implementation PRs before it gets a milestone.
