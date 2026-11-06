---
id: M0-12
title: "spike(provider): registry capability matrix: Zot"
milestone: M0
lane: platform
size: S
priority: p1
labels: [area/provider, kind/spike, good first issue]
depends: []

---
**Question to answer**

Which cells of the capability matrix (`docs/architecture.md` §5) are true for Zot, measured
with `oras`, `curl` and the registry's own API, not from memory.

**Method**

Run Zot from the dev stack. Check referrers API, `_catalog` with pagination, delete, immutable-tag options, blob redirect when the storage is S3 (Zot has an S3 driver; try with MinIO) and tag listing limits. Zot is our primary dev registry, so also record the exact config we need.

For each row: repo created on push · referrers API (push and list) · repo listing and its
pagination · delete manifest · immutable tags · blob redirect (307 and to where) · max blob size
· anything that rejects our manifest profile (artifactType, empty config, custom media types).

**Deliverable**

`docs/registries/zot.md`: the filled row, the exact commands and responses (trimmed), the
registry version tested, and "what the provider must do differently" as a short list. Link the
page from `docs/registries/README.md`. M1-12 consolidates the pages.

**Timebox**

Half a day (S). If blocked (account, access), write what was learned and stop.
