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
  lacks the action. The list and the search leave out the repos the caller may not read.
- **Audit.** Every change is one row of the audit log ([rbac.md](spec/rbac.md) §5): the
  principal, the action (`repo.create`, `repo.update`, `repo.delete`, `repo.reindex`,
  `repo.cache.purge`, `delete` for each manifest of a version delete, `pypi.yank` for a yank
  or an unyank, `user.create`,
  `user.rename`, `user.disable`, `user.enable`, `user.password`, `user.delete`,
  `binding.create`, `binding.delete`, `token.create`, `token.revoke`), the repo, the client
  address and details: the change, its source (`api`, `cli`, `config` or `bootstrap`) and the
  request id.
- **Lists** that can be long are pages: `?limit=` (100 when absent, 1 to 1000) and `?cursor=`,
  the `next` of the page before, an opaque string; the last page has no `next`.
- **Any other method or path** under `/api/` answers `404 NOT_FOUND`, `OPTIONS` too, but the
  preflight of an origin of `server.corsOrigins` ([A web UI in a browser](#a-web-ui-in-a-browser)).

### Errors

Every error is answered with one envelope:

```json
{"error": {"code": "INVALID", "message": "name: \"Scratch\" must be lowercase letters and digits separated by single '.', '_' or '-', at most 64 characters"}}
```

| Status | Code | When |
| --- | --- | --- |
| 400 | `INVALID` | the request breaks a rule; the message starts with the field it is about |
| 401 | `UNAUTHENTICATED` | credentials are needed, or the ones sent were refused; with `WWW-Authenticate: Basic realm="lajstrom"` |
| 403 | `FORBIDDEN` | the caller lacks the action, by its bindings or by the scopes of its token, or the admin role for a system resource: creating a repo, users, bindings, the audit log, every token, the registries; or the registry refused a write for a limit or a quota of its own (a prefetch into a full Harbor project), which the message names ([operations.md](operations.md#a-registrys-limits-and-quotas)) |
| 404 | `NOT_FOUND` | no such repo, user, token or endpoint |
| 408 | `INVALID` | the body stopped: no byte came within `server.stallTimeout` ([configuration.md](configuration.md#server)); the message is fixed, and the connection is closed |
| 409 | `CONFLICT` | the repo exists, has versions and the delete is not forced, or is a member of a group; a live user has the name; a live token of the owner has the name; the binding exists; the change would leave no enabled user with an admin binding; a reindex that overlaps runs; a pointer names the version to delete, or the registry refuses to delete its manifest alone |
| 412 | `CONFLICT` | a conditional delete found the version changed since the client read it, or published after the cut-off; nothing was deleted |
| 413 | `INVALID` | the body is larger than 64 KiB |
| 429 | `UNAVAILABLE` | too many failed logins: the password is not checked until the time in `Retry-After` has passed ([auth.md](auth.md#failed-logins)) |
| 501 | `UNAVAILABLE` | a version delete on a registry that does not delete manifests (`capabilities.deleteManifest: false`); the message names the registry, and nothing was deleted |
| 502 | `UNAVAILABLE` | a version delete that the registry refused: it rejected Lajstrom's credentials or the request; the message is the status text and names the variants deleted before, the cause is in the server log with the request id |
| 503 | `UNAVAILABLE` | the index database does not answer, or a registry's circuit breaker is open; try again after `Retry-After` ([operations.md](operations.md#readiness)); or the registry took a write (a prefetch) and does not serve it yet, `Retry-After: 30`, and the message names what it does not serve after the status text ([operations.md](operations.md#an-eventually-consistent-registry)) |
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
| `format` | a format plugin the server has: `raw`, `pypi`, `apt`, `maven`, `npm`, `rpm`, `terraform`; or `oci`, the pass-through repos of the OCI facade, which need no plugin ([http-routing.md](spec/http-routing.md) §1) | never |
| `name` | unique within the format; lowercase letters and digits separated by single `.`, `_` or `-`, at most 64 characters ([manifest-profile.md](spec/manifest-profile.md) §4.1); none that ends in `blobs`, `.uploads` or `.sync`, or is `index.json`, `oci-layout`, `manifests`, `referrers` or `tags`, names that a registry's storage or routes keep for themselves ([ADR 0017](adr/0017-the-grammar-of-oci-names.md)) | never |
| `registry` | one of `registries[].name` of the configuration; none on a group, whose resource has no `registry` | never: the artifacts stay in the registry they were pushed to |
| `kind` | `hosted` (the default; may be left out), `proxy`, a repo that serves an upstream and keeps what it fetched, or `group`, one URL for several repos of its format ([proxy-and-group.md](spec/proxy-and-group.md)); `proxy` and `group` need a format whose plugin has them (npm, maven, pypi; apt a group only), or `oci`, which needs none | never |
| `anonymousRead` | readable without credentials, when `auth.anonymousRead` allows it; default `false` | `PATCH` |
| `policy` | a JSON object of the keys below, or `null` for none; none on a group, whose resource has no `policy` | `PATCH`, merged |
| `upstream` | a proxy repo only, required there: `name`, one of `upstreams[].name` of the repo's format in the [configuration](configuration.md#upstreams), and `metadataTTL` (`"10m"`, `0s` to `168h`), `negativeTTL` (`"2m"`, `0s` to `24h`), `offline` (`false`), `allow` and `deny` (at most 256 name patterns each, `*` matching any run: `"@acme/*"`) ([proxy-and-group.md](spec/proxy-and-group.md) §2.2, §3). The resource shows every key; the upstream's URL and credentials are configuration and never part of the API, and a key the server does not know is `400` | see [proxy-and-group.md](spec/proxy-and-group.md) §2.2 |
| `group` | a group only, required there: `members`, the names of 1 to 16 repos of the group's format, in the order they are read, each `hosted` or `proxy` and at most one `hosted`, and `merge` (at most 256 name patterns): the names whose versions are merged even when the hosted member holds them ([proxy-and-group.md](spec/proxy-and-group.md) §2.3, §8). The resource shows both keys. A group of `oci` repos has no `merge`, may have several hosted members, and has `paths` instead: each member's name to the patterns of the paths it serves, `<path>`, `<prefix>/*` or `*`, every member one at least and no pattern on two members (§12.1). A group of apt repos has hosted members only, any number of them, and an empty `merge`; it renders and signs its own indexes (§8.6) | `members` and `paths` by the admin role, `merge` by `manage` |
| `createdAt`, `updatedAt` | set by the server | `updatedAt` on every change |
| `permissions`, `stats`, `signingKeys` | answered by `GET`, never sent: what the request may do on the repo, and what the index holds of it ([below](#what-the-request-may-do-and-what-the-repo-holds)) | with the bindings and the content |

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

#### What the request may do, and what the repo holds

`GET /api/v1/repos` and `GET /api/v1/repos/<format>/<name>` answer three fields more, read when
the request is answered (the examples above leave them out;
[http-routing.md](spec/http-routing.md) §4, #957):

| Field | What |
| --- | --- |
| `permissions` | of `read`, `write`, `delete` and `manage` ([rbac.md](spec/rbac.md) §2), those the request's principal has on the repo, within the scopes of its credential, in that order. A group's are its own, whatever its members allow |
| `stats` | counts of the index: `packages`; `versions`, distinct versions of its packages; `manifests`, one per variant of a version; `files`, their file layers; `size`, the sum of those layers' sizes in bytes, a blob of two manifests counted twice and the metadata layer not at all, which is not what the registry stores; `lastPublishedAt`, the newest publish time of a manifest, `null` without one. Reindex rebuilds them. A group and an `oci` repo, which index no versions, have none |
| `signingKeys` | an apt or rpm repo only: its signing keys, public facts only, the active one first, then the retired ones, the most recently retired first: `fingerprint` (uppercase hexadecimal), `algorithm` (`ed25519`, `rsa2048`, `rsa3072`, `rsa4096` or `ecdsa-p256`), `createdAt`, `retiredAt` (`null` for the active one). `[]` before the first signed document makes the first key, and on a server that signs nothing ([operations.md](operations.md#repo-keys)) |

```json
{"format": "apt", "name": "ubuntu-internal", "registry": "local", "kind": "hosted", "anonymousRead": false, "policy": null,
 "createdAt": "2026-11-07T12:00:00Z", "updatedAt": "2026-11-07T12:00:00Z",
 "permissions": ["read", "write"],
 "stats": {"packages": 12, "versions": 30, "manifests": 41, "files": 41, "size": 73400320, "lastPublishedAt": "2026-11-14T09:12:03Z"},
 "signingKeys": [{"fingerprint": "6C1E0F3B…", "algorithm": "ed25519", "createdAt": "2026-11-12T08:00:00Z", "retiredAt": null},
                 {"fingerprint": "A04D7E91…", "algorithm": "rsa4096", "createdAt": "2026-11-07T12:00:00Z", "retiredAt": "2026-11-12T08:00:00Z"}]}
```

The state of a reindex is not among them: [`GET …/reindex`](#reindex-post-and-get-apiv1reposformatnamereindex)
answers it, to `manage`.

#### Policy

The keys of `policy`, each with what it is when absent:

| Key | Values | Absent | Read by |
| --- | --- | --- | --- |
| `maxFileSize` | the largest file an upload, or a blob of `/v2`, may carry, in bytes: an integer, at least 1; a larger one is `413`. On every repo, an `oci` one too; the registry's `capabilities.maxBlobSize` bounds it as well, when that is smaller ([operations.md](operations.md#large-blobs-ml-models)) | the format's default (#246): npm 512 MiB, pypi and maven 4 GiB, apt and raw 8 GiB, oci 16 GiB | raw `PUT`, apt `PUT …/upload`, pypi upload, maven `PUT` of a file, as the bytes come; a blob upload of `/v2` (`POST ?digest=`, `PATCH`, `PUT ?digest=` of a session), on an `oci` repo too: `413 SIZE_INVALID` when the size it declares would carry the blob past the limit, before anything streams, or where the bytes of the session, counted across its `PATCH`es and the closing `PUT`, pass it, and the session ends; the manifest `PUT` of `/v2` (`oras push`), whose file layers ingest checks: `413 SIZE_INVALID` naming the file. Reindex keeps what the registry has |
| `blobRedirect` | `"auto"`: a blob download, the facade's blob `GET` and a frontend's file `GET`, is a `307` to the registry's storage when the registry redirects it there (a presigned URL of its object store), so the bytes bypass Lajstrom. `"never"`: Lajstrom follows the registry's redirects and streams every blob of the repo itself, for clients that cannot follow a redirect to another host. The registry's `capabilities.blobRedirect: false` ([configuration.md](configuration.md)) does the same for all its repos | `"auto"` | every blob read ([http-routing.md](spec/http-routing.md) §2, §3) |
| `enforcePull` | `true`: `GET` and `HEAD` of a manifest through `/v2` that policy refuses are `403 DENIED`; blobs, tag lists and referrers never ([policy.md](spec/policy.md) §6.5, #101). The frontends refuse a denied version whatever this key says | `false` | the manifest reads of `/v2` |
| `quarantineNewVersions` | a proxy repo only: a duration from `1m` to `8760h` (`"72h"`) during which a version new upstream is neither listed, served nor fetched, from the upstream's publish time where its document gives one, else from the first time the repo saw it ([policy.md](spec/policy.md) §4); `"0s"` is none | none | the proxy core's documents and fetches, and every download of the repo |
| `encryption` | `{"mode", "key", "recipients"}`: whether the repo keeps its files encrypted in the registry ([ADR 0009](adr/0009-encryption-at-rest-in-the-registry.md), [manifest-profile.md](spec/manifest-profile.md) §11). `mode` `"none"` or `"at-rest"`, required in the object. `key`: the key URI of an RSA decryption key in a KMS (`awskms:///…`, `gcpkms://…/cryptoKeyVersions/<n>`, [operations.md](operations.md#encryption-at-rest)) that the repo's key is, or `""` (the default) for an RSA key of 3072 bits that Lajstrom makes and seals with the master key; it applies to the next key made, the repo's first or a rotation's. `recipients`: at most 8 public keys as PEM (`-----BEGIN PUBLIC KEY-----`), RSA of 2048 bits or more or EC P-256, P-384 or P-521, for which every new data key is wrapped too, for `skopeo` and `ctr-enc` and a recovery without Lajstrom; `at-rest` needs at least one, the recovery recipient, so that losing the master key or the KMS key does not lose the files. `at-rest` is refused (`400`, naming `policy.encryption.recipients`) without a recipient, when a repo is created with it, changed to it, or left without its last recipient, and so is a repo of the configuration's `repos` that has it; it is refused on an `oci` repo and on a group, and on a server without the master key. A change applies to the files stored after it: the files the repo holds keep their form, and every one of them stays readable | `{"mode": "none"}` | every write of the repo, a frontend's upload and a blob or manifest of `/v2`, and every read of an encrypted layer ([http-routing.md](spec/http-routing.md) §2.1, §3); reindex, which needs the repo's keys |
| `maven.sessionTimeout` | a maven repo only: how long a publication session of a deploy lives after its last request, a duration from `1m` to `24h` (`"45m"`, `"2h"`). When it is up, the files of the session are dropped and nothing is published: there is no seal that would publish a version missing files (#53). A change applies to the sessions opened or touched after it | `"30m"` | the publication sessions of maven deploys |
| `maven.maxSessionFiles` | a maven repo only: how many files the publication session of a deploy may store, an integer from 1 to 999 (a version's manifest has a layer for each and one for its metadata, and a manifest of `/v2` at most 1 000). A signature (`.asc`, `.asc.asc`) and a `.sigstore.json` count; a checksum file, of any file, and `maven-metadata.xml` with its checksums are never stored and never count. The next file is `400` naming the bound and this key. It bounds one session: a deploy of a version that exists opens a session of its own ([http-routing.md](spec/http-routing.md) §3) | `256` | the publication sessions of maven deploys |
| `maven.versionPolicy` | a maven repo only: which versions a deploy may add, `"release"` or `"snapshot"`. A release repo takes release versions and refuses a SNAPSHOT build; a snapshot repo takes the builds of SNAPSHOT versions (`1.0-SNAPSHOT`, deployed as `1.0-20261114.101112-3`: each deploy one more build, under its own tag) and refuses a release. A repo is one or the other, as Maven repositories are ([ADR 0004](adr/0004-path-first-addressing.md)): give clients a release repo and a snapshot repo. The refusal is `400` naming this key, at the first file of a deploy before it is stored, and on `/v2`. A change applies to the deploys after it: the versions the repo holds stay, served and reindexed as before ([http-routing.md](spec/http-routing.md) §3, maven) | `"release"` | the maven deploy path and every publish to a maven repo |

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
a move from another registry ([migrating.md](migrating.md)). npm only: a proxy of another format
is `400`. The dist-tags and deprecations of the upstream's packuments that the proxy kept become
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
    "removed": 1, "pending": 0, "deletedPending": 0, "degraded": 0, "referrers": 4, "attachments": 4,
    "foreign": [{"repository": "charts/demo", "tag": "0.1.0", "digest": "sha256:…", "mediaType": "application/vnd.oci.image.manifest.v1+json"}],
    "orphanReferrers": [], "orphanJournal": [], "drift": [], "split": [], "repositoryUnknown": [],
    "repairedReferrers": [], "blobMap": 0, "skipped": "", "errors": [], "truncated": false
  }
}
```

`deletedPending` counts the tags the registry still lists whose manifests were deleted through
Lajstrom less than `reconcile.graceAge` before the run, on a registry not classified `strong`:
the run does not index them again
([operations.md](operations.md#on-an-eventually-consistent-registry)). In the report of a run
of an earlier release it is 0, as is every count that release did not write, and a list it did
not write is `null`.

On an encrypted repo the report counts the rows of `blob_map` written or seen (`blobMap`), and a
run that cannot open the repo's keys skips the repo whole and says why in `skipped` (`""`
otherwise): no master key, a key whose seal does not open, no client for its KMS
([operations.md](operations.md#encryption-at-rest)).

`repairedReferrers` lists, for a run of the reconcile loop or with `verify` on a registry without
the referrers API, each `sha256-<hex>` index that Lajstrom pushed again with the referrers it
lacked: `{"repository", "subject", "index", "added": [digests]}`
([operations.md](operations.md#what-a-run-does)). `openapi.yaml` describes every field of the
report and the items of each list.

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

A page of the repo's packages, ordered by name unless `?sort=` says otherwise (`?limit=`,
`?cursor=` as in [Conventions](#conventions)), each with what a list of packages shows. It needs
`read` on the repo. The cursor holds the place of the page's last package, its name, or the
value of the sort and its name, not a position: packages published or deleted while a client
pages neither repeat nor skip the others.

```sh
curl -s "$LAJSTROM/api/v1/repos/apt/ubuntu-internal/packages?attr=lajstrom.deb.suite:jammy&sort=updated&facet=lajstrom.deb.arch"
```

```json
{
  "packages": [
    {
      "name": "acme-agent",
      "nameOci": "acme-agent",
      "createdAt": "2026-03-02T10:00:00Z",
      "updatedAt": "2026-11-02T09:00:00Z",
      "latestVersion": "2.5.0",
      "latestVariants": ["jammy/main/amd64", "jammy/main/arm64"],
      "lastPublished": "2.5.0",
      "versions": 14,
      "manifests": 28,
      "size": 48213004,
      "summary": "Acme monitoring agent",
      "attrs": {"lajstrom.deb.arch": "amd64", "lajstrom.deb.component": "main", "lajstrom.deb.suite": "jammy"},
      "pointers": []
    }
  ],
  "facets": {"lajstrom.deb.arch": [{"value": "amd64", "count": 41}, {"value": "arm64", "count": 17}]},
  "next": "…"
}
```

| Parameter | Keeps or does |
| --- | --- |
| `prefix` | the packages whose name starts with it |
| `q` | the packages whose name, or the summary of one of whose manifests, contains it; ASCII letters in either case |
| `attr=<key>:<value>` | repeatable: the packages with a manifest that has the attribute; the key is the full annotation key, as `attrs` has it. Two values of one key are alternatives; different keys hold on the same manifest |
| `sort` | `name` (the default), `updated` (the newest manifest's publish time), `versions` (how many versions) or `size` (of all its files) |
| `order` | `asc` or `desc`; `asc` for `name` and `desc` for the others when absent. A tie follows by name |
| `facet=<key>` | repeatable: adds `facets`, the values of the key in the repo's manifests, each with the number of packages that have it, in byte order, at most 1000. They count the whole repo, whatever the other parameters |

At most 16 `attr` and 16 `facet`; a key that is not `lajstrom.` followed by letters, digits,
`.`, `_` and `-` is `400`, and so is an `attr` without `:`, an unknown `sort` or `order`, and a
cursor of another sort.

- **`createdAt` and `updatedAt`** are the publish times of the package's first and newest
  manifest.
- **`latestVersion`** is the version the format's client installs by default, `null` when it
  would install none: npm the `latest` dist-tag, as the packument has it (the journal's, else the
  highest release by SemVer, else the highest version); maven the `release` of its
  `maven-metadata.xml`, the highest version in Maven's order that is not a pre-release, `null`
  when every version is one; pypi the highest version by PEP 440 that is not a pre-release and
  not yanked, as pip picks it: a pre-release only when the package has no other version, a
  yanked one only when every candidate is yanked, never a version that is not PEP 440; apt and
  rpm the highest by dpkg's and by rpm's order; raw, whose client has none, the newest by
  publish time. `latestVariants` are its variants, `[]` for a format without them and when
  `latestVersion` is `null`.
- **`lastPublished`** is the first version of [its versions list](#list-versions-get-apiv1reposformatnamepackagespackageversions):
  the newest by the publish time of its first variant; `null` without versions.
- **`versions` and `manifests`** count the package's versions and its manifests, one per variant
  of each version; **`size`** adds up the sizes of the file layers of all of them, a blob that
  two share twice.
- **`summary` and `attrs`** are those of the head manifest, the first variant by name of
  `latestVersion`, or of `lastPublished` when it is `null`: `attrs` its `lajstrom.*`
  annotations, as a version has them; `summary` one line of its metadata layer (pypi `summary`,
  deb `Description`, npm `description`, rpm `summary`), `null` when it has none: maven and raw.
  A version written before the release that added it gets its summary when `serve` starts, in
  the background, or, when its metadata layer was too large to keep, at a reindex with
  `verify`; it has none until then.
- **`pointers`** are the package's pointers that name one of its versions, as the
  [versions list](#list-versions-get-apiv1reposformatnamepackagespackageversions) shows them on
  each version, with that version: npm's dist-tags as the journal has them,
  `{"kind": "dist-tag", "name": "next", "version": "3.0.0-rc.1"}`, by kind and name; `[]` for
  the other formats.

A group has no packages: the list is empty, its facets too. The sorts other than `name`, `q`,
`attr` and `facet` read every package of the repo, and take longer on a large one.

### Get a package: `GET /api/v1/repos/<format>/<name>/packages/<package>`

One package as the head of its page shows it: its row of the list above, `ociRepository`, the
OCI repository that holds it as for [a version](#get-a-version-get-apiv1reposformatnamepackagespackageversionsversion),
and `latestMetadata`, the metadata layer of its head manifest, `null` when there is none or it
was too large to keep. It needs `read`; `404` when the repo or the package does not exist, and on
a group. `<package>` is one path segment, as for the delete below.

```json
{
  "name": "requests", "nameOci": "requests",
  "createdAt": "2026-03-02T10:00:00Z", "updatedAt": "2026-11-02T09:00:00Z",
  "latestVersion": "2.32.3", "latestVariants": ["py3-none-any", "sdist"],
  "lastPublished": "2.33.0rc1",
  "versions": 12, "manifests": 24, "size": 2481034,
  "summary": "Python HTTP for Humans.", "attrs": {}, "pointers": [],
  "ociRepository": "pypi/internal/requests",
  "latestMetadata": {"name": "requests", "version": "2.32.3", "summary": "Python HTTP for Humans."}
}
```

### List versions: `GET /api/v1/repos/<format>/<name>/packages/<package>/versions`

A page of the package's versions, newest first, with what a retention job decides on and what
a conditional delete compares: each version's variants with their manifest digests and facts,
its publish times, and the pointers that name it. It needs `read`; `404` when the repo or the
package does not exist. `<package>` is one path segment, as for the delete below.

```json
{
  "versions": [
    {
      "version": "2.1.0",
      "publishedAt": "2026-11-02T09:00:00Z",
      "lastModified": "2026-11-02T09:00:00Z",
      "variants": [
        {
          "variant": "", "tag": "2.1.0", "digest": "sha256:…", "publishedAt": "2026-11-02T09:00:00Z",
          "artifactType": "application/vnd.lajstrom.npm.v1", "attrs": {}, "degraded": false,
          "size": 18321, "fileCount": 1,
          "attachments": {"signature": 1, "sbom": 1, "scan": 0, "provenance": 1, "attestation": 0, "other": 0}
        }
      ],
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
- **`yanked`** is on a yanked pypi version only: `{"reason": "…"}`, the reason `""` when the
  yank gave none (Yank a pypi version, below). A version without it is not yanked; no other
  format has it.
- **`?without=<kind>`** keeps the variants that have no attachment of that kind, `signature`,
  `sbom`, `scan`, `provenance`, `attestation` or `other`, whatever its label (#89): the versions
  a scanner has not seen yet. Given again, a variant must lack every kind given:
  `?without=sbom&without=signature`. A version is listed with those of its variants, at their
  times, so the cursor of such a page continues the same filter. Another value is `400`. It is
  read from the index: an attachment pushed past Lajstrom counts after the next reindex.

```sh
curl -s -u "$USER:$TOKEN" "$LAJSTROM/api/v1/repos/pypi/internal/packages/requests/versions?without=sbom" |
  jq -r '.versions[] | .version as $v | .variants[] | "\($v) \(.variant) \(.digest)"'
```
- **A variant's facts** are the index's, so reindex rebuilds them, and a page reads them in one
  statement, not one per row: `artifactType`, `attrs` (the manifest's `lajstrom.*` annotations
  by their full key) and `degraded` (its metadata layer could not be read) as
  [Get a version](#get-a-version-get-apiv1reposformatnamepackagespackageversionsversion) has
  them; `size`, the sizes of its file layers added up, and `fileCount`, their number; and
  `attachments`, its
  [attachments](#attachments-of-a-version-get-apiv1reposformatnamepackagespackageversionsversionattachments)
  counted by kind, each of the six kinds there with `0`, so that `attachments.sbom` is the
  length of `…/attachments?variant=<variant>&kind=sbom` (when it is at most `limit`). A
  degraded variant has them all: they come from its manifest, not from its metadata layer.

### Get a version: `GET /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>`

One version as the page of a web UI shows it: its row of the list above, its variants' facts
included, the OCI repository that holds it, and for each variant the rest of what the index has
of its manifest: the metadata layer and the files. `?variant=` keeps one variant (`""`,
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
      "degraded": false,
      "size": 64928,
      "fileCount": 1,
      "attachments": {"signature": 0, "sbom": 0, "scan": 0, "provenance": 0, "attestation": 0, "other": 0},
      "metadata": {"name": "requests", "version": "2.32.3", "summary": "Python HTTP for Humans."},
      "indexedAt": "2026-11-02T09:00:01Z",
      "files": [
        {"name": "requests-2.32.3-py3-none-any.whl", "mediaType": "application/vnd.lajstrom.pypi.wheel.v1+zip",
         "digest": "sha256:…", "size": 64928, "attrs": {"org.opencontainers.image.title": "requests-2.32.3-py3-none-any.whl"}}
      ],
      "ociRepository": "pypi/internal/requests"
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
- **`ociRepository`** is `<format>/<name>/<package path>`, the repository of a manifest on
  `/v2`: a variant's is that of its manifest, and the version's that of its first variant (the
  variants are in the order of their names). They differ only in a package split across two
  forms of its OCI name (#642, [index-schema.md](spec/index-schema.md#invariants-and-rules)):
  a variant that reindex found at the other form is at that form.
- **Not in it:** the manifest itself and its referrers (signatures, SBOMs, attestations), which
  are live reads of the registry:
  [the manifests](#the-manifests-of-a-version-get-apiv1reposformatnamepackagespackageversionsversionmanifest)
  and [the referrers](#the-referrers-of-a-version-get-apiv1reposformatnamepackagespackageversionsversionreferrers)
  of the version (#951); the index's labelled list of the referrers is
  [the attachments of the version](#attachments-of-a-version-get-apiv1reposformatnamepackagespackageversionsversionattachments)
  (#89). Both are on `/v2` under the variant's `ociRepository` too:
  `/v2/<ociRepository>/manifests/<digest>` and `/v2/<ociRepository>/referrers/<digest>`. A
  file's download URL is not in it either: it is the format's own, as in the
  [quick starts](quickstart/).

### Attachments of a version: `GET /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>/attachments`

The signatures, SBOMs, scan reports, provenance and other attestations of a version (#89): the
referrers of its variants' manifests, one entry per attachment, each labelled with whether a key
the operator trusts signed it ([manifest-profile.md](spec/manifest-profile.md) §6). Lajstrom
runs no scanner and signs nothing: a producer pushes them to `/v2` with `cosign attest`,
`cosign sign` or `oras attach`, on a version of any format, and Lajstrom stores, lists and
labels them ([the guide](guides/sbom-and-scans.md)). It needs `read`; `404` as for the version.

| Query | Keeps |
| --- | --- |
| `variant` | the attachments of this variant (`""` for a format without variants) |
| `kind` | `signature`, `sbom`, `scan`, `provenance`, `attestation` or `other`; another value is `400` |
| `predicateType` | this predicate type, as written: `https://cyclonedx.org/bom` |
| `verified` | `true` the verified attachments, `false` the others; another value is `400` |
| `limit` | at most this many, the newest: 1 to 1000, 1000 by default; `truncated` says that there are more |

```sh
curl -s "$LAJSTROM/api/v1/repos/pypi/internal/packages/requests/versions/2.32.3/attachments?kind=sbom"
```

```json
{
  "attachments": [
    {
      "variant": "py3-none-any",
      "subject": "sha256:5c11…",
      "digest": "sha256:d06d…",
      "layer": "sha256:bbbe…",
      "mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
      "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json",
      "kind": "sbom",
      "predicateType": "https://cyclonedx.org/bom",
      "size": 859,
      "annotations": {"dev.sigstore.bundle.content": "dsse-envelope", "dev.sigstore.bundle.predicateType": "https://cyclonedx.org/bom",
                      "org.opencontainers.image.created": "2026-11-14T12:20:33Z"},
      "verification": "verified",
      "signedBy": "scanner",
      "createdAt": "2026-11-14T12:20:33Z",
      "indexedAt": "2026-11-14T12:20:34Z"
    }
  ],
  "truncated": false
}
```

- **The order and the history.** Newest first by `createdAt` (the referrer's
  `org.opencontainers.image.created`, which cosign v3 sets), those without one after the
  others, then by `digest` and `layer`. Every result is kept: a scan run again with a new
  database is one more entry, and "the latest scan per tool" is the first entry of that tool.
- **`subject`** is the variant's manifest digest, the `<digest>` of
  `/v2/<ociRepository>/referrers/<digest>`; **`digest`** is the referrer's and **`layer`** the
  attachment's layer in it. cosign's legacy manifests (`tag` `sha256-<hex>.sig`, `.att`) hold
  one attachment per layer.
- **`verification`**: `verified` when a key of `attachments.trust` or a keyless identity of
  `attachments.keyless`, trusted for the predicate type, signed it, and what it signed names the
  variant's manifest (`signedBy` is the key's or the identity's name); `untrusted` when no
  trusted key verifies a signature, or a keyless signature does not verify against the trusted
  root or comes from an identity nobody trusts; `unsigned` without one (a raw SBOM); `invalid`
  when it does not parse or a trusted signer signed something about another manifest;
  `unsupported` for a keyless signature without `attachments.keyless`, cosign's legacy keyless
  layers, notation, a message signature, or a layer above `attachments.maxBlobSize`. `reason`
  says why it is not verified. Nothing is fetched to verify: a keyless bundle's transparency log
  entry is checked from the bundle and the trusted root, a key signature's not at all.
- **`tool`, `toolVersion`, `database`, `input`** are the producer's
  `lajstrom.attachment.*` annotations (§6.2), when it set them: `oras attach --annotation` does,
  `cosign attest` cannot. Without them the first three are read from the statement's predicate:
  a `cosign-vuln` report's scanner and database, a CycloneDX BOM's tool, an SPDX document's
  `Tool:` creator.
- **Everything comes from the index**, so reindex rebuilds it. The label is decided when the
  attachment is indexed, at its push or by a reindex; after a change of `attachments.trust`, the
  next reindex of the repo labels every attachment again. A referrer pushed straight to the
  registry, past Lajstrom, is listed after the next reindex. The referrers themselves stay on
  `/v2/<ociRepository>/referrers/<digest>`.

### The manifests of a version: `GET /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>/manifest`

The manifest of each variant of a version (#951), read from the registry: the index names the
digest and the OCI repository, and the manifest is fetched by that digest and checked against
it. `?variant=` keeps one variant (`""` for a format without variants). It needs `read`; `404`
as for the version, and when the registry does not have a variant's manifest.

```sh
curl -s "$LAJSTROM/api/v1/repos/pypi/internal/packages/requests/versions/2.32.3/manifest?variant=py3-none-any"
```

```json
{
  "manifests": [
    {
      "variant": "py3-none-any",
      "digest": "sha256:5c11…",
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "size": 1268,
      "manifest": {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "artifactType": "application/vnd.lajstrom.pypi.v1",
        "config": {"mediaType": "application/vnd.oci.empty.v1+json", "digest": "sha256:44136f…", "size": 2},
        "layers": [{"mediaType": "application/vnd.lajstrom.pypi.wheel.v1+zip", "digest": "sha256:…", "size": 64928,
                    "annotations": {"org.opencontainers.image.title": "requests-2.32.3-py3-none-any.whl"}}],
        "annotations": {"lajstrom.version": "2.32.3", "lajstrom.variant": "py3-none-any", "org.opencontainers.image.created": "2026-11-02T09:00:00Z"}
      }
    }
  ]
}
```

- **One manifest per variant**, in variant order; `manifest` is the manifest as JSON, whose
  `annotations` are the manifest's own. Its exact bytes, which have the digest, are
  `/v2/<ociRepository>/manifests/<digest>`.
- **Caching.** A manifest never changes, but the version can: a delete and a new publish give
  it another digest. The answer has `Cache-Control: no-cache`, and an answer of one variant has
  `ETag: "<digest>"`: a client that sends it back in `If-None-Match` gets `304 Not Modified`,
  decided from the index without a registry read. An answer of several variants has no `ETag`;
  ask for one variant to have it.
- **The registry** is read once per variant (two requests: the digest resolved, the manifest
  fetched), at most `server.maxManifestSize` bytes each. `503 UNAVAILABLE` when it does not
  answer or its circuit breaker is open, as everywhere.

### The referrers of a version: `GET /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>/referrers`

The manifests whose `subject` is a variant's manifest (#951), as the registry lists them now:
the backend's referrers API, or the `sha256-<hex>` tag schema where it has none, or both with
`referrers.alwaysTag` ([manifest-profile.md](spec/manifest-profile.md) §6). `?variant=` keeps
one variant, `?artifactType=` the referrers of that type. It needs `read`; `404` as for the
version.

```json
{
  "referrers": [
    {
      "variant": "py3-none-any",
      "subject": "sha256:5c11…",
      "digest": "sha256:d06d…",
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json",
      "size": 787,
      "annotations": {"dev.sigstore.bundle.predicateType": "https://cyclonedx.org/bom"}
    }
  ]
}
```

- **Live, one entry per referrer manifest**, per variant in variant order and in the registry's
  order within one. A referrer pushed past Lajstrom is here at once, and in
  [the attachments](#attachments-of-a-version-get-apiv1reposformatnamepackagespackageversionsversionattachments)
  after the next reindex; the attachments are per layer, labelled with their verification, and
  read without the registry.
- A variant whose OCI repository the registry does not have has no referrers. `503
  UNAVAILABLE` when the registry does not answer.

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

### Yank a pypi version: `PUT` and `DELETE /api/v1/repos/pypi/<name>/packages/<package>/versions/<version>/yank`

Marks every file of a version as yanked in the simple index (PEP 592), or clears the mark: pip
and uv then skip the version unless a requirement pins it with `==` (`pip install pkg==1.1`),
and install it with a warning when one does. The files stay where they are and are served as
before; a yank deletes nothing. PyPI yanks in its web UI, and no client has a yank command, so
this is the way to do it in Lajstrom.

```sh
curl -s -X PUT "$LAJSTROM/api/v1/repos/pypi/internal/packages/requests/versions/2.32.0/yank" \
  -d '{"reason": "breaks proxies with HTTPS_PROXY set"}'
curl -s "$LAJSTROM/api/v1/repos/pypi/internal/packages/requests/versions/2.32.0" | jq .yanked
curl -s -X DELETE "$LAJSTROM/api/v1/repos/pypi/internal/packages/requests/versions/2.32.0/yank"
```

The body of `PUT` is optional: `{"reason": "…"}`, 1 to 4096 bytes of UTF-8. No body, `{}`, or a
`reason` that is `null` or `""` is a yank without a reason (`data-yanked=""` in the HTML index,
`"yanked": true` in the JSON one). Both answer `204` and need `write` on the repo, as `npm deprecate` does (the
`writer` role). `<package>` and `<version>` are one path segment each, as for the delete above,
and `<version>` is the version as the index spells it (`GET …/versions` lists them).

- **Idempotent.** A yank of a version yanked with the same reason, and an unyank of a version
  that is not yanked, change nothing and write no audit row. A yank of a yanked version with
  another reason replaces the reason.
- **The whole version.** Every file of the version is yanked, those uploaded after the yank too,
  as PyPI yanks releases; `?variant=` is `400`.
- **In the registry.** Each yank and unyank is a small manifest of the package's journal, under a
  tag `_j<seq>` of its OCI repository ([manifest-profile.md](spec/manifest-profile.md) §8), so
  `lajstrom reindex` restores it. A version deleted keeps its yank: the same version uploaded
  again is yanked until it is unyanked.
- **Audit.** One row per change, action `pypi.yank`, `details.op` `yank` or `unyank`, with the
  version, the journal's `seq` and the entry's digest.
- **Errors.** `404` when the repo, the package or the version does not exist; `400` on a repo
  that is not a hosted pypi repo, for a reason that is too long or not UTF-8, or for a body field
  other than `reason`; `503` with `Retry-After` when the registry does not serve the new entry
  within the visibility timeout
  ([operations.md](operations.md#an-eventually-consistent-registry)), and a retry completes it.

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

### Open deploys: `GET /api/v1/repos/<format>/<name>/sessions`

A page of the repo's open publication sessions, by id ([Conventions](#conventions)): the deploys
of a maven repo that wait for their `maven-metadata.xml`, each a session of one version opened by
its first file ([http-routing.md](spec/http-routing.md) §3, maven), whose time is not up. It
needs `manage` on the repo: the files of a deploy are not published yet. A session that
committed, was aborted or expired is not listed; the audit log has the publishes. A repo of a
format that opens no session, and a group, have none; `404` when there is no such repo.

```sh
curl -s "$LAJSTROM/api/v1/repos/maven/releases/sessions"
```

```json
{"sessions": [{
  "id": 31, "key": "com.example:lib-core:1.4.0",
  "principal": "token:8", "files": [
    {"name": "com/example/lib-core/1.4.0/lib-core-1.4.0.pom", "digest": "sha256:…", "size": 1711},
    {"name": "com/example/lib-core/1.4.0/lib-core-1.4.0.jar", "digest": "sha256:…", "size": 48213}],
  "committing": false,
  "createdAt": "2026-11-15T10:00:02Z", "updatedAt": "2026-11-15T10:00:04Z", "expiresAt": "2026-11-15T10:30:04Z"}]}
```

| Field | What |
| --- | --- |
| `key` | `<groupId>:<artifactId>:<version>`; a SNAPSHOT build's version is the build's, `1.0-20261114.101112-3` |
| `principal`, `principalName` | who opened it, `user:<uid>` or `token:<id>`, and a user's name now; only that principal adds files to it and commits it |
| `files` | the files stored so far, in the order they first came: `name`, the path below the repo, `digest` and `size` |
| `refused` | when there are any, the files that a checksum file disagreed with, by name, with the reason: the session commits once they are uploaded again |
| `committing` | `true` while a commit of the session runs |
| `updatedAt`, `expiresAt` | its last change, and when it ends with nothing published unless it commits: `maven.sessionTimeout` after its last request ([Policy](#policy)) |

No route aborts a session.

### Prefetch a proxy repo: `POST /api/v1/repos/<format>/<name>/prefetch`

Fetches versions of a list of package names into a proxy repo before a client asks for them,
each as a client's miss would fetch it ([proxy-and-group.md](spec/proxy-and-group.md) §5.1):
the upstream's document, the claim, the download, the checksum, the ingest, and an audit row
`proxy.fetch` per version with the caller as the principal (`proxy.reject` for one refused). It
is how packages move from another registry (#441). `manage` on the repo, checked before the body
is read; npm, and `oci`, which takes images (below); a proxy of another format is `400`.

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

### Search: `GET /api/v1/search?q=`

Packages by name and summary, and manifests and files by digest, over the repos the caller may
read (#956): a repo it may not read adds nothing, and the answer does not tell whether one
exists. `?format=` keeps the repos of one format; `?limit=` and `?cursor=` as in
[Conventions](#conventions), with no total. `q` is required, at most 256 bytes. Anyone may ask;
`anonymous` searches the repos it may read.

```sh
curl -s -u alice "$LAJSTROM/api/v1/search?q=sha256:3f2a9c1e"
```

```json
{
  "hits": [
    {"kind": "version", "format": "raw", "repo": "installers", "name": "acme-agent/2.5.0/acme-agent.tar.gz",
     "version": "3f2a9c1e0d4b7a65", "variant": "", "digest": "sha256:…", "matched": "file",
     "file": {"name": "acme-agent/2.5.0/acme-agent.tar.gz", "digest": "sha256:3f2a9c1e…"}},
    {"kind": "package", "format": "pypi", "repo": "internal", "name": "requests",
     "latestVersion": "2.32.3", "summary": "Python HTTP for Humans.", "updatedAt": "2026-11-02T09:00:00Z"}
  ],
  "next": "…"
}
```

The hits come in ranks, each in byte order of the repo's format and name, then the package's
name (then the version, the variant and the file):

1. when `q` is a digest's prefix, `sha256:` or not and 8 to 64 hex digits: the manifests whose
   digest starts with it, `matched: "manifest"`, then the file layers whose digest does,
   `matched: "file"` with the `file`: "which package is this blob from?";
2. the packages named `q`, then those whose name starts with it, then those whose name contains
   it, then those with a manifest whose summary contains it; ASCII letters in either case. A
   package is in the first of these it matches, once.

A group adds no hits; its members do, as repos of their own. Matching a name or a summary reads
every package of the repos searched; a digest reads an index.

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

Of an `oci` proxy repo it takes images, one reference a line, and sends them as `images`:

```sh
kubectl get pods -A -o jsonpath='{range ..containers[*]}{.image}{"\n"}{end}' > images.txt
lajstrom admin repo prefetch oci/hub --from-file images.txt --host docker.io --platform linux/amd64,linux/arm64 --report report.json
lajstrom admin repo prefetch oci/hub --from-file report.json --failed --report report.json
```

- `--platform` names the platforms of an index (repeated, or separated by commas), for every
  image whose line names none. A line may name its own after the reference,
  `registry.k8s.io/pause:3.10 linux/amd64,linux/arm64`. The images go in batches of the same
  platforms.
- `--host` keeps the references of one registry and strips it, so that a list from `kubectl` or
  `helm template` goes in as it is: with `--host docker.io`, `docker.io/library/nginx:1.27`,
  `index.docker.io/bitnami/redis:7.4` and `nginx:1.27` (Docker Hub's, without a registry) are
  sent as `library/nginx:1.27`, `bitnami/redis:7.4` and `nginx:1.27`, and
  `registry.k8s.io/pause:3.10` is skipped. The skipped references are in the report's
  `skipped`. Without `--host`, references are sent as they are.
- A line is printed per result: the status, the reference, its kind, platform and digest, the
  bytes fetched, and for a failure its status, its message and the wait of a `503`. `--failed`
  sends again the images of failed results, each with the platforms the report gives it;
  `--host` does not apply to them, since the report's references are below the repo.
- `--all-versions` is refused for an `oci` repo, and `--platform` and `--host` for the others.

## Users

Local users, OIDC users and service accounts, managed by the admin role; what a user is, its uid
and its name, is in [auth.md](auth.md#the-uid-and-the-name), and what a service account is in
[auth.md](auth.md#service-accounts). `<user>` in a path is the user's name or its uid.

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
| `kind` | `local`; `oidc`, made by its first OIDC login; `service`, a service account. Set at the creation, never changed |
| `disabled` | a disabled user's credentials and tokens are refused from the next request on |
| `createdAt` | set by the server |
| `owner` | a service account's owner: `user:<uid>` of a person, or `group:<name>`; left out for every other kind |
| `ownerName` | the name of a user owner; left out for a group |
| `ownerDisabled` | `true` while a user owner is disabled; the service account keeps working unless `auth.serviceAccounts.onOwnerDisabled` is `disable` ([Change](#change-patch-apiv1usersuser)). Left out when `false` |

A password is never answered.

```json
{
  "uid": "01a1c137-0c2e-7f43-9b6a-3b1c5d2e8a10",
  "name": "ci-release",
  "kind": "service",
  "disabled": false,
  "createdAt": "2026-11-15T10:00:00Z",
  "owner": "user:01a1c136-51a6-7883-a350-b1cf6000a23f",
  "ownerName": "alice"
}
```

### List: `GET /api/v1/users`

`{"users": […], "next": "…"}`: a page of the live users, ordered by name ([Conventions](#conventions)).
`?kind=service` lists the service accounts only, and `?kind=local` and `?kind=oidc` the users of
those kinds; another kind is `400`.

### Create: `POST /api/v1/users`

The body has `name`, `password` (8 to 1024 bytes) and optionally `admin`, `true` to bind the
user to the admin role on `*/*` in the same transaction. `201` with the resource and `Location:
/api/v1/users/<name>`; a name that a live user has is `409`.

A service account has `"kind": "service"` and `owner`, and neither `password` nor `admin`:
`user:<name>` or `user:<uid>` of a live user that is not a service account, kept as its
`user:<uid>`, or `group:<name>`, a group of the OIDC logins, which need not have members yet. An
owner that is no live user, a service account, or neither a user nor a group is `400`, and so are
a password and `admin` (bind it with [Bindings](#bindings)). `kind` is `local` when left out; an
`oidc` user is made by its first login only.

```sh
curl -s -u admin -X POST "$LAJSTROM/api/v1/users" \
  -H 'Content-Type: application/json' -d '{"name": "ci-release", "kind": "service", "owner": "group:platform"}'
```

### Get: `GET /api/v1/users/<user>`

The resource; `404` for a user that does not exist or was deleted.

### Change: `PATCH /api/v1/users/<user>`

The body has `name`, `disabled`, a service account's `owner`, or several of them. `disabled`
changes first, then the owner, then the name; each change is an audit event of its own
(`user.owner` for the owner), and a value the user has already is none. `200` with the resource.
A new name that another live user has is `409`; disabling the last enabled user with an admin
binding is `409` (a service account's admin binding does not count); an owner given to a user
that is not a service account is `400`. With `auth.serviceAccounts.onOwnerDisabled: disable`,
disabling a user disables the enabled service accounts it owns in the same transaction, a
`user.disable` event each with `owner` in its details; `"disabled": false` on the owner later
enables none of them.

### Password: `POST /api/v1/users/<user>/password`

The body is `{"password": "…"}`; `204`. The old password fails from the next request on. A
service account has no password: `400`.

### Delete: `DELETE /api/v1/users/<user>`

`204`. The user's tokens are revoked and its bindings deleted; the row is kept, under the name
`~<uid>`, so that the audit log still names it, and the name is free at once
([auth.md](auth.md#the-uid-and-the-name)). Deleting the last enabled user with an admin binding
is `409`, and so is deleting a user that owns a live service account, naming the accounts: give
them another owner, or delete them, first. A deletion is not undone.

### Who may

The admin role only, for every endpoint of users, a user's own record included (the caller
reads itself with `GET /api/v1/whoami`); anyone else is `403`, anonymous `401`. A personal access
token with scopes and a deploy token never manage users.

### `lajstrom admin user`

The same, straight against the database of the configuration: `create`, `list`, `rename`,
`disable`, `enable`, `set-password`, `delete` ([auth.md](auth.md#local-users-and-passwords)), and
for service accounts `create --service --owner`, `list --kind service` and `set-owner`
([auth.md](auth.md#service-accounts)).

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
| `role` | `reader`, `attacher`, `writer`, `maintainer` or `admin` ([rbac.md](spec/rbac.md) §2) |
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
stopped in between. `principalKind` is `service` when the principal is a service account's
`user:<uid>` ([auth.md](auth.md#service-accounts)). `repo`, `subject`, `digest`, `ip`, `details`,
`principalName`, `principalKind` and `error` are left out when empty.

## Tokens

Personal access tokens belong to a user, deploy tokens to a repo or to a mask of repos; what
they are, how they authenticate and what they may do is in [auth.md](auth.md#tokens). Both resources have one
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
| `user`, `repo`, `repoMask` | the owner, one of the three, the others left out: the user of a personal access token, `<format>/<name>` of the repo of a deploy token, or the mask of a deploy token over a mask (`oci/*`, [below](#deploy-tokens-over-a-mask)) |
| `name` | 1 to 64 bytes of printable UTF-8, unique among the owner's live tokens (neither revoked nor expired) |
| `scopes` | `read`, `write` and `attach`, one or more; `write` holds `attach` (#89). A personal access token may have none, `[]`: its user's full rights. A deploy token needs one, and `delete` and `manage` are refused |
| `expiresAt` | a personal access token of a person always has one, at most 365 days ahead, 90 days when the creation gives none; a service account's is bounded by `auth.serviceAccounts.maxTokenTTL` instead, 365 days when the creation gives none, and with the bound `0` may have none ([auth.md](auth.md#service-accounts)); a deploy token may have none, `null`, and never expires |
| `lastUsedAt` | the last request with the token, to the minute; `null` before the first |
| `createdAt`, `revokedAt` | set by the server; `revokedAt` is `null` while the token is not revoked |
| `hashPrefix` | the first 8 hexadecimal characters of the sha256 of the secret, to tell tokens apart; the secret itself is never answered again |

### Create: `POST /api/v1/users/<user>/tokens`, `POST /api/v1/repos/<format>/<repo>/deploy-tokens`

`<user>` is the user's name or its uid, as in [Users](#users). The body has `name`, and
optionally `scopes` (required for a deploy token) and the expiry, `expiresAt` (RFC 3339) or
`expiresIn` (a number of days, `30d`, or a duration, `12h`), one of them at most. `201` with the
resource and `secret`, and `Location` naming the token (`/api/v1/users/<name>/tokens/<id>`, with
the user's name, the path of its `DELETE`); `400` for a field that breaks its rule, an expiry in
the past, or more than a year ahead on a personal access token (a service account's is bounded
by `auth.serviceAccounts.maxTokenTTL`, a deploy token's not at all),
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

### Deploy tokens over a mask

A deploy token bound to a mask in the grammar of a binding's, `<format>/<repo>` with `*` for a
whole segment, reaches every repo the mask matches, those created later included
([auth.md](auth.md#a-deploy-token-over-a-mask)). They are a resource of the system: the
`admin` role only, checked before the body is read, so that any other principal gets `403`
(`anonymous` `401`) whatever the body says, a `maintainer` on `*/*` included. **Creating one
takes the admin's password** as the request's credential (decided on #911): with a personal
access token, with scopes or without, the admin's too, or with a deploy token, `POST
/api/v1/deploy-tokens` is `403` before the body is read, saying that a deploy token over a mask
is created with the admin's password. Listing and revoking take the admin role with any
credential that reaches it.

- `POST /api/v1/deploy-tokens` with `repoMask`, `name`, `scopes` (required, `read` and/or
  `write`) and the expiry as above: `201` with the resource and its secret, and `Location`
  `/api/v1/deploy-tokens/<id>`; `400` for a mask that is not two segments, a format this server
  does not serve or a repo name that breaks its rule (naming `repoMask`), and for the fields
  above; `409` when the name is taken by a live token over the same mask. The mask need not
  match a repo now.
- `GET /api/v1/deploy-tokens[?repoMask=<mask>]`: `200` with `{"tokens": [...]}`, every deploy
  token over a mask, or those over that mask as it was written (`oci/*` does not list a token
  over `*/*`), in the order of their creation, the revoked and expired ones included.
- `DELETE /api/v1/deploy-tokens/<id>`: `204`, again `204` for a token revoked already; `404`
  for an id that is not a deploy token over a mask (a repo's own is revoked under its repo).

A repo's `GET …/deploy-tokens` lists its own tokens only, not those over a mask that matches it.

```sh
curl -s -u admin -X POST "$LAJSTROM/api/v1/deploy-tokens" \
  -H 'Content-Type: application/json' -d '{"repoMask": "oci/*", "name": "talos-nodes", "scopes": ["read"]}'
```

```json
{"id": 9, "kind": "deploy", "repoMask": "oci/*", "name": "talos-nodes", "scopes": ["read"], "expiresAt": null, "lastUsedAt": null, "createdAt": "2026-11-14T12:00:00Z", "revokedAt": null, "hashPrefix": "5d1e0c42", "secret": "ljs_dep_…"}
```

### Every token: `GET /api/v1/tokens`

The admin role lists every token at once, for an access page: `{"tokens": […], "next": "…"}`,
a page by id ([Conventions](#conventions)), which is the order of their creation, of the personal
access tokens of every user, a deleted user's too, under the name it had, the deploy tokens
of every repo and those over a mask, the revoked and expired ones included. Each is the resource
above, never its secret. The filters add up:

| Parameter | Keeps |
| --- | --- |
| `kind` | `pat` or `deploy`; another value is `400` |
| `user` | the personal access tokens of the live user of that name or uid; a user that does not exist has none |
| `repo` | `<format>/<name>`: the deploy tokens of that repo, not those over a mask that matches it; a repo that does not exist has none |
| `repoMask` | the deploy tokens over that mask as it was written: `oci/*` does not list a token over `*/*` |
| `live` | `true`: those neither revoked nor expired when the request is answered; `false` or absent: all |

A personal access token that a web UI creates when a person signs in
([A browser on the same origin](#a-browser-on-the-same-origin)) is listed as any other.

```sh
curl -s -u admin "$LAJSTROM/api/v1/tokens?kind=deploy&live=true&limit=50"
```

### Who may

A user's own personal access tokens, or the admin's for any user; the deploy tokens of a repo with `manage` on it; the deploy tokens over a mask, the admin's, created with the admin's password only; every token at once, the admin's. A deploy token, and a
personal access token with scopes, asking for any of these is `403`, even for its own repo or
its own user: tokens never manage.

### `lajstrom admin token`

The same from the command line, straight against the index database of the configuration,
audited with the source `cli`; `--user`, `--repo` and `--mask` choose the kind and the owner,
exactly one of them:

```sh
lajstrom admin token create (--user <name> | --repo <format>/<repo> | --mask <format>/<repo>) --name <label> [--scopes read,write] [--expires 90d|<RFC 3339>]
lajstrom admin token list (--user <name> | --repo <format>/<repo> | --mask <format>/<repo>)
lajstrom admin token revoke <id>
```

`create` prints the resource and its secret as JSON on one line, and nothing else on standard
output; `list` a table with the columns `ID`, `KIND`, `NAME`, `SCOPES`, `EXPIRES`, `LAST USED`,
`REVOKED` and `HASH`; `revoke` nothing.

## Policy

Rules and verdicts say which versions a repo serves ([policy.md](spec/policy.md), ADR 0008). A rule is a pattern over coordinates, a verdict a signed
attestation about one manifest; the frontends leave a refused version out of their indexes and
answer its download `403`.

### A rule: `GET`, `POST /api/v1/policy/rules`, `DELETE /api/v1/policy/rules/<id>`

```json
{
  "id": 7,
  "repoMask": "pypi/proxy-pypi",
  "name": "requests",
  "version": "==2.32.0",
  "variant": "*",
  "effect": "deny",
  "reason": "CVE-2026-1234",
  "createdBy": "user:01938e1c-4b52-7a6f-9d1e-2f0c4a6b8d10",
  "createdAt": "2026-11-15T10:00:00Z",
  "expiresAt": null,
  "expired": false
}
```

`POST` takes `repoMask`, `name`, `reason`, and optionally `version` (`*`), `variant` (`*`),
`effect` (`deny`) and `expiresAt`; `201` with the rule, `400` naming a field that policy.md §1.3
refuses. A mask of one repo needs `manage` on it, any other mask the admin role. `GET` lists every
rule for the admin role, or with `?repo=<format>/<repo>` the rules whose mask matches the repo, for
whoever may read it; by id. `DELETE` answers `204`.

```sh
curl -s -u alice -X POST "$LAJSTROM/api/v1/policy/rules" -H 'Content-Type: application/json' \
  -d '{"repoMask": "pypi/proxy-pypi", "name": "requests", "version": "==2.32.0", "reason": "CVE-2026-1234"}'
```

### Verdicts: `GET /api/v1/policy/verdicts?digest=&repo=`

The verdicts the index holds of a manifest digest, of a repo, or both (one of the two is
required), in the repos the caller may read, newest first:

```json
{
  "items": [
    {
      "repo": "apt/internal", "package": "openssl", "version": "3.0.2-0ubuntu1", "variant": "amd64",
      "subject": "sha256:…", "attestation": "sha256:…", "verdict": "deny", "reason": "CVE-2026-1234",
      "source": "lajstrom:manual", "scope": "apt/internal", "validUntil": null, "signedBy": "lajstrom",
      "createdAt": "2026-11-15T10:00:00Z", "inForce": true
    }
  ]
}
```

### Deny a version: `POST` and `DELETE /api/v1/repos/<format>/<repo>/packages/<package>/versions/<version>/deny`

`POST` with `{"reason", "validUntil"}` (`validUntil` optional) signs a `deny` verdict with the
server's policy key for each variant of the version, or for the one `?variant=` names, attaches
each as a referrer of its manifest, and answers `201` with `{"items": [...]}` as above. `DELETE`
removes the verdicts of source `lajstrom:manual` of the version (their referrers), `204`. Both need
`manage` on the repo; `501 UNAVAILABLE` without the policy key (a master key or
`policy.signingKey`). On an `oci` repo `<package>` is the path below the repo and `<version>` a tag
or a digest.

### The deny list: `GET /api/v1/policy/denylist`, `GET /api/v1/policy/key`

`GET /api/v1/policy/denylist` answers the deny list of policy.md §8, the entries of the repos the
caller may read: the rules and verdicts in force that refuse, the digests and the URL paths they
refuse. The signed form is the artifact `lajstrom/policy/denylist` in the registry, which `oras
pull` reads from the registry with the registry's credentials, its greatest tag the newest list. `GET /api/v1/policy/key` answers the policy key's
public key as PEM, to anyone: what `cosign verify-attestation --key` and `cosign verify-blob
--key` take.

## Webhooks

A webhook sends the events of the repos of its mask, a version published or deleted, a repo
created or deleted, to a URL: a signed CloudEvent, again until the receiver answers `2xx`
([webhooks.md](spec/webhooks.md); running them: [operations.md](operations.md#webhooks)). The
webhooks of one repo are under the repo, for whoever may manage it; those of any mask under
`/api/v1/webhooks`, for the admin role. The secret is only in the answer of a creation or a
rotation, the one time it is shown.

### The resource

```json
{
  "id": 3,
  "mask": "raw/assets",
  "url": "https://ci.example.com/hooks/lajstrom",
  "events": ["version.published"],
  "active": true,
  "description": "rebuild the site",
  "createdBy": "user:01938e1c-4b52-7a6f-9d1e-2f0c4a6b8d10",
  "createdAt": "2026-11-14T12:00:00Z",
  "updatedAt": "2026-11-14T12:00:00Z"
}
```

| Field | Rule |
| --- | --- |
| `id` | set by the server |
| `mask` | a repo mask, as a binding's: `raw/assets`, `raw/*`, `*/payments`, `*/*`. Under a repo it is the repo, and the body may leave it out; it never changes |
| `url` | `http` or `https`, with a host, no user information, no fragment, at most 2048 bytes; `https` and a public address unless `webhooks.allowedHosts` lists its host ([configuration.md](configuration.md#webhooks)) |
| `events` | among `version.published`, `version.deleted`, `repo.created`, `repo.deleted`, `policy.rule.created`, `policy.rule.deleted`, `policy.verdict.created`, `policy.verdict.deleted`; `[]`, every type |
| `active` | `true` by default; an inactive webhook gets no new event |
| `description` | at most 256 bytes |
| `createdBy`, `createdAt`, `updatedAt` | set by the server |

### Create: `POST /api/v1/repos/<format>/<repo>/webhooks`, `POST /api/v1/webhooks`

The body has `url`, and optionally `events`, `active` and `description`; under
`/api/v1/webhooks`, `mask` too. `201` with the resource, `secret` (`whsec_` and base64, the key
of the `webhook-signature` HMAC) and `Location`; `400` naming a field that breaks its rule, a
`mask` under a repo other than the repo; `404` for a repo that does not exist; `501
UNAVAILABLE` without a master key (`keys.masterKeyEnv` or `keys.masterKeyFile`), which every
secret is encrypted with.

```sh
curl -s -u alice -X POST "$LAJSTROM/api/v1/repos/raw/assets/webhooks" \
  -H 'Content-Type: application/json' -d '{"url": "https://ci.example.com/hooks/lajstrom", "events": ["version.published"]}'
```

### List, get: `GET …/webhooks`, `GET …/webhooks/<id>`

`200` with `{"webhooks": [...]}` by id, or one resource; never a secret. Under a repo, the
webhooks whose mask is exactly the repo; under `/api/v1/webhooks`, every one. An id that is not
of the repo's mask is `404` under the repo.

### Change: `PATCH …/webhooks/<id>`

The fields given of `url`, `events`, `active` and `description` change; `mask` is `400`. `200`
with the resource as it is now.

### Delete: `DELETE …/webhooks/<id>`

`204`; the webhook and its delivery log go, and its pending deliveries are not sent.

### A new secret: `POST …/webhooks/<id>/secret`

`200` with `{"secret": "whsec_…"}`, once; the old secret signs nothing from then on, the retries
of earlier events included. `501 UNAVAILABLE` without a master key.

### Test: `POST …/webhooks/<id>/test`

Sends a `webhook.ping` to the webhook now, active or not, once, from the replica that took the
request, and answers its row of the delivery log (below): `delivered` with the status, or
`failed` with Lajstrom's words for why. `501 UNAVAILABLE` without a master key.

### The delivery log: `GET …/webhooks/<id>/deliveries[?limit=&cursor=]`

A page, newest first, `{"deliveries": [...], "next"}`:

```json
{"id": 41, "eventId": "01938e2a-…", "eventType": "version.published", "state": "pending", "attempts": 2,
 "nextAt": "2026-11-14T12:01:30Z", "lastAt": "2026-11-14T12:00:30Z", "lastStatus": 503, "lastError": "answered 503",
 "createdAt": "2026-11-14T12:00:00Z"}
```

`state` is `pending` (due at `nextAt`), `delivered` (with `deliveredAt`) or `failed`;
`lastStatus` is absent when no answer came, and `lastError` never quotes the receiver nor names
the address a host resolved to. Rows older than `webhooks.retention` are gone.

### Who may

Under `/api/v1/repos/<format>/<repo>/webhooks`, `manage` on the repo, decided before the repo is
looked up (the `401`, `404`, `403` of [rbac.md](spec/rbac.md) §3); under `/api/v1/webhooks` the
admin role, before the body is read. A deploy token and a personal access token with scopes
never manage: `403`. Every change is an audit row, `webhook.create`, `webhook.update`,
`webhook.delete` or `webhook.secret`, with the URL's host in `details.urlHost` and never the
URL.

### `lajstrom admin webhook`

The same from the command line, straight against the index database of the configuration, as
the anonymous principal with the source `cli`; `create`, `rotate` and `test` need the master key
of the configuration, and `test` sends from the host it runs on:

```sh
lajstrom admin webhook create --mask <format>/<repo> --url https://… [--events version.published,…] [--description …] [--inactive]
lajstrom admin webhook list [--mask <format>/<repo>]
lajstrom admin webhook update <id> [--url …] [--events …] [--active=false] [--description …]
lajstrom admin webhook delete <id>
lajstrom admin webhook rotate <id>
lajstrom admin webhook test <id>
lajstrom admin webhook deliveries <id> [--limit 20]
```

`create` prints the resource and its secret as JSON on one line, `rotate` the new secret, `update`
and `test` the resource and the row of the delivery log (`test` exits with status 1 when the ping
was not delivered); `list` and `deliveries` print tables.

## Registries: `GET /api/v1/registries`

The backend registries of the configuration ([configuration.md](configuration.md#registries)),
what Lajstrom found of each and whether it answers, for the admin role
([http-routing.md](spec/http-routing.md) §4, #952). `{"replica", "registries": […]}`, by name, as
the replica that answers knows them: each replica probes and pings the registries itself, and
`replica` names it as `/-/readyz` and the reindex runs do (host name and process id).

```sh
curl -s -u admin "$LAJSTROM/api/v1/registries"
```

```json
{"replica": "lajstrom-a 7", "registries": [{
  "name": "local", "type": "generic", "url": "http://zot:5000", "prefix": "lajstrom/",
  "vendor": "zot", "support": "ci",
  "capabilities": {"createOnPush": true, "referrers": true, "catalog": true, "deleteManifest": true, "immutableTags": false,
    "blobRedirect": true, "referrersAlwaysTag": false, "maxBlobSize": 0, "consistency": "strong", "consistencyBound": "0s",
    "maxNameDepth": 0, "maxNameLength": 246, "tagsFoldCase": false, "buildRoutes": false},
  "repos": ["npm/internal", "raw/assets"],
  "status": {"state": "ok", "checkedAt": "2026-11-15T10:00:20Z", "answeredAt": "2026-11-15T10:00:20Z", "latencyMs": 3,
    "message": null, "breaker": "closed"}}]}
```

| Field | What |
| --- | --- |
| `name`, `type`, `url`, `prefix` | as `registries[]` has them; Lajstrom's credentials for the registry are never answered |
| `vendor` | the product the probe detected from the registry's answer to `GET /v2/` and its host, as [`lajstrom registry probe`](registries/README.md#measuring-a-registry-lajstrom-registry-probe) names it: `distribution`, `zot`, `gitlab`, `ghcr`, `harbor`, `nexus`, `gitea`, `forgejo`, `jfrog`, `quay`, `dockerhub`, `ecr`, or `unknown`; `null` before a probe of the replica answered |
| `support` | the vendor's tier in this release ([registries/README.md](registries/README.md#support-tiers)): `ci`, `manual`, `hosted` or `untested`; `null` with `vendor` |
| `capabilities` | what ingest and the facade go by: the probe's findings, the vendor table's and `registries[].capabilities` over them ([configuration.md](configuration.md#registry-capabilities)); `consistencyBound` a duration. `null` before a probe of the replica succeeded |
| `repos` | the repos on the registry, `<format>/<name>`, by format and name |
| `status.checkedAt`, `status.answeredAt` | the replica's last ping of the registry (`GET /v2/` with Lajstrom's credentials, every 20 seconds), and the last one answered `200`; `null` before the first |
| `status.latencyMs`, `status.message` | how long the last ping took, and its error when it failed, as the provider words it: it names neither a credential nor the instance prefix; `null` when it did not fail |
| `status.breaker` | the registry's circuit breaker: `closed`, `open` (requests fail at once, `503`) or `half-open` (trials go through) ([operations.md](operations.md#readiness)) |
| `status.state` | `down`: no ping was answered `200` for a minute, which `/-/readyz` counts as not ready, or the breaker is open; `degraded`: the last ping failed or the breaker is half-open; `ok` otherwise |

The answer sends no request to a registry. Anyone else is `403`, anonymous `401`.

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
  "login": {"tokenLifetime": 43200},
  "caller": {"principal": "anonymous", "kind": "anonymous", "name": "anonymous", "admin": false}
}
```

| Field | What |
| --- | --- |
| `version`, `commit`, `date` | the build, as `GET /-/version` and `lajstrom version` report it |
| `externalURL` | `server.externalURL` ([configuration.md](configuration.md#server)); `""` on the dev stack when it leaves it empty |
| `anonymousRead` | `auth.anonymousRead`, the server-wide switch: when `false`, no repo is readable without credentials, whatever its own `anonymousRead` |
| `formats` | the format plugins the server serves, by name, each with the artifact type of its manifests. `oci` repos need no plugin and are not listed |
| `authModes` | how a person signs in: `password`, a local user's password, over Basic and to [`POST /api/v1/login`](#sign-in-post-apiv1login); `token`, a personal access token; and `oidc` while the OIDC login (#69) is on |
| `login` | `{"tokenLifetime": 43200}`: the server has [`POST /api/v1/login`](#sign-in-post-apiv1login), whose token, bound to a DPoP key, lives that many seconds (`auth.login.tokenLifetime`); with `oidc` in `authModes`, `/-/oidc/login` takes `return_to`, `code_challenge` and `dpop_jkt` for a web UI. A server before it answers no `login` |
| `caller` | the request's principal, as [`GET /api/v1/whoami`](#who-am-i-get-apiv1whoami) answers it, and `admin`: whether the request's credential reaches the admin role (a personal access token with scopes, or a deploy token, does not). Without credentials it is `anonymous`, with the name `anonymous` |

Unlike whoami, a request without credentials gets `200`. Credentials that are refused get `401`, as on every route.

## A web UI in a browser

A web UI ([lajstrom-ui](https://github.com/lajstrom/lajstrom-ui)) reads this API from the same
origin, its container serving it at `/ui/` behind the same edge as Lajstrom, or from an origin
of `server.corsOrigins`, object storage behind a CDN for one
([http-routing.md](spec/http-routing.md) §1 and §4).

- **Signing in.** The API sets no cookie and reads none. The UI asks for the person's name and
  password and signs in with them: [`POST /api/v1/login`](#sign-in-post-apiv1login) answers a
  personal access token bound to a key of the browser ([DPoP](#the-dpop-bound-token)), which
  expires after `auth.login.tokenLifetime`. The UI then sends the token as
  `Authorization: DPoP` with a new proof of the key on every request. A server without that
  route takes `POST /api/v1/users/<name>/tokens` with Basic,
  `{"name": "<one name per sign-in>", "expiresIn": "12h"}`, and that token goes as
  `Authorization: Bearer`. Signing out revokes the token:
  `DELETE /api/v1/users/<name>/tokens/<id>`, with the token itself. A person may also paste a
  personal access token they already have, sent as a bearer. Signing out then only forgets it.
- **Every sign-in is a token.** Each one appears in the person's token list and as a
  `token.create` row of the audit log, and its sign-out as `token.revoke`. A tab that is closed
  without signing out leaves its token live until it expires.
- **Where the token is kept** is the UI's choice. Any script of the origin can read it there.
  Every answer of Lajstrom forbids scripts on that origin
  ([operations.md](operations.md#what-lajstrom-answers-browsers)), so a file a writer uploaded
  as HTML cannot read the token. The UI's own pages carry the UI container's policy. A bound
  token read out of the browser is of no use without its key, which the UI keeps
  non-extractable; a script on the origin can still use the key while the page is open
  ([threat model](security/threat-model.md) B-5).
- **No browser login dialog.** The UI sends every request with `credentials: "omit"`. The Basic
  challenge of a `401` then opens no browser dialog, and no cached Basic credentials go along.
- **No CSRF token.** A browser never adds a bearer to a request by itself. The one credential it
  adds is cached Basic, and an unsafe request from another site is refused before credentials
  are checked ([operations.md](operations.md#what-lajstrom-answers-browsers)), but from an origin
  of `server.corsOrigins`, which the operator lists as the UI's own.
- **Another origin.** With `server.corsOrigins` empty, the default, no answer has an
  `Access-Control-*` header, so a page of another origin can neither read an answer nor send a
  request with a bearer. An origin it lists ([configuration.md](configuration.md#server)) gets,
  on `/api/` and `/-/version`, `Access-Control-Allow-Origin` with itself on every answer, with
  `Access-Control-Expose-Headers: Date, Retry-After, WWW-Authenticate, X-Request-Id` (`Date`
  for a UI whose clock is off to sign its proofs by the server's), and a preflight answered
  `204` for `Authorization`, `Content-Type` and `DPoP`, the proof of a sign-in (#1170), cached
  10 minutes. There is never `Access-Control-Allow-Credentials`: the UI sends its bearer
  with `credentials: "omit"`. The links to files and the login are navigations and need no CORS.

```sh
curl -si -X OPTIONS -H 'Origin: https://ui.example.com' -H 'Access-Control-Request-Method: GET' \
  -H 'Access-Control-Request-Headers: authorization,dpop' "$LAJSTROM/api/v1/repos"
```

```http
HTTP/1.1 204 No Content
Access-Control-Allow-Headers: Authorization, Content-Type, DPoP
Access-Control-Allow-Methods: GET, HEAD, POST, PUT, PATCH, DELETE
Access-Control-Allow-Origin: https://ui.example.com
Access-Control-Max-Age: 600
Vary: Origin
```

## Sign in: `POST /api/v1/login`

A web UI signs in with the person's name and password and a DPoP proof (RFC 9449) of a key of
the browser, and gets a personal access token bound to that key ([rbac.md](spec/rbac.md) §10).
The request takes no credential: its `Authorization` header is ignored, so a UI that still holds
a revoked token signs in all the same.

```sh
curl -s -X POST -H 'Content-Type: application/json' -H "DPoP: $PROOF" \
  -d '{"username": "alice", "password": "…"}' "$LAJSTROM/api/v1/login"
```

```json
{"id": 41, "kind": "pat", "user": "alice", "name": "login 2026-11-15T09:30:00Z 3f9a", "scopes": [],
 "expiresAt": "2026-11-15T21:30:00Z", "lastUsedAt": null, "createdAt": "2026-11-15T09:30:00Z",
 "revokedAt": null, "hashPrefix": "5c1d0e7a", "dpopJkt": "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I",
 "secret": "ljs_pat_…", "tokenType": "DPoP"}
```

`201`, with `Location: /api/v1/users/alice/tokens/41`: the token as
[`POST /api/v1/users/<user>/tokens`](#create-post-apiv1usersusertokens-post-apiv1reposformatrepodeploy-tokens)
answers it, its secret shown once, with `dpopJkt`, the thumbprint of the proof's key, and
`tokenType: "DPoP"`. It has no scopes, and expires after `auth.login.tokenLifetime`
([configuration.md](configuration.md#auth)), 12 hours by default and at most 24; there is no
refresh, and the UI signs in again. Signing out is `DELETE /api/v1/users/alice/tokens/41` with
the token itself. The token is in alice's token list, and its creation is a `token.create` row by
alice, `createdBy` `password`, with `dpopJkt`.

`$PROOF` is a JWT that the UI signs with its key: the header `{"typ": "dpop+jwt", "alg":
"ES256", "jwk": <the public P-256 key>}`, the claims `{"jti": <random>, "htm": "POST", "htu":
"<server.externalURL>/api/v1/login", "iat": <now>}`. A login without a proof, or whose proof is
refused or was used, is `400` saying why, before the password is checked.

The password is checked as HTTP Basic checks it, with the same
[throttles of failed logins](auth.md#failed-logins): `401` whatever the reason, an unknown user,
a disabled one, one without a password (an OIDC user), a wrong password or a token in its place;
`429` with `Retry-After` for a throttled account or client address. A body that is neither
`{"username", "password"}` nor `{"code", "codeVerifier"}` is `400`.

**With OIDC** ([auth.md](auth.md#a-web-ui)), while `auth.oidc` is on, the UI signs a person in
through the IdP and gets the same answer:

1. It makes a PKCE verifier (RFC 7636: 43 to 128 characters of `A-Z a-z 0-9 - . _ ~`), keeps it
   in the tab, and sends the person to
   `/-/oidc/login?return_to=<a page of the UI>&code_challenge=<base64url(sha256(verifier))>&code_challenge_method=S256&dpop_jkt=<thumbprint>`,
   the thumbprint (RFC 7638) of the DPoP key it signs in with (RFC 9449 §10). `return_to` must
   be on the origin of `server.externalURL` or of `server.corsOrigins`, without a fragment;
   anything else, and one of the three without the others, is a `400` page.
2. After the IdP's login, the callback answers `302` to `<return_to>#code=<code>`, instead of
   the page that shows a token.
3. The UI posts the code and the verifier, with a proof of that key:

```sh
curl -s -X POST -H 'Content-Type: application/json' -H "DPoP: $PROOF" \
  -d '{"code": "<code>", "codeVerifier": "<verifier>"}' "$LAJSTROM/api/v1/login"
```

The answer is `201` with the token, named `oidc <time> <4 hex>` and bound to the key, which
expires after the shorter of `auth.login.tokenLifetime` and `auth.oidc.tokenLifetime`; its
`token.create` row says `createdBy` `oidc`. A code is redeemed once, within the 10 minutes of
its login, with the verifier of its challenge and a proof of the key of its `dpop_jkt` only: a
code that is unknown, used, expired, of another verifier or of another key is `400`, and a wrong
verifier or key uses it up. A user disabled since the login is `401`.

### The DPoP-bound token

The token of a sign-in works on `/api/` only, as `Authorization: DPoP <token>` with a new proof
of its key in the header `DPoP` on every request ([http-routing.md](spec/http-routing.md) §4):

```http
GET /api/v1/whoami HTTP/1.1
Authorization: DPoP ljs_pat_…
DPoP: eyJ0eXAiOiJkcG9wK2p3dCIsImFsZyI6IkVTMjU2IiwiandrIjp7…
```

The proof's claims are `htm`, the request's method; `htu`, `server.externalURL` and the
request's path, without the query; `iat`, within 5 minutes of the server's clock; `ath`, the
base64url SHA-256 of the token; and a new `jti`: a proof is used once, on any replica. The only
algorithm is ES256, and the server sends no `DPoP-Nonce`. A refused proof is `401` with
`WWW-Authenticate: DPoP error="invalid_dpop_proof", error_description="…", algs="ES256"` and
the reason in the message; a token that is unknown, revoked, expired, or bound to another key is
`401` with `DPoP error="invalid_token"`. The token is refused as `Authorization: Bearer`, in
Basic credentials, and everywhere but `/api/`: docker, pip, npm and `/v2/token` cannot use it.

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
([auth.md](auth.md#groups-and-group-bindings)). A binding's `principal` may be `group:<name>`, or a
`cert:` or `k8s:` principal or pattern ([auth.md](auth.md#node-and-workload-identity)); its
`name` is then absent.
