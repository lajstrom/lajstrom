<!-- The design of groups of oci repos for #848, written on 2026-11-13 after the lead's decision; input for the contract step that comes with the code. -->

# Groups of `oci` repos: one pull address, one member per repository path

Design note for [#848](https://github.com/lajstrom/lajstrom/issues/848), written on 2026-11-13.
The lead decided on the issue (2026-11-13): **read-only groups of `oci` repos, one member per
repository path**; a design note now, the code after the #442 stack (the `oci` proxy repos: PRs
#835 to #869, and #874, #880 to #882 and the fixes stacked on them) is merged.

This note is not a contract. ADR 0013, ADR 0018 (#835), proxy-and-group.md (§1, §2.3, §2.5,
§8, and §11 as #856 brings it), http-routing.md §6 and rbac.md §7 still say that a group of
`oci` repos is refused, and they win until the contract step changes them, with the code.

Read for it: proxy-and-group.md §2.3 to §2.5 and §8 (the npm groups), ADR 0013 points 6 and 7,
[ADR 0018](https://github.com/lajstrom/lajstrom/pull/835) and spec §11
([#856](https://github.com/lajstrom/lajstrom/pull/856)), rbac.md §7, http-routing.md §6, and
docs/quickstart/oci-proxy.md ([#869](https://github.com/lajstrom/lajstrom/pull/869)). A section
number without a file is proxy-and-group.md's.

## Summary

- A group of `oci` repos is `oci/<group>/<path>` on `/v2`, read-only. Its members are 1 to 16
  hosted and proxy `oci` repos.
- **Each path is served by exactly one member**, which the group's routes choose: path patterns
  per member, the most specific pattern winning. The same pattern on two members is refused at
  create, at `PATCH` and at start. A route is decided without I/O, and a group never asks a
  second member: not on a `404`, not on a `5xx`.
- **The member answers everything for the path**, as at its own URL: a tag, a digest, an index
  and its children, blobs, `tags/list`, referrers. Nothing is merged across members.
- **Every write is `405 UNSUPPORTED`**, `Allow: GET, HEAD`, once the principal may read the
  group.
- **Read on the group is the whole permission**, as for npm (rbac.md §7). The admin role sets
  the members and the routes.
- **Audit:** a proxy member's fetches through the group are its `proxy.fetch` and `proxy.reject`
  rows with `details.via`, the group. Reads and refused writes write no row.
- **Code:** the facade resolves a group's path to its member, as #866 resolves an `oci` proxy;
  `oci` gets no plugin and no `format.Grouper`. The routes are checked by `internal/proxy`,
  `internal/api` and `internal/config`, and stored in `repos.group`; no migration and no
  exported interface change.
- Six questions for the lead are at the end (§9).

## 1. What it is for

One pull address over internal images and the mirrors: `docker pull
lajstrom.example.com/oci/all/acme/api:1.4` from the hosted repo, `…/oci/all/nginx:1.27` from the
Docker Hub proxy, with one login and one base in CI, and the pull side of a Nexus or Artifactory
docker group that teams leave. Several upstreams under one address work only by explicit routes
(§2), since a path does not say which registry it is from.

An example, used through the note:

```yaml
upstreams:
  - {name: dockerhub, format: oci, url: "https://registry-1.docker.io"}
  - {name: ghcr, format: oci, url: "https://ghcr.io"}
repos:
  - {name: internal, format: oci, registry: local}
  - {name: hub, format: oci, registry: local, kind: proxy, upstream: {name: dockerhub, deny: ["acme/*"]}}
  - {name: ghcr, format: oci, registry: local, kind: proxy, upstream: {name: ghcr, allow: ["oras-project/*"]}}
  - name: all
    format: oci
    kind: group
    group:
      members: [internal, hub, ghcr]
      paths:
        internal: ["acme/*"]
        ghcr: ["oras-project/*"]
        hub: ["*"]
```

| The client asks for | The route | The member and its path |
| --- | --- | --- |
| `oci/all/acme/api:1.4` | `acme/*` | `internal`, `acme/api` |
| `oci/all/oras-project/oras:v1.3.0` | `oras-project/*` | `ghcr`, `oras-project/oras` |
| `oci/all/library/nginx:1.27` | `*` | `hub`, `library/nginx` |
| `oci/all/nginx:1.27` (docker) | `*` | `hub`, `library/nginx`: the member's Docker Hub rule (§11.7) |

## 2. One member per repository path

### 2.1 Routes

`group.paths` maps each member to the patterns of the paths it serves. A pattern is one of:

- `<path>`: that path alone (`acme/api`);
- `<prefix>/*`: every path below the prefix, not the prefix itself, as `*` of §3 matches
  (`acme/*` matches `acme/api` and `acme/tools/lint`, not `acme`);
- `*`: every path.

The components are those of the OCI name grammar (ADR 0017); no other `*` is allowed, so that
the choice below is always defined. A path's member is the one whose pattern matching it is the
most specific: the path itself, then the longest prefix, then `*`. The patterns that can match
one path are thus ordered, and one of them wins, provided that no pattern is on two members.

**An overlap is refused where it is written.** The same pattern on two members is `400` on the
API, naming both (`group.paths.ghcr[0]: "*" is also group.paths.hub[0]`), and a configuration
error at start: the bootstrap creates a group through the API, and a group it refuses stops
`serve`, as a refused policy does. Nested patterns are not overlaps; the most specific one wins.
The check needs the group's row alone, never what a member holds, so it is done once, when the
row is written.

**A route needs no I/O.** It is decided from the group's row, so a path goes to the same member
on every replica, whatever the members hold and whatever an upstream answers. A path that no
route selects is `404 NAME_UNKNOWN`.

**No fallback.** The member's answer is the group's: its `404`, its `5xx`, its `503
TOOMANYREQUESTS`. Another member is never asked, and a digest is never looked up in another
member, though content addressing would allow it: the index would then come from one member and
a child from another, and a path would serve bytes its member does not hold.

**The member's own rules still apply.** A proxy member checks its lists (§11.2); a path they
refuse is `404 NAME_UNKNOWN` through the group, as §3 has a group's member "not have" a name
its lists refuse. The member's backend checks its own name (`CheckName`, §11.2), and an offline
member is offline through the group too (§4.4). The client's name, `oci/<group>/<path>`, is
bounded at 255 bytes by the facade as every name is.

**Routing internal names.** A route to a proxy sends the paths it selects to the upstream. Route
every internal prefix to the hosted member (`acme/*`), and add `acme` alone when it is a
repository: hub's `*` would otherwise ask Docker Hub for `library/acme`. The proxy's `deny`
(§11.2) is still the guard that holds when the proxy is read at its own URL.

**Docker Hub's short names.** A route matches the path as the client sent it, and the member's
own rule comes after: docker sends `oci/all/nginx`, containerd as a mirror sends `library/nginx`,
and both reach a `*` member that is Docker Hub, which keeps both as `library/nginx`. A group that
routes `library/*` to hub and `*` to another member sends `nginx` to the other one (§9, question
5).

### 2.2 Routes rather than the first member that has the path

The comment that put the decision to the lead sketched another rule: the members in order, and
the first that has the path answers for it (a hosted member by its `oci_repos` row, a proxy
member by its lists and its upstream's answer, with negative rows). The routes are recommended
instead (§9, question 1):

| | Routes | The first member that has the path |
| --- | --- | --- |
| What decides | the group's row | what the members hold and what the upstreams answer |
| The cost of a miss | none | an upstream request for each proxy member before the one that has the path, once per `negativeTTL` |
| One member per path | by construction | needs a row per path and member: a tag's `404` does not reliably say whether the repository exists, and Docker Hub answers `401` for a missing repository and a private one alike (ADR 0018 point 5). Without it, `latest` can come from one member and `1.27` from the next |
| Internal names | a route sends `acme/*` to the hosted member whether it holds the path or not: the upstream is never asked | as npm's §8.2: protected only while the hosted member has the path; a repository not pushed yet, or deleted, goes upstream unless `deny` stops it |
| Between upstreams | a path goes to the upstream its route names | a namespace registered on Docker Hub shadows the same path of ghcr.io or quay.io ordered after it: ADR 0013's dependency confusion, between registries |
| Stability | the member changes only when the routes do | changes when a member gains or loses a path; a pinned digest can move to another member |
| Configuration | a pattern list per member, often `acme/*` and `*` | the member order |
| Several hosted members | possible: the routes say which serves a path | needs §2.3's "at most one hosted" |

### 2.3 Settings

The resource of the example's group:

```json
{"format": "oci", "name": "all", "kind": "group", "anonymousRead": false,
 "group": {"members": ["internal", "hub", "ghcr"],
           "paths": {"internal": ["acme/*"], "ghcr": ["oras-project/*"], "hub": ["*"]}},
 "createdAt": "2026-12-01T10:00:00Z", "updatedAt": "2026-12-01T10:00:00Z"}
```

- `members` as §2.3 has it: 1 to 16 repos of the group's format, each hosted or proxy, none
  twice. Their order means nothing to an `oci` group; it is kept so that the API, the
  `group_members` rows and the `409` on deleting a member stay those of npm groups. Several
  hosted members are allowed in an `oci` group (§9, question 4): the routes say which one serves
  a path, and a later write step would know where a push goes.
- `paths`: every member has at least one pattern; a key that is not a member, a pattern that is
  not one of the three forms, a pattern twice in one member and more than 256 patterns in all are
  `400`, naming the field.
- `merge` is refused on an `oci` group: nothing is merged.
- `registry` and `policy` are refused, as on every group.
- The admin role changes `members` and `paths`, which change what the group's readers get, as
  `members` does today. A `PATCH` of `members` that leaves a member without patterns, or a
  pattern on a member it removes, is `400`: the two are sent together. `manage` changes
  `anonymousRead`.

`members` stay `group_members` rows; `paths` go to `repos.group`, a JSON column that holds
`{"merge"}` for an npm group today. The configuration's `repos[].group` takes the same keys.

## 3. What a group answers, and what it refuses

| Request | Through the group |
| --- | --- |
| `GET`, `HEAD` `manifests/<tag>` | the member's answer. Hosted: its backend's tag. Proxy: §11.4, the `oci.tag` row while fresh, a `HEAD` upstream after, the fetch of what it names on a miss |
| `GET`, `HEAD` `manifests/<digest>` | the member's. A proxy fetches a digest it neither holds nor caches, which is a counted pull on Docker Hub, as at its own URL |
| an index and its children | one path, so one member, always |
| `GET`, `HEAD` `blobs/<digest>` | the member's. A proxy serves only a blob that a manifest held or cached at its path lists (§11.5), streamed to the first client by the tee of §11.8 |
| `GET` `tags/list` | the member's list, with `n` and `last`; `name` and the `Link` name the group's path. Hosted: its backend's tags. Proxy: its backend's without the keep-tags, so only the tags pushed to the backend (§11.6) |
| `GET` `referrers/<digest>` | the member's, `artifactType` filter included; never merged. A signature pushed to another member is not seen. The tag schema's fallback (`sha256-<hex>` tags) answers from the same member |
| `GET /v2/_catalog` | recommended (§9, question 3): to a principal who may read the group, each member's repositories (`oci_repos`) that the routes send to that member, as `oci/<group>/<path>`: what the group serves without asking an upstream |
| a path no route selects, or one a proxy member's lists refuse | `404 NAME_UNKNOWN`, to a reader of the group |
| `PUT` of a manifest, a referrer included; every route below `blobs/uploads/`; `DELETE` of a manifest, a tag or a blob | `405 UNSUPPORTED` with `Allow: GET, HEAD`, whatever the path, once the principal may read the group; the group's denial otherwise (rbac.md §3) |
| a mount whose `from` is a group's path | from the routed member's OCI repository, when it is on the target's registry and the principal may read the group; otherwise a plain upload session, as for any source it cannot mount from |
| `/v2/token` | a scope of a group's path gets `pull` only: no action pushes or deletes through a group |

**Why `405 UNSUPPORTED`.** Today every `/v2` route of a group is `404 NAME_UNKNOWN`: a group has
no OCI repository. A group of `oci` repos has one for reads, so a `404` to a push would say the
name does not exist when a pull of it works. `403 DENIED` would send the user after a permission
that does not exist. `405 UNSUPPORTED` is the distribution spec's code for an operation the
registry does not support, and what an `oci` proxy already answers to a manifest push. The read
check comes first so that a write does not tell a stranger that the group exists. docker's push
gets a token with `pull` only and stops at the `405` of its first upload.

The member's statuses pass through: `404 MANIFEST_UNKNOWN` and `BLOB_UNKNOWN`, `400
NAME_INVALID` from the backend's name check, `403` naming a size bound, `502`, `503
TOOMANYREQUESTS` with `Retry-After`, `504` (http-routing.md §6, proxy-and-group.md §6 and
§11).

## 4. A proxy member and a hosted member behind one URL

A client cannot tell them apart, but by what the proxy's own rules make visible:

| | A path routed to a hosted member | A path routed to a proxy member |
| --- | --- | --- |
| A tag | the backend's | the upstream's, cached for `metadataTTL`, revalidated with a `HEAD`; a stale row while the upstream fails (§11.4) |
| The first pull | from the backend | fetched on the miss: per platform, the layers streamed to the first client (§11.5, §11.8) |
| `tags/list` | every tag | the tags pushed to the backend only |
| Referrers | what was pushed to it | as on a hosted `oci` repo (§11.9): what was pushed to the proxy repo, not the upstream's |
| The upstream down | no effect | the stale rows and what the backend holds; else `502`, `504` or `503` |
| A push | to `oci/internal/<path>`, its own URL; then pulled through the group | `405` through the group; at its own URL, referrers only (http-routing.md §6) |

The hosted member is unchanged: its `oci_repos` rows, reindex and deletes are its own. A proxy
member's fetch through the group is its own fetch, with its claims, rows and limits; the group
stores nothing (§8.5 holds), and reindex and the reconcile loop pass it by.

A signature for a mirrored image is pushed to the proxy repo's URL, with `write` on the proxy
(rbac.md §7), and is then seen through the group; one pushed to the hosted member at the same
path is not, since the path is the proxy's. A blob that two members hold is held by each: the
mounts of §11.5 stay within one repo, and mounting across members is not in this design.

## 5. What containerd and docker see

**containerd and Talos.** The group is a mirror of `docker.io`, as a proxy is in #869's guide:

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"

[host."https://lajstrom.example.com/v2/oci/all"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

`override_path` keeps the group in the path. For `nginx:1.27` containerd asks
`/v2/oci/all/library/nginx/manifests/1.27?ns=docker.io`; the route of `library/nginx` decides,
and `ns` is ignored, as ADR 0018 point 1 ignores it on a proxy. Talos takes the same in
`machine.registries.mirrors."docker.io"`, with `overridePath: true`.

The routes do not know which registry a path is from. A mirror of `ghcr.io` pointed at the group
would send `oras-project/oras` to the routes, and a path without its own route would go to
hub's `*`: the wrong upstream. So each other registry stays mirrored to its proxy member's own
URL (`/v2/oci/ghcr`), as #869 configures it, and the group serves `docker.io` and the internal
paths. Routing by `ns` would let one `_default` mirror serve every registry; it is a question
(§9, question 2), since docker and crane do not send it and the same URL would then answer
differently to different clients.

**docker and crane** name the group in the reference: `docker pull
lajstrom.example.com/oci/all/acme/api:1.4`, `docker pull lajstrom.example.com/oci/all/nginx:1.27`,
`crane pull lajstrom.example.com/oci/all/oras-project/oras:v1.3.0 oras.tar`. docker sends
`oci/all/nginx` without `library/`; the route sees `nginx`, and a Docker Hub member keeps it as
`library/nginx`. One `docker login lajstrom.example.com` serves every member. A `docker push`
to the group is `405`; the push goes to the member's own URL. The docker daemon's
`registry-mirrors` is not covered here: whether it can reach a repo below `/v2/oci/<repo>/` is
the same question for a group as for the proxy of #442, and #869's guide configures containerd
and Talos only.

## 6. RBAC

**Read on the group is the whole permission**, as rbac.md §7 says of every group. For a read
through the group, the members' bindings, deploy tokens and `anonymousRead` are not asked. It
grants the paths the routes select and nothing else of a member: a group can serve
`acme/public/*` of a hosted repo whose other paths stay private.

Asking for read on the member too was weighed and is not recommended: the group's one address
would then work only for principals bound on every member, an anonymous pull through it would
need `anonymousRead` on the members, which opens them at their own URLs too, and the rule would
differ from the npm groups that operators already know. The cost is rbac.md §7's: who may read
the group reads the routed paths of its members, so `members` and `paths` are the admin role's.

- **A proxy member's fetch** through the group is the server's, as at the proxy's URL: the reader
  needs `read` on the group and nothing on the proxy.
- **Writes** need nothing more: there is none. A principal that may read the group gets `405`; one
  that may not gets the group's denial.
- **Tokens.** The token endpoint keeps `pull` alone on a group's path: `push` and `delete` grant
  nothing there. A deploy token of a group reads only, as today (`internal/auth/tokens.go`
  refuses `write` on a group).
- **The group's resource** names its members and routes to its readers, as rbac.md §7 already
  lets it name its members.

## 7. Audit

- **Reads** are not audited (rbac.md §5), through a group or not.
- **A proxy member's fetch** through the group writes the member's rows, as §11.5 has them:
  `proxy.fetch` per manifest pushed, `proxy.reject` for refused bytes, the principal the reader,
  `repo` the proxy, `details.upstream` and `details.ref`, and `details.via`, `oci/<group>`, as
  rbac.md §7 has it for npm.
- **A refused write** writes no row: nothing was written, and it is refused before anything
  starts.
- **The group itself**: `repo.create`, `repo.update` (whose `changed` names `group.members` or
  `group.paths`) and `repo.delete`, as for every repo.
- **Logs**: the access log has the path the client sent, under the group; the facade logs the
  routed member at debug.

## 8. What changes in code, and where

**No plugin for `oci`, no `format.Grouper`.** `Grouper` is a `Proxier` with `MergeGroup`: its
hooks are documents of names, which `oci` does not have, and nothing is merged here. #866 keeps
`oci` groups refused through `checkGroup` in `internal/api/group.go`, which asks for a
`Grouper`, and `internal/proxy/group.go`'s `resolveGroup`, which refuses a format without one.
The `oci` group is served by the facade instead, as #866 serves an `oci` proxy (`ociProxy`,
`OCIProxy`). `resolveGroup` and the npm path do not change, and no exported interface of
`internal/format`, `internal/provider`, `internal/index` or `internal/ingest` changes.

**The facade** (`internal/facade`):

- `resolve` (facade.go) refuses every group today. For a group of format `oci` it returns the
  target of the routed member: `repo` the member, `via` the group (a new field of `target`),
  `name` the name the client sent, `path` the member's path after its own rule (`OCIPath`, #866),
  `oci` the member's OCI name. The route is decided after the principal's read on the group, so
  that a path no route selects is `NAME_UNKNOWN` only to a reader. Groups of other formats stay
  `NAME_UNKNOWN`.
- `authorize` evaluates `t.via` when it is set: with a bearer by the name the client sent, which
  is the group's; without, `format.Authorize` on the group.
- One check before the handlers: a write on a target with `via` is the read check, then `405
  UNSUPPORTED` with `Allow: GET, HEAD`.
- The `OCIProxy` calls of ociproxy.go get `format.WithVia(ctx, group)`, so that `proxy.fetch`
  and `proxy.reject` carry `details.via`. `OCIProxy` gains `OCIRoute(ctx, group, path)`, which
  returns the member and its path, so that the facade does not import `internal/proxy`, as #866
  keeps it.
- `mountSource` (uploads.go) authorizes a source on its `via`.
- `catalogNames` and `repositories` (lists.go) list a group's routed paths, if question 3 is
  answered so.
- `issueToken` (tokens.go) keeps `pull` alone on a scope of a group's path.
- `tags` needs nothing: it already names `t.name`, the client's.

**`internal/proxy`**: `GroupSettings` gains `Paths`; `ParseGroup` checks them for an `oci` group
and refuses `merge` there; a pure function routes a path, beside `Match`; `Core.OCIRoute` reads
`ListGroupMembers` and the routes. The npm fetcher's `proxy.reject` reads `format.ViaFrom`
(`reject`, download.go); an `oci` proxy's rows do not carry `via` yet, neither `proxy.reject`
(`ociReject`, ocimanifest.go) nor `proxy.fetch`, which the mirror path of `internal/ingest`
writes (#863). Both must, the background completion of §11.5, step 5 (#881) included, which
keeps the values of the request that started it.

**`internal/api`**: `checkGroup` lets `oci` through as `checkKind` does for an `oci` proxy
(#866); `checkMembers` allows several hosted members in an `oci` group; the `PATCH` of `paths`
is the admin role's and is checked with `members`; `openapi.yaml` gets `group.paths`.

**`internal/config`**: `validateMembers` and `memberKind` (validate.go) take an `oci` group's
shape, several hosted members included; the patterns and the overlaps are checked by the API
when the bootstrap creates the group, and a refusal stops `serve`, as for a policy today.
`internal/config` cannot import `internal/proxy`, which imports it.

**`internal/index`**: nothing. `repos.group` is JSON and takes `paths` without a migration;
`ListGroupMembers` exists. A request on a group costs one more index read than a request on its
member (the members), and the route a match over at most 256 patterns.

**Documents**: the contract step brings proxy-and-group.md (§1, §2.3, §2.5, a new section for
`oci` groups, §11.9), http-routing.md §6 (the group column of the `/v2` rows), rbac.md §7,
index-schema.md (`repos.group`) and configuration.md; then the guide gets a section on the group
in docs/quickstart/oci-proxy.md, and api.md the key.

**Pull requests**, after the contract step:

1. The settings: `internal/proxy`, `internal/api`, `internal/config`, openapi, with the table
   tests of the patterns (the forms, the most specific, the overlaps) and of the validation.
2. The facade: resolve, authorize, the `405`, `via` in the audit, the token, with tests against a
   fake `OCIProxy`: a tag, a digest and a blob through a hosted and a proxy member, the `405`
   after the read check, a stranger's denial, a path without a route, the `name` and `Link` of
   `tags/list`, referrers.
3. The catalog, an integration test against Zot, the e2e with crane and containerd (`hosts.toml`
   pointed at the group), and the guide.

**Later, and additive**: pushes through the group to the routed member when it is hosted (the
Nexus and Artifactory shape, with a target the routes make unambiguous); routing by `ns`;
mounts across members on one registry.

## 9. Questions for the lead

1. **Routes, or the first member that has the path?** The note recommends routes configured per
   member (§2.1, compared in §2.2); the comment that put the decision sketched the member order
   with presence checks. With routes: does the most specific pattern win (recommended, so that
   `acme/*` and `*` can live together), or is any nesting refused too?
2. **`?ns=`.** Ignored, as ADR 0018 does on a proxy (recommended), or a later key that routes
   containerd's requests by the registry they are for, so that one `_default` mirror serves
   every registry, though docker and crane would get other answers at the same URL?
3. **`_catalog`.** The group's routed paths, listed to its readers (recommended), or nothing, as
   npm groups and today's refused `oci` groups?
4. **Several hosted members** in an `oci` group (recommended: the routes say which serves a
   path), or §2.3's "at most one hosted", as for npm?
5. **Docker Hub's short names.** Route the path as the client sent it (recommended: the member's
   rule applies after), or route a one-component path as `library/<path>` in every group?
6. **The contract step.** A new ADR that amends ADR 0013 points 1 and 6 (a kind needs a plugin's
   hooks; a group has at most one hosted member, which hides the proxies) and ADR 0018 points 1
   and 5 (groups of `oci` repos stay refused), since accepted ADRs are changed by a new one
   (docs/adr/README.md), then the spec texts; or the spec texts alone, with this note as their
   record?
