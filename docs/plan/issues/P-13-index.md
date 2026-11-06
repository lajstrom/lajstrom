---
id: P-13
title: "feat(index): control-plane snapshot into the registry and restore"
milestone: none
lane: none
size: L
priority: p2
labels: [area/index, kind/feature, post-mvp]
depends: []
---
**Goal**

Periodic export of repos/users/bindings (secrets encrypted with the master key) as an OCI artifact under `<prefix>_meta/config`; `lajstrom restore --from-registry`.

**Status**

Post-MVP. Not scheduled; discussed at the Friday review once v0.1.0 is out. Comments and design
notes welcome; no implementation PRs before it gets a milestone.
