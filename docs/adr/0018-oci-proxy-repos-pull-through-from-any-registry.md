# 0018. An oci proxy repo pulls through from any OCI registry

- Status: proposed
- Date: 2026-11-12
- Deciders: gecube

## Context

[#442](https://github.com/lajstrom/lajstrom/issues/442) asks for a pull-through cache:
`docker pull <lajstrom>/oci/<proxy>/library/nginx:1.27` fetches on a miss, keeps the manifests,
indexes and blobs in the backend, and serves them with RBAC and audit; containerd, and Talos through
it (#340), use the proxy as a mirror. The lead decided on #442 that the upstream is generic: one
client for any registry that speaks the distribution API (Docker Hub, ghcr.io, quay.io,
registry.k8s.io, a private Harbor), with Docker Hub's rules on top, not one client per registry. The
plan is on the issue
([comment](https://github.com/lajstrom/lajstrom/issues/442#issuecomment-6827564110)).

Three texts in force stand in the way:

- [ADR 0013](0013-proxy-and-group-repos.md) point 1: "`oci` repos stay hosted", and
  proxy-and-group.md §1.
- ADR 0013 point 3: a fetched artifact is a manifest of the profile with `lajstrom.origin.*`
  annotations, and the client is answered from the registry after the ingest. An image cannot be
  rebuilt so: annotating a foreign manifest changes its digest, which is what the client asked for.
- http-routing.md §6: on a proxy, "`/v2` never fetches". For an `oci` proxy, `/v2` is the read
  route.

The forces:

- **Content addressing.** A manifest names its blobs, an index its manifests, by digest. The digest
  is the checksum (INV-31) and the identity: what Lajstrom keeps must be the upstream's bytes.
- **Backends refuse incomplete documents.** A manifest whose blob is missing:
  `400 MANIFEST_BLOB_UNKNOWN` on Zot and distribution (zot.md). An index whose child is missing,
  measured on the dev stack on 2026-11-12: distribution 3.1.2 `400 MANIFEST_BLOB_UNKNOWN` (its
  default `validation.manifests.indexes.platforms: all`), Zot v2.1.23 `201`; Quay announces
  `X-Sparse-Manifest-Support: false` (quay.md).
- **Upstream tags move; some backends' tags cannot.** The repositories Lajstrom creates on ECR are
  `IMMUTABLE` unless `registries[].ecr.imageTagMutability` says otherwise; Quay, Harbor, Nexus,
  GitLab and Zot have policies that refuse a re-point (quirks.md, Tag mutability). Where a re-point
  is allowed, Quay stops serving the old manifest by digest and Gitea deletes it (quay.md,
  gitea.md). Lajstrom never re-points a tag (CLAUDE.md, ADR 0005); INV-7 leaves `oci` repos out only
  because their tags are the client's.
- **Docker Hub's limits** ([pulls](https://docs.docker.com/docker-hub/usage/pulls/),
  [usage](https://docs.docker.com/docker-hub/usage/), read 2026-11-12): per 6 hours, 100 pulls per
  IPv4 address or IPv6 /64 without credentials, 200 for Personal, unlimited for Pro, Team and
  Business (fair use). A pull is a manifest `GET`; a `HEAD` does not count; a multi-arch image
  counts once per architecture. Answers carry `ratelimit-limit` and `ratelimit-remaining`; past the
  limit, a `429`. The documented header is `100;w=21600`, but an anonymous `HEAD` of `library/nginx`
  on the same day carried `ratelimit-limit: 100;w=3600`. Every replica behind one egress address
  shares one anonymous budget. `nginx:latest` lists 8 platforms of about 60 MB and 8 attestation
  manifests.
- **Client timeouts.** containerd's CRI cancels a pull that gets no new byte for
  `image_pull_progress_timeout`, 5m0s by default
  ([config.md](https://github.com/containerd/containerd/blob/main/docs/cri/config.md)). ADR 0013
  point 3 sends no byte before the whole file is in the registry: a layer whose copy takes more than
  5 minutes (3 GB at 10 MB/s, 15 GB at 50 MB/s) fails the first pull.
- **How upstreams authenticate**, measured on 2026-11-12. The bearer realm is on the registry's own
  host for ghcr.io, quay.io, registry.k8s.io, gcr.io, public.ecr.aws and nvcr.io, and on
  `auth.docker.io` for Docker Hub (the vendor table, #354, #397); mcr.microsoft.com asks for none.
  registry.k8s.io answers a manifest with a `307` to a regional `*-docker.pkg.dev`, and blobs
  everywhere go to CDNs and object storage on other hosts. For a repository that does not exist,
  Docker Hub and quay.io grant an anonymous token without the `pull` action, then answer the read
  `401` (`error="insufficient_scope"`), as for a private one; ghcr.io refuses the token request,
  `403 DENIED`. Docker Hub refuses a token request with a wrong password, `401`.
- **Backend names.** `oci/<repo>/<upstream path>` must pass `Capabilities.CheckName`: GitLab behind
  its token service takes 4 components, so `oci/hub/library/nginx` fits and `oci/ghcr/o/img/sub`
  does not. Quay's master refuses a new repository ending in `…/build`
  ([quay/quay#8070](https://github.com/quay/quay/issues/8070), quay.md, The build rule): Docker
  Hub's 197 public `<namespace>/build` images cannot be held on a Quay release that carries it.

## Considered options

Six points for the lead. Each option is weighed on correctness, the rule that a tag never moves
(INV-7), immutable backends (ECR), storage, upstream rate limits, client timeouts and complexity.
What every option shares is under [Decision](#decision).

### 1. Manifests and indexes

- **A. Cached documents, lazy per platform.** A tag's digest, and the exact bytes of a manifest or
  an index checked against their digest, are `proxy_meta` rows; a manifest is served from its row
  until the backend holds it, as ADR 0013 serves npm's documents from the cache and its files from
  the registry. A blob is fetched when a client asks for it. A manifest is pushed by digest once all
  its blobs are held, an index once all its children are; untagged, Zot collects them by default, so
  each needs a tag of some kind (point 2). A pull of `linux/arm64` stores that platform and costs
  Docker Hub one pull. The index of a multi-arch image is rarely complete, so it stays a row: the
  registry holds the platforms' manifests and blobs, the database the index and the tag. A lost
  database costs a counted `GET` per index again, and nothing held. Complexity: which cached
  manifest lists which blob, and the push on completion.
- **B. Eager.** On a tag's first miss, fetch the index, every child and every blob, push them all,
  then answer. The registry always holds whole images, and no document lives only in the database.
  It costs every platform's bytes (`nginx:latest`: about 510 MB instead of 64), one Docker Hub pull
  per architecture (a tenth of the anonymous budget for one image), and the first manifest request
  waits for all of it, past any client's timeout, unless the manifest is answered from a cache in
  the meantime, which is A with a prefetch.
- **C. A, with a platform manifest's blobs prefetched when it is served.** The layers copy in
  parallel before containerd asks for them. A `GET` that is not a pull (`docker manifest inspect`,
  `crane manifest`, Renovate) copies gigabytes for nothing.

A sparse index pushed at once is no option for every backend: distribution and Quay refuse it.

**Recommended: A.** Its cost follows what clients pull, and the upstream's bytes are kept as they
are. C can become a repo setting later.

### 2. Tags

- **A. Re-pointed in the backend** when the upstream moves and the new manifest is complete, under
  the tag lock. The backend is a mirror with tags, which reindex and a registry backup keep. But
  Lajstrom then re-points tags, against CLAUDE.md, ADR 0005 and INV-7's statement. ECR's
  `IMMUTABLE`, Lajstrom's default there, and every tag policy refuse it: the guide must ask for a
  registry entry with `MUTABLE` tags for proxies, and on an immutable one the backend keeps the
  first digest while the cache names another, which reindex brings back after a lost database. On
  Quay and Gitea the old digest stops being served, so a node pulling it mid-rollout gets a `404`
  until it is fetched again. Under 1A a multi-arch tag can be pushed only once every platform is
  held, except on a backend that takes a sparse index (Zot): A needs 1B.
- **B. Only in `proxy_meta`.** No upstream tag reaches the backend. A held manifest, and an index
  once complete, is pushed by digest under a keep-tag of Lajstrom's own form, written once and never
  moved, so that the backend's collection of untagged manifests (Zot's by default, distribution's
  `garbage-collect --delete-untagged`, ECR lifecycle rules) leaves it; `tags/list` answers the tags
  the rows know and hides the keep-tags. It holds on every backend, ECR's `IMMUTABLE` included,
  keeps "never re-point a tag" without an exception, and is not hit by Quay's 120 s tag-list lag
  (#803). Tags become a cache, as a proxy's npm dist-tags are (ADR 0013 point 5): a lost database
  costs one `HEAD` per tag while online; offline, it serves by digest only. An operator browsing the
  backend sees keep-tags where `1.27` was expected.
- **C. Written once, first digest wins**, the current one in `proxy_meta`. It works on ECR and never
  re-points, but once the upstream moves the backend's tag is stale, and after a lost database
  reindex would bring back a months-old `latest` without a word. The multi-arch limit of A applies.

**Recommended: B.** It is the only option that holds on ECR's default and with lazy fetching. Open:
the keep-tag's form, which must stay clear of the referrers tag schema `sha256-<hex>`.

### 3. Large layers

- **A. Wait for the full copy**, as ADR 0013 point 3 says. The blob streams from the upstream into a
  backend upload session, hashed on the way and committed with its digest (ADR 0014, no spool), and
  is served from the backend after the visibility barrier (#446), hashed again (INV-40). The fetch
  runs on its own context (ADR 0013 point 4): when containerd gives up after 5 minutes the copy goes
  on, and a later try of the kubelet finds the layer held. The first pull of a multi-gigabyte layer
  shows one or more `ImagePullBackOff`s, then succeeds. The simplest. Whether docker and crane give
  up the same way was not measured.
- **B. A verified tee.** The request that holds the claim gets the upstream's bytes as they go into
  the backend: an exception to "the client is answered from the registry". distribution's proxy mode
  tees with `io.MultiWriter(w, bw)`
  ([proxyblobstore.go](https://github.com/distribution/distribution/blob/main/registry/proxy/proxyblobstore.go)):
  there a slow client slows the cache, a client that leaves cancels it, and each concurrent miss
  fetches from the remote. A tee can be made safe:
  1. Only a blob whose digest and size a manifest held or cached at that path gives: the
     `Content-Length` is known, and nothing else tees.
  2. The last byte is held back, as INV-40 does on a download, until the bytes have the digest
     **and** the backend has committed the blob. No client completes a blob that the backend does
     not hold or whose bytes are not its digest; a refusal aborts the connection, and the client
     retries the layer.
  3. The copy never waits for the client: the client reads through a bounded buffer of a few MiB and
     is dropped (its connection aborted) when it falls behind or leaves; the fetch goes on.
  4. Only the claimant tees. A `Range` request, and waiters on other replicas, wait as in A.
  5. Nothing is kept outside the registry but that buffer (ADR 0001).

  The first byte comes at once, and the claimant's pull never trips the watchdog; waiters on other
  replicas still can. Complexity: a three-way stream and its aborts, about 300 lines with tests
  (est.); the hold-back exists (`format.digestWriter`).
- **C. Redirect the client to the upstream** while the copy runs. The client gets an upstream URL
  (proxy-and-group.md §7 forbids it) and must reach and authenticate to the upstream, which a node
  behind a mirror often cannot. Rejected.

**Recommended: B, built after A.** A is the first implementation, and enough for the Talos criterion
of #442, whose images are small. The tee is a pull request of its own: the 5-minute watchdog is the
default of the client #442 names, and it fails exactly the large images a cache is for.

### 4. Docker Hub

**`library/`.** containerd sends `library/nginx` to a mirror for `nginx`; docker sends what is typed
after the proxy's host (`oci/hub/nginx`); Docker Hub's API knows no `nginx` without `library/`.

- A. No rule: `oci/hub/nginx` is `404`, and the guide says `library/nginx`. One copy; docker users
  trip on it.
- B. Fetch `nginx` as `library/nginx` and store it under the name asked: two copies, two counted
  pulls.
- C. Canonical: on an upstream with the rule, a one-component path is `library/<name>` on every
  `/v2` route, where the facade splits the name. One copy, both spellings work, `_catalog` lists
  `library/nginx`. The rule is on by the upstream's host (`registry-1.docker.io`, `docker.io`), as
  the vendor table's realm hosts are, and a key turns it on for a Docker Hub mirror on another host.

**Recommended: C.**

**The `401` for a repository that does not exist.** proxy-and-group.md §6 reads a `401` as
Lajstrom's credentials refused: `502`, an error in the log, no negative row, and every mistyped name
reaches the upstream again.

- A. Keep §6.
- B. A `401` or `403` on a repository read after a token was granted is `404` with a negative row
  (`negativeTTL`); a refused token request, which is how Docker Hub answers a wrong password, stays
  `502`. On an upstream without credentials, any `401` or `403` is `404`, since no credential of
  Lajstrom's can be wrong (ghcr.io's `403 DENIED` included). A credential that lost access to one
  private repository, or an upstream that blocks the egress address with a `403`, then looks like a
  missing repository for `negativeTTL`.

**Recommended: B.**

**Rate limits.** Every option revalidates a tag with `HEAD` (not counted), never `GET`s a manifest
that the backend or the cache holds by digest, and serves a stale row on a `429` (proxy-and-group.md
§4.2), otherwise `503` with `Retry-After` (§6).

- A. That only.
- B. A, and `ratelimit-limit` and `ratelimit-remaining` (the window read from `w=`, never assumed)
  feed a gauge per upstream, with a warning below 10 %.
- C. B, and Lajstrom stops its counted `GET`s when the budget is spent. Docker Hub sends no reset
  time and the budget is shared with every host behind the address, so a local count is wrong both
  ways.

**Recommended: B**, and the guide tells operators to give the upstream a credential (a Docker Hub
username, a PAT in `passwordFile`).

### 5. Groups of `oci` repos

- A. In scope: one URL for hosted and proxied images, as Nexus's docker group. A group has no OCI
  repository today (http-routing.md §6); it would need `/v2` resolved per request (a tag from the
  hosted member first, a blob from the member whose manifest lists it), merged `tags/list` and
  referrers, uploads and mounts sent to the hosted member, and ADR 0013 point 6's hiding by
  repository path. About as large as the proxy.
- B. Out of scope: groups of `oci` repos stay refused (`checkKind`), and get an issue of their own.
  containerd and Talos list several endpoints per mirror, in order, which covers much of the need;
  docker's `registry-mirrors` takes one, for Docker Hub only.

**Recommended: B.**

### 6. How the generic upstream authenticates

- A. The generic provider's client, read-only (a new `generic.NewUpstream`): the bearer challenge,
  anonymous tokens, Basic exchanged at the realm (or sent to the upstream's origin when it asks for
  Basic), or a static token (`tokenFile`), with `realmTransport`'s rule as it is: a realm on the
  upstream's host, or over `https` on a host of a new `upstreams[].tokenRealmHosts` or of the vendor
  table (`auth.docker.io`); any other realm is an error naming its host. Redirects of manifests and
  blobs follow proxy-and-group.md §9.2: credentials to the upstream's origin only, `https` and
  public addresses for any other host (a new `PublicRedirects` option: today the provider follows a
  backend's redirects anywhere, the backend being the operator's). One OCI client with its retries
  and breaker; INV-20 as it is.
- B. The proxy's npm client (`proxy.Upstream`) taught the challenge: §9.2 is there already, but the
  challenge, the realm rule and the OCI requests would be written twice.
- C. Anonymous only, at first: no private upstream, and one anonymous Docker Hub budget for a fleet.
- A variant of A lets an anonymous token request go to a realm on any public `https` host, since no
  credential goes there. It saves a line of configuration for an unknown registry, and makes the
  realm rule depend on whether credentials are set. Not recommended.

Credentials that are neither a password nor a token, such as a private ECR's 12-hour tokens, are out
of scope. **Recommended: A.**

## Decision

Proposed: the lead chooses an option for each point above. What every option shares:

1. **Kind.** An `oci` repo may be a `proxy`: this amends ADR 0013 point 1. `upstreams[].format: oci`
   names a registry's root URL. One generic upstream client, and Docker Hub's rules (point 4) by
   host.
2. **What is kept** is the upstream's bytes: no manifest of the profile and no `lajstrom.origin.*`
   annotation, so ADR 0013 point 3 does not apply to `oci`. The origin is in the audit row
   `proxy.fetch` (`details.upstream`, `details.ref`). Every push goes through `internal/ingest`,
   with the visibility barrier (#446), and records `oci_repos`.
3. **What may be fetched.** A blob only when a manifest held or cached at that path lists it, so its
   size is checked against `maxFileSize` and `maxBlobSize` before the download; a digest that
   nothing lists is `404` without an upstream request. The digest is the checksum (INV-31); a
   manifest fetched by tag must agree with `Docker-Content-Digest` when the upstream sends one. A
   blob streams into the backend, without a spool (ADR 0014).
4. **Before any upstream request:** the allow and deny lists on the upstream path, `CheckName` on
   `oci/<repo>/<path>`, offline mode. Quay's build rule is a `4xx` naming the backend and a
   `proxy.reject`, never a `500`.
5. **Claims and the cache** as ADR 0013 points 4 and 5: `fetch_claims` keyed by repo, path and
   reference; tags and documents in `proxy_meta`; revalidation by `HEAD`.
6. **`/v2` fetches** on an `oci` proxy, which amends http-routing.md §6 for `oci`; the `PUT` of a
   manifest that is not a referrer stays `405`; containerd's `?ns=` is ignored.

On acceptance, ADR 0013's status line names what 0018 supersedes, as ADR 0005's names 0017: point
1's "`oci` repos stay hosted", point 3 for `oci`, and with 3B its rule that the client is answered
from the registry. A `contract` pull request then brings proxy-and-group.md (§1, §6, a new §11),
http-routing.md §6, index-schema.md (the `proxy_meta` kinds), guarantees.md (INV-3, INV-7, INV-31,
INV-33) and configuration.md (`upstreams[].format: oci`, `upstreams[].tokenRealmHosts`) to the
options chosen. Option 2A would change CLAUDE.md's tag rule as well.

## Consequences

Of the recommended set (1A; 2B; 3B after 3A; 4: C, B, B; 5B; 6A):

- docker, crane and containerd pull from any registry through one repo kind, on every backend
  Lajstrom supports, ECR's `IMMUTABLE` included, and Lajstrom still never re-points a tag. A second
  pull makes no upstream blob request; a cold pull costs Docker Hub one pull per platform pulled,
  and revalidation costs none.
- **The registry is no longer the whole of a proxy.** Tags, and the indexes of which not every
  platform was pulled, live in `proxy_meta`: a backup of the registry restores the platforms'
  manifests and blobs, not `nginx:1.27`. A lost database costs a `HEAD` per tag and a counted `GET`
  per index; offline after that, pulls by digest only. INV-33 changes for `oci`.
- Storage follows what is pulled. The backend shows keep-tags rather than the upstream's tags, and a
  held manifest whose keep-tag an operator deletes is fetched again on the next miss.
- Until the tee lands, the first pull of a layer that copies for more than 5 minutes goes through
  `ImagePullBackOff` before it succeeds; with the tee, the claimant streams and other replicas wait.
- A mistyped name is a cheap `404`; a credential that lost access to a private repository is a `404`
  too, for `negativeTTL`.
- Follow-ups: the `contract` pull request above; an issue for groups of `oci` repos; registry.k8s.io
  and gcr.io as configuration for Talos (#340), with no code since the client is generic; Quay's
  build rule in the vendor table (#662), so that it is refused before the upstream is asked; the
  threat model's rows for upstream realms and blob redirects; a private ECR as an upstream.
