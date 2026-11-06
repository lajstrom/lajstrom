---
id: M0-14
title: "spike(provider): registry capability matrix: GHCR"
milestone: M0
lane: platform
size: S
priority: p1
labels: [area/provider, kind/spike, good first issue]
depends: []

---
**Question to answer**

Which cells of the capability matrix (`docs/architecture.md` §5) are true for GHCR, measured
with `oras`, `curl` and the registry's own API, not from memory.

**Method**

Use a personal PAT. Verify: create on push, referrers API, listing via the packages REST API (no `_catalog`), delete via REST, rate limits observed, anonymous pull on public packages, blob redirect.

For each row: repo created on push · referrers API (push and list) · repo listing and its
pagination · delete manifest · immutable tags · blob redirect (307 and to where) · max blob size
· anything that rejects our manifest profile (artifactType, empty config, custom media types).

**Deliverable**

`docs/registries/ghcr.md`: the filled row, the exact commands and responses (trimmed), the
registry version tested, and "what the provider must do differently" as a short list. Link the
page from `docs/registries/README.md`. M1-12 consolidates the pages.

**Timebox**

Half a day (S). If blocked (account, access), write what was learned and stop.
