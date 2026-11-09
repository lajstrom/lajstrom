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
  `Authorization: Bearer` with a personal access token, on every route ([auth.md](auth.md)):
  a request with credentials runs as the user (`user:<uid>`, for a personal access token
  too) or the deploy token (`token:<id>`, Basic `deploy:<token>`), one without as `anonymous`,
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
  principal, the action (`repo.create`, `repo.update`, `repo.delete`, `user.create`,
  `user.rename`, `user.disable`, `user.enable`, `user.password`, `user.delete`,
  `binding.create`, `binding.delete`, `token.create`, `token.revoke`), the repo, the client address and details: the change, its source (`api`,
  `cli` or `config`) and the request id.
- **Lists** that can be long are pages: `?limit=` (100 when absent, 1 to 1000) and `?cursor=`,
  the `next` of the page before, an opaque string; the last page has no `next`.
- **Any other path** under `/api/` answers `404 NOT_FOUND`.

### Errors

Every error is answered with one envelope:

```json
{"error": {"code": "INVALID", "message": "name: \"Scratch\" must be lowercase letters and digits separated by single '.', '_' or '-', at most 64 characters"}}
```

| Status | Code | When |
| --- | --- | --- |
| 400 | `INVALID` | the request breaks a rule; the message starts with the field it is about |
| 401 | `FORBIDDEN` | credentials are needed, or the ones sent were refused; with `WWW-Authenticate: Basic realm="lajstrom"` |
| 403 | `FORBIDDEN` | the caller lacks the action: now by the scopes of its token, after #45 by its bindings too |
| 404 | `NOT_FOUND` | no such repo, user, token or endpoint |
| 409 | `CONFLICT` | the repo exists, or has versions and the delete is not forced; a live token of the owner has the name |
| 413 | `INVALID` | the body is larger than 64 KiB |
| 429 | `UNAVAILABLE` | too many failed logins: the password is not checked until the time in `Retry-After` has passed ([auth.md](auth.md#failed-logins)) |
| 501 | `UNAVAILABLE` | the endpoint comes with a later issue; the message names it |
| 500 | `INTERNAL` | anything else; the message is the status text, the cause is in the server log with the request id (`X-Request-Id`) |

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
| `format` | a format plugin the server has: `raw`, `pypi`, `apt`, `maven`; or `oci`, the pass-through repos of the OCI facade, which need no plugin ([http-routing.md](spec/http-routing.md) §1) | never |
| `name` | unique within the format; lowercase letters and digits separated by single `.`, `_` or `-`, at most 64 characters ([manifest-profile.md](spec/manifest-profile.md) §4.1) | never |
| `registry` | one of `registries[].name` of the configuration | never: the artifacts stay in the registry they were pushed to |
| `kind` | `hosted`, the only kind in this version; may be left out | never |
| `anonymousRead` | readable without credentials, when `auth.anonymousRead` allows it; default `false` | `PATCH` |
| `policy` | a JSON object, or `null`; its keys come with the features that read them (`maxFileSize`, `blobRedirect`, `maven.sessionTimeout`), below | `PATCH` |
| `createdAt`, `updatedAt` | set by the server | `updatedAt` on every change |

The keys of `policy` that the server reads:

| Key | Values | Read by |
| --- | --- | --- |
| `maxFileSize` | the largest file an upload may carry, in bytes; a larger one is `413`. No limit when absent | raw `PUT`, apt `PUT …/upload` |
| `blobRedirect` | `"auto"` (also when absent): a blob download, the facade's blob `GET` and a frontend's file `GET`, is a `307` to the registry's storage when the registry redirects it there (a presigned URL of its object store), so the bytes bypass Lajstrom. `"never"`: Lajstrom follows the registry's redirects and streams every blob of the repo itself, for clients that cannot follow a redirect to another host. The registry's `capabilities.blobRedirect: false` ([configuration.md](configuration.md)) does the same for all its repos | every blob read ([http-routing.md](spec/http-routing.md) §2, §3) |

The server does not check the keys yet: a `maxFileSize` that is not a non-negative integer makes an upload `500`, and a `blobRedirect` that is not `"never"` is read as `"auto"`.

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
`policy`. `201` with the resource and `Location: /api/v1/repos/<format>/<name>`; `400` for a
field that breaks its rule, `409` when the repo exists.

```sh
curl -s -X POST "$LAJSTROM/api/v1/repos" \
  -H 'Content-Type: application/json' \
  -d '{"format": "raw", "name": "scratch", "registry": "local"}'
```

### Get: `GET /api/v1/repos/<format>/<name>`

`200` with the resource, `404` when there is no such repo.

```sh
curl -s "$LAJSTROM/api/v1/repos/raw/scratch"
```

### Change: `PATCH /api/v1/repos/<format>/<name>`

The body has the fields to change, `anonymousRead` and `policy`; a field left out stays as it
is, and `"policy": null` removes the policy. `format`, `name`, `registry` and `kind` cannot
change and are `400`: moving a repo to another registry would orphan its artifacts. `200` with
the resource as it is now; a body that changes nothing changes nothing, not even `updatedAt`.

```sh
curl -s -X PATCH "$LAJSTROM/api/v1/repos/raw/scratch" \
  -H 'Content-Type: application/json' \
  -d '{"anonymousRead": true, "policy": {"maxFileSize": 104857600}}'
```

### Delete: `DELETE /api/v1/repos/<format>/<name>`

`204`. A repo that has versions is refused with `409` and their number
(`repo raw/scratch has 3 versions; delete them or pass force=true`), unless `?force=true`. Then
every row of the index that refers to the repo goes with it: packages, versions and files, the
rendered indexes, deploy tokens, signing keys, publication sessions and tag claims. The
artifacts stay in the registry, untouched;
the audit row says how many versions the repo had. Role bindings name repos by mask and stay.
`404` when there is no such repo.

```sh
curl -s -X DELETE "$LAJSTROM/api/v1/repos/raw/scratch?force=true"
```

### Reindex: `POST` and `GET /api/v1/repos/<format>/<name>/reindex`

`POST` starts a reindex of the repo on the replica that received it
([operations.md](operations.md#reindex) says what a run does) and answers `202` with the run as
soon as it holds the lock, before it reads the registry. The body is optional:
`{"prune": false, "verify": false}`, the defaults of `lajstrom reindex --repo`; `prune` removes the
rows of manifests that are gone from the registry, `verify` reads every manifest again. A run that
overlaps, of this repo or of every repo, on any replica or in the CLI, is `409 CONFLICT` naming
where it runs and since when. The start is an audit row `repo.reindex`. `GET` answers the latest
run that covered the repo, its own or a run of every repo, with the repo's part of the report:

```json
{
  "id": 12, "state": "done", "source": "api", "principal": "user:alice", "replica": "lajstrom-a 7",
  "allRepos": false, "prune": true, "verify": false,
  "startedAt": "2026-11-09T10:00:00Z", "heartbeatAt": "2026-11-09T10:00:30Z", "finishedAt": "2026-11-09T10:00:41Z",
  "report": {
    "repo": "raw/assets", "ociRepositories": 3, "manifests": 120, "added": 2, "updated": 0, "unchanged": 117,
    "removed": 1, "degraded": 0, "referrers": 4,
    "foreign": [{"repository": "charts/demo", "tag": "0.1.0", "digest": "sha256:…", "mediaType": "application/vnd.oci.image.manifest.v1+json"}],
    "orphanReferrers": [], "drift": [], "errors": [], "truncated": false
  }
}
```

`state` is `running`, `done` (it went to the end, whatever the report lists), `failed` (the run's
own error is in `error`) or `abandoned` (its heartbeat stopped, a later start closed it);
`report` is `null` while it runs. Both need `manage` on the repo; `404` when there is no such
repo, or, for `GET`, when no run has covered it.

```sh
curl -s -X POST "$LAJSTROM/api/v1/repos/raw/assets/reindex" -d '{"prune": true}'
curl -s "$LAJSTROM/api/v1/repos/raw/assets/reindex" | jq '.state, .report.removed'
```

### Delete a version: `DELETE /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>`

Removes a bad version everywhere it is visible ([http-routing.md](spec/http-routing.md) §4):
every variant of the version, or with `?variant=` the one variant (`?variant=` with no value is
the variant of a format that has none). Each variant is one manifest, deleted in the registry as
`DELETE /v2/<format>/<name>/<path>/manifests/<digest>` deletes it: its referrers first
(signatures, attestations, SBOMs, cosign's `sha256-<hex>.sig` tags, the tag-schema index), then
the manifest, then its rows in the index, the package's with its last version, and the repo's
rendered indexes (apt `Packages` and `Release`, the simple index, `maven-metadata.xml`), which the
next `GET` renders without it. Each manifest writes one audit row, action `delete`, with the
number of referrers in `details.referrers`. Blobs are left to the registry's garbage collection.

`<package>` is the name in the format's spelling, one path segment: a raw path escapes its `/`
as `%2F`, and a reverse proxy in front must pass it on encoded. `204`. It needs `delete` on the
repo (the `maintainer` role); `404` when the repo, the package, the version or the variant does
not exist; `501 UNAVAILABLE` naming the registry when it cannot delete manifests
(`capabilities.deleteManifest: false`, GHCR, distribution with deletes off), and nothing
changed; `409 CONFLICT` when the registry refuses to delete this manifest alone. The variants
deleted before an error stay deleted.

```sh
curl -s -X DELETE "$LAJSTROM/api/v1/repos/pypi/libs/packages/requests/versions/2.32.0?variant=sdist"
curl -s -X DELETE "$LAJSTROM/api/v1/repos/raw/assets/packages/docs%2Freadme.txt/versions/0123456789abcdef"
```

## `lajstrom admin repo`

The same operations from the command line, straight against the index database of the
configuration, without HTTP; run `lajstrom migrate` first. A change is audited with the source
`cli`.

```sh
lajstrom admin repo create raw/scratch --registry local [--anonymous-read] [--policy '{"maxFileSize": 104857600}']
lajstrom admin repo list [--format raw] [--json]
lajstrom admin repo delete raw/scratch [--force]
```

`create` prints the resource as JSON on one line, `list` a table (`--json`: the document of
`GET /api/v1/repos`), `delete` nothing. An error goes to standard error with exit status 1.

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

`204`; `404` for an id that is not a binding. The last admin binding of an enabled user is
`409`, so that somebody can still manage the server.

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
  "details": {"id": 2, "role": "writer", "repoMask": "raw/*", "source": "api", "requestId": "…"}
}
```

`repo`, `subject`, `digest`, `ip`, `details` and `principalName` are left out when empty.

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

### Create: `POST /api/v1/users/<name>/tokens`, `POST /api/v1/repos/<format>/<repo>/deploy-tokens`

The body has `name`, and optionally `scopes` (required for a deploy token) and the expiry,
`expiresAt` (RFC 3339) or `expiresIn` (a number of days, `30d`, or a duration, `12h`), one of
them at most. `201` with the resource and `secret`, and `Location` naming the token
(`/api/v1/users/<name>/tokens/<id>`, the path of its `DELETE`); `400` for a field that breaks its
rule, `expiresAt` over a year ahead or in the past, both `expiresAt` and `expiresIn`, or
`delete` in the scopes of a deploy token; `404` for a user or repo that does not exist; `409`
when the name is taken. The user need not be enabled.

```sh
curl -s -u alice -X POST "$LAJSTROM/api/v1/users/alice/tokens" \
  -H 'Content-Type: application/json' -d '{"name": "laptop", "scopes": ["read"], "expiresIn": "30d"}'
curl -s -u admin -X POST "$LAJSTROM/api/v1/repos/raw/assets/deploy-tokens" \
  -H 'Content-Type: application/json' -d '{"name": "ci", "scopes": ["write"]}'
```

```json
{"id": 8, "kind": "deploy", "repo": "raw/assets", "name": "ci", "scopes": ["write"], "expiresAt": null, "lastUsedAt": null, "createdAt": "2026-11-07T12:00:00Z", "revokedAt": null, "hashPrefix": "876f72f1", "secret": "ljs_dep_…"}
```

### List: `GET /api/v1/users/<name>/tokens`, `GET /api/v1/repos/<format>/<repo>/deploy-tokens`

`200` with `{"tokens": [...]}` in the order of their creation, the revoked and expired ones
included, never a secret; `404` for a user or repo that does not exist.

### Revoke: `DELETE /api/v1/users/<name>/tokens/<id>`, `DELETE /api/v1/repos/<format>/<repo>/deploy-tokens/<id>`

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

## Who am I: `GET /api/v1/whoami`

`200` with the principal of the request ([auth.md](auth.md#whoami)): its canonical form, its
kind and its name. Every principal may ask; no authorization is involved. Without credentials it
is `401` with the Basic challenge.

```sh
curl -s -u alice "$LAJSTROM/api/v1/whoami"
```

```json
{"principal": "user:alice", "kind": "user", "name": "alice"}
```
