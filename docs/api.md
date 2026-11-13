# Management API

The management API is the REST interface to the control plane: repos, users, bindings, tokens
and the audit log. It lives under `/api/v1` on the same host as everything else
([http-routing.md](spec/http-routing.md) §1 and §4). The examples use the dev stack:

```sh
LAJSTROM=http://localhost:8080
```

The OpenAPI 3.1 document of these endpoints is
[`internal/api/openapi.yaml`](../internal/api/openapi.yaml), served at
`GET /api/v1/openapi.yaml` to anyone. A test holds it to the server's routes, and
`make api-lint` lints it with vacuum.

## Conventions

- **JSON.** Request bodies are one JSON object of at most 64 KiB; a field the endpoint does not
  know is an error, so a typo never passes silently. Answers are `application/json`. Times are
  RFC 3339 in UTC.
- **Authentication** is HTTP Basic with a local user's password or a token, or
  `Authorization: Bearer` with a personal access token or a deploy token, on every route
  ([auth.md](auth.md)): a request with credentials runs as the user (`user:<uid>`, for a
  personal access token too) or the deploy token (`token:<id>`, Basic `deploy:<token>`, under
  any user name, or `Bearer ljs_dep_…`), one without as `anonymous`,
  and credentials that are refused are `401` at once. `GET /api/v1/whoami` tells who the
  caller is. **Authorization** is RBAC's ([rbac.md](spec/rbac.md)): the bindings of the
  principal decide, behind the scopes of its token ([auth.md](auth.md#what-a-token-may-do)).
  Reading a repo and listing it need `read` on it, changing, deleting and reindexing
  it and managing its deploy tokens need `manage`, creating a repo and managing users and
  bindings need the `admin` role, and the tokens of a user are the user's or the admin's ([rbac.md](spec/rbac.md) §2 and §3). A
  denial does not tell whether a repo exists: `401` with a Basic challenge for a caller without
  credentials, `404` for one without any access to the repo, `403` for one that can read it but
  lacks the action. The list leaves out the repos the caller may not read.
- **Audit.** Every change is one row of the audit log ([rbac.md](spec/rbac.md) §5): the
  principal, the action (`repo.create`, `repo.update`, `repo.delete`, `repo.reindex`,
  `repo.cache.purge`, `delete` for each manifest of a version delete, `user.create`,
  `user.rename`, `user.disable`, `user.enable`, `user.password`, `user.delete`,
  `binding.create`, `binding.delete`, `token.create`, `token.revoke`), the repo, the client
  address and details: the change, its source (`api`, `cli`, `config` or `bootstrap`) and the
  request id.
- **Lists** that can be long are pages: `?limit=` (100 when absent, 1 to 1000) and `?cursor=`,
  the `next` of the page before, an opaque string; the last page has no `next`.
- **Any other method or path** under `/api/` answers `404 NOT_FOUND`, `OPTIONS` too: the API
  answers no CORS ([A browser on the same origin](#a-browser-on-the-same-origin)).

### Errors

Every error is answered with one envelope:

```json
{"error": {"code": "INVALID", "message": "name: \"Scratch\" must be lowercase letters and digits separated by single '.', '_' or '-', at most 64 characters"}}
```

| Status | Code | When |
| --- | --- | --- |
| 400 | `INVALID` | the request breaks a rule; the message starts with the field it is about |
| 401 | `UNAUTHENTICATED` | credentials are needed, or the ones sent were refused; with `WWW-Authenticate: Basic realm="lajstrom"` |
| 403 | `FORBIDDEN` | the caller lacks the action, by its bindings or by the scopes of its token, or the admin role for a system resource: creating a repo, users, bindings, the audit log |
| 404 | `NOT_FOUND` | no such repo, user, token or endpoint |
| 408 | `INVALID` | the body stopped: no byte came within `server.stallTimeout` ([configuration.md](configuration.md#server)); the message is fixed, and the connection is closed |
| 409 | `CONFLICT` | the repo exists, has versions and the delete is not forced, or is a member of a group; a live user has the name; a live token of the owner has the name; the binding exists; the change would leave no enabled user with an admin binding; a reindex that overlaps runs; a pointer names the version to delete, or the registry refuses to delete its manifest alone |
| 412 | `CONFLICT` | a conditional delete found the version changed since the client read it, or published after the cut-off; nothing was deleted |
| 413 | `INVALID` | the body is larger than 64 KiB |
| 429 | `UNAVAILABLE` | too many failed logins: the password is not checked until the time in `Retry-After` has passed ([auth.md](auth.md#failed-logins)) |
| 501 | `UNAVAILABLE` | a version delete on a registry that does not delete manifests (`capabilities.deleteManifest: false`); the message names the registry, and nothing was deleted |
| 502 | `UNAVAILABLE` | a version delete that the registry refused: it rejected Lajstrom's credentials or the request; the message is the status text and names the variants deleted before, the cause is in the server log with the request id |
| 503 | `UNAVAILABLE` | the index database does not answer, or a registry's circuit breaker is open; try again after `Retry-After` ([operations.md](operations.md#readiness)) |
| 500 | `INTERNAL` | anything else, a panic of a handler too; the message is the status text, the cause is in the server log with the request id (`X-Request-Id`) |

## Repos

A repo is one format and one name in one backend registry ([ADR 0004](adr/0004-path-first-addressing.md)).
There is no auto-creation on push: a repo exists before anything is pushed to it. Repos are
created through this API, by `lajstrom admin repo`, or from the `repos` section of the
configuration at startup ([configuration.md](configuration.md#repos)); all three validate and
audit alike.

### The resource

```json
{
  "format": "raw",
  "name": "scratch",
  "registry": "local",
  "kind": "hosted",
  "anonymousRead": false,
  "policy": null,
  "createdAt": "2026-11-07T12:00:00Z",
  "updatedAt": "2026-11-07T12:00:00Z"
}
```

| Field | Rule | Changes |
| --- | --- | --- |
| `format` | a format plugin the server has: `raw`, `pypi`, `apt`, `maven`, `npm`; or `oci`, the pass-through repos of the OCI facade, which need no plugin ([http-routing.md](spec/http-routing.md) §1) | never |
| `name` | unique within the format; lowercase letters and digits separated by single `.`, `_` or `-`, at most 64 characters ([manifest-profile.md](spec/manifest-profile.md) §4.1); none that ends in `blobs`, `.uploads` or `.sync`, or is `index.json`, `oci-layout`, `manifests`, `referrers` or `tags`, names that a registry's storage or routes keep for themselves ([ADR 0017](adr/0017-the-grammar-of-oci-names.md)) | never |
| `registry` | one of `registries[].name` of the configuration; none on a group, whose resource has no `registry` | never: the artifacts stay in the registry they were pushed to |
| `kind` | `hosted` (the default; may be left out), `proxy`, a repo that serves an upstream and keeps what it fetched, or `group`, one URL for several repos of its format ([proxy-and-group.md](spec/proxy-and-group.md)); `proxy` and `group` need a format whose plugin has them (npm), or `oci`, which needs none | never |
| `anonymousRead` | readable without credentials, when `auth.anonymousRead` allows it; default `false` | `PATCH` |
| `policy` | a JSON object of the keys below, or `null` for none; none on a group, whose resource has no `policy` | `PATCH`, merged |
| `upstream` | a proxy repo only, required there: `name`, one of `upstreams[].name` of the repo's format in the [configuration](configuration.md#upstreams), and `metadataTTL` (`"10m"`, `0s` to `168h`), `negativeTTL` (`"2m"`, `0s` to `24h`), `offline` (`false`), `allow` and `deny` (at most 256 name patterns each, `*` matching any run: `"@acme/*"`) ([proxy-and-group.md](spec/proxy-and-group.md) §2.2, §3). The resource shows every key; the upstream's URL and credentials are configuration and never part of the API, and a key the server does not know is `400` | see [proxy-and-group.md](spec/proxy-and-group.md) §2.2 |
| `group` | a group only, required there: `members`, the names of 1 to 16 repos of the group's format, in the order they are read, each `hosted` or `proxy` and at most one `hosted`, and `merge` (at most 256 name patterns): the names whose versions are merged even when the hosted member holds them ([proxy-and-group.md](spec/proxy-and-group.md) §2.3, §8). The resource shows both keys. A group of `oci` repos has no `merge`, may have several hosted members, and has `paths` instead: each member's name to the patterns of the paths it serves, `<path>`, `<prefix>/*` or `*`, every member one at least and no pattern on two members (§12.1) | `members` and `paths` by the admin role, `merge` by `manage` |
| `createdAt`, `updatedAt` | set by the server | `updatedAt` on every change |

A proxy repo of the upstream `npmjs` and a group of the hosted repo `npm/internal` and that
proxy ([proxy-and-group.md](spec/proxy-and-group.md) §2.4):

```json
{"format": "npm", "name": "npmjs", "registry": "local", "kind": "proxy", "anonymousRead": false, "policy": null,
 "upstream": {"name": "npmjs", "metadataTTL": "10m", "negativeTTL": "2m", "offline": false, "allow": [], "deny": ["@acme/*"]},
 "createdAt": "2026-11-10T17:42:47Z", "updatedAt": "2026-11-10T17:42:47Z"}
{"format": "npm", "name": "all", "kind": "group", "anonymousRead": false,
 "group": {"members": ["internal", "npmjs"], "merge": []},
 "createdAt": "2026-11-10T17:43:51Z", "updatedAt": "2026-11-10T17:43:51Z"}
```

A group holds nothing of its own: its routes read its members in order, a name its hosted member
holds hiding the versions a proxy member has of it but for the names of `merge`, and its writes
go to the hosted member, with the permission there ([proxy-and-group.md](spec/proxy-and-group.md)
§8, [rbac.md](spec/rbac.md) §7). A binding on the group lets a user read every member through
it. A group of `oci` repos serves each path from the one member its routes choose, and takes no
write ([proxy-and-group.md](spec/proxy-and-group.md) §12):

```json
{"format": "oci", "name": "all", "kind": "group", "anonymousRead": false,
 "group": {"members": ["internal", "hub"], "paths": {"hub": ["*"], "internal": ["acme/*"]}},
 "createdAt": "2026-11-10T17:44:02Z", "updatedAt": "2026-11-10T17:44:02Z"}
```

#### Policy

The keys of `policy`, each with what it is when absent:

| Key | Values | Absent | Read by |
| --- | --- | --- | --- |
| `maxFileSize` | the largest file an upload, or a blob of `/v2`, may carry, in bytes: an integer, at least 1; a larger one is `413`. On every repo, an `oci` one too; the registry's `capabilities.maxBlobSize` bounds it as well, when that is smaller ([operations.md](operations.md#large-blobs-ml-models)) | the format's default (#246): npm 512 MiB, pypi and maven 4 GiB, apt and raw 8 GiB, oci 16 GiB | raw `PUT`, apt `PUT …/upload`, pypi upload, maven `PUT` of a file, as the bytes come; a blob upload of `/v2` (`POST ?digest=`, `PATCH`, `PUT ?digest=` of a session), on an `oci` repo too: `413 SIZE_INVALID` when the size it declares would carry the blob past the limit, before anything streams, or where the bytes of the session, counted across its `PATCH`es and the closing `PUT`, pass it, and the session ends; the manifest `PUT` of `/v2` (`oras push`), whose file layers ingest checks: `413 SIZE_INVALID` naming the file. Reindex keeps what the registry has |
| `blobRedirect` | `"auto"`: a blob download, the facade's blob `GET` and a frontend's file `GET`, is a `307` to the registry's storage when the registry redirects it there (a presigned URL of its object store), so the bytes bypass Lajstrom. `"never"`: Lajstrom follows the registry's redirects and streams every blob of the repo itself, for clients that cannot follow a redirect to another host. The registry's `capabilities.blobRedirect: false` ([configuration.md](configuration.md)) does the same for all its repos | `"auto"` | every blob read ([http-routing.md](spec/http-routing.md) §2, §3) |
| `maven.sessionTimeout` | a maven repo only: how long a publication session of a deploy lives after its last request, a duration from `1m` to `24h` (`"45m"`, `"2h"`). When it is up, the files of the session are dropped and nothing is published: there is no seal that would publish a version missing files (#53). A change applies to the sessions opened or touched after it | `"30m"` | the publication sessions of maven deploys |
| `maven.maxSessionFiles` | a maven repo only: how many files the publication session of a deploy may store, an integer from 1 to 999 (a version's manifest has a layer for each and one for its metadata, and a manifest of `/v2` at most 1 000). A signature (`.asc`, `.asc.asc`) and a `.sigstore.json` count; a checksum file, of any file, and `maven-metadata.xml` with its checksums are never stored and never count. The next file is `400` naming the bound and this key. It bounds one session: a deploy of a version that exists opens a session of its own ([http-routing.md](spec/http-routing.md) §3) | `256` | the publication sessions of maven deploys |

The server checks a policy whole, when a repo is created and after every change: a key it does
not know, a value out of its range, and `maven` on a repo of another format are `400`, with the
key path first in the message (`policy.maxFileSize: must be an integer number of bytes, at
least 1`). A member whose value is `null` is no key, and a policy without keys is `null`.

`<format>/<name>` is the repo's path on every protocol (`/raw/scratch/…`), its OCI path in the
registry below the instance prefix, and its name in role bindings.

### List: `GET /api/v1/repos`

`200` with the repos ordered by format and name. `?format=<format>` keeps one format; a format
the server does not have is `400`.

```sh
curl -s "$LAJSTROM/api/v1/repos?format=raw"
```

```json
{"repos": [{"format": "raw", "name": "scratch", "registry": "local", "kind": "hosted", "anonymousRead": false, "policy": null, "createdAt": "2026-11-07T12:00:00Z", "updatedAt": "2026-11-07T12:00:00Z"}]}
```

### Create: `POST /api/v1/repos`

The body has `format`, `name` and `registry`, and optionally `kind`, `anonymousRead` and
`policy`, and `upstream` for a proxy; a group has `group` and neither `registry` nor `policy`.
`201` with the resource and
`Location: /api/v1/repos/<format>/<name>`; `400` for a field that breaks its rule, `409` when the
repo exists. A `registry` that takes none of the repo's names is a `400` too: one whose names are
shallower than three components below its prefix (Docker Hub, or a lower
`registries[].capabilities.maxNameDepth`), or shorter than `<format>/<name>/` and one byte. The
registry's capabilities are those of its probe; when they are not known (the registry does not
answer), the repo is created with a warning in the log, and each publish checks its name.
`lajstrom admin repo create` checks the same, probing the registry; the repos of the
configuration are not checked, and the start of `serve` logs such a registry at error.

```sh
curl -s -X POST "$LAJSTROM/api/v1/repos" \
  -H 'Content-Type: application/json' \
  -d '{"format": "raw", "name": "scratch", "registry": "local"}'
# a proxy of the upstream npmjs of the configuration, which never fetches the scope @acme
curl -s -X POST "$LAJSTROM/api/v1/repos" \
  -H 'Content-Type: application/json' \
  -d '{"format": "npm", "name": "npmjs", "registry": "local", "kind": "proxy", "upstream": {"name": "npmjs", "deny": ["@acme/*"]}}'
# one URL for the hosted npm/internal and that proxy: internal's names hide npmjs's
curl -s -X POST "$LAJSTROM/api/v1/repos" \
  -H 'Content-Type: application/json' \
  -d '{"format": "npm", "name": "all", "kind": "group", "group": {"members": ["internal", "npmjs"]}}'
```

### Get: `GET /api/v1/repos/<format>/<name>`

`200` with the resource, `404` when there is no such repo.

```sh
curl -s "$LAJSTROM/api/v1/repos/raw/scratch"
```

### Change: `PATCH /api/v1/repos/<format>/<name>`

The body has the fields to change, `anonymousRead` and `policy`; a field left out stays as it
is. `policy` is merged into the repo's policy as a JSON merge patch
([RFC 7396](https://www.rfc-editor.org/rfc/rfc7396)): a key it has is set, a key whose value is
`null` is removed, `maven` is merged key by key, and the keys it does not name stay; the
policy that results is checked as a whole ([Policy](#policy)), and when it is refused nothing
changes. `"policy": null` removes the whole policy. `format`, `name` and `registry` cannot
change and are `400`: moving a repo to another registry would orphan its artifacts. `kind`
changes once, from a proxy to a hosted repo, in a body of its own ([below](#a-proxy-becomes-a-hosted-repo)).
On a proxy repo, `upstream` is merged into its settings the same way (`allow` and `deny` are
replaced whole) and checked whole; it cannot be removed. Changing `upstream.name` needs the admin
role, since it changes what every reader of the repo gets, and drops the repo's cache of the old
upstream; the other keys need `manage` ([rbac.md](spec/rbac.md) §7). On a group, `group` is
merged the same way, each list replaced whole and a key of `paths` set to `null` removed;
changing `members` or `paths` needs the admin role, `merge` needs `manage`, and the members are
checked as on a create. The admin role is asked for a change,
not for the key: a body that sends back the `upstream.name` or the `group.members` the repo has,
as a body that sends back what `GET` answered does, needs `manage` alone. Another name, or other
members, the same members in another order included, since a group reads its members in order,
is `403` for a caller without the admin role, and nothing of the body changes.
`200` with the resource as it is now; a body that changes nothing changes nothing, not even
`updatedAt`. The audit row has the policy as the merge left it.

```sh
# the policy is {"blobRedirect": "never", "maven": {"sessionTimeout": "45m"}}
curl -s -X PATCH "$LAJSTROM/api/v1/repos/maven/releases" \
  -H 'Content-Type: application/json' \
  -d '{"anonymousRead": true, "policy": {"maxFileSize": 104857600}}'
curl -s -X PATCH "$LAJSTROM/api/v1/repos/maven/releases" \
  -H 'Content-Type: application/json' \
  -d '{"policy": {"blobRedirect": null, "maven": {"sessionTimeout": "2h"}}}' | jq -c .policy
```

```json
{"maven":{"sessionTimeout":"2h"},"maxFileSize":104857600}
```

```sh
# a proxy that serves only what it holds, and fetches nothing of the scope @acme or @corp
curl -s -X PATCH "$LAJSTROM/api/v1/repos/npm/npmjs" \
  -H 'Content-Type: application/json' \
  -d '{"upstream": {"offline": true, "deny": ["@acme/*", "@corp/*"]}}' | jq -c .upstream
# the versions of lodash merged across the members, though the hosted repo has its own lodash
curl -s -X PATCH "$LAJSTROM/api/v1/repos/npm/all" \
  -H 'Content-Type: application/json' \
  -d '{"group": {"merge": ["lodash"]}}' | jq -c .group
```

```json
{"name":"npmjs","metadataTTL":"10m","negativeTTL":"2m","offline":true,"allow":[],"deny":["@acme/*","@corp/*"]}
{"members":["internal","npmjs"],"merge":["lodash"]}
```

#### A proxy becomes a hosted repo

`{"kind": "hosted"}`, alone in the body, by the admin role, turns a proxy repo into a hosted one
that keeps every version it holds ([proxy-and-group.md](spec/proxy-and-group.md) §2.6): the end of
a move from another registry ([migrating.md](migrating.md)). npm only, until pypi and maven have
proxies. The dist-tags and deprecations of the upstream's packuments that the proxy kept become
entries of the repo's journal, through the one write path, as `npm dist-tag` and `npm deprecate`
would write them, with an audit row each by the admin: from then on they live in the registry and
survive a reindex. A dist-tag of a version the repo does not hold is left out. The versions do not
move, keep their `lajstrom.origin.*` annotations and, when the upstream gave their publish time,
their date. The repo then takes publishes, and its upstream settings and cached documents are
gone. One audit row `repo.convert` has the upstream it had, the packages it held, the journal ops
written and what was left out.

`200` with the resource, now `hosted`. `400` on a group, beside another key, or for another kind;
`409` while a fetch from the upstream runs, and when the repo is a member of a group that has a
hosted member already (a group has one). A failure after the kind changed (the registry during the
journal) leaves a hosted repo with the documents not yet written: the same request resumes it. On a
hosted repo with nothing left to write it changes nothing. A hosted repo never becomes a proxy.

```sh
curl -s -X PATCH "$LAJSTROM/api/v1/repos/npm/legacy" -H 'Content-Type: application/json' -d '{"kind": "hosted"}' | jq -c '{kind, upstream}'
curl -s "$LAJSTROM/api/v1/audit?repo=npm/legacy&action=repo.convert" | jq -c '.events[0].details'
```

```json
{"kind":"hosted","upstream":null}
{"from":"proxy","ops":5,"packages":3,"requestId":"9d7bd4da…","skipped":[],"source":"api","to":"hosted","upstream":"verdaccio"}
```

### Delete: `DELETE /api/v1/repos/<format>/<name>`

`204`. A repo that has versions is refused with `409` and their number
(`repo raw/scratch has 3 versions; delete them or pass force=true`), unless `?force=true`. Then
every row of the index that refers to the repo goes with it: packages, versions and files, the
rendered indexes, deploy tokens, signing keys, publication sessions and tag claims. The
artifacts stay in the registry, untouched;
the audit row says how many versions the repo had. Role bindings name repos by mask and stay.
`404` when there is no such repo. A repo that is a member of a group is `409` naming the groups,
`force` or not: change the group's members first. A proxy's cached documents of its upstream go
with it, and what it fetched stays in the registry like a hosted repo's artifacts. A group has
no versions: its delete needs no `force`, and its members stay as they are.

```sh
curl -s -X DELETE "$LAJSTROM/api/v1/repos/raw/scratch?force=true"
```

### Reindex: `POST` and `GET /api/v1/repos/<format>/<name>/reindex`

`POST` starts a reindex of the repo on the replica that received it
([operations.md](operations.md#reindex) says what a run does) and answers `202` with the run as
soon as it holds the lock, before it reads the registry. The body is optional, and an empty one,
`Content-Length: 0` or chunked, is none: `{"prune": false, "verify": false}`, the defaults of
`lajstrom reindex --repo`; `prune` removes the rows of manifests that are gone from the registry,
`verify` reads every manifest again. A run that overlaps, of this repo or of every repo, on any
replica or in the CLI, is `409 CONFLICT` naming where it runs and since when. The start is an
audit row `repo.reindex`. `GET` answers the latest run that covered the repo, its own or a run of
every repo, with the repo's part of the report:

```json
{
  "id": 12, "state": "done", "source": "api", "principal": "user:01a1c136-51a6-7883-a350-b1cf6000a23f",
  "replica": "lajstrom-a 7", "allRepos": false, "prune": true, "verify": false,
  "startedAt": "2026-11-09T10:00:00Z", "heartbeatAt": "2026-11-09T10:00:30Z", "finishedAt": "2026-11-09T10:00:41Z",
  "report": {
    "repo": "raw/assets", "ociRepositories": 3, "manifests": 120, "added": 2, "updated": 0, "unchanged": 117,
    "removed": 1, "pending": 0, "deletedPending": 0, "degraded": 0, "referrers": 4,
    "foreign": [{"repository": "charts/demo", "tag": "0.1.0", "digest": "sha256:…", "mediaType": "application/vnd.oci.image.manifest.v1+json"}],
    "orphanReferrers": [], "orphanJournal": [], "drift": [], "split": [], "repositoryUnknown": [],
    "repairedReferrers": [], "errors": [], "truncated": false
  }
}
```

`deletedPending` counts the tags the registry still lists whose manifests were deleted through
Lajstrom less than `reconcile.graceAge` before the run, on a registry not classified `strong`:
the run does not index them again
([operations.md](operations.md#on-an-eventually-consistent-registry)). It is absent from the
report of a run of an earlier release.

`repairedReferrers` lists, for a run of the reconcile loop or with `verify` on a registry without
the referrers API, each `sha256-<hex>` index that Lajstrom pushed again with the referrers it
lacked: `{"repository", "subject", "index", "added": [digests]}`
([operations.md](operations.md#what-a-run-does)).

`state` is `running`, `done` (it went to the end, whatever the report lists), `failed` (the run's
own error is in `error`) or `abandoned` (its heartbeat stopped, a later start closed it);
`report` is `null` while it runs, and for a run of every repo that stopped before it reached this
repo. Both need `manage` on the repo; `404` when there is no such repo, or, for `GET`, when no
run has covered it. A proxy repo is reindexed from what it fetched,
as a hosted repo from what was published; a group holds nothing, and its report is empty.

```sh
curl -s -X POST "$LAJSTROM/api/v1/repos/raw/assets/reindex" -d '{"prune": true}'
curl -s "$LAJSTROM/api/v1/repos/raw/assets/reindex" | jq '.state, .report.removed'
```

### List packages: `GET /api/v1/repos/<format>/<name>/packages`

A page of the repo's packages, ordered by name (`?limit=`, `?cursor=` as in
[Conventions](#conventions)); `?prefix=` keeps those whose name starts with it. It needs `read`
on the repo. The cursor holds the place of the page's last package, its name, not a position:
packages published or deleted while a client pages neither repeat nor skip the others.

```json
{"packages": [{"name": "@acme/widgets", "createdAt": "2026-03-02T10:00:00Z"}], "next": "…"}
```

`createdAt` is the publish time of the package's first version. A group has no packages: the
list is empty.

### List versions: `GET /api/v1/repos/<format>/<name>/packages/<package>/versions`

A page of the package's versions, newest first, with what a retention job decides on and what
a conditional delete compares: each version's variants with their manifest digests, its
publish times, and the pointers that name it. It needs `read`; `404` when the repo or the
package does not exist. `<package>` is one path segment, as for the delete below.

```json
{
  "versions": [
    {
      "version": "2.1.0",
      "publishedAt": "2026-11-02T09:00:00Z",
      "lastModified": "2026-11-02T09:00:00Z",
      "variants": [{"variant": "", "tag": "2.1.0", "digest": "sha256:…", "publishedAt": "2026-11-02T09:00:00Z"}],
      "pointers": [{"kind": "dist-tag", "name": "latest"}]
    }
  ],
  "next": "…"
}
```

- **The publish time** of a variant is `org.opencontainers.image.created` of its manifest
  (`versions.created_at`), which reindex rebuilds: the frontends write it from the server's
  clock when they build the manifest; a manifest pushed on `/v2` carries the time its client
  wrote, which the server refuses when it is more than `server.maxCreatedAhead` (5 minutes)
  after its clock or `server.maxCreatedBehind` (an hour) before it. A version's `publishedAt`
  is that of its first variant and orders the list, `lastModified` that of its newest variant:
  a wheel added to an old version makes it modified, not newer.
- **The order** is `publishedAt` descending, then the version string descending (byte order). The
  cursor holds the place of the page's last version, `publishedAt` and version, so a version
  published while a client pages comes before the first page and shifts nothing: a job that
  ranks versions by this list ranks an old version lower than it is now, never higher, and
  deletes fewer, never more.
- **Pointers** name a version and can move to another: on npm the dist-tags
  ([http-routing.md](spec/http-routing.md), npm), `latest` included. No other format has them.
  A version a pointer names is refused by a delete unless it is forced.

### Get a version: `GET /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>`

One version as the page of a web UI shows it: its row of the list above, the OCI repository
that holds it, and for each variant what the index has of its manifest: the artifact type, the
`lajstrom.*` annotations, the metadata layer and the files. `?variant=` keeps one variant (`""`,
the empty value, is the variant of a format that has none); `publishedAt`, `lastModified` and
`pointers` stay the version's, of all its variants. It needs `read`; `404` when the
repo, the package, the version or the variant does not exist, and on a group, which has no
versions. `<package>` and `<version>` are one path segment each, as for the delete below.

```sh
curl -s "$LAJSTROM/api/v1/repos/pypi/internal/packages/requests/versions/2.32.3"
```

```json
{
  "version": "2.32.3",
  "publishedAt": "2026-11-02T09:00:00Z",
  "lastModified": "2026-11-02T09:00:00Z",
  "variants": [
    {
      "variant": "py3-none-any",
      "tag": "2.32.3__py3-none-any",
      "digest": "sha256:…",
      "publishedAt": "2026-11-02T09:00:00Z",
      "artifactType": "application/vnd.lajstrom.pypi.v1",
      "attrs": {},
      "metadata": {"name": "requests", "version": "2.32.3", "summary": "Python HTTP for Humans."},
      "degraded": false,
      "indexedAt": "2026-11-02T09:00:01Z",
      "files": [
        {"name": "requests-2.32.3-py3-none-any.whl", "mediaType": "application/vnd.lajstrom.pypi.wheel.v1+zip",
         "digest": "sha256:…", "size": 64928, "attrs": {"org.opencontainers.image.title": "requests-2.32.3-py3-none-any.whl"}}
      ]
    }
  ],
  "pointers": [],
  "ociRepository": "pypi/internal/requests"
}
```

- **Everything comes from the index**, so it is what reindex rebuilds from the manifests: `attrs`
  are the manifest's `lajstrom.*` annotations by their full key (`lajstrom.deb.arch`),
  `metadata` is the metadata layer, `null` when there is none or it was too large to keep, and
  `degraded` says the metadata layer could not be read and the row came from the annotations
  alone. `indexedAt` is when ingest or reindex last wrote or saw the row. A file's `attrs` are
  its layer's annotations, and the files are in the manifest's order.
- **Not in it:** the manifest itself and its referrers (signatures, SBOMs, attestations), which
  are live reads of the registry (#951). Both are on `/v2` under `ociRepository`:
  `/v2/<ociRepository>/manifests/<digest>` and `/v2/<ociRepository>/referrers/<digest>`. A
  file's download URL is not in it either: it is the format's own, as in the
  [quick starts](quickstart/).

### Delete a version: `DELETE /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>`

Removes a bad version everywhere it is visible ([http-routing.md](spec/http-routing.md) §4):
every variant of the version, or with `?variant=` the one variant (`?variant=` with no value is
the variant of a format that has none). Each variant is one manifest, deleted in the registry as
`DELETE /v2/<format>/<name>/<path>/manifests/<digest>` deletes it: its referrers first
(signatures, attestations, SBOMs, cosign's `sha256-<hex>.sig` tags, the tag-schema index), then
the manifest, then its rows in the index, the package's with its last version, and the repo's
rendered indexes (apt `Packages` and `Release`, the simple index, `maven-metadata.xml`), which the
next `GET` renders without it. Each manifest writes one audit row, action `delete`, with the
number of referrers in `details.referrers`, and the token of the request in `details.token`
(`token:<id>`) when it came with one. A manifest whose delete the registry answers `404` while
it still serves it by digest, as Quay does for a referrer pushed by digest (#817), was not
deleted: it is listed in `details.kept` and not counted in `details.referrers`, and a warning
names it; the delete goes on, since the registry keeps it whatever Lajstrom asks. Blobs are left
to the registry's garbage collection.

`<package>` is the name in the format's spelling, one path segment: a raw path escapes its `/`
as `%2F`, and a reverse proxy in front must pass it on encoded. `204`. It needs `delete` on the
repo (the `maintainer` role); `404` when the repo, the package, the version or the variant does
not exist; `501 UNAVAILABLE` naming the registry when it cannot delete manifests
(`capabilities.deleteManifest: false`, GHCR, distribution with deletes off), and nothing
changed, except for a variant whose OCI repository the registry does not know any more
(reindex's `repositoryUnknown`): its rows go, with the audit row, and nothing is written to the
registry; `409 CONFLICT` when the registry refuses to delete this manifest alone. The variants
deleted before an error stay deleted, and the error's message names them, with their digests,
whatever its status: `…; deleted before the failure, and still deleted: 1.0 sdist (sha256:…)`.
On a proxy repo the version goes as on a hosted one, and the next request for it fetches it
again; a group has no versions, `404`: the version is deleted in the member that holds it.

**Conditions.** A client that read the version first (a retention job, a UI) makes the delete a
compare-and-delete: the server checks each condition again under the locks that the delete
holds, which a move of a pointer waits for ([index-schema.md](spec/index-schema.md),
Coordination), so that nothing changes between the check and the delete. A version can change
after the read and before the delete only by being deleted and published again, by gaining or
losing a variant, or by a pointer moving onto it; each is refused, and nothing is deleted.

| Condition | Refused with |
| --- | --- |
| `If-Match: "sha256:…", "sha256:…"`: the digests of the variants the client read, each a quoted entity tag; the variants the delete takes must have exactly these digests. `*` matches any | `412 CONFLICT`: the version changed since it was read; the message names its digests now |
| `If-Unmodified-Since: <HTTP-date>`: the cut-off; no variant the delete takes may have been published after it, in whole seconds | `412 CONFLICT`: the version was published after the cut-off |
| always, unless `?force=true`: no pointer names the version | `409 CONFLICT` naming the pointers (`dist-tag latest`) |

A delete without the headers has no condition of the client's and keeps working as before: an
operator who removes a leaked secret does not wait for a grace age, and with `?force=true` not
for a dist-tag either; the dist-tag then names a version that is gone, which the packument leaves
out until it moves. Unlike RFC 9110, which ignores `If-Unmodified-Since` beside `If-Match`, both
are checked: they are two conditions, the digests and the age. A value that is not an entity
tag list or an HTTP-date is `400`, never ignored: a condition is not dropped silently. The
conditions are checked for every variant before the first is deleted, and again for each under
its lock.

```sh
curl -s -X DELETE "$LAJSTROM/api/v1/repos/pypi/libs/packages/requests/versions/2.32.0?variant=sdist"
curl -s -X DELETE "$LAJSTROM/api/v1/repos/raw/assets/packages/docs%2Freadme.txt/versions/0123456789abcdef"
curl -s -X DELETE "$LAJSTROM/api/v1/repos/npm/internal/packages/@acme%2Fwidgets/versions/1.0.0" \
  -H 'If-Match: "sha256:4f0c…"' -H 'If-Unmodified-Since: Sun, 01 Nov 2026 00:00:00 GMT'
```

### Delete versions in a batch: `POST /api/v1/repos/<format>/<name>/versions:delete`

The delete of a retention job: a list of versions, each with the digests of its variants as the
client read them, and the job's cut-off. Each item is checked and deleted on its own, under its
own locks, with the conditions of the single delete: its digests as `If-Match`, the cut-off as
`If-Unmodified-Since`, and pointers always refused (a batch is never forced). The batch is not
all-or-nothing, and a rerun is idempotent: an item deleted the first time is `gone` the second.
It needs `delete` on the repo, checked before the body is read; `404` when the repo does not
exist.

```json
{
  "items": [{"package": "@acme/widgets", "version": "1.0.0", "digests": ["sha256:…"]}],
  "cutoff": "2026-08-01T00:00:00Z",
  "dryRun": true,
  "job": "retention-2026-11-11"
}
```

`items` is 1 to 1000 versions (the body is at most 64 KiB, about 300 of them), `cutoff` an
RFC 3339 time (absent: none), `dryRun` checks every item and deletes nothing, and `job` names the
batch in the audit rows (`details.job`, with `details.token`), up to 64 letters, digits, `.`, `_`
and `-`, starting with a letter or a digit; without it the server makes one. A client that sends
one job in several requests ties them together. The answer is `200` with a result per item, in
the order of the request:

```json
{
  "job": "retention-2026-11-11",
  "dryRun": false,
  "results": [
    {"package": "@acme/widgets", "version": "1.0.0", "status": "deleted"},
    {"package": "@acme/widgets", "version": "1.1.0", "status": "protected", "message": "…",
     "pointers": [{"kind": "dist-tag", "name": "beta"}]}
  ]
}
```

| Status | The item |
| --- | --- |
| `deleted` | was deleted, every variant; in a dry run, would be |
| `changed` | has other digests than the item's: published again, or a variant added or removed; `digests` has them now |
| `protected` | a pointer names it; `pointers` has them |
| `young` | was published after the cut-off; `lastModified` says when |
| `gone` | does not exist, or its package does not: deleted meanwhile, or by an earlier run |
| `error` | failed otherwise, `message` says why; the other items go on. A registry that cannot delete manifests fails every item so. When the request ends (its client went away), the item in flight finishes and the items after it are not attempted: `error`, with the message `not attempted`, and one warning in the server's log with their count |

When a variant's delete is refused after another variant of the item was deleted (a race under
the lock), the item has the refusal's status and its message names the variants deleted.

```sh
curl -s -X POST "$LAJSTROM/api/v1/repos/npm/internal/versions:delete" \
  -d '{"items": [{"package": "@acme/widgets", "version": "1.0.0", "digests": ["sha256:4f0c…"]}], "dryRun": true}'
```

`lajstrom admin retention` is a client of these endpoints that a scheduler runs
([operations.md](operations.md#retention)).

### Purge a proxy's cache: `DELETE /api/v1/repos/<format>/<name>/upstream-cache[?name=<package>]`

Drops what a proxy repo keeps of its upstream's documents ([proxy-and-group.md](spec/proxy-and-group.md)
§4.3), of one package with `?name=`, and the documents rendered from them; the next read asks the
upstream again. What the repo fetched stays: its versions are deleted as a hosted repo's are.
`manage` on the repo; `204`, `400` on a repo that is not a proxy or for a `?name=` that is no
package name of the format, one audit row `repo.cache.purge` with `details.name` when one name
was purged.

```sh
curl -s -X DELETE "$LAJSTROM/api/v1/repos/npm/npmjs/upstream-cache?name=left-pad"
```

### Prefetch a proxy repo: `POST /api/v1/repos/<format>/<name>/prefetch`

Fetches versions of a list of package names into a proxy repo before a client asks for them,
each as a client's miss would fetch it ([proxy-and-group.md](spec/proxy-and-group.md) §5.1):
the upstream's document, the claim, the download, the checksum, the ingest, and an audit row
`proxy.fetch` per version with the caller as the principal (`proxy.reject` for one refused). It
is how packages move from another registry (#441). `manage` on the repo, checked before the body
is read; npm, until pypi and maven have proxies (#67, #68), and `oci`, which takes images (below).

```json
{"names": ["@acme/widgets", "left-pad"], "allVersions": false}
```

`names` is 1 to 100 package names in the format's spelling. Without `allVersions` the versions
are those the upstream's dist-tags name, or its newest when it has none; with it, every version
the upstream lists. The answer is `200` with a result per version, and one per name that failed
before its versions, in the order of the names:

```json
{"results": [
  {"name": "@acme/widgets", "version": "2.1.0", "status": "fetched"},
  {"name": "@acme/widgets", "version": "3.0.0-rc.1", "status": "held"},
  {"name": "left-pad", "status": "failed", "httpStatus": 403, "message": "… does not mirror \"left-pad\": deny: left-*"}
]}
```

| Status | The version |
| --- | --- |
| `fetched` | was fetched by this request and is now held |
| `held` | the repo held it already: nothing was asked of the upstream for it |
| `shared` | another request or replica fetched it meanwhile |
| `failed` | `httpStatus` is the status a request of its file would have had (`404` not upstream, `403` refused by `allow` or `deny` or above a size bound, `502` a checksum that differs or an upstream that failed, `504` a timeout), and `message` says why; the other versions go on |

A rerun is idempotent: what the repo holds is `held`, and what failed is tried again. `400` on a
repo that is not a proxy or for a bad `names`, `409` on an offline proxy (it asks its upstream
nothing), `404` when the repo does not exist. One request fetches at most 4 versions at a time
and answers when every version has ended; a long list goes in batches, as
`lajstrom admin repo prefetch` sends it.

```sh
curl -s -X POST "$LAJSTROM/api/v1/repos/npm/legacy/prefetch" -d '{"names": ["@acme/widgets"], "allVersions": true}'
```

On an `oci` proxy repo the same call fetches images before a client pulls them, each the way a
pull goes ([proxy-and-group.md](spec/proxy-and-group.md) §11.10): before a rollout over a poor link
to the upstream, so that the nodes are then served from the backend whatever the link does.

```json
{"images": ["nginx:1.27", "bitnami/redis@sha256:…"], "platforms": ["linux/amd64", "linux/arm64"]}
```

`images` is 1 to 100 references below the repo, `<path>[:<tag>][@<digest>]`: `latest` without a
tag or a digest, the digest when both are given, and on a Docker Hub upstream `nginx` is
`library/nginx`. A reference that names a registry (`docker.io/library/nginx`) is `failed`, `400`:
`lajstrom admin repo prefetch --host` strips it. `platforms` is required for a reference that names
an index: without it the image is `failed`, `400`, and the message lists the index's platforms. A
reference that names a single manifest takes no `platforms`. Platforms are compared as containerd
normalises them (`linux/arm64/v8` is `linux/arm64`, `linux/arm` is `linux/arm/v7`); an
attestation manifest (`unknown/unknown`) is fetched only when named.

For each image the tag is resolved (its row while fresh, else a `HEAD`), the index and the
manifest of each platform are fetched under their claims, and every blob of a manifest the
backend does not hold is mounted from another path of the repo where the backend holds it, or
fetched; then the manifests, the index once every platform is held, and the tag are pushed. The
answer has, per image, a result for what the reference names (`kind` `index` or `manifest`), one
per platform, and one per blob that was not held:

```json
{"results": [
  {"name": "nginx:1.27", "kind": "index", "digest": "sha256:…", "status": "fetched", "bytes": 10229},
  {"name": "nginx:1.27", "kind": "manifest", "platform": "linux/amd64", "digest": "sha256:…", "status": "fetched", "bytes": 72364016},
  {"name": "nginx:1.27", "kind": "blob", "platform": "linux/amd64", "digest": "sha256:…", "status": "fetched", "bytes": 28227259},
  {"name": "nginx:1.27", "kind": "manifest", "platform": "linux/arm64", "digest": "sha256:…", "status": "failed", "httpStatus": 503, "retryAfter": 60,
   "message": "1 of its 7 blobs failed; sha256:…: upstream hub answered 429: …"},
  {"name": "bitnami/redis:7.4", "status": "failed", "httpStatus": 404, "message": "… upstream hub does not have bitnami/redis:7.4"}
]}
```

`held` is what the repo had: nothing was asked of the upstream for it, and a blob mounted from
another path of the repo counts. A manifest is `failed` when one of its blobs is, `fetched` when
this request fetched it or one of them, `shared` when only another request did, else `held`, and
its `bytes` count its blobs'. `retryAfter` comes with a `503`, the upstream's `429`. A second run
is one `held` per image and platform. An index fetched for some of its platforms stays in the
cache with its tag, and the repo answers it from there, offline too. One request resolves at most
4 images at a time, and then fetches at most 4 blobs at a time. On Docker Hub a cold image costs
one counted pull for its index and one per platform: 50 images of two platforms are 150, above the
anonymous 100 an hour.

```sh
curl -s -X POST "$LAJSTROM/api/v1/repos/oci/hub/prefetch" -d '{"images": ["nginx:1.27"], "platforms": ["linux/amd64", "linux/arm64"]}'
```

## `lajstrom admin repo`

The same operations from the command line, straight against the index database of the
configuration, without HTTP; run `lajstrom migrate` first. A change is audited with the source
`cli`.

```sh
lajstrom admin repo create raw/scratch --registry local [--anonymous-read] [--policy '{"maxFileSize": 104857600}']
lajstrom admin repo create npm/npmjs --registry local --kind proxy --upstream '{"name": "npmjs", "deny": ["@acme/*"]}'
lajstrom admin repo create npm/all --kind group --group '{"members": ["internal", "npmjs"]}'
lajstrom admin repo list [--format raw] [--json]
lajstrom admin repo delete raw/scratch [--force]
```

`create` prints the resource as JSON on one line, `list` a table (`--json`: the document of
`GET /api/v1/repos`), `delete` nothing. An error goes to standard error with exit status 1.

`prefetch` is the exception: a client of the server's API, `POST …/prefetch` above, since only
the server reaches the upstream with its credentials. It reads no configuration; the server
and the token are those of `lajstrom admin retention`, `--url` or `LAJSTROM_URL`, and
`--token-file`, `LAJSTROM_TOKEN_FILE` or `LAJSTROM_TOKEN`, a token with `manage` on the repo.

```sh
lajstrom admin repo prefetch npm/legacy --from-file names.txt [--all-versions] [--batch 20] [--report report.json]
lajstrom admin repo prefetch npm/legacy --from-file report.json --failed --report report.json
```

The names file has one package name a line; blank lines and what follows `#` are skipped, and a
name listed twice goes once; `-` reads standard input. The names go in batches of `--batch` (1 to
100, default 20), each request bounded by `--timeout` (default `30m`). A line is printed per
version as each batch is answered, `fetched`, `held`, `shared` or `failed` with the status and
the message, then the counts; `--report` writes the results as JSON. `--failed` reads such a
report instead of a list and sends again the names of its failed results, so that a run is
resumed where it failed. A request that fails (the server down, a token without `manage`) fails
its names and stops the run, as do `SIGINT`, `SIGTERM` and `SIGHUP`: the names left are failed,
not attempted, and the report says so. Exit status 0 when every version was fetched, held or
shared, 1 when one failed or the run stopped, 2 on a usage error.

## Users

Local users, managed by the admin role; what a user is, its uid and its name, is in
[auth.md](auth.md#the-uid-and-the-name). `<user>` in a path is the user's name or its uid.

### The resource

```json
{
  "uid": "01a1c136-51a6-7883-a350-b1cf6000a23f",
  "name": "alice",
  "kind": "local",
  "disabled": false,
  "createdAt": "2026-11-09T16:48:44Z"
}
```

| Field | Rule |
| --- | --- |
| `uid` | set by the server, a UUID v7; never changes |
| `name` | 1 to 64 bytes of lowercase letters, digits, `.`, `_` and `-`, starting with a letter or a digit, not of the form of a uid, `deploy` reserved; unique among the live users |
| `kind` | `local` in this version |
| `disabled` | a disabled user's credentials and tokens are refused from the next request on |
| `createdAt` | set by the server |

A password is never answered.

### List: `GET /api/v1/users`

`{"users": […], "next": "…"}`: a page of the live users, ordered by name ([Conventions](#conventions)).

### Create: `POST /api/v1/users`

The body has `name`, `password` (8 to 1024 bytes) and optionally `admin`, `true` to bind the
user to the admin role on `*/*` in the same transaction. `201` with the resource and `Location:
/api/v1/users/<name>`; a name that a live user has is `409`.

### Get: `GET /api/v1/users/<user>`

The resource; `404` for a user that does not exist or was deleted.

### Change: `PATCH /api/v1/users/<user>`

The body has `name`, `disabled` or both. `disabled` changes first, then the name; each change
is an audit event of its own, and a value the user has already is none. `200` with the resource.
A new name that another live user has is `409`; disabling the last enabled user with an admin
binding is `409`.

### Password: `POST /api/v1/users/<user>/password`

The body is `{"password": "…"}`; `204`. The old password fails from the next request on.

### Delete: `DELETE /api/v1/users/<user>`

`204`. The user's tokens are revoked and its bindings deleted; the row is kept, under the name
`~<uid>`, so that the audit log still names it, and the name is free at once
([auth.md](auth.md#the-uid-and-the-name)). Deleting the last enabled user with an admin binding
is `409`. A deletion is not undone.

### Who may

The admin role only, for every endpoint of users, a user's own record included (the caller
reads itself with `GET /api/v1/whoami`); anyone else is `403`, anonymous `401`. A personal access
token with scopes and a deploy token never manage users.

### `lajstrom admin user`

The same, straight against the database of the configuration: `create`, `list`, `rename`,
`disable`, `enable`, `set-password`, `delete` ([auth.md](auth.md#local-users-and-passwords)).

### The first admin: `POST /api/v1/bootstrap`

The body has `token`, the one-time bootstrap token that `serve` made valid at start, `name` and
`password` ([auth.md](auth.md#bootstrap-the-first-admin)). No credentials: the token is the
credential, and no role is asked. `201` with the resource of the new user, bound to the admin
role on `*/*`, and `Location: /api/v1/users/<name>`; the token is used. The audit event is
`user.create` by `anonymous`, with the source `bootstrap`.

| Answer | When |
| --- | --- |
| `404 NOT_FOUND` | an enabled user has an admin binding, whatever the body: the message is the one of a path that is no endpoint |
| `403 FORBIDDEN` | the token is not the valid one; or it is, and it was used or expired, which the message says |
| `429 UNAVAILABLE` | 10 wrong tokens from the client address within a minute; `Retry-After` says when to try again, the right token too. Only with `server.trustedProxies` or `server.tls`, which give the server a client address: without either, wrong tokens are not counted and never refuse the right one ([auth.md](auth.md#bootstrap-the-first-admin)) |
| `400 INVALID` | the name or the password breaks its rule; the token stays valid |
| `409 CONFLICT` | a live user has the name; the token stays valid |

`lajstrom admin bootstrap --name <name>` sends it: the token from `--token-file`, or a prompt;
the password from standard input with `--password-stdin`, or a prompt; the server from
`--server`, or `server.listen` of the configuration on loopback.

## Bindings

A binding gives a user a role on the repos of a mask ([auth.md](auth.md#bindings),
[rbac.md](spec/rbac.md) §3). The admin role manages them.

### The resource

```json
{
  "id": 2,
  "principal": "user:01a1c136-51a6-7883-a350-b1cf6000a23f",
  "name": "alice",
  "role": "writer",
  "repoMask": "raw/*",
  "createdAt": "2026-11-09T16:48:44Z"
}
```

| Field | Rule |
| --- | --- |
| `id` | set by the server; the `{id}` of `DELETE` |
| `principal` | `user:<uid>` of a live user: a creation may name the user `user:<name>` or `user:<uid>`, and the uid is stored, so that a rename does not move the binding |
| `name` | the user's name now, for people; answered, never sent |
| `role` | `reader`, `writer`, `maintainer` or `admin` ([rbac.md](spec/rbac.md) §2) |
| `repoMask` | `<format>/<repo>`, each segment a name or `*`; the format one that is served; an `admin` binding is on `*/*` only |
| `createdAt` | set by the server |

### List: `GET /api/v1/bindings`

`{"bindings": […], "next": "…"}`: a page of the bindings ordered by id ([Conventions](#conventions)),
of every principal or of `?principal=user:<name>` or `user:<uid>`; a user that does not exist
has none.

### Create: `POST /api/v1/bindings`

The body has `principal`, `role` and `repoMask`. `201` with the resource and `Location:
/api/v1/bindings/<id>`. A user that does not exist, a principal that is not a user (a deploy
token gets its repo and scopes, `anonymous` a repo's `anonymousRead`), a role or a mask that
breaks its rule are `400`; the same binding again is `409`. The binding counts from the next
request on, on every replica.

### Delete: `DELETE /api/v1/bindings/<id>`

`204`; `400` for an id that is not a positive integer, `404` for one that is no binding. The last
admin binding of an enabled user is `409`, so that somebody can still manage the server.

### Who may

The admin role only, also to list one's own bindings; anyone else is `403`, anonymous `401`.

### `lajstrom admin binding`

The same, straight against the database of the configuration: `create`, `list`, `delete`
([auth.md](auth.md#bindings)).

## Audit

### List: `GET /api/v1/audit`

The admin role reads the audit log ([operations.md](operations.md#audit)):
`{"events": […], "next": "…"}`, a page of the events newest first ([Conventions](#conventions)).
The cursor holds the place of the last event, not a position, so events written while a client
pages do not shift the pages after the first.

| Parameter | Selects |
| --- | --- |
| `repo` | `<format>/<name>` |
| `principal` | `user:<name>` (the live user of that name, by its uid), `user:<uid>`, `token:<id>`, `anonymous` |
| `action` | one action: `push`, `user.create`, … |
| `state` | `ok`, `pending`, `failed` or `unknown` ([operations.md](operations.md#audit)) |
| `since`, `until` | RFC 3339 times, `since` inclusive and `until` exclusive |

```json
{
  "id": 41,
  "at": "2026-11-09T16:48:44.981899Z",
  "principal": "user:01a1c136-5180-7b5e-95ce-428e99e68887",
  "principalName": "admin",
  "action": "binding.create",
  "subject": "user:01a1c136-51a6-7883-a350-b1cf6000a23f",
  "ip": "127.0.0.1",
  "details": {"id": 2, "role": "writer", "repoMask": "raw/*", "source": "api", "requestId": "…"},
  "state": "ok"
}
```

`state` is `ok` but for a write to the registry, whose row is `pending` while the write runs,
then `ok`, `failed` with the `error` the client was answered, or `unknown` when its writer
stopped in between. `repo`, `subject`, `digest`, `ip`, `details`, `principalName` and `error`
are left out when empty.

## Tokens

Personal access tokens belong to a user, deploy tokens to a repo; what they are, how they
authenticate and what they may do is in [auth.md](auth.md#tokens). Both resources have one
shape, and the secret only in the answer of a creation, the one time it is shown.

### The resource

```json
{
  "id": 7,
  "kind": "pat",
  "user": "alice",
  "name": "laptop",
  "scopes": ["read"],
  "expiresAt": "2026-12-07T12:00:00Z",
  "lastUsedAt": "2026-11-07T12:03:11Z",
  "createdAt": "2026-11-07T12:00:00Z",
  "revokedAt": null,
  "hashPrefix": "c68464d8"
}
```

| Field | Rule |
| --- | --- |
| `id` | set by the server; the `{id}` of `DELETE` |
| `kind` | `pat` or `deploy`, from the collection |
| `user`, `repo` | the owner: the user of a personal access token, `<format>/<name>` of the repo of a deploy token; the other is left out |
| `name` | 1 to 64 bytes of printable UTF-8, unique among the owner's live tokens (neither revoked nor expired) |
| `scopes` | `read`, `write` or both. A personal access token may have none, `[]`: its user's full rights. A deploy token needs one, and `delete` and `manage` are refused |
| `expiresAt` | a personal access token always has one, at most 365 days ahead, 90 days when the creation gives none; a deploy token may have none, `null`, and never expires |
| `lastUsedAt` | the last request with the token, to the minute; `null` before the first |
| `createdAt`, `revokedAt` | set by the server; `revokedAt` is `null` while the token is not revoked |
| `hashPrefix` | the first 8 hexadecimal characters of the sha256 of the secret, to tell tokens apart; the secret itself is never answered again |

### Create: `POST /api/v1/users/<user>/tokens`, `POST /api/v1/repos/<format>/<repo>/deploy-tokens`

`<user>` is the user's name or its uid, as in [Users](#users). The body has `name`, and
optionally `scopes` (required for a deploy token) and the expiry, `expiresAt` (RFC 3339) or
`expiresIn` (a number of days, `30d`, or a duration, `12h`), one of them at most. `201` with the
resource and `secret`, and `Location` naming the token (`/api/v1/users/<name>/tokens/<id>`, with
the user's name, the path of its `DELETE`); `400` for a field that breaks its rule, an expiry in
the past, or more than a year ahead on a personal access token (a deploy token's has no bound),
both `expiresAt` and `expiresIn`, or `delete` in the scopes of a deploy token, or `write` in
those of a group's (a write through a group is its hosted member's, which the group's token does
not reach); `404` for a user or repo that does not exist; `409` when the name is taken. The user
need not be enabled. A personal access token is created with a password only: with a personal
access token as the credential, with scopes or without, the admin's too, or with a deploy token,
`POST /api/v1/users/<user>/tokens` is `403` before the body is read ([auth.md](auth.md#rules)).

```sh
curl -s -u alice -X POST "$LAJSTROM/api/v1/users/alice/tokens" \
  -H 'Content-Type: application/json' -d '{"name": "laptop", "scopes": ["read"], "expiresIn": "30d"}'
curl -s -u admin -X POST "$LAJSTROM/api/v1/repos/raw/assets/deploy-tokens" \
  -H 'Content-Type: application/json' -d '{"name": "ci", "scopes": ["write"]}'
```

```json
{"id": 8, "kind": "deploy", "repo": "raw/assets", "name": "ci", "scopes": ["write"], "expiresAt": null, "lastUsedAt": null, "createdAt": "2026-11-07T12:00:00Z", "revokedAt": null, "hashPrefix": "876f72f1", "secret": "ljs_dep_…"}
```

### List: `GET /api/v1/users/<user>/tokens`, `GET /api/v1/repos/<format>/<repo>/deploy-tokens`

`200` with `{"tokens": [...]}` in the order of their creation, the revoked and expired ones
included, never a secret; `404` for a user or repo that does not exist.

### Revoke: `DELETE /api/v1/users/<user>/tokens/<id>`, `DELETE /api/v1/repos/<format>/<repo>/deploy-tokens/<id>`

`204`: the token is refused from the next request on, on every replica. A token revoked already
is `204` again and stays as it is. An id that is not a token of that user or repo is `404`.

### Who may

A user's own personal access tokens, or the admin's for any user; the deploy tokens of a repo with `manage` on it. A deploy token, and a
personal access token with scopes, asking for any of these is `403`, even for its own repo or
its own user: tokens never manage.

### `lajstrom admin token`

The same from the command line, straight against the index database of the configuration,
audited with the source `cli`; `--user` and `--repo` choose the kind, one of them and not both:

```sh
lajstrom admin token create (--user <name> | --repo <format>/<repo>) --name <label> [--scopes read,write] [--expires 90d|<RFC 3339>]
lajstrom admin token list (--user <name> | --repo <format>/<repo>)
lajstrom admin token revoke <id>
```

`create` prints the resource and its secret as JSON on one line, and nothing else on standard
output; `list` a table with the columns `ID`, `KIND`, `NAME`, `SCOPES`, `EXPIRES`, `LAST USED`,
`REVOKED` and `HASH`; `revoke` nothing.

## Server information: `GET /api/v1/info`

What a client needs to know before it signs in. Anyone may ask; no authorization is involved.

```sh
curl -s "$LAJSTROM/api/v1/info"
```

```json
{
  "version": "v0.1.0",
  "commit": "28bf6ca",
  "date": "2026-12-04T12:00:00Z",
  "externalURL": "https://art.example.com",
  "anonymousRead": true,
  "formats": [{"name": "apt", "artifactType": "application/vnd.lajstrom.deb.v1"}, {"name": "maven", "artifactType": "application/vnd.lajstrom.maven.v1"}],
  "authModes": ["password", "token"],
  "caller": {"principal": "anonymous", "kind": "anonymous", "name": "anonymous", "admin": false}
}
```

| Field | What |
| --- | --- |
| `version`, `commit`, `date` | the build, as `GET /-/version` and `lajstrom version` report it |
| `externalURL` | `server.externalURL` ([configuration.md](configuration.md#server)); `""` on the dev stack when it leaves it empty |
| `anonymousRead` | `auth.anonymousRead`, the server-wide switch: when `false`, no repo is readable without credentials, whatever its own `anonymousRead` |
| `formats` | the format plugins the server serves, by name, each with the artifact type of its manifests. `oci` repos need no plugin and are not listed |
| `authModes` | how a person signs in: `password`, a local user's password over Basic, and `token`, a personal access token. The OIDC login (#69) adds `oidc` |
| `caller` | the request's principal, as [`GET /api/v1/whoami`](#who-am-i-get-apiv1whoami) answers it, and `admin`: whether the request's credential reaches the admin role (a personal access token with scopes, or a deploy token, does not). Without credentials it is `anonymous`, with the name `anonymous` |

Unlike whoami, a request without credentials gets `200`. Credentials that are refused get `401`, as on every route.

## A browser on the same origin

A web UI ([lajstrom-ui](https://github.com/lajstrom/lajstrom-ui)) reads this API from the same
origin: its container serves it at `/ui/` behind the same edge as Lajstrom
([http-routing.md](spec/http-routing.md) §1 and §4).

- **Signing in.** The API sets no cookie and reads none. The UI asks for the person's name and
  password and creates a personal access token with them:
  `POST /api/v1/users/<name>/tokens` with Basic, `{"name": "<one name per sign-in>", "expiresIn": "12h"}`.
  It then sends the token as `Authorization: Bearer` on every request. Signing out revokes it:
  `DELETE /api/v1/users/<name>/tokens/<id>`, with the token itself. A person may also paste a
  personal access token they already have. Signing out then only forgets it.
- **Every sign-in is a token.** Each one appears in the person's token list and as a
  `token.create` row of the audit log, and its sign-out as `token.revoke`. A tab that is closed
  without signing out leaves its token live until it expires.
- **Where the token is kept** is the UI's choice. Any script of the origin can read it there.
  Every answer of Lajstrom forbids scripts on that origin
  ([operations.md](operations.md#what-lajstrom-answers-browsers)), so a file a writer uploaded
  as HTML cannot read the token. The UI's own pages carry the UI container's policy.
- **No browser login dialog.** The UI sends every request with `credentials: "omit"`. The Basic
  challenge of a `401` then opens no browser dialog, and no cached Basic credentials go along.
- **No CSRF token.** A browser never adds a bearer to a request by itself. The one credential it
  adds is cached Basic, and an unsafe request from another site is refused before credentials
  are checked ([operations.md](operations.md#what-lajstrom-answers-browsers)).
- **Same origin only.** No answer has an `Access-Control-*` header, so a page of another origin
  can neither read an answer nor send a request with a bearer. A UI on another origin, such as
  object storage behind a CDN, needs the allowlist of #949.

## Who am I: `GET /api/v1/whoami`

`200` with the principal of the request ([auth.md](auth.md#the-uid-and-the-name)): its canonical
form, its kind and its name; a user's canonical form is `user:<uid>`, and its `uid` is a field of
its own. Every principal may ask; no authorization is involved. Without credentials it is `401`
with the Basic challenge.

```sh
curl -s -u alice "$LAJSTROM/api/v1/whoami"
```

```json
{"principal": "user:01a1c136-51a6-7883-a350-b1cf6000a23f", "kind": "user", "name": "alice", "uid": "01a1c136-51a6-7883-a350-b1cf6000a23f"}
```

A user of an OIDC login also has `groups`, its groups that count now, those of its latest login
([auth.md](auth.md#groups-and-group-bindings)). A binding's `principal` may be `group:<name>`; its
`name` is then absent.
