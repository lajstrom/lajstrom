# Spec: proxy and group repos

- Status: normative ([ADR 0013](../adr/0013-proxy-and-group-repos.md), accepted 2026-11-10); §11,
  `oci` proxy repos, since [ADR 0018](../adr/0018-oci-proxy-repos-pull-through-from-any-registry.md)
  (2026-11-12). Changes need an issue labelled `contract`.
- Implemented by: `internal/proxy` (the fetcher, the metadata cache and the resolution of
  groups; format-independent), the plugins of the formats that have the kinds (npm first,
  ADR 0012; pypi and maven with #67, #68), `internal/api` and `internal/config` for the
  settings.
- Related: [index-schema.md](index-schema.md#proxy-and-group-repos) (the tables),
  [http-routing.md](http-routing.md) §6 (routes and statuses), [rbac.md](rbac.md) §7,
  [manifest-profile.md](manifest-profile.md) §3.1 (the origin annotations),
  [ADR 0008](../adr/0008-policy-verdicts-and-enforcement.md) (policy).

## 1. Kinds

| Kind | Holds | Reads | Writes |
| --- | --- | --- | --- |
| `hosted` | what was published to it | from the index and the registry | the format's publish routes, `/v2` |
| `proxy` | what it fetched from its upstream, as ordinary manifests; an `oci` proxy the upstream's own manifests and blobs (§11) | the upstream's documents, cached (§4); files from the registry, fetched on a miss (§5) | none from clients but the delete of a held version and referrers on `/v2` (http-routing.md §6) |
| `group` | nothing | its members' documents merged, files from the member that has them (§8) | sent to its hosted member (§8.4) |

The kind is set when the repo is created; the one change is a proxy that becomes a hosted repo
(§2.6), once and one way. A repo of any kind has the routes
of its format: a client cannot tell a proxy or a group from a hosted repo. A format has the
kinds `proxy` and `group` when its plugin provides the hooks of §10; in M4 that is npm.
`oci` repos, which have no plugin, are `hosted` or `proxy` (§11); a group of `oci` repos is
refused ([#848](https://github.com/lajstrom/lajstrom/issues/848)).

## 2. Settings

### 2.1 Upstreams: the configuration

An upstream is the operator's, like a registry: the configuration names it, and repos refer to
it by name. The API never takes a URL or a file path (§9). Keys, for configuration.md:

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `upstreams[].name` | name | required | Unique; the grammar of `registries[].name`. Example: `npmjs`. |
| `upstreams[].format` | plugin name | required | The format it serves; a plugin of this build that has the kind `proxy`. Only a proxy repo of this format names it. |
| `upstreams[].url` | absolute `http(s)` URL | required | Base of every request, e.g. `https://registry.npmjs.org/`; a path is allowed, credentials, a query and a fragment are not. `http` is accepted with a warning at start: what it serves is not authenticated. An `oci` upstream's URL is the root of the registry, `https://ghcr.io`, without a path: the API is at `/v2/` of the host. |
| `upstreams[].auth.username` | string | empty | With `passwordFile`: HTTP Basic. |
| `upstreams[].auth.passwordFile` | path | empty | File that holds the password. |
| `upstreams[].auth.passwordEnv` | environment variable name | empty | Variable that holds the password of `username`, as `registries[].auth.passwordEnv`; not with `passwordFile`. |
| `upstreams[].auth.tokenFile` | path | empty | File that holds a bearer token (`Authorization: Bearer`), npm's `_authToken`. Not with `username`. |
| `upstreams[].tokenRealmHosts` | list of host names | the vendor table's | An `oci` upstream's: the hosts besides its own that the realm of its bearer challenge may be on, over `https` (§11.1), as `registries[].tokenRealmHosts`. |
| `upstreams[].dockerHub` | bool | `true` on `registry-1.docker.io` and `docker.io`, else `false` | An `oci` upstream's: it is Docker Hub or a mirror of it, and a path of one component is `library/<path>` (§11.7). |
| `upstreams[].timeout` | duration | `30s` | Bounds the wait for the response headers (connecting and TLS at most 10 s each) and every wait for the next bytes of a body. `0s` means the default. |
| `upstreams[].retries` | integer | `2` | Retries of a request, as `registries[].retries` defines them: `429`, `5xx` but `501` and `505`, timeouts, connections refused or reset; backoff from 250 ms, at most 3 s. `0` means the default, a negative value none. |
| `upstreams[].maxMetadataSize` | bytes | `67108864` (64 MiB) | Largest document the upstream may answer for one name; a larger one is not cached (§6). `0` means the default. |

A credential file is read when `serve` starts, as `keys.masterKeyFile` is: missing, unreadable
or empty is a configuration error of `serve`, and a mode that lets the group or others read it
is used, with a warning. A rotated secret takes a restart. Spaces and line breaks around the
value are ignored.

An egress proxy is the environment's: `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY`, as the
connections to the registries read them. A deployment whose registry must not go through it
lists the registry in `NO_PROXY`.

A repo that names an upstream the configuration no longer has is served as if it were offline
(§4.4), with an error at every start naming the repo and the upstream; `serve` starts anyway,
as it does with a registry that does not answer.

### 2.2 A proxy repo: `upstream`

The repo resource of `/api/v1/repos` has `upstream`, an object, on a proxy repo only:

| Key | Values | Absent | Changes |
| --- | --- | --- | --- |
| `name` | one of `upstreams[].name`, of the repo's format | required | the admin role (rbac.md §7); the change drops the repo's `proxy_meta` rows |
| `metadataTTL` | duration, `0s` to `168h`; `0s` revalidates on every read | `"10m"` | `manage` |
| `negativeTTL` | duration, `0s` to `24h`; `0s` keeps no negative entry | `"2m"` | `manage` |
| `offline` | bool: never ask the upstream (§4.4) | `false` | `manage` |
| `allow` | list of name patterns (§3), at most 256 | every name | `manage` |
| `deny` | list of name patterns (§3), at most 256 | none | `manage` |

`PATCH` merges `upstream` as it merges `policy` (RFC 7396), and the result is checked whole. A
change counts from the next request on every replica: the TTLs apply to the rows already
cached, since a row's expiry is computed when it is read (§4.1).

### 2.3 A group: `group`

| Key | Values | Changes |
| --- | --- | --- |
| `members` | the names of 1 to 16 repos of the group's format, in order; each `hosted` or `proxy` (a group is not a member), at most one `hosted`, no repo twice | the admin role; replaced whole |
| `merge` | list of name patterns (§3), at most 256: names whose versions are merged across members even when the hosted member holds them (§8.2) | `manage` |

A group has no registry and no policy: `registry` and `policy` are absent from its resource,
and a create or a `PATCH` that sets them is `400`.

### 2.4 The resource, the API and the bootstrap

A proxy of npmjs.org, and a group of a hosted repo `npm/internal` and that proxy:

```json
{"format": "npm", "name": "npmjs", "registry": "local", "kind": "proxy", "anonymousRead": false,
 "policy": null,
 "upstream": {"name": "npmjs", "metadataTTL": "10m", "negativeTTL": "2m", "offline": false,
              "allow": [], "deny": ["@acme/*"]},
 "createdAt": "2026-12-01T10:00:00Z", "updatedAt": "2026-12-01T10:00:00Z"}
```

```json
{"format": "npm", "name": "all", "kind": "group", "anonymousRead": false,
 "group": {"members": ["internal", "npmjs"], "merge": []},
 "createdAt": "2026-12-01T10:00:00Z", "updatedAt": "2026-12-01T10:00:00Z"}
```

The configuration that goes with them:

```yaml
upstreams:
  - name: npmjs
    format: npm
    url: "https://registry.npmjs.org/"
repos:
  - { name: internal, format: npm, registry: local }
  - { name: npmjs, format: npm, registry: local, kind: proxy,
      upstream: { name: npmjs, deny: ["@acme/*"] } }
  - { name: all, format: npm, kind: group, group: { members: [internal, npmjs] } }
```

`POST /api/v1/repos` takes `kind`, `upstream` and `group` beside the fields of a hosted repo. The
resource shows the upstream's name, never its URL or credentials. The `repos` section of the
configuration takes the same keys, `repos[].kind`, `repos[].upstream` and `repos[].group`, with
the same checks; the bootstrap creates the hosted and proxy repos of the list before its groups,
so that a group may name a member listed after it. A repo that is present is left as it is,
with a warning for each of `upstream` and `group` that differs, as for `policy`.

### 2.5 Validation

Each is `400` on the API, naming the field first, and a configuration error at start
(`repos[3].group.members[1]: "npmjs" is a group`):

- `kind` not one of the three, or a kind the format does not have (`format raw has no proxy
  repos`);
- `upstream` on a repo that is not a proxy, or missing on one; `group` likewise;
- `upstream.name` not a configured upstream, or one of another format;
- a TTL that is not a duration in its range; a pattern that is empty, longer than 256 bytes, or
  has a space or a control character; more than 256 patterns in a list;
- a member that does not exist, is a group, is of another format, is named twice, or is the
  second hosted member; more than 16 members;
- on a group, a `registry` or a `policy`.

Deleting a repo that is a member of a group is `409` naming the groups, `force` or not.

### 2.6 A proxy becomes a hosted repo

`PATCH /api/v1/repos/<format>/<repo>` with `{"kind": "hosted"}`, alone in the body, by the admin
role (rbac.md §7), turns a proxy repo into a hosted one that keeps everything it holds (#441,
ADR 0013 point 1). It is the end of a move from another registry (§5.1): the repo then takes
publishes, and its format's state that a proxy reads from its upstream's documents lives in its
own journal in the registry (npm: the dist-tags and the deprecations, manifest-profile.md §8),
where it survives the loss of the database.

1. **Refused:** a repo that is a group (`400`); a format whose plugin cannot adopt a proxy's
   documents (§10, `format.Adopter`; `400`); a proxy with a fetch claim that is not stale, a
   fetch running (`409`); a proxy that is a member of a group that has a hosted member already
   (`409` naming the group: a group has at most one hosted member, §2.3).
2. **The ops.** For each package the repo holds, the plugin turns the document the proxy kept of
   it (`proxy_meta`, the kind of `Adopter.AdoptKind`; npm the full packument) into journal ops
   (`Adopter.AdoptOps`): npm adds each dist-tag whose version the repo holds, and the
   deprecation of each held version that has one. A dist-tag of a version the repo does not hold
   is left out and reported. A package without a kept document gets no op: its packument is then
   what a hosted repo renders of its versions, `latest` the highest.
3. **The flip,** in one transaction: `kind` becomes `hosted` and `upstream` NULL
   (`index.Store.ConvertRepo`, which refuses a live fetch claim in the same transaction), the
   repo's rendered documents are invalidated, and the audit row `repo.convert` is written with
   the counts and what was left out. From the next request on, every replica serves the repo as
   a hosted one; a group that has it as a member has it as its hosted member. A fetch that a
   request started just before the flip may still end: its version is then one of the hosted
   repo, with its origin, as the prefetched ones are.
4. **The journal.** The ops of each package go through the one write path
   (`Publisher.Journal`: the package's journal lock, the claims, the `_j<seq>` tags, the
   visibility barrier, an audit row per op by the admin). Then the proxy's cached documents of
   the repo are deleted, and so is the rest of `proxy_meta` of it.
5. **Nothing moves in the registry.** A version's OCI repository is `<format>/<repo>/<package
   path>` whatever the kind, and the versions keep their `lajstrom.origin.*` annotations
   (manifest-profile.md §3.1): they say where the bytes came from, and the hosted repo serves
   them as its own.

The answer is `200` with the repo's resource, now `hosted`, without `upstream`. A failure after
the flip (the registry or the index during step 4) leaves a hosted repo whose journal lacks the
ops of some packages and that still has their cached documents: the same `PATCH` on it, a hosted
repo, resumes from those documents, since ops written twice fold to the same state (INV-29). On
a hosted repo without cached documents it changes nothing and answers `200`. A converted repo
cannot become a proxy again.

The times: the packument of a hosted repo dates each version by
`org.opencontainers.image.created` (`versions.created_at`), which a proxied ingest sets to the
upstream's publish time when the upstream gives one (manifest-profile.md §3.1, #441), so the
dates survive the conversion; a version fetched before that change keeps the time of its fetch.

## 3. Names: allow, deny and merge

A pattern is matched against the plugin's key of a name (§10: npm the name, `@scope/name`; pypi
the PEP 503 name; maven `groupId:artifactId`), ignoring ASCII case. `*` matches any run of
characters, `/` and `:` included, and every other character matches itself: `@acme/*`,
`com.acme:*`, `*-internal`.

A proxy fetches a name when `allow` is empty or a pattern of it matches, and no pattern of
`deny` matches; `deny` wins. A name it does not fetch is `403 DENIED` naming the list and the
pattern that refused it (`deny: @acme/*`, or `allow` when no pattern of it matches), as ADR
0008's rules will answer, and no request goes to the upstream for it; through a group, the
member does not have the name (§8.1), so a name that no other member has is the group's `404`.
The lists say what a proxy mirrors; refusing a version for a reason, with `403`, is ADR 0008's
rules, which will be asked at the same point (§5, step 3) once they are implemented (#101).

`deny` also bounds what a proxy forwards for a client (§9.2): a name it refuses never leaves the
server (#514). The plugin asks the core (layout.md, `Proxy.Denies`) and leaves the name out of
the request before it goes, and a request that cannot lose a name without losing others (npm
6's audit tree) is refused when it names one (http-routing.md, npm, Audit). `allow` does not
bound a forwarded request: a name outside it is not mirrored, and is not private either.

## 4. The metadata cache

### 4.1 The rows

`proxy_meta` (index-schema.md) keeps, per proxy repo, name key and document kind, the upstream's
document as the plugin chose to keep it (§10), its `ETag` and `Last-Modified`, and `checked_at`,
when the upstream last answered for it. A row is `present` (a document) or `absent` (the
upstream answered `404` or `410`). It is fresh while `checked_at` plus the repo's current
`metadataTTL` (`negativeTTL` for an absent row) is in the future.

### 4.2 A read

1. A name outside the lists (§3): `403`, no row, no request.
2. A fresh row: used as it is.
3. No row, or an expired one: a request to the upstream (`If-None-Match` and
   `If-Modified-Since` from the row when it has them):
   - `200`: the plugin parses the document (§10); the row is replaced, `present`;
   - `304`: `checked_at` moves, nothing else;
   - `404`, `410`: the row becomes `absent`, unless `negativeTTL` is `0s`, which deletes it;
   - anything else, or a document above `maxMetadataSize` or one the plugin cannot parse: the
     row stays as it was and is used, however old; without a row the document is rendered
     from the versions the repo holds (§7), and without those the read fails with the status
     of §6. Logged at warn with the upstream, the name and the status.
4. A row that is written invalidates the repo's rendered documents of the name (`index_cache`)
   in the same transaction; a `304` invalidates nothing.

Two replicas that refresh one name at once each ask the upstream and the second write wins;
the documents are small beside the files, and one replica asks once per name at a time.

### 4.3 Invalidation

A row is dropped or made stale by its TTL; by a file of the name that the upstream answers
`404` or `410` (§5, step 7), which makes the row stale; by
`DELETE /api/v1/repos/<format>/<repo>/upstream-cache[?name=<name>]` (http-routing.md §6); by a
change of `upstream.name`; by deleting the repo. A change of `allow` or `deny` needs none: the
lists are read on every request. Reindex neither reads nor writes `proxy_meta`, and
`ResetDerived` leaves it: it is a cache of the upstream, not of the registry. Dropping the table
costs upstream requests and nothing else.

Expired `absent` rows are deleted once a minute, with the expired sessions; `present` rows stay
until replaced or dropped.

### 4.4 Offline

A proxy is offline when `upstream.offline` is true or its upstream is not configured. It
sends no request: a row is used whatever its age, a name without a row is rendered from the
versions the repo holds (§7), and a file it does not hold is `404`.

## 5. Fetch on miss

A request for a file of a proxy repo, directly or through a group, runs these steps.

1. The plugin maps the path to the artifact and the file (§10): name, version, variant, file
   name. A path it cannot map is the format's `404`.
2. A name outside the lists (§3): `403` (through a group: the member does not have it).
3. The index has the version, with the file: it is served as a hosted file is
   (`format.ServeBlob`), and nothing else happens. This is where ADR 0008's `policy.Decider`
   will be asked (#101), before step 4, for a held version and a fetch alike.
4. Offline (§4.4): `404`.
5. The name's document (§4.2) lists the version and the file: their URL and the upstream's
   checksums. A version it does not list, or a name it does not have, is `404`.
6. **The claim.** On one replica, concurrent misses of one artifact share one fetch. Across
   replicas, the fetch takes a `fetch_claims` row keyed by repo, package path and tag (the
   plugin's `EscapeName` and the profile's `Tag`). A fresh claim of another replica: the request
   waits, looking the version up in the index after 250 ms and then twice as long each time, at
   most 2 s apart, until the version appears (step 3), the claim is gone without it (this
   request takes the claim, once), or the claim goes stale (it takes it over). The claim beats
   every 30 seconds and is stale 2 minutes after its last beat.
7. **The download.** First, a name the registry does not take (`provider.Capabilities.CheckName`:
   the depth or the length of its OCI repository) stops the fetch before any request (`403`
   naming the limit, §6; an audit row `proxy.reject`, #599): ingest would not publish it, and
   the upstream is not at fault. The file the entry lists (npm: the tarball; an entry lists one) is
   requested from the upstream (§9.2), and its body streams into an upload session of the
   registry through the one write path, as a client's upload does (`Publisher.UploadStream`,
   [ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)): ingest computes sha256 and the
   checksums the plugin needs, the plugin's inspector reads it as it passes, and the fetcher
   hashes it with the algorithm of step 8. Nothing of it is written to a disk. A
   `Content-Length`, or a body, above the repo's `policy.maxFileSize` or the registry's
   `capabilities.maxBlobSize` stops the fetch (`403`, §6; an audit row `proxy.reject`). An
   upstream `404` or `410` makes the name's row stale (§4.3).
8. **The check.** The strongest checksum the entry gives (sha512, then sha256, then sha1) must
   match the bytes, which the fetcher knows when the body ends, before the session is closed.
   A mismatch refuses the fetch: the session is cancelled and nothing is stored, an audit row
   `proxy.reject` and a warning name both values. A body that the plugin's inspector refuses
   before its end is read to it, so that bytes other than the upstream's are refused for their
   checksum. An entry without a checksum is refused the same way, before the file is asked,
   unless the format's ADR says that its upstream publishes none.
9. **The ingest.** The plugin builds the upload from the streamed file as it would a client's
   (`Plugin.Build`, from what its inspector read), with `StreamUpload.Origin` set (§10), and
   `Publisher.UploadStream` runs with the principal of the request: Build, Validate, the tag
   claims, the close of the session, the push of the manifest, the index rows, the audit row
   `proxy.fetch` instead of `push`, as for any upload. The manifest carries
   `lajstrom.origin.url` and `lajstrom.origin.checksum` and no `lajstrom.uploaded-by`
   (manifest-profile.md §3.1). An artifact that Validate refuses is `proxy.reject` and `502`:
   the upstream served it, the client did nothing wrong. A tag that holds other bytes keeps
   them (ADR 0005): the repo serves what it holds, and the change upstream is logged at warn.
   A refusal, a body the upstream cuts short, and a failure of the registry cancel the session:
   nothing of the fetch is stored.
10. The claim is released whatever the outcome; on success the file is served from the
    registry as in step 3.

The fetch runs on a context of its own, bounded by the upstream's timeouts and the server's
shutdown, so that a client that disconnects does not cancel it for the requests that wait on
it. It holds a connection to the upstream and an upload session of the registry, and no
temporary file; `server.maxConcurrentUploads`, which counts the requests that upload, does not
count it.

### 5.1 Prefetch

A prefetch fetches the versions of a list of names into a proxy repo before a client asks for
them, through the steps of §5. It is how packages move from another registry (#441): a proxy
repo whose upstream is the old registry, a prefetch of its names, and then the upstream switched
off (§4.4). It is `POST /api/v1/repos/<format>/<repo>/prefetch` (http-routing.md §6) with
`manage` on the repo (rbac.md §7), of a format whose plugin has the hook of §10
(`format.Prefetcher`; npm).

1. A repo that is not a proxy is `400`, and so is a format without the hook. An offline repo
   (§4.4) is `409`: it asks its upstream nothing.
2. For each name: the lists (§3), then the documents of the hook's kinds, read as §4.2 reads
   them: used while fresh, else asked again. The first kind lists the entries (npm: the full
   packument); the others are read so that the repo keeps them when it goes offline (npm: the
   abbreviated packument, which `npm install` asks for). A name the lists refuse, one the
   upstream does not have, one whose first document lists no version, and one whose first
   document cannot be had, is one `failed` result with the status of §6. Another kind that
   cannot be had is a `failed` result of the name too, after its entries. The other names go
   on.
3. The versions it takes: with `allVersions`, every entry of the document. Without it, the
   entries of the versions that the document's pointers name (npm: the targets of its
   dist-tags); when they name none of the listed versions, the newest entry by publish time.
4. Each entry runs §5 from step 3, as a miss of its first file would. A version the repo holds
   is `held`, and nothing else happens. Otherwise come the claim, the download, the check and
   the ingest, with the principal of the request, its audit rows `proxy.fetch` and
   `proxy.reject`, and its statuses. One request fetches at most 4 entries at a time.
5. The answer is `200` with one result per entry, and one per name that failed before its
   entries, in the order of the names: `fetched`; `held`; `shared`, fetched meanwhile by
   another request or replica; or `failed`, with the status a request of the file would have
   had and its message. A request whose client goes away fetches no further entry; a fetch that
   has started ends (§5).

A prefetch is idempotent. Run again, it finds held what it fetched, at the cost of an index
lookup per entry, and it fetches again what failed. It takes 1 to 100 names a request; a client
sends a long list in batches (`lajstrom admin repo prefetch`). It reads no list of every name of
the upstream: npm has no standard one, and the operator makes the list (docs/migrating.md).

## 6. Errors

| The upstream, or the fetch | A document | A file |
| --- | --- | --- |
| answers | served (§7) | ingested, then served |
| a name outside the lists | `403` naming the list and the pattern; through a group, the member does not have it | the same |
| offline and nothing held | `404` | `404` |
| `404`, `410` | `404`; an `absent` row | `404`; the name's row made stale |
| `401`, `403` (Lajstrom's own credentials) | the cached row, else the held versions, else `502` | `502`; logged at error |
| on an `oci` proxy, `401`, `403` on a repository read after a token was granted, or any `401`, `403` of an upstream without credentials (§11.7) | `404`; an `absent` row | `404` |
| `429` | the cached row, else the held versions, else `503` with `Retry-After` (the upstream's, at most 60 s; 5 s without one) | `503` with `Retry-After` |
| `5xx`, a connection or TLS error, a refused redirect (§9.2), a document too large or unparsable | the cached row, else the held versions, else `502` | `502` |
| a timeout (§2.1) | as above, `504` | `504` |
| a name the registry does not take (`Capabilities.CheckName`, #599) | | `403`, naming the limit, before any download; `proxy.reject` |
| a file above `maxFileSize` (else the format's default) or `maxBlobSize` | | `403`, naming the bound; `proxy.reject` |
| a checksum mismatch; an artifact Validate refuses | | `502`; `proxy.reject` |
| the registry or the index during the ingest | | as for an upload (layout.md, `StatusOf`) |

"The held versions" is the document rendered from what the repo holds (§7) when there is at
least one; otherwise the status of the row. A message names the upstream by its `name`, never
its URL, a credential or a body the upstream sent. A format answers in its own error form
(npm's JSON `error`) with these statuses.

## 7. What a proxy serves

The plugin renders a proxy's document from the cached upstream document and the versions the
repo holds (§10, `RenderProxy`), and it is kept in `index_cache` as a hosted repo's is:

- While the upstream answers, its listing is the listing: a version that the repo holds and the
  upstream no longer lists is not listed, so resolvers do not choose it, and its file stays
  downloadable at its URL, so lockfiles keep working.
- For a version the repo holds, what the index says of the bytes (checksums, size) wins over
  the upstream's document: the client checks what it is served.
- Every link to a file names this repo (or the group the request came through, §8), under
  `server.externalURL`; a client never gets an upstream URL.
- Without a document (offline, or the upstream down and no row), the plugin renders what the
  repo holds, as a hosted repo's document.

## 8. Groups

### 8.1 Resolution

A request on group `G` for name `N` asks the members of `G` in order. A member takes part unless
it is hidden (§8.2). The hosted member answers from its index; a proxy member as a proxy does
(§4, §7), and a name outside its lists (§3) is one it does not have, with every request it makes upstream counted as its own, not the group's.

A proxy member that fails for `N` (§6: its upstream errs, and it has neither a row nor a held
version) does not take part in this answer, and the failure is logged at warn with the member and
its upstream: the group answers from the members that have `N`. When none has it, the group
answers with the first such failure's status, and with `404` when no member failed.

### 8.2 Hosted names hide the upstream

When the hosted member of `G` holds `N`, that is, its index has a package of the package path
of `N` (so spellings that differ in ASCII case count, manifest-profile.md §4.1), every proxy
member is hidden for `N`: none of its versions of `N` is listed, none of its files of `N` is
served, and no request goes to its upstream for `N`. A name that matches a pattern of
`group.merge` is not hidden: its versions are merged as in §8.3.

The rule holds only while the hosted member has the name. A scope with nothing published yet,
or a package whose last version was deleted, is open to the upstream: an operator protects
internal names by `deny` on the proxy (`"deny": ["@acme/*"]`), which holds whether the hosted
member has the name or not.

### 8.3 Merging

The plugin merges the documents of the members that take part, in member order
(§10, `MergeGroup`): the union of their versions, and for a version that several members have,
the first member's entry; format-level fields (npm's `dist-tags`) by the format's rules (npm:
[http-routing.md](http-routing.md), npm, **Group**). Every link names the group. A name that no
member has is `404`. The hosted member's document is the one its own URL serves, from its
`index_cache`; a proxy member's is the one of §7.

A file of `N` is served by the first member taking part that holds it, or, for a proxy member
whose document lists it, fetched there (§5) and served. When the hosted member holds `N` and
not the version, the answer is `404`.

### 8.4 Writes

A write route of the format on `G` (npm `publish`, `unpublish`, `dist-tag`) is the same request
on the hosted member `H`: the ingest, the audit row and the index rows are `H`'s, with
`details.via` the group, and the permission is `H`'s (rbac.md §7). A group without a hosted
member answers a write with `405`.

The read that a write of the format starts from, the format's write form of a document (npm's
`GET /<name>?write=true`, which `deprecate` and `unpublish` send back changed), is `H`'s own
document, with its links under the group, once the principal may read the group: a write sends
back what it read, and `H` takes no version of another member. Without a hosted member it is
`405`, as the write would be.

### 8.5 Caching

A group stores nothing: no `packages`, `versions`, `files`, `oci_repos`, `index_cache` or
`proxy_meta` rows. Its documents are merged on every request from the members' cached ones, so
a member's change is seen on the next request and nothing is invalidated. The `ETag` of a
group's document is the sha256 of the group's `updatedAt` and of the `sha256` of each
member document that took part, in order, so that a conditional request is answered `304`
without sending the body. The merge runs for it too in M4: `format.Proxy.Document` takes no
validator, and the merge is of documents already in memory.

## 9. Security

### 9.1 Who sets what

The URL of an upstream and its credentials are the operator's, in the configuration; no API call
and no role sets them (threat model S-1 stays as it is for configured URLs). The admin role
chooses the upstream of a proxy repo and the members of a group; `manage` the rest (§2, rbac.md
§7).

### 9.2 Requests to an upstream

- **Credentials** are sent only to the origin (scheme, host, port) of `upstreams[].url`. A
  redirect to another origin is followed without them. The client's `Authorization`, cookies
  and other headers are never forwarded: the request to the upstream is the fetcher's own, with
  `User-Agent: lajstrom/<version>`.
- **Redirects**: at most 5. `https` to `http` is refused (`502`), whatever the host; `http` to
  `https` is followed.
- **Addresses**: the host of `upstreams[].url` is the operator's choice and may be internal. A
  URL on any other host, from the upstream's document (pypi's files host, a CDN) or from a
  redirect, must be `https` and must resolve, when the connection is made, to public unicast
  addresses only: loopback, private (RFC 1918, RFC 4193), link-local (the cloud metadata
  endpoint), CGNAT, unspecified and multicast addresses are refused (`502`). With an egress
  proxy (§2.1) Lajstrom does not resolve the target: it refuses an address literal of those
  ranges, and the egress proxy enforces the rest.
- **What is kept**: the URL a file was asked for, before redirects, without userinfo, query or
  fragment, becomes `lajstrom.origin.url`. A redirect's target, which may be a presigned URL, is
  never stored and never logged.
- **Forwarded requests**: a request that only the upstream can answer (npm audit,
  http-routing.md) is sent by the core as these rules say, below `upstreams[].url`, with what
  the plugin chose of the client's request (layout.md, `Proxy.Forward`): its body, at most
  16 MiB, without the names `deny` refuses (§3), and a `Content-Type`, never a header the
  client sent. The answer is returned to that client and kept nowhere, bounded by
  `maxMetadataSize`; an offline proxy (§4.4) sends nothing.

### 9.3 Logs and audit

Every upstream request is logged at debug: upstream name, method, path without the query,
status, duration. A proxied ingest is logged at info and is the audit row `proxy.fetch`; a
refused one is `proxy.reject` (rbac.md §7). A credential file's content, a token and the
upstream's response body never reach a log or an error.

## 10. Plugin hooks

This section describes what a plugin provides for `proxy` and `group`. The Go shapes are
[layout.md](layout.md) §2, "Proxy and group repos": `format.Proxier` (#309), and
`format.Grouper`, a `Proxier` with `MergeGroup` (#311); the write target is
`format.WriteTarget`. The sketch they came from:

```go
// Proxier: a format with proxy and group repos.
type Proxier interface {
    // Key is the name as the cache and the patterns see it: npm the name, pypi the PEP 503
    // name, maven groupId:artifactId.
    Key(name string) (string, error)
    // MetadataRequest is the request for the document of kind of name, relative to the
    // upstream URL, with its Accept header (npm: the abbreviated packument).
    MetadataRequest(name, kind string) (UpstreamRequest, error)
    // ParseMetadata reads the upstream's document: what to keep in proxy_meta (it may trim),
    // and the artifacts it lists.
    ParseMetadata(name, kind string, body []byte) (*UpstreamIndex, error)
    // FileRequest maps a path below /<format>/<repo>/ to the artifact and the file it asks for.
    FileRequest(path string) (ArtifactRef, error)
    // RenderProxy renders a proxy's document of name (§7): kept is nil without a document.
    RenderProxy(ctx context.Context, repo index.Repo, name, kind string, kept []byte, held []index.Version, base string) (Document, error)
    // MergeGroup merges the documents of the members that take part, in order (§8.3).
    MergeGroup(ctx context.Context, group index.Repo, name, kind string, docs []MemberDocument, base string) (Document, error)
}
type UpstreamIndex struct {
    Keep    []byte
    Entries []UpstreamEntry          // one per artifact: version and variant
}
type UpstreamEntry struct {
    Version, Variant string
    Files     []UpstreamFile         // what one fetch downloads (npm: the tarball)
    Published time.Time              // the upstream's publish time, zero when none (ADR 0008 quarantine)
}
type UpstreamFile struct {
    Name      string
    URL       string                 // absolute, or relative to the upstream URL
    Checksums map[string]string      // sha512, sha256, sha1 → lowercase hex
}
```

The rest is the core's and no plugin repeats it: the lists, the cache and its TTLs, the claims,
the upstream client, the stream and the check, the ingest call, the statuses, which members take
part in a group. The plugin's handlers ask the core for a document or a file of a repo of any
kind and do not branch on the kind; a write asks the core for its target repo (the repo itself,
a group's hosted member, or `405`).

The same contract step added: `Upload.Origin` (the URL and the verified checksum), from which
ingest writes the origin annotations and the audit row `proxy.fetch`; the index methods of
`proxy_meta`, `fetch_claims`, `group_members` and the new `repos` columns; and the sentinels
for `405`, `502`, `503` and `504` in `format.StatusOf`.

The conversion of a proxy (§2.6, #441) adds `format.Adopter`, a `Proxier` with two methods; a
format whose plugin lacks it has proxies that stay proxies:

```go
// Adopter: a Proxier whose proxy repos may become hosted repos.
type Adopter interface {
    Proxier
    // AdoptKind is the kind of the kept document AdoptOps reads (npm: the full packument).
    AdoptKind() string
    // AdoptOps returns the journal ops that give a hosted repo holding the versions held of
    // name the pointers and the deprecations of kept, and describes what it left out.
    AdoptOps(name string, kept []byte, held []string) (ops []JournalOp, skipped []string, err error)
}
```

A prefetch (§5.1, #441) adds `format.Prefetcher`, a `Proxier` with two methods; a format whose
plugin lacks it has no prefetch, and its proxies still serve misses:

```go
// Prefetcher: a Proxier whose proxy repos take a prefetch.
type Prefetcher interface {
    Proxier
    // PrefetchKinds are the kinds of the documents a prefetch reads: the first lists the
    // entries it fetches (npm: the full packument), the others it keeps for offline (npm: the
    // abbreviated packument).
    PrefetchKinds() []string
    // Pointed returns the versions that the pointers of a document of kind of name, as
    // ParseMetadata kept it, name (npm: the targets of its dist-tags).
    Pointed(name, kind string, kept []byte) ([]string, error)
}
```

## 11. `oci` proxy repos

[ADR 0018](../adr/0018-oci-proxy-repos-pull-through-from-any-registry.md). An `oci` proxy pulls
through from any registry of the distribution API: `docker pull <lajstrom>/oci/<repo>/<path>:<tag>`
asks the upstream for `<path>:<tag>` on a miss and keeps what it fetched in the backend at the
repo's OCI name of `<path>` (http-routing.md §2). It has no plugin and no format routes: the core
answers it on `/v2` (http-routing.md §6). §3's lists, §4's TTLs, negative rows and offline mode,
§5's claims, §6's statuses and §9 hold, with the changes below. What it keeps is the upstream's
bytes: no manifest of the profile and no `lajstrom.origin.*` annotation, which would change a
digest the client asked for (manifest-profile.md §3.1 does not apply).

### 11.1 The upstream

`upstreams[].format: oci` names a registry by the root URL of its API (§2.1). Lajstrom asks it
through the generic provider's client, read-only: its retries and its circuit breaker, no
instance prefix, no capability probe. Its authentication is the upstream's own:

- none: anonymous, with an anonymous bearer token where the registry asks for one;
- `auth.username` with `auth.passwordFile` or `auth.passwordEnv`: Basic where the registry asks
  for it, and the credentials of its token realm;
- `auth.tokenFile`: a static bearer token.

A bearer challenge is answered at its realm when the realm is on the upstream's host name, or
over `https` on a host of `upstreams[].tokenRealmHosts` or of the vendor table (`auth.docker.io`
for `registry-1.docker.io` and `docker.io`); any other realm is `502`, naming its host. The
credentials go to the upstream's origin and to that realm only. A manifest or a blob that the
upstream redirects to another host (registry.k8s.io's regional mirrors, a CDN, object storage) is
followed without them, over `https` only and to public addresses only, as §9.2 says of a URL on
another host. Cloud authentication (ECR through IAM, Artifact Registry through a service
account) is not in this version.

### 11.2 Names, lists and limits

The path below the repo is the upstream's repository, `library/nginx`: the lists of §3 match it
as a name, `*` within a component. Before any request to the upstream, in this order: the lists
(`403 DENIED`); `Capabilities.CheckName` of the backend on the repo's OCI name of the path
(`400 NAME_INVALID`, naming the limit); offline mode (§4.4). A backend that refuses the name of a
push by a rule of its own (Quay's `…/build`, quay.md) is a `4xx` that names the backend and an
audit row `proxy.reject`, never a `500`.

### 11.3 What is kept, and where

Every `proxy_meta` row of an `oci` proxy has the path as its name (index-schema.md):

| Kind | Content | Written when |
| --- | --- | --- |
| `oci.tag:<tag>` | `{"digest", "mediaType", "size"}` of what the upstream's tag names; the digest is also the row's `etag`; `absent` for a tag the upstream does not have | the tag is resolved upstream (§11.4) |
| `oci.manifest:<digest>` | the exact bytes of a manifest or an index, checked against the digest; `absent` for a digest the upstream does not have | it is fetched (§11.4) |
| `oci.ref:<digest>` | `{"mediaType", "size", "parents"}`: what a manifest or an index at the path says of a blob or a child it lists, and the digests of the manifests and indexes that list it | a manifest or an index that lists it is fetched |
| `oci.held:<digest>` | empty | the backend holds the blob, or the manifest or index under its keep-tag (§11.5) |
| `oci.fixed-tags` | empty | the backend refused to move a tag of the OCI repository (§11.6) |

The backend holds the blobs that were asked for, each manifest whose blobs it holds and each
index whose children it holds, every one under its keep-tag (§11.6), and the upstream's tags
where it lets tags move. A row is fresh for `metadataTTL`, an `absent` one for `negativeTTL`
(§4.1); `present` rows stay until replaced or purged (§4.3). The `oci.ref`, `oci.held` and
`oci.fixed-tags` rows have no TTL: they say what was listed, held or refused.

### 11.4 A manifest

`HEAD` or `GET` of `manifests/<ref>`:

1. **A tag** is the digest of its `oci.tag` row while the row is fresh, with no request. An
   expired row, or none, is revalidated with a `HEAD` of the tag upstream: the same digest moves
   `checked_at`, another replaces the row, and a tag the upstream does not have is an `absent`
   row and `404 MANIFEST_UNKNOWN`. A tag of the keep-tag form (§11.6) is `404`.
2. **When the upstream does not answer**, or answers `429` or `5xx` (§6), the row is used however
   old. Without a row, the tag the backend holds answers, unless the repository has an
   `oci.fixed-tags` row: its backend tags are stale. Without either, the status of §6: `503`
   with `Retry-After` for a `429`, `502`, `504`. Offline (§4.4): the row whatever its age, else
   the backend's tag with the same exception, else `404`.
3. **The digest** is answered from the backend when it holds the manifest; else from its
   `oci.manifest` row; else, unless offline, it is fetched: a `GET` of the digest upstream, under
   the claim (repo, path, digest) of §5 and once per replica. The bytes must have the digest and
   be at most `server.maxManifestSize`; a manifest the upstream answered by tag must agree with
   its `Docker-Content-Digest` when it sends one. A mismatch is `502` and `proxy.reject`. The
   `oci.manifest` row and an `oci.ref` row per blob it lists (a manifest: its config and layers;
   an index: its manifests) are written in one transaction.
4. **The answer** carries the manifest's own media type, `Docker-Content-Digest` and
   `Content-Length`, and on a `GET` its bytes. A child of an index is fetched when it is asked for,
   so a pull of one platform fetches that platform alone. When every blob of a cached manifest is
   held, the read pushes it (§11.5, step 3), as a completion lost to a concurrent write is caught.

### 11.5 A blob, and the push

`GET` of `blobs/<digest>` that the backend does not hold:

1. The digest's `oci.ref` row gives its size and media type. A digest that no manifest held or
   cached at the path lists is `404 BLOB_UNKNOWN`, with no request to the upstream. A size above
   the repo's `policy.maxFileSize` or the backend's `maxBlobSize` is `403` naming the bound and
   `proxy.reject`, before the download. A `HEAD` is answered from the row, without a fetch.
2. Under the claim (repo, path, digest), the blob is asked of the upstream and streamed into a
   backend upload session, hashed on the way, and committed with its digest
   ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)); nothing is spooled. Bytes
   whose digest or size differ are not committed: `502`, `proxy.reject`. The `oci.held` row
   follows the commit. Other requests for the blob wait as §5, step 6, says, and are served from
   the backend.
3. **The completion.** Each manifest of the blob's `oci.ref` parents whose every blob is held is
   pushed by digest under its keep-tag; then each index that lists such a manifest, once every
   child is held; then the upstream's tags that name what was pushed (§11.6). Every push goes
   through `internal/ingest`, waits for the visibility barrier, records the OCI name in
   `oci_repos`, and writes the audit row `proxy.fetch` with `details.upstream` and `details.ref`
   (the digest, and the tag of a tag push).
4. The claim holder is answered from the stream as it goes into the backend (§11.8); every other
   request from the backend.

A blob that the backend held and lost, to a garbage collection of a blob no pushed manifest named
yet, is fetched again on its next miss.

### 11.6 Tags

- **The keep-tag.** Every manifest and every complete index Lajstrom pushes is pushed under the
  tag `_keep.<algorithm>.<encoded>` of its digest (`_keep.sha256.<64 hex>`), cut to 128
  characters: written once, never moved, never shown by `tags/list`, clear of the referrers tag
  schema `sha256-<hex>` (manifest-profile.md §6). The collection of untagged manifests leaves it,
  so a digest that clients pin stays when an upstream tag moves on.
- **Live tags.** Where the backend lets tags move, the upstream's tag is pushed, under the tag
  lock of its digest (index-schema.md, Coordination), once what it names is held: the manifest,
  or the index with every child. When the upstream moves the tag and the new manifest or index is
  held, it is pushed again. A tag whose index is not complete stays in `proxy_meta` only, until
  it is.
- **Backends whose tags do not move.** Lajstrom tells them by `Capabilities.ImmutableTags` (the
  vendor table, the probe, `registries[].capabilities.immutableTags`; on ECR an
  `imageTagMutability` of `IMMUTABLE*`), and otherwise by trying: a tag push that the backend
  refuses as a conflict (`provider.ErrConflict`: Quay's `409 TAG_IMMUTABLE`, Harbor's `412`,
  GitLab's `IMMUTABLE_TAG`, ECR's refusal) writes the repository's `oci.fixed-tags` row, since
  policies go by repository and tag pattern, and is logged at warn with the registry. From then
  on the upstream's tags of that repository stay in `proxy_meta` only, on every replica; a purge
  of the cache (§4.3) tries once more. The read that met the refusal is answered from the rows;
  the tags pushed before it keep their first digest in the backend and answer nothing (§11.4,
  step 2). Zot behind a policy without `update` answers `403 DENIED`, which reads as refused
  credentials: its operator sets `capabilities.immutableTags: true`.
- **The exception, and its bounds.** This is the one place where Lajstrom moves a tag: only in an
  `oci` proxy repo, only one of the upstream's tags, only to a digest the upstream named for it
  and the backend holds. It never moves a keep-tag, a tag of a hosted or managed repo, or a tag on
  a conflict with a client's push.
- `tags/list` of an `oci` proxy is the backend's, without the keep-tags.

### 11.7 Docker Hub

- **`library/`.** On an upstream with `dockerHub` (§2.1), a path of one component is
  `library/<path>` on every `/v2` route, where the facade splits the name: `oci/hub/nginx` and
  `oci/hub/library/nginx` are one repository, stored once, as `library/nginx`.
- **A repository that does not exist.** Docker Hub and quay.io grant an anonymous token without
  the `pull` action for it and then answer the read `401`, as for a private one; ghcr.io refuses
  the token, `403 DENIED`. A `401` or `403` of a repository read after a token was granted is
  `404` with an `absent` row; a refused token request, a wrong password, stays `502`. On an
  upstream without credentials, any `401` or `403` is `404`.
- **Rate limits.** A tag is revalidated with `HEAD`, which Docker Hub does not count, and a
  manifest or a blob the backend or the cache holds is never asked for with `GET`: a cold pull
  costs Docker Hub one counted pull per manifest fetched, the index and each platform pulled, and
  revalidation none. The `ratelimit-limit` and `ratelimit-remaining` headers of any upstream feed
  the gauges `lajstrom_proxy_upstream_ratelimit_limit`, `_remaining` and `_window_seconds` per
  upstream, the window read from `w=` and never assumed; below 10 % remaining, a warning. A
  `429` serves the stale row (§11.4, step 2), otherwise `503 TOOMANYREQUESTS` with `Retry-After`
  (the upstream's, at most 60 s; 5 s without one). Every replica behind one egress address shares
  one anonymous budget; an upstream credential raises it.

### 11.8 The verified tee

The request that holds a blob's claim (§11.5, step 2) is served from the upstream's bytes as
they go into the backend, an exception to §5's "served from the registry" so that a large layer's
first byte does not wait for its whole copy (containerd cancels a pull that gets no byte for
`image_pull_progress_timeout`, 5 minutes by default):

1. Only a blob whose digest and size a held or cached manifest gives tees: the `Content-Length`
   is known. A `Range` request, and every request that waits on the claim, are served from the
   backend once the blob is held.
2. The last byte is held back, as INV-40 does on a download, until the bytes have the digest
   **and** the backend has committed the blob. No client completes a blob the backend does not
   hold or whose bytes are not its digest; a refusal aborts the connection, and the client tries
   the layer again.
3. The copy never waits for the client: the client reads through a bounded buffer of a few MiB
   and is dropped, its connection aborted, when it falls behind or leaves; the fetch goes on.
4. Nothing is kept outside the registry but that buffer
   ([ADR 0001](../adr/0001-oci-registry-as-sole-storage.md)).

### 11.9 What an `oci` proxy does not do

Groups of `oci` repos are refused ([#848](https://github.com/lajstrom/lajstrom/issues/848)). A
client's `PUT` of a manifest that is not a referrer is `405 UNSUPPORTED`; referrers, their blob
uploads and deletes are as on a hosted `oci` repo (http-routing.md §6). Nothing but `oci_repos`
is indexed of what the backend holds; reindex rebuilds those rows from the backend's catalog or
`oci_repos`, and neither reads nor writes `proxy_meta`.
