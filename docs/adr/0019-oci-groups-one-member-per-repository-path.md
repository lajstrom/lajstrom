# 0019. Groups of oci repos: read-only, one member per repository path

- Status: accepted
- Date: 2026-11-13
- Deciders: gecube

## Context

[#848](https://github.com/lajstrom/lajstrom/issues/848) asks for one pull address over internal
images and the mirrors: `docker pull <lajstrom>/oci/all/acme/api:1.4` from a hosted repo,
`…/oci/all/nginx:1.27` from a Docker Hub proxy, with one login and one base in CI. It is the pull
side of the Nexus and Artifactory docker groups that teams leave. The design note
[docs/design/oci-groups-2026-11.md](../design/oci-groups-2026-11.md) (#964) weighs it in full;
the lead decided its six questions on #848 on 2026-11-13
([comment](https://github.com/lajstrom/lajstrom/issues/848#issuecomment-6850237543)).

Three texts in force stand in the way:

- [ADR 0013](0013-proxy-and-group-repos.md) point 1: a format has a kind when its plugin provides
  the hooks of point 9, and `oci` has no plugin.
- ADR 0013 point 6: a group's members are read in order, at most one of them hosted; a name the
  hosted member holds hides the proxies; the members' documents are merged. Point 7: a write
  through a group goes to its hosted member.
- [ADR 0018](0018-oci-proxy-repos-pull-through-from-any-registry.md) point 1 and its option 5:
  groups of `oci` repos stay refused.

The forces:

- **Content addressing.** An index names its manifests, a manifest its blobs, by digest. If an
  index came from one member and a child from another, or a digest were looked up in a member
  other than the one that served the tag, a path would serve bytes its member does not hold, and
  a signature pushed beside one image would vouch for another's.
- **Presence costs a request and is not reliable.** A proxy member has a path only if its
  upstream says so. A tag's `404` does not tell whether the repository exists, and Docker Hub and
  quay.io answer `401` for a repository that does not exist and for a private one alike (ADR
  0018, option 4). Asking each proxy member in turn costs an upstream request per member before
  the one that has the path, once per `negativeTTL`.
- **Dependency confusion between registries.** In member order, a namespace that someone
  registers on Docker Hub shadows the same path of ghcr.io ordered after it, and a hosted
  repository not pushed yet goes upstream: ADR 0013's attack, between registries.
- **Replicas** must send a path to the same member, whatever each has cached.
- **Clients.** containerd, as a mirror with `override_path`, asks
  `/v2/oci/all/library/nginx/manifests/1.27?ns=docker.io`; docker and crane name the group in the
  reference, send `oci/all/nginx` without `library/`, and send no `ns`.
- **Elsewhere.** Nexus and Artifactory docker groups: ordered members, writes to one member.
  Harbor, Quay and Zot have no groups.

## Considered options

**Scope.** Stay refused, with mirror lists in `hosts.toml`: no single address for docker or CI.
A full group (per tag, merged tag lists and referrers, writes to the hosted member): about as
large as the proxy, a blob `GET` must find the member whose manifest lists it, signatures cross
members, and once pipelines push through it the write path is a promise. **Read-only, one member
per repository path: chosen** by the lead on 2026-11-13.

**How a path finds its member.**

- **A. The first member in order that has the path**, by an `oci_repos` row or an upstream's
  answer, with negative rows. The cost and the doubt of presence above; a path can change member
  when a member gains or loses it, so `latest` and `1.27` can come from two members.
- **B. Routes configured per member**, path patterns decided from the group's row alone. No
  request to decide, the same member on every replica, an internal prefix never sent upstream,
  and several hosted members possible.
- With B: nested patterns refused, or the most specific one winning so that `acme/*` and `*` can
  live together.

**Chosen: B, the most specific pattern winning.**

**Containerd's `?ns=`.** Route by it, so that one `_default` mirror serves every registry: the
same URL would answer docker and crane, which do not send it, otherwise. **Chosen: ignored**, as
ADR 0018 point 1 ignores it on a proxy.

**`/v2/_catalog`.** Nothing, as for npm groups; or the paths the group serves without asking an
upstream. **Chosen: the routed paths.**

**Hosted members.** At most one, as ADR 0013 point 6; or several, since the routes say which one
serves a path. **Chosen: several.**

**Docker Hub's one-component names.** Route `nginx` as `library/nginx` in every group, or route
the path as the client sent it and let the member's own rule apply after. **Chosen: as sent.**

**The contract step.** The spec texts alone, with the note as their record; or a new ADR, since
accepted ADRs are changed by a new one ([README](README.md)). **Chosen: this record**, then the
spec texts.

## Decision

Decided by the lead on 2026-11-13; the merge of this record is his approval. Normative text:
[proxy-and-group.md](../spec/proxy-and-group.md) §12, with
[http-routing.md](../spec/http-routing.md) §6, [rbac.md](../spec/rbac.md) §7 and
[index-schema.md](../spec/index-schema.md), which a `contract` pull request brings to this record.

1. **Kind.** An `oci` repo may be a `group`. It has no plugin and no `format.Grouper`: the facade
   serves it on `/v2`, as it serves an `oci` proxy. This amends ADR 0013 point 1 for `oci`, and
   supersedes "groups of `oci` repos stay refused" of ADR 0018 point 1 and option 5.
2. **Members.** 1 to 16 `oci` repos, each hosted or proxy, none twice; any number of them hosted.
   Their order means nothing to an `oci` group. This replaces ADR 0013 point 6 for `oci`, but for
   its read permission (point 5 below): no member order, no hosted member hiding the proxies, no
   merge and no `merge` key.
3. **Routes.** `group.paths` maps each member to the patterns of the paths it serves: `<path>`,
   `<prefix>/*` (every path below the prefix, not the prefix itself) or `*`; every member has at
   least one. A path is served by the member whose matching pattern is the most specific: the path
   itself, then the longest prefix, then `*`. The same pattern on two members is refused at the
   create, at a `PATCH` and at start, where the bootstrap creates the group through the API.
   - A route is decided from the group's row, with no I/O. A path that no route selects is
     `404 NAME_UNKNOWN`.
   - The member's answer is the group's, its `404` and `5xx` included: no other member is asked,
     and a digest is never looked up in another member.
   - The member's own rules still apply: a proxy member's lists, offline mode, its backend's name
     check.
4. **One member answers everything for a path**, as at its own URL: a tag, a digest, an index and
   its children, blobs, `tags/list`, referrers. Nothing is merged across members.
5. **Read on the group is the whole permission**, as ADR 0013 point 6 has it: the members'
   bindings, deploy tokens and `anonymousRead` are not asked. The admin role sets `members` and
   `paths`.
6. **Writes.** Every write through the group (a manifest `PUT`, a referrer's included, every
   route of a blob upload, a `DELETE`) is `405 UNSUPPORTED` with `Allow: GET, HEAD`, once the
   principal may read the group; the group's denial otherwise. This replaces ADR 0013 point 7 for
   `oci`: a hosted member takes pushes at its own URL. The token endpoint grants `pull` only on a
   group's path.
7. **`?ns=` is ignored.** A route matches the path as the client sent it, and a Docker Hub
   member's `library/` rule (ADR 0018 point 5) applies after, on the member's side: `nginx` and
   `library/nginx` both reach a `*` member that is Docker Hub, which keeps both as `library/nginx`.
8. **`/v2/_catalog`** lists to a reader of the group each member's repositories (`oci_repos`)
   that the routes send to that member, as `oci/<group>/<path>`.
9. **Audit.** A proxy member's fetches through the group are its own rows, `proxy.fetch` and
   `proxy.reject`, with `details.via` the group, as for npm. Reads and refused writes write no
   row.
10. **Storage.** A group stores nothing (ADR 0013 point 6). `paths` is kept in `repos.group` and
    the members stay `group_members` rows; no migration and no exported interface of
    `internal/format`, `internal/provider`, `internal/index` or `internal/ingest` changes.

ADR 0013's and ADR 0018's status lines name what this record supersedes.

## Consequences

- docker, crane, CI jobs and containerd's `docker.io` mirror pull internal images and Docker Hub
  through one URL and one login. Each other registry stays mirrored at its proxy member's own
  URL: the routes do not know which registry a path is from, and a ghcr.io mirror pointed at the
  group would send paths without a route of their own to Docker Hub. docker's `registry-mirrors`
  is not covered, as for the proxy.
- A route to a proxy sends what it selects upstream: internal prefixes must be routed to the
  hosted member (`acme/*`, and `acme` alone when it is a repository). A proxy's `deny` remains the
  guard at the proxy's own URL.
- A signature for a path is seen through the group only when pushed to the routed member. A blob
  two members hold is held by each: no mount across members.
- A request on a group costs one more index read than on its member, and a match over at most 256
  patterns.
- Who may read a group reads the routed paths of its members, so `members` and `paths` are the
  admin role's, as `members` is for npm.
- Later, and additive: pushes through the group to the routed member when it is hosted; routing by
  `ns`; mounts across members.
- Follow-ups: the `contract` pull request of the spec texts; the code (the settings, the facade,
  the catalog, the tests); the section on groups of the `oci` proxy guide.
