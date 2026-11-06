---
id: M0-15
title: "spike(provider): registry capability matrix: Harbor"
milestone: M0
lane: platform
size: S
priority: p2
labels: [area/provider, kind/spike]
depends: []

---
**Question to answer**

Which cells of the capability matrix (`docs/architecture.md` §5) are true for Harbor, measured
with `oras`, `curl` and the registry's own API, not from memory.

**Method**

Use `demo.goharbor.io` or a local Harbor via its installer (heavy; timebox strictly). Verify: push into an existing project, referrers (2.10+), project/repo listing API, delete, immutability rules, blob redirect with S3, quotas and the error they produce.

For each row: repo created on push · referrers API (push and list) · repo listing and its
pagination · delete manifest · immutable tags · blob redirect (307 and to where) · max blob size
· anything that rejects our manifest profile (artifactType, empty config, custom media types).

**Deliverable**

`docs/registries/harbor.md`: the filled row, the exact commands and responses (trimmed), the
registry version tested, and "what the provider must do differently" as a short list. Link the
page from `docs/registries/README.md`. M1-12 consolidates the pages.

**Timebox**

Half a day (S). If blocked (account, access), write what was learned and stop.
