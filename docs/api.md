# Management API

The management API is the REST interface to the control plane: repos now, users, tokens,
bindings and the audit log later. It lives under `/api/v1` on the same host as everything else
([http-routing.md](spec/http-routing.md) §1 and §4). The examples use the dev stack:

```sh
LAJSTROM=http://localhost:8080
```

## Conventions

- **JSON.** Request bodies are one JSON object of at most 64 KiB; a field the endpoint does not
  know is an error, so a typo never passes silently. Answers are `application/json`. Times are
  RFC 3339 in UTC.
- **Authentication** comes with #45: every endpoint is open in this version. From then on,
  reading a repo and listing it need `read` on it, changing, deleting and reindexing it need
  `manage`, and creating a repo needs the `admin` role ([rbac.md](spec/rbac.md) §2 and §3). A
  denial does not tell whether a repo exists: `401` with a Basic challenge for a caller without
  credentials, `404` for one without any access to the repo, `403` for one that can read it but
  lacks the action. The list leaves out the repos the caller may not read.
- **Audit.** Every change is one row of the audit log ([rbac.md](spec/rbac.md) §5): the
  principal, the action (`repo.create`, `repo.update`, `repo.delete`), the repo, the client
  address and details: the change, its source (`api`, `cli` or `config`) and the request id.
- **Not in this version.** The users, tokens, bindings and `whoami` endpoints of
  [http-routing.md](spec/http-routing.md) §4 come with #47, the audit log with #46. Until then
  they, and any other path under `/api/`, answer `404 NOT_FOUND`.

### Errors

Every error is answered with one envelope:

```json
{"error": {"code": "INVALID", "message": "name: \"Scratch\" must be lowercase letters and digits separated by single '.', '_' or '-', at most 64 characters"}}
```

| Status | Code | When |
| --- | --- | --- |
| 400 | `INVALID` | the request breaks a rule; the message starts with the field it is about |
| 401 | `FORBIDDEN` | credentials are needed; with `WWW-Authenticate: Basic realm="lajstrom"` (after #45) |
| 403 | `FORBIDDEN` | the caller lacks the action (after #45) |
| 404 | `NOT_FOUND` | no such repo or endpoint |
| 409 | `CONFLICT` | the repo exists, or has versions and the delete is not forced |
| 413 | `INVALID` | the body is larger than 64 KiB |
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
| `policy` | a JSON object, or `null`; its keys come with the formats (`maxFileSize`, `maven.sessionTimeout`) | `PATCH` |
| `createdAt`, `updatedAt` | set by the server | `updatedAt` on every change |

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

### Reindex: `POST /api/v1/repos/<format>/<name>/reindex`

Rebuilds the repo's index from the registry. It comes with #29: until then `501 UNAVAILABLE`,
or `404` when there is no such repo.

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
