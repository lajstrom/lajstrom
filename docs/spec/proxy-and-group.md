# Spec: proxy and group repos

- Status: normative once [ADR 0013](../adr/0013-proxy-and-group-repos.md) is accepted
  (2026-11-10). Changes need an issue labelled `contract`.
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
| `proxy` | what it fetched from its upstream, as ordinary manifests | the upstream's documents, cached (§4); files from the registry, fetched on a miss (§5) | none from clients but the delete of a held version and referrers on `/v2` (http-routing.md §6) |
| `group` | nothing | its members' documents merged, files from the member that has them (§8) | sent to its hosted member (§8.4) |

The kind is set when the repo is created and never changes. A repo of any kind has the routes
of its format: a client cannot tell a proxy or a group from a hosted repo. A format has the
kinds `proxy` and `group` when its plugin provides the hooks of §10; in M4 that is npm.
`oci` repos are `hosted` only.

## 2. Settings

### 2.1 Upstreams: the configuration

An upstream is the operator's, like a registry: the configuration names it, and repos refer to
it by name. The API never takes a URL or a file path (§9). Keys, for configuration.md:

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `upstreams[].name` | name | required | Unique; the grammar of `registries[].name`. Example: `npmjs`. |
| `upstreams[].format` | plugin name | required | The format it serves; a plugin of this build that has the kind `proxy`. Only a proxy repo of this format names it. |
| `upstreams[].url` | absolute `http(s)` URL | required | Base of every request, e.g. `https://registry.npmjs.org/`; a path is allowed, credentials, a query and a fragment are not. `http` is accepted with a warning at start: what it serves is not authenticated. |
| `upstreams[].auth.username` | string | empty | With `passwordFile`: HTTP Basic. |
| `upstreams[].auth.passwordFile` | path | empty | File that holds the password. |
| `upstreams[].auth.tokenFile` | path | empty | File that holds a bearer token (`Authorization: Bearer`), npm's `_authToken`. Not with `username`. |
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

The repo resource of `/api/v1/repos` gains `upstream`, an object, on a proxy repo only:

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

`POST /api/v1/repos` takes `kind`, `upstream` and `group` beside the fields it takes today. The
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

## 3. Names: allow, deny and merge

A pattern is matched against the plugin's key of a name (§10: npm the name, `@scope/name`; pypi
the PEP 503 name; maven `groupId:artifactId`), ignoring ASCII case. `*` matches any run of
characters, `/` and `:` included, and every other character matches itself: `@acme/*`,
`com.acme:*`, `*-internal`.

A proxy fetches a name when `allow` is empty or a pattern of it matches, and no pattern of
`deny` matches; `deny` wins. A name it does not fetch is `403 DENIED` naming the list and the
pattern that refused it (`deny: @acme/*`, or `allow` when no pattern of it matches), as ADR
0008's rules answer, and no request goes to the upstream for it; through a group, the member
does not have the name (§8.1), so a name that no other member has is the group's `404`. The
lists say what a proxy mirrors; refusing a version for a reason, with `403`, is ADR 0008's rules, which are asked at
the same point (§5, step 3).

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
   will be asked, before step 4, for a held version and a fetch alike.
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
7. **The download.** Each file the entry lists (npm: one tarball) is requested from the
   upstream (§9.2) and spooled to `$TMPDIR` as an upload is, while sha256 and the algorithms of
   the upstream's checksums are computed. A `Content-Length`, or a body, above the repo's
   `policy.maxFileSize` or the registry's `capabilities.maxBlobSize` stops the fetch (`403`,
   §6; an audit row `proxy.reject`). An upstream `404` or `410` makes the name's row stale (§4.3).
8. **The check.** The strongest checksum the entry gives (sha512, then sha256, then sha1) must
   match the spool. A mismatch refuses the fetch: nothing is pushed, an audit row
   `proxy.reject` and a warning name both values. An entry without a checksum is refused the
   same way, unless the format's ADR says that its upstream publishes none.
9. **The ingest.** The plugin builds the upload from the spooled files as it would a client's
   (`Plugin.Build`), with `Upload.Origin` set (§10), and the fetcher calls `Publisher.Upload`
   with the principal of the request: Build, Validate, the tag claims, the push, the index rows,
   the audit row `proxy.fetch` instead of `push`, as for any upload. The manifest carries
   `lajstrom.origin.url` and `lajstrom.origin.checksum` and no `lajstrom.uploaded-by`
   (manifest-profile.md §3.1). An artifact that Validate refuses is `proxy.reject` and `502`:
   the upstream served it, the client did nothing wrong. A tag that holds other bytes keeps
   them (ADR 0005): the repo serves what it holds, and the change upstream is logged at warn.
10. The claim is released and the spool removed, whatever the outcome; on success the file is
    served from the registry as in step 3. The spool is never served from.

The fetch runs on a context of its own, bounded by the upstream's timeouts and the server's
shutdown, so that a client that disconnects does not cancel it for the requests that wait on
it. It counts as an upload for `server.maxConcurrentUploads`: it holds a temporary file and a
connection to the registry.

## 6. Errors

| The upstream, or the fetch | A document | A file |
| --- | --- | --- |
| answers | served (§7) | ingested, then served |
| a name outside the lists | `403` naming the list and the pattern; through a group, the member does not have it | the same |
| offline and nothing held | `404` | `404` |
| `404`, `410` | `404`; an `absent` row | `404`; the name's row made stale |
| `401`, `403` (Lajstrom's own credentials) | the cached row, else the held versions, else `502` | `502`; logged at error |
| `429` | the cached row, else the held versions, else `503` with `Retry-After` (the upstream's, at most 60 s; 5 s without one) | `503` with `Retry-After` |
| `5xx`, a connection or TLS error, a refused redirect (§9.2), a document too large or unparsable | the cached row, else the held versions, else `502` | `502` |
| a timeout (§2.1) | as above, `504` | `504` |
| a file above `maxFileSize` or `maxBlobSize` | | `403`, naming the bound; `proxy.reject` |
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
the upstream client, the spool and the check, the ingest call, the statuses, which members take
part in a group. The plugin's handlers ask the core for a document or a file of a repo of any
kind and do not branch on the kind; a write asks the core for its target repo (the repo itself,
a group's hosted member, or `405`).

The same contract step adds: `Upload.Origin` (the URL and the verified checksum), from which
ingest writes the origin annotations and the audit row `proxy.fetch`; the index methods of
`proxy_meta`, `fetch_claims`, `group_members` and the new `repos` columns; and the sentinels
for `405`, `502`, `503` and `504` in `format.StatusOf`.
