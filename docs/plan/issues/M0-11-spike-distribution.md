---
id: M0-11
title: "spike(provider): registry capability matrix: docker distribution v3"
milestone: M0
lane: platform
size: S
priority: p1
labels: [area/provider, kind/spike, good first issue]
depends: []

---
**Question to answer**

Which cells of the capability matrix (`docs/architecture.md` §5) are true for docker distribution v3, measured
with `oras`, `curl` and the registry's own API, not from memory.

**Method**

Run distribution v3 from the dev stack with and without `storage.delete.enabled`, and with the S3 driver against MinIO (compose profile) to observe blob redirects. Check `_catalog` pagination behaviour with 150 repos.

For each row: repo created on push · referrers API (push and list) · repo listing and its
pagination · delete manifest · immutable tags · blob redirect (307 and to where) · max blob size
· anything that rejects our manifest profile (artifactType, empty config, custom media types).

**Deliverable**

`docs/registries/distribution.md`: the filled row, the exact commands and responses (trimmed), the
registry version tested, and "what the provider must do differently" as a short list. Link the
page from `docs/registries/README.md`. M1-12 consolidates the pages.

**Timebox**

Half a day (S). If blocked (account, access), write what was learned and stop.
