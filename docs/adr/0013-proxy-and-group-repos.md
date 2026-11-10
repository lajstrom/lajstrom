# 0013. Proxy and group repos: fetch on miss into the one write path, hosted names hide the upstream

- Status: accepted
- Date: 2026-11-10
- Deciders: gecube

## Context

M4 (decided on 2026-11-10) is a Verdaccio-style npm registry: private packages and npmjs.org
under one URL, `registry=https://host/npm/<group>/` in `.npmrc`. That takes two kinds of repo
beside `hosted`, which is the only kind so far (index-schema.md, `repos.kind`):

- a **proxy**, which serves an upstream (npmjs.org) and keeps what it fetched;
- a **group**, which serves several repos of one format under one URL, in order, and takes
  the publishes for its hosted member.

npm comes first; pypi and maven follow on the same core (#67, #68), so the core must not know
npm. ADR 0012 (#302) is npm itself: its profile, routes and dist-tags journal. This record is
the part that every format shares, and the hooks a format plugin provides to take part.

The forces:

- **ADR 0001.** The registry is the only storage, and the index is derived. A fetched file
  is an artifact like any other: if it lives anywhere but the registry, backups and reindex do
  not see it.
- **One write path.** Every manifest goes through `internal/ingest`, which claims, pushes,
  indexes and audits.
- **Replicas.** Two replicas that miss the same tarball at once must not both download it, and
  must not race on its tag.
- **Dependency confusion.** A group that merges an internal `@acme/billing` with an npmjs
  package of the same name installs whichever version is higher. That is the attack Alex
  Birsan published in 2021; Artifactory answers it with the priority resolution of its virtual
  repos, Nexus with routing rules.
- **SSRF.** A proxy makes Lajstrom an HTTP client of URLs that are not the operator's
  registries. Today no non-admin controls a URL (threat model S-1); a proxy must keep that
  true, and it adds URLs that come from the upstream's own documents and redirects.
- **ADR 0008.** The proxy is the enforcement point of policy rules before a fetch; the design
  must leave that seam where ADR 0008 put it.

## Considered options

**What a fetched file becomes.**

- Pass-through: stream from the upstream, keep nothing. No offline mode, every install is the
  upstream's load, and nothing to attach a verdict to.
- A cache of our own (a directory, a table of blobs). Breaks ADR 0001.
- An ordinary manifest of the profile, ingested through the one write path, with its origin in
  annotations. Chosen.

**Where the upstream's metadata lives.** In the registry as an artifact: its tag would be
re-pointed on every refresh, or a journal would grow with every TTL. Not cached: no offline
mode, and every resolve is an upstream request. In the database, as a cache that may be dropped
at any time: chosen. It is not derived from the registry, so reindex leaves it alone.

**One fetch per artifact across replicas.** No coordination: the same bytes give the same
digest and ingest shares the claim, but every replica downloads, and an upstream that changes
the bytes between two downloads makes one of them a `409`. A Postgres advisory lock held for the
download: it holds a pool connection for minutes. A coordination row with a heartbeat, as
`reindex_runs` has: chosen.

**Who sets an upstream URL.**

- `manage` on the repo, through the API: any maintainer points the server at the cloud
  metadata endpoint or at the backend registry.
- The admin, through the API, with a guard on the addresses and the credential files limited
  to one directory: workable, but the admin role is an API role, not a shell on the host, and
  the guard is the only thing between it and the internal network.
- The operator, in the configuration: `upstreams[]` beside `registries[]`, and a proxy repo
  names one by name, as a repo names its registry. Chosen; it is Verdaccio's `uplinks` too.

**How a group answers for a name.** A union of every member, first member winning a version
(Nexus): dependency confusion by default. The first member that has the name, alone: a proxy
listed before the hosted member hides it. A name that a hosted member holds hides every proxy
member's versions of that name, and the group may list names that are merged anyway: chosen,
as the issue asks.

**Writes through a group.** Refused, as Nexus does: npm keeps one registry URL for install and
publish, so users would need two. Sent to the group's one hosted member and authorized there:
chosen.

## Decision

Normative text: [proxy-and-group.md](../spec/proxy-and-group.md), with the tables in
[index-schema.md](../spec/index-schema.md#proxy-and-group-repos), the routes and statuses in
[http-routing.md](../spec/http-routing.md) §6, the permissions in [rbac.md](../spec/rbac.md) §7
and the annotations in [manifest-profile.md](../spec/manifest-profile.md) §3.1.

1. **Kinds.** `repos.kind` is `hosted`, `proxy` or `group`, set at creation and never changed.
   A proxy and a group have the routes of their format, so a client cannot tell the kinds
   apart. A format supports a kind when its plugin provides the hooks of point 9; npm does in
   M4. `oci` repos stay hosted.
2. **Upstreams belong to the operator.** The configuration names them (`upstreams[]`: name,
   format, URL, credentials as file paths, timeout, retries, metadata size bound). A proxy repo
   names one, and the API never takes a URL or a path. The repo's own settings are the
   metadata TTL, the negative TTL, offline mode and allow and deny lists of names.
3. **A fetched artifact is an ordinary manifest.** On a miss the fetcher downloads the file to
   a spool, verifies the checksum the upstream's metadata gives, and hands it to
   `Publisher.Upload`, which builds and validates it as it would an upload. The manifest has
   the annotations `lajstrom.origin.url` and `lajstrom.origin.checksum`, so reindex rebuilds a
   proxy repo from the registry like any other, and a backup of the registry holds it. The
   client is answered from the registry after the ingest, never from the spool.
4. **One fetch per artifact.** A `fetch_claims` row, keyed like a tag claim by repo, package
   path and tag, is taken before the download, beats every 30 seconds and goes stale after 2
   minutes. Another replica that misses the same artifact waits for the version to appear. The
   fetch runs on its own context, so a client that leaves does not cancel it for the others.
5. **Metadata is a cache.** `proxy_meta` keeps the upstream's document of a name (what the
   plugin chooses to keep), its validators and when it was last checked. A fresh row is used as
   it is; an expired one is revalidated with a conditional request; an upstream that does not
   answer leaves the stale row in use. An upstream `404` is a negative row. Offline mode never
   asks the upstream. Reindex neither reads nor writes the table, and dropping it costs
   upstream requests, nothing else.
6. **Groups.** Members are hosted or proxy repos of the group's format, in order, at most one
   of them hosted. Read on the group is the whole permission for reads. A name that the hosted
   member holds (by package path) hides every proxy member's versions of the name, unless the
   name matches the group's `merge` patterns. Otherwise the members' documents are merged in
   order, the first member winning a version. A group stores nothing: no derived rows, no
   rendered documents, so there is nothing to invalidate when a member changes.
7. **Writes through a group** go to its hosted member and need the action on that member.
   Without a hosted member, a write is `405`, and so is any write to a proxy except the delete
   of a held version and the referrers of `/v2`.
8. **Security.** Credentials are read from files named in the configuration, are sent only to
   the upstream's own origin, and the client's credentials are never forwarded. A redirect never
   goes from `https` to `http`. A URL on another host than the configured one, from a document
   or a redirect, must resolve to public addresses. A proxied ingest is an audit row
   `proxy.fetch` whose principal is the reader whose request caused it; a refused fetch is
   `proxy.reject`.
9. **Plugin hooks** (proxy-and-group.md §10): the key of a name, the upstream request for a
   name's document, the parse of that document into what to keep and the artifacts it lists
   with their file URLs and checksums, the mapping of a file request to an artifact, the
   rendering of a proxy's document, and the merge of a group's. The core does the rest. These
   hooks, `Upload.Origin` and the new index methods are a later contract step of their own: this
   record describes them and changes no exported Go interface.

## Consequences

- npm users get Verdaccio's one URL with a guard Verdaccio lacks: an internal name cannot be
  shadowed by a public one. The guard holds only while the hosted member has the name; a scope
  that is still empty, or a package whose every version was deleted, is open. The spec tells
  operators to deny their own scopes on the proxy.
- Everything a proxy fetched survives the loss of the database and of the upstream, and is
  served, signed and deleted like a hosted artifact. The first install of a version waits for
  the download and the push before its first byte, and large files feel it more than npm
  tarballs do. pip's 15-second read timeout makes this a question for #67.
- A version whose bytes are several files (maven: jar, POM, sources) is one manifest in the
  profile, and fetching all of them on the first miss is a choice for #67. The core fetches
  what the plugin lists for one artifact.
- `proxy_meta` grows with every name ever asked for. Negative rows are swept when they expire;
  positive rows stay until purged (`DELETE /api/v1/repos/<format>/<repo>/upstream-cache`). A
  bound on its size is follow-up work.
- An upstream is added or changed with the configuration and a restart, as a registry is, and a
  rotated credential file takes a restart, as a TLS certificate does.
- The threat model gets a new surface: a review of S-1 and the upstream rows when the
  implementation lands.
- ADR 0008's `policy.Decider` is asked at the place this record leaves for it: after the
  allow and deny lists and before the fetch. Each upstream entry carries the upstream's publish
  time where the format has one, for quarantine.
- Follow-up issues: the contract step for the hooks and the index methods; the npm proxy and
  group issues of M4; #67 and #68 for pypi and maven.
