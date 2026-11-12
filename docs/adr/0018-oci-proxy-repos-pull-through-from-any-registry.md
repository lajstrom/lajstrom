# 0018. An oci proxy repo pulls through from any OCI registry

- Status: accepted; amended by #876, #875 (below, Amendments)
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
  is allowed, the manifest the tag named is left untagged: Gitea deletes it at once, Quay stops
  serving it by digest, and Zot (by default) and GitLab's online collector (variant B) remove
  untagged manifests (quay.md, gitea.md, gitlab.md). Lajstrom never re-points a tag (CLAUDE.md, ADR
  0005); INV-7 leaves `oci` repos out only because their tags are the client's.
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

Six points, each weighed on correctness, the rule that a tag never moves (INV-7), immutable backends
(ECR), storage, upstream rate limits, client timeouts and complexity. The choice closes each point;
[Decision](#decision) states it.

### 1. Manifests and indexes

- **A. Cached documents, lazy per platform.** A tag's digest, and the exact bytes of a manifest or
  an index checked against their digest, are `proxy_meta` rows; a manifest is served from its row
  until the backend holds it, as ADR 0013 serves npm's documents from the cache and its files from
  the registry. A blob is fetched when a client asks for it. A manifest is pushed once all its blobs
  are held, an index once all its children are. A pull of `linux/arm64` stores that platform and
  costs Docker Hub one pull. The index of a multi-arch image is rarely complete, so it stays a row.
  Complexity: which cached manifest lists which blob, and the push on completion.
- **B. Eager.** On a tag's first miss, fetch the index, every child and every blob, push them all,
  then answer. The registry always holds whole images. It costs every platform's bytes
  (`nginx:latest`: about 510 MB instead of 64), one Docker Hub pull per architecture (a tenth of the
  anonymous budget for one image), and a first manifest request that waits for all of it, past any
  client's timeout.
- **C. A, with a platform manifest's blobs prefetched when it is served.** A `GET` that is not a
  pull (`docker manifest inspect`, `crane manifest`, Renovate) copies gigabytes for nothing.

A sparse index pushed at once is no option for every backend: distribution and Quay refuse it.
**Chosen: A.**

### 2. Tags

- **A. Re-pointed in the backend, alone.** The backend shows live tags. But on Gitea, Quay, Zot and
  GitLab B the digest the tag named is lost, at once or at the next collection, though clients pin
  it, and on ECR's `IMMUTABLE` (Lajstrom's default there) and under any tag policy the re-point is
  refused.
- **B. Only in `proxy_meta`.** No upstream tag reaches the backend; each held digest gets a keep-tag
  of Lajstrom's own form, written once, so that the collection of untagged manifests leaves it.
  Holds everywhere, moves no tag; the backend shows keep-tags only, and a registry backup has no
  `nginx:1.27`.
- **C. Written once, first digest wins.** After the upstream moves, the backend's tag is stale, and
  reindex after a lost database would bring back a months-old `latest` without a word.
- **D. Both, where the backend allows it.** Every held digest gets its write-once keep-tag, as in B,
  so a pinned digest stays. Where the backend lets a tag move, Lajstrom also pushes the upstream's
  tag and moves it when the upstream does: the backend shows live tags. Where it does not, B. It is
  an exception to "never re-point a tag", scoped to the upstream's tags of an `oci` proxy.

**Chosen: D.**

### 3. Large layers

- **A. Wait for the full copy**, as ADR 0013 point 3 says, and serve from the backend. The fetch
  runs on its own context (ADR 0013 point 4), so when containerd gives up after 5 minutes the copy
  goes on and a later try finds the layer held: the first pull of a multi-gigabyte layer goes
  through `ImagePullBackOff` before it succeeds.
- **B. A verified tee.** The request that holds the claim gets the upstream's bytes as they go into
  the backend, under the safety rules of the Decision. distribution's proxy mode tees with
  `io.MultiWriter(w, bw)`
  ([proxyblobstore.go](https://github.com/distribution/distribution/blob/main/registry/proxy/proxyblobstore.go)):
  there a slow client slows the cache, a client that leaves cancels it, and each concurrent miss
  fetches from the remote; the rules below avoid all three.
- **C. Redirect the client to the upstream** while the copy runs. The client gets an upstream URL
  (proxy-and-group.md §7 forbids it) and must reach the upstream, which a node behind a mirror often
  cannot.

**Chosen: B, in the first implementation.**

### 4. Docker Hub

- **`library/`.** containerd sends `library/nginx` to a mirror for `nginx`; docker sends what is
  typed after the proxy's host (`oci/hub/nginx`); Docker Hub's API knows no `nginx` without
  `library/`. A, no rule: `oci/hub/nginx` is `404`. B, fetch as `library/nginx` and store under the
  name asked: two copies, two counted pulls. C, canonical: one copy, both spellings work. **Chosen:
  C.**
- **The `401` for a repository that does not exist.** A, keep proxy-and-group.md §6: `502`, an error
  in the log, no negative row, and every mistyped name reaches the upstream again. B, `404` with a
  negative row. **Chosen: B.**
- **Rate limits.** A, react to `429` only. B, A and a gauge from `ratelimit-*`. C, B and a local
  stop when the budget is spent: Docker Hub sends no reset time and the budget is shared with every
  host behind the address, so a local count is wrong both ways. **Chosen: B.**

### 5. Groups of `oci` repos

In scope, a group needs `/v2` resolved per request (a tag from the hosted member first, a blob from
the member whose manifest lists it), merged `tags/list` and referrers, and uploads sent to the
hosted member: about as large as the proxy. containerd and Talos list several endpoints per mirror,
which covers much of the need. **Chosen: out of scope**, settled in
[#848](https://github.com/lajstrom/lajstrom/issues/848); groups of `oci` repos stay refused.

### 6. How the generic upstream authenticates

- **A. The generic provider's client, read-only**, with the realm rule of `registries[]`: one OCI
  client, its retries and breaker, INV-20 as it is.
- **B. The proxy's npm client (`proxy.Upstream`) taught the bearer challenge**: the challenge, the
  realm rule and the OCI requests written twice.
- **C. Anonymous only**: no private upstream, and one anonymous Docker Hub budget for a fleet.

A variant of A that lets an anonymous token request go to any public `https` realm makes the realm
rule depend on whether credentials are set. **Chosen: A**, with authentication configured per
upstream.

## Decision

Decided by the lead on 2026-11-12; the merge of this record is his approval.

1. **Kind.** An `oci` repo may be a `proxy`: this amends ADR 0013 point 1. `upstreams[].format: oci`
   names a registry's root URL. The upstream client is generic, with Docker Hub's rules (point 5) on
   top. `/v2` fetches on an `oci` proxy, which amends http-routing.md §6 for `oci`; the `PUT` of a
   manifest that is not a referrer stays `405`; containerd's `?ns=` is ignored. Groups of `oci`
   repos stay refused (#848).
2. **What is kept** is the upstream's bytes: no manifest of the profile and no `lajstrom.origin.*`
   annotation, so ADR 0013 point 3 does not apply to `oci`. The origin is in the audit row
   `proxy.fetch` (`details.upstream`, `details.ref`). Every push goes through `internal/ingest`,
   with the visibility barrier (#446), and records `oci_repos`. Before any upstream request: the
   allow and deny lists on the upstream path, `CheckName` on `oci/<repo>/<path>`, offline mode.
   Quay's build rule is a `4xx` naming the backend and a `proxy.reject`, never a `500`. Claims and
   the cache are ADR 0013 points 4 and 5: `fetch_claims` keyed by repo, path and reference.
3. **Manifests and indexes are cached documents, fetched per platform** (1A). A tag's digest, and a
   manifest's or an index's exact bytes checked against the digest, are `proxy_meta` rows; a
   manifest fetched by tag must agree with `Docker-Content-Digest` when the upstream sends one. A
   manifest is pushed by digest once every blob it lists is held, an index once every child is.
4. **Tags: live where the backend lets them move, kept in `proxy_meta` always** (2D).
   - The `proxy_meta` row answers a tag; without one (a lost database, an upstream down, offline), a
     tag the backend holds answers, unless the fallback below is on for that repository.
   - Every manifest pushed, and every index once complete, gets a keep-tag of Lajstrom's own form,
     clear of the referrers tag schema `sha256-<hex>`: written once, never moved, never shown by
     `tags/list`. A digest that clients pin stays when its tag moves on.
   - Where the backend lets a tag move, the upstream's tag is pushed once what it names is held, and
     pushed again, under the tag lock, when the upstream moves it and the new manifest or index is
     held. A tag whose index is not complete stays in `proxy_meta` only, until it is.
   - Where it does not, the upstream's tags stay in `proxy_meta` only. Lajstrom tells by the
     registry's `Capabilities.ImmutableTags` (the vendor table, the probe,
     `registries[].capabilities.immutableTags`; on ECR, `imageTagMutability` `IMMUTABLE*`), and
     otherwise by trying: a push of a tag that the backend refuses as a conflict (Quay's
     `409 TAG_IMMUTABLE`, Harbor's `412`, GitLab's `IMMUTABLE_TAG`, ECR's refusal: the provider's
     `ErrConflict`) turns the fallback on for that OCI repository, since policies go by repository
     and tag pattern. The fallback is remembered in a `proxy_meta` row of its own kind, so other
     replicas and restarts do not try again (a purge of the cache tries once more), and logged at
     warn with the registry. The pull that met the refusal is answered from the row; the tags pushed
     before it keep their first digest in the backend and answer nothing. Zot behind a policy
     without `update` answers `403 DENIED`, which reads as refused credentials: there the operator
     sets `capabilities.immutableTags: true`.
   - **This is an exception to "never re-point a tag"**, explicit and scoped: Lajstrom moves a tag
     only in an `oci` proxy repo, only one of the upstream's tags, only to a digest the upstream
     named for it and the backend holds. It never moves a keep-tag, a tag of a hosted or managed
     repo, or a tag on a conflict with a client's push. The implementation pull request that first
     moves a tag changes CLAUDE.md's invariant line and INV-7 in guarantees.md to say so: a contract
     change this record approves.
5. **Docker Hub's rules.** The first goes by the upstream's host; the others serve every upstream.
   - `library/`, on `registry-1.docker.io` and `docker.io` as the vendor table's realm hosts are,
     with a key that turns it on for a Docker Hub mirror elsewhere: a one-component path is
     `library/<name>` on every `/v2` route, where the facade splits the name, so `nginx` is stored
     once, as `library/nginx`.
   - A `401` or `403` on a repository read after a token was granted, which is how Docker Hub and
     quay.io answer for a repository that does not exist, is `404` with a negative row
     (`negativeTTL`); a refused token request (a wrong password) stays `502`. On an upstream without
     credentials, any `401` or `403` is `404`, ghcr.io's `403 DENIED` included.
   - A tag is revalidated with `HEAD`, which Docker Hub does not count, and a manifest or blob that
     the backend or the cache holds is never asked for with `GET`. `ratelimit-limit` and
     `ratelimit-remaining` feed a gauge per upstream, the window read from `w=` and never assumed,
     with a warning below 10 %. A `429` serves the stale row, otherwise `503` with `Retry-After`
     (the upstream's, at most 60 s; 5 s without one).
6. **Blobs and the verified tee** (3B, part of the first implementation, possibly its own pull
   request after the basic path). A blob is fetched only when a manifest held or cached at that path
   lists it, so its size is checked against `maxFileSize` and `maxBlobSize` before the download; a
   digest nothing lists is `404` without an upstream request. It streams into a backend upload
   session, hashed, and is committed with its digest (ADR 0014, no spool). The request that holds
   the claim is served from that stream, an exception to ADR 0013 point 3's "answered from the
   registry", under these rules:
   1. Only a blob whose digest and size a held or cached manifest gives: the `Content-Length` is
      known, and nothing else tees.
   2. The last byte is held back, as INV-40 does on a download, until the bytes have the digest
      **and** the backend has committed the blob. No client completes a blob the backend does not
      hold or whose bytes are not its digest; a refusal aborts the connection, and the client
      retries the layer.
   3. The copy never waits for the client: the client reads through a bounded buffer of a few MiB
      and is dropped (its connection aborted) when it falls behind or leaves; the fetch goes on.
   4. Only the claim holder tees. A `Range` request, and other waiters, are served from the backend
      once the blob is held.
   5. Nothing is kept outside the registry but that buffer (ADR 0001).
7. **Authentication, per upstream**, through the generic provider's client, read-only (a new
   `generic.NewUpstream`): anonymous; Basic, a `username` and its password from a file
   (`passwordFile`) or the environment (`passwordEnv`, as `registries[].auth` takes it); or a static
   bearer token (`tokenFile`). A bearer challenge is answered at its realm when the realm is on the
   upstream's host, or over `https` on a host of the upstream's `tokenRealmHosts` or of the vendor
   table (`auth.docker.io`); any other realm is an error naming its host. Credentials go to the
   upstream's origin and that realm only; a redirect of a manifest or a blob to another host must be
   `https` and resolve to public addresses (proxy-and-group.md §9.2, a new `PublicRedirects` option
   of the provider). Cloud-specific authentication, ECR through IAM and GCR or Artifact Registry
   through a service account, is later work.

ADR 0013's status line names what this record supersedes, as ADR 0005's names 0017. A `contract`
pull request brings proxy-and-group.md (§1, §6, a new §11), http-routing.md §6, index-schema.md (the
`proxy_meta` kinds), guarantees.md (INV-3, INV-31, INV-33) and configuration.md
(`upstreams[].format: oci`, `upstreams[].tokenRealmHosts`, `upstreams[].auth.passwordEnv`, the
Docker Hub key) to this record; CLAUDE.md and INV-7 change with the first tag moved (point 4).

## Consequences

- docker, crane and containerd pull from any registry through one repo kind, on every backend
  Lajstrom supports, ECR's `IMMUTABLE` included. A second pull makes no upstream blob request; a
  cold pull costs Docker Hub one pull per platform pulled, and revalidation costs none.
- **Lajstrom now moves tags**, in one place: the upstream's tags of an `oci` proxy, on backends that
  allow it. The rule elsewhere is unchanged. A digest a client pinned stays served through its
  keep-tag, and the backend shows keep-tags beside the live tags.
- On a backend that lets tags move, the backend shows the upstream's tags for what is complete:
  single-platform images, and multi-arch ones once every platform was pulled; reindex and a registry
  backup keep those. Everywhere else, tags and incomplete indexes live in `proxy_meta`: a backup of
  the registry restores manifests and blobs, not `nginx:1.27`, and a lost database costs a `HEAD`
  per tag and a counted `GET` per index the backend lacks. INV-33 changes for `oci`.
- Finding a tag policy that the capability does not describe costs one refused push per OCI
  repository; Zot behind a policy needs `capabilities.immutableTags: true`, the first use of that
  capability.
- The claim holder's pull streams from its first byte. Waiters on other replicas still wait for the
  whole copy and can meet containerd's 5-minute timeout; a claim holder too slow for the buffer is
  dropped and waits like them. The tee is a three-way stream with its own aborts: new code that
  needs its tests (a slow client, a client gone, a refused commit, a digest mismatch).
- Storage follows what is pulled.
- A mistyped name is a cheap `404`; a credential that lost access to a private repository, or an
  upstream that blocks the egress address with a `403`, is a `404` too, for `negativeTTL`.
- Follow-ups: the `contract` pull request above; #848; registry.k8s.io and gcr.io as configuration
  for Talos (#340), with no code since the client is generic; Quay's build rule in the vendor table
  (#662), so that it is refused before the upstream is asked; the threat model's rows for upstream
  realms and blob redirects; cloud-specific upstream authentication.

## Amendment: a platform a client pulled is held whole (#876, 2026-11-12)

Point 1 rejected option C, prefetching a platform manifest's blobs when it is served, because of
what a `GET` that is not a pull would copy. A Talos boot through the proxy then showed what A alone
costs (#876):
- A client asks only for the layers it does not hold, and containerd keeps layers by digest across
  images.
- kube-apiserver and kube-scheduler took 14 of their 15 layers from kube-controller-manager, so they
  never asked for those layers at their own paths, and their manifests were never pushed.
- So their copy in the backend stayed incomplete, an offline repo could not serve them, and a node
  with an empty store fetched those layers from the upstream again.

Point 3 of the Decision gains two rules:
- **Mount before fetch.** A blob the backend holds at another path of the repo is mounted, not
  fetched (proxy-and-group.md §11.5, step 2). This holds offline too.
- **A platform a client pulled is held whole.** Every blob a platform's manifest lists is made held
  at the path, in the background and never before an answer (§11.5, step 5):
  - once a client got the manifest, each blob the backend holds at another path of the repo is
    mounted, offline too; nothing is fetched;
  - once a client asked for one of the manifest's layers at the path, each blob still missing is
    fetched under its claim.

Option C is thus taken only for a platform a client actually pulled, never for an index's other
platforms. A `GET` that is not a pull (Renovate, `crane manifest`, `skopeo inspect`) reads a
manifest and maybe its config, never a layer, so it costs mounts only, and point 1's cost does not
apply to it. A layer the backend holds nowhere and no client asks for, because the nodes got it
from outside Lajstrom, waits for a client's request for a layer of that platform, or for a
prefetch (#886).

## Amendment: a request that does not hold the claim gets a transit (#875, 2026-11-12)

The Context named one client timeout, the CRI's `image_pull_progress_timeout` of 5 minutes without
a byte. There is a shorter one before it: containerd's HTTP transport waits
`ResponseHeaderTimeout: 30 * time.Second` for the headers of every answer
(`core/remotes/docker/registry.go:267`, v2.3.4), and `hosts.toml` cannot change it; an ingress in
front of Lajstrom usually gives up after 60 s. Rule 4 of point 6 answered every request but the
claim holder from the backend once the blob is held, with no headers before it. #875 measured what
that costs: on kind with two nodes and a 2 GiB layer from an upstream at 2.8 MB/s, the node that
held the claim pulled through the tee in 12 min 29 s, and the other failed after 31 s with
`timeout awaiting response headers`. So did a request on another replica waiting on the claim, and
the claim holder's own retry after the tee dropped it. A DaemonSet, or a Deployment spread over
nodes, of an image with large layers (CUDA, PyTorch: 2–5 GB) fails on every node but one for as
long as the copy takes.

Option 3B said its rules avoid three things distribution's proxy does; the third, "each concurrent
miss fetches from the remote", is now taken on purpose, as distribution's `ServeBlob` serves a blob
in flight "from the remote store directly". Headers first (an immediate `200` and the body from the
backend after the commit) was considered and not taken alone: containerd then waits under the CRI's
5 minutes, so a copy longer than that still fails those nodes.

Rule 4 of point 6 becomes:

4. **Only the claim holder tees; the other requests get a transit.** While the claim's copy runs,
   on this replica or another, a `GET` without `Range` that does not hold the claim opens a `GET`
   of its own from the upstream, with the fetch's redirect, credential and digest rules and its
   resumes (proxy-and-group.md §11.5), and streams it to its client. Its last byte is held until
   the bytes have the digest; other bytes abort the connection and write `proxy.reject`. It writes
   nothing anywhere: the backend gets the claim holder's one copy. `upstreams[].maxConcurrentTransits`
   caps the transits of one blob on a replica (unset or `-1`: no cap, the default; `0`: none); a
   request beyond the cap, a `Range` request, and every request once the blob is held are served
   from the backend.

INV-31 changes for `oci` proxy repos from "fetched from the upstream once across replicas" to
"stored once": while a copy runs, the concurrent clients cost one more transfer from the upstream
each (egress; Docker Hub does not count blob `GET`s), within the cap.
