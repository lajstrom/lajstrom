---
id: P-04
title: "feat(repo): proxy repositories: upstream fetch, wrap, push, TTL metadata cache (pypi and maven first)"
milestone: none
lane: none
size: L
priority: p2
labels: [area/core, kind/feature, post-mvp]
depends: []
---
**Goal**

Flow D. `kind: proxy` repos with an upstream URL, metadata cache in `proxy_meta`, miss → fetch → ingest as a normal artifact; negative caching; allow/deny lists; offline mode.

**Status**

Post-MVP. Not scheduled; discussed at the Friday review once v0.1.0 is out. Comments and design
notes welcome; no implementation PRs before it gets a milestone.
