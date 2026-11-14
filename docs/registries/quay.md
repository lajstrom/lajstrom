# Project Quay

What Project Quay, the open-source upstream of Red Hat Quay, answered on 2026-11-12 as an OCI
backend, measured with `lajstrom registry probe`, `curl`, `oras`, the integration tests and two
end-to-end scenarios. The lead asked to try Quay locally; no issue named it. A repository is
`<namespace>/<name>`, the namespace a user or an organisation, and the name may hold `/`
(`FEATURE_EXTENDED_REPOSITORY_NAMES`, on by default). Lajstrom's manifests are taken and
published; deletes, the catalog, ranges and an upload continued on another replica fail, for
reasons of Quay's, all but the ranges ones the provider can work around ([What the provider
must do differently](#what-the-provider-must-do-differently)). The [summary](README.md#project-quay)
is in the README; this page has the details. Quay's master refuses new repositories named
`…/build` since 2026-11-05: [The build rule](#the-build-rule) measures it before and after, and
since #662 Lajstrom refuses such a new package on Quay before it pushes anything.

## Version tested

- Project Quay **3.18.2** (released 2026-10-20), `quay.io/projectquay/quay:3.18.2`, index
  `sha256:4456201ca6bfdc0bbe78ca7cfe262887df41147ddbf452dd8d51f8d330a41acb` (linux/amd64,
  arm64, ppc64le, s390x; 288 MiB to pull for arm64, 1.28 GB on disk). The image calls itself
  `Quay v3.18.1-nightly-202610201956` (`GET /config`): its version string was not bumped.
- Its own Postgres, `postgres:17.11-alpine` (the dev stack's image) with `pg_trgm`, its own
  Redis, `redis:7-alpine` (7.4.12), blobs on local storage in a volume. The Quay container
  used about 6 GiB of memory.
- On macOS with OrbStack, arm64; the tests with Go 1.27.4 at d44b112e.

## Run it

[deploy/compose/quay/compose.yaml](../../deploy/compose/quay/compose.yaml), a compose project
of its own (`lajstrom-quay`) beside the dev stack, its
[config.yaml](../../deploy/compose/quay/config.yaml), and
[quay.sh](../../deploy/compose/quay/quay.sh), which sets it up through the REST API, never the
UI:

```sh
deploy/compose/quay/quay.sh up      # Quay on http://localhost:5012, about 20 s with a set-up volume
deploy/compose/quay/quay.sh stop    # the containers; the volumes stay
deploy/compose/quay/quay.sh down    # with the volumes and the token files
```

| What | How |
| --- | --- |
| Configuration | `config.yaml`: `SETUP_COMPLETE: true` (no config editor), `SERVER_HOSTNAME: localhost:5012`, from which Quay builds its bearer realm, Postgres, Redis, local storage, Database authentication, `FEATURE_USER_INITIALIZE`, `FEATURE_PROXY_CACHE` and `FEATURE_IMMUTABLE_TAGS` on, mail, builds and the security scanner off |
| Postgres | `pg_trgm` created by an init script before Quay's first migration |
| Superuser | `quayadmin`, the first user, by `POST /api/v1/user/initialize` with `access_token: true`; its OAuth token, given once, in `deploy/compose/quay/.work/admin.token` (ignored by git) |
| Organisation | `lajstrom-it`, by `POST /api/v1/organization/` |
| Robot | `lajstrom-it+lajstrom`, by `PUT /api/v1/organization/lajstrom-it/robots/lajstrom`, in a team `lajstrom` with the role `admin`; its token in `.work/robot.token` |
| Lajstrom | `prefix: lajstrom-it/`, `auth.username: lajstrom-it+lajstrom`, the robot's token in the variable `passwordEnv` names; `server.maxManifestSize: 1048576`; no `capabilities`, which the vendor table sets since #803 ([Configuration](#configuration)) |

The team's role matters. With `creator` the robot may create repositories and administers what
it creates, but a repository that does not exist is 401 `UNAUTHORIZED` to it, not 404
`NAME_UNKNOWN`: the provider reads that as bad credentials (`TestIntegration/stat manifest`
failed so). With `admin` it is 404. The healthcheck asks `/_internal_ping` on the container's
port, not `/health/instance`, which asks `SERVER_HOSTNAME`, the published port.
`LAJSTROM_QUAY_PORT` moves the published port; `SERVER_HOSTNAME` must move with it.

## The row

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Quay | 3.18.2, local storage, `prefix: lajstrom-it/` (an organisation) | Bearer from `/v2/auth`, 1 h; Basic on `/v2/` 401 | yes, at the token request for `push`, inside an existing organisation, at any depth | yes, with the `artifactType` filter, but **400 for a subject that does not exist** and no `OCI-Subject`: oras-go writes the tag schema too | **none without credentials**: 200 with an empty list; with a token, creation order, 100 a page, `last` ignored | yes, with its tags, **but not through oras-go**: the token endpoint refuses `delete` | none by default; a policy: 409 `TAG_IMMUTABLE` | none on local storage | **fail**: every delete, the catalog, ranges, a session across replicas; publishing works |

`lajstrom registry probe` ran against it (2026-11-12, `--registry-prefix lajstrom-it/
--max-blob-size 256MiB --max-manifest-size 32MiB -v`). Its cells that hold:

```text
Routing words in a name           ok: 20 blob HEADs in each name, all 200; GET 200
Referrers API                     yes, with the artifactType filter; no tag schema
OCI-Subject on a push w/ subject  no
Tag list order and pagination     lexical, byte by byte; n/last/Link
Immutable tags                    none: a tag re-points with 201, and the manifest it named is deleted
Blob redirect                     none: the bytes
Range and Accept-Ranges           none: 200 with the whole blob; no Accept-Ranges
Largest blob measured             256 MiB in one PUT and in one PATCH; no limit met
Manifest size limit               4 MiB is an HTML 413
Lajstrom's manifest profile       accepted, stored as pushed
Upload: chunk out of order        an empty 416
Upload: chunk size limit          none met: 256 MiB in one PATCH
Upload: session status (GET)      204 with Range: bytes=0-3, the PATCH said 0-2
What the probe finds              referrers=false catalog=true   (vendor=distribution)
Overrides needed                  referrers: true; consistency: eventual
```

And the cells it got wrong, each for a reason of this page: "Create on push" *no* and
"Read-after-write lag" *catalog not within 2m0s* (its catalog reads go without credentials and
see nothing, [Catalog](#catalog)); "Delete …", "Upload: cancel", "mount", "wrong digest" and
"session across replicas" *not measured* (the token endpoint refuses `delete`,
[Tokens](#tokens-and-the-delete-action); the probe stops the uploads row at the cancel). Those
were measured by curl below. Its run left
`lajstrom-it/lajstrom-probe-6ae5e398/` behind, the deletes refused.

## What the provider must do differently

1. **Ask `push` where oras-go asks `delete`**: done by [#801](https://github.com/lajstrom/lajstrom/issues/801).
   Quay's token endpoint knows the actions `pull`, `push` and `*`; `delete` in a scope is
   `400 INVALID_REQUEST` "Unable to decode repository and actions", and oras-go puts it in the
   scope of every `DELETE`. Every delete through Lajstrom failed: a version, a referrer, the
   facade's `DELETE` (400 `BLOB_UPLOAD_INVALID` to the client, a code about uploads), raw's
   (502), the upload cancel. With a `push` token Quay deletes ([Delete](#delete)). Once the
   probe named Quay (item 3), the provider's token requests ask `push` where oras-go asks
   `delete`, and `pull,push` for its `pull,delete`, the scope Quay's own challenge to a
   `DELETE` names; the token is cached under the scope oras-go asked. Other registries are
   asked as oras-go asks. A fallback for every registry, `push` again after a token
   endpoint's 400 to `delete`, was not chosen: it would ask token services the table does not
   know for an action oras-go did not, which some grant apart from `delete`; it would read every
   400 of a token endpoint as this one (the build rule's `NAME_INVALID` is one too) and send a
   second token request; and a `push` scope creates the repository it names on Quay. That last
   holds here too: a delete in a repository that does not exist creates it, empty and private;
   Lajstrom deletes what its index lists.
2. **Do not trust an anonymous catalog** ([#802](https://github.com/lajstrom/lajstrom/issues/802)).
   `_catalog` without credentials is `200 {"repositories":[]}`, no challenge, so oras-go never
   authenticates it, the probe says `catalog=true`, and `ListRepositories` returns nothing:
   reindex from an empty index finds no repository. Until then the vendor table sets
   `catalog: false` for Quay (item 3; reindex reads `oci_repos`, which passed:
   `TestIntegrationNoCatalog/config`).
3. **Name the vendor**: done by [#803](https://github.com/lajstrom/lajstrom/issues/803). Quay was
   `vendor=distribution`, and so `consistency: strong`, which it is not: it caches a tag list
   for 120 s and a catalog page for 60 s ([Consistency](#consistency)). The provider tells it by
   `X-Sparse-Manifest-Support` on `GET /v2/`, which Quay sends on the 401 and on the 200, and
   quay.io by its host too, and calls it `vendor=quay`. Its entry in the vendor table sets
   `catalog: false` (item 2), `referrers: false` and `consistency: eventual` within `130s`.
   Items 1 and 7 hang on it. quay.io answered `GET /v2/`, the anonymous catalog, the token
   endpoint (`delete` 400) and the referrers API (200, and 400 for a subject that does not
   exist) alike on 2026-11-12; its cache times were not measured.

   `referrers: false`, the tag schema, and not the API that Quay has, which the issue asked
   for: run through Lajstrom (with #801's `push` token), the API answered wrongly where the tag
   schema did not. A client's referrers of a deleted version got Quay's 400 `MANIFEST_INVALID`
   (`TestIntegrationDelete`), and a signature deleted through Lajstrom stayed in the list
   (`TestIntegrationDeleteReferrer`), since Quay keeps a referrer pushed by digest
   ([Delete](#delete)); the API is cached 60 s besides. With the tag schema the index is a
   tag, read fresh and written again without the deleted referrer: `TestIntegrationDeleteReferrer`
   passes, and the referrers of a deleted version are an empty list.
4. **Keep its own offset of an upload session** ([#804](https://github.com/lajstrom/lajstrom/issues/804)).
   `GET` of a session answers one byte ahead (`Range: bytes=0-5` after `0-4`), as Forgejo
   does ([forgejo.md](forgejo.md#uploads)); a replica that continues from it sends its chunk one
   byte late and gets 416.
5. Expect no ranges: a `Range` is 200 with the whole blob and no `Accept-Ranges`, as on Gitea
   ([gitea.md](gitea.md#what-the-provider-must-do-differently), item 3). RFC 9110 allows it;
   the facade and `/raw/` pass it on. Nothing to change unless Lajstrom serves ranges itself.
6. Expect a manifest limit of 1 MiB: one byte more is an HTML `413` from Quay's nginx.
   Lajstrom's `server.maxManifestSize` (4 MiB by default) should be 1 MiB in front of Quay; the
   vendor table has no capability for a manifest limit.
7. **Refuse a new name that ends in a build route**: done by [#662](https://github.com/lajstrom/lajstrom/issues/662).
   Quay's master refuses to create a repository whose name ends in `build`, `build/<x>` or
   `build/<x>/logs|status`, at the token request, 400 `NAME_INVALID`, after the client's upload
   began. The vendor table says `buildRoutes: true` for Quay, and every write path refuses a
   package the index does not have whose OCI name ends so, `400` naming the rule, before anything
   goes to Quay; a package the index has keeps publishing ([What Lajstrom does](#what-lajstrom-does-662)).

Nothing else: the token service, the robot, create on push at any depth, the profile's media
types, the tag list, mounts, the monolithic `POST`, the streamed `PATCH` and the digest check
work as the provider expects.

## Tokens and the delete action

`GET /v2/` answers `401`, `WWW-Authenticate: Bearer realm="http://localhost:5012/v2/auth",
service="localhost:5012"`, `Docker-Distribution-API-Version: registry/2.0`,
`X-Sparse-Manifest-Support: false`, and a body `true`. The token is an RS256 JWT for an hour:

```text
GET /v2/auth?service=localhost:5012&scope=repository:lajstrom-it/raw/a/b:pull,push   (robot)   200
    {"iss":"quay","aud":"localhost:5012","exp"-"nbf":3600,"sub":"lajstrom-it+lajstrom",
     "access":[{"type":"repository","name":"lajstrom-it/raw/a/b","actions":["push","pull"]}],
     "context":{"entity_kind":"robot",…}}
… scope=…:delete, …:pull,push,delete                                                  400 INVALID_REQUEST "Unable to decode repository and actions: repository:lajstrom-it/raw/a/b:delete"
… scope=…:*, …:pull,push,*                                                            200
… scope=registry:catalog:*                                                            400 INVALID_REQUEST
… no scope                                                                            200, "access":[]
… scope=repository:quayadmin/x:pull,push   (another namespace)                       200, "actions":[]
… wrong password                                                                      401
… anonymous                                                                           200, "sub":"(anonymous)", "actions":[]
GET /v2/ with Basic (the robot's token)                                               401
```

A user's password and `$oauthtoken` with an OAuth token work in place of the robot on the
token endpoint. Quay's 401 to a `DELETE` names `scope="repository:<name>:pull,push"`; oras-go
adds its `delete` to it. Quay's `DELETE` handlers want write permission, which `push` grants
(`require_repo_write` in `endpoints/v2/manifest.py`). Since #801 the provider asks `push` in its
place for Quay (item 1).

## Organisations, robots and create on push

Quay creates a repository at the token request: a `push` scope for a name that does not exist,
in an organisation where the token's user may create repositories, creates it, private
(`CREATE_PRIVATE_REPO_ON_PUSH`); a namespace is never created (`CREATE_NAMESPACE_ON_PUSH`
off). The robot pushed below its organisation at any depth
(`lajstrom-it/maven/repo/org/example/group/artifact/sub/1/2/3`); `quayadmin/x`, `nosuchorg/x`
and a name of one component were 401 to it. Lajstrom's names need `prefix: <organisation>/`.
A repository the robot cannot read is 401 `UNAUTHORIZED` to `tags/list`, a manifest `HEAD` and
the referrers API, also when it does not exist, unless the robot is an administrator of the
organisation (the team role `admin`): then 404 `NAME_UNKNOWN`.

## Media types

The profile is taken and read back as pushed: `oras push --artifact-type
application/vnd.lajstrom.raw.v1` with a layer `application/vnd.lajstrom.raw.file.v1`, a config
`application/vnd.lajstrom.deb.metadata.v1+json`, and the probe's raw profile manifest, all
201. 3.18.2 has `ALLOWED_OCI_ARTIFACT_TYPES` in its configuration, which did not refuse any of
them. A manifest `GET` without an OCI type in `Accept` (none, `*/*`, Docker's v2 type alone)
is 404 `MANIFEST_UNKNOWN`, as on Harbor and distribution.

## Referrers

```text
GET referrers/<subject>                        200 application/vnd.oci.image.index.v1+json, the referrers
GET referrers/<subject>?artifactType=…         200 OCI-Filters-Applied: artifactType, the one
GET referrers/sha256:0000…  (no such manifest)  400 MANIFEST_INVALID "manifest invalid"
GET referrers/… in a repository that does not exist   404 NAME_UNKNOWN (401 for a robot that is not an admin)
PUT a manifest with subject                    201, no OCI-Subject; also when the subject does not exist
oras attach twice                              the tag sha256-<subject> appears: oras-go writes the tag schema
DELETE the subject by digest                   202; its referrer stays, 200 by digest
```

The probe's own referrers request asks a repository that does not exist, so it cannot tell
(`referrers=false`); the provider then decides per repository, and with no `OCI-Subject` writes
and lists the tag schema (`TestIntegration`: "referrers listed through the sha256-<digest> tag
schema"). Quay keeps a manifest pushed by digest under a hidden tag: without expiry when it has
a `subject`, for 6 hours otherwise (`PUSH_TEMP_TAG_EXPIRATION_SEC`, `endpoints/v2/manifest.py`
and `data/model/oci/manifest.py`; read, not run). Lajstrom tags every version.

## Catalog

```text
GET /v2/_catalog                    (no credentials)          200 {"repositories":[]}
GET /v2/_catalog?n=2                (a token asked without scope)
    200 Link: </v2/_catalog?n=2&next_page=gAAAAABq9bkz…>; rel="next"   ["lajstrom-it/raw/a/b","lajstrom-it/raw/oras/hello"]
GET /v2/_catalog?n=2&last=lajstrom-it/raw/oras/hello          the same first page: last is read as a page token
GET /v2/_catalog?n=1000                                        100 of 165, and a Link: 100 a page at most
three new repositories z, a, m                                 listed z, a, m, at the end: creation order
```

The list is what the token's user may read: the robot sees its organisation, not `imm/` of
another. Quay caches a page for 60 s. Its REST API lists an organisation's repositories,
`GET /api/v1/repository?namespace=lajstrom-it`.

## Tags

The probe's seven tags list lexically, byte by byte (`1.10`, `1.2`, `A`, `Z`, `_x`, `a`, `b`),
with `n`/`last` and a relative `Link`. The tag list is cached for 120 s per repository, `last`
and `n`:

```text
GET tags/list of a new repository   {"tags":[]}
PUT a as v1, PUT b as v2            201 201
GET tags/list                       {"tags":[]}       still, the cached answer
GET tags/list?n=1                   {"tags":["v1"]}   another cache key
```

A tag of a repository that does not exist is 404 `NAME_UNKNOWN` (401 for a robot that is not
an admin).

## Consistency

A manifest by digest and by tag, a blob, and a tag list never read before were seen at once
(the probe's three writes). A tag list read before a write is not, for up to 120 s
([Tags](#tags)), and a catalog page for up to 60 s; the referrers list is cached 60 s too
(`data/cache/cache_key.py`). `consistency: eventual` with `consistencyBound: 130s` describes
it, which the vendor table says since [#803](https://github.com/lajstrom/lajstrom/issues/803):
every publish reads its tag, its blobs and, for a referrer, its subject's referrers (the
tag-schema index, a tag) back, for up to 130 s (260 s for a referrer) where
`ingest.visibilityTimeout` is shorter.

## Delete

With a `pull,push` token (oras-go alone cannot get one for a `DELETE`; the provider asks it since
#801, item 1):

```text
a under v1 and t2; DELETE manifests/sha256:<a>     202   v1 404, t2 404, a 404; its layer 200
b under v2 and t3; DELETE manifests/v2             202   v2 404, t3 200, b 200
DELETE manifests/t3, b's last tag                  202   b by digest 404: a manifest goes with its last tag
DELETE blobs/sha256:<layer>                        405 UNSUPPORTED
the subject of a referrer, by digest               202; the referrer stays
a referrer pushed by digest, DELETE by digest      404 MANIFEST_UNKNOWN; it stays, 200 by digest, listed by the API
```

The last row was measured by curl on 2026-11-12, for #803: the referrer is held by its hidden tag
([Referrers](#referrers)), which a `DELETE` does not remove. The provider reads the 404 as gone
already, so a version deleted through Lajstrom leaves its referrers in Quay; with the tag schema
Lajstrom no longer lists them (item 3).

`/v2` has no repository delete; the REST API has (`DELETE /api/v1/repository/<ns>/<name>`, not
tried). A manifest that no live tag holds is not served by digest; Quay's collector removes it
after the organisation's time machine (two weeks by default, `DEFAULT_TAG_EXPIRATION`); not
waited for.

## Immutable tags

None by default: a tag re-points with 201, and the manifest it named is no longer served by
digest (the probe). With `FEATURE_IMMUTABLE_TAGS` and an organisation policy
`{"tagPattern": "^(?!sha256-).*$"}` (`POST /api/v1/organization/imm/immutabilitypolicy/`):

```text
PUT a as v1                         201
PUT a as v1 again (the same bytes)  409 TAG_IMMUTABLE "tag is immutable and cannot be overwritten"
PUT b as v1                         409 TAG_IMMUTABLE
PUT a, then b as sha256-0000        201 201: the pattern leaves the tag schema out
DELETE a by digest, DELETE tag v1   409 TAG_IMMUTABLE each
```

The provider reads 409 as `ErrConflict`. Even the same manifest again under its tag is refused;
ingest resolves a tag before it pushes, so its idempotent repeat does not push (read). A pattern
must leave `sha256-*` out, as the one above does, or the tag schema of a second referrer cannot
be written; under such a policy Lajstrom cannot delete a version.

## Blob reads, ranges, sizes

- `GET` of a blob: 200, `Content-Type: application/octet-stream`, `Content-Length`,
  `Docker-Content-Digest`, `Cache-Control: max-age=31536000`; no `Accept-Ranges`; `Range:
  bytes=0-9` and a range past the end are 200 with the whole blob. No redirect on local
  storage; S3 storage was not run.
- 256 MiB in one `PUT` and in one `PATCH`: 201 (the probe); `MAXIMUM_LAYER_SIZE` is 20 GB by
  default. Larger was not tried: the machine's disk was nearly full.
- Manifests: 1 MiB is 201, 1 MiB + 1 byte is 413 with nginx's HTML page (curl, a manifest
  padded with an annotation).

## Uploads

```text
POST blobs/uploads/                    202  Location: http://localhost:5012/v2/<name>/blobs/uploads/<id> (absolute), Range: bytes=0--1
PATCH Content-Range 0-4                202  Range: 0-4
GET (and HEAD) the session             204  Range: bytes=0-5     one byte ahead, with a bytes= prefix
PATCH Content-Range 7-11 (out of order) 416  Range: 0-5, an empty text/html body
PATCH 5-9                              202  Range: 0-9;   GET: Range: bytes=0-10
PUT ?digest=                           201
PATCH without Content-Range, twice     202 202: streamed chunks after the first write are taken
POST ?digest= with the bytes           201: a monolithic upload in one request
PUT ?digest= of other bytes            400 BLOB_UPLOAD_INVALID; the session is gone (404)
the empty blob                         201;   HEAD 200
DELETE the session (a push token)      204;   GET and PATCH afterwards 404 BLOB_UPLOAD_UNKNOWN
POST ?mount=&from= (pull on from)      201, and the blob 200 in the new repository
```

## The build rule

An `oci` proxy repo on a Quay backend cannot hold Docker Hub's `<namespace>/build` images that it
did not hold before: since #662 their pull is `400 NAME_INVALID` naming the rule, before the
upstream is asked ([proxy-and-group.md](../spec/proxy-and-group.md) §11.2,
[What Lajstrom does](#what-lajstrom-does-662)); on a Quay with the rule the push of their first
manifest would be refused at the token request.

Quay's commit [77b369a](https://github.com/quay/quay/commit/77b369a411) (quay/quay#7858,
PROJQUAY-11582, on master since 2026-11-05, in no release) refuses a **new** repository whose
name, without the namespace, has a component `build` after a non-empty prefix followed by
nothing, by one component, or by one component and `logs` or `status`: `foo/build`,
`foo/build/bar`, `foo/build/bar/logs`. `build/foo`, `foo/building/bar` and `foo/build/bar/extra`
stay allowed, and so does `build` alone (`paketobuildpacks/build` in an organisation
`paketobuildpacks`). It applies to the token request of a push, the REST API's create, a proxy
cache and organisation mirroring; existing repositories are exempt. The commit's reason is the
web UI, where `/repository/org/foo/build/bar` can mean build `bar` of `org/foo`. Reported
upstream as [quay/quay#8070](https://github.com/quay/quay/issues/8070).

**Before and after.** BEFORE is the released 3.18.2 above. AFTER is the same image with the
five Python files the commit changes (`util/names.py`, `endpoints/v2/v2auth.py`,
`endpoints/api/repository.py`, `data/registry_model/registry_proxy_model.py`,
`workers/repomirrorworker/__init__.py`) replaced by 3.18.2's own files with the commit's hunks
applied (one import hunk's context adapted: 3.18.2 lacks a neighbouring import), bind-mounted
read-only, on the same volumes; the commit's web UI change was not applied. It is not a master
build: no published image has the commit (quay.io's newest are builds of the `redhat-*`
branches; `5.1-unstable`, of the branch that follows master, was built on 2026-10-29), and a
build of master's Dockerfile did not fit the machine (4.6 GiB free; its stages need
`ubi9/go-toolset`, 389 MiB compressed, two Node stages with their `node_modules`, a Python
build stage and a 1.3 GB result). The code the scenarios meet was compared with master's:
`util/names.py` is the same file, `v2auth.py` differs in one unrelated call, and the proxy
model's `lookup_repository` and `fetch_manifest_by_tagname` of `endpoints/v2/manifest.py` are
the same, so the 500 below is master's too (read, not run on master).

Every scenario ran on both, 2026-11-12, with `quayadmin` in organisations of their own
(`rule-before`, `rule-after`; proxy caches of docker.io `hub-before`, `hub-after`):

| Scenario | Request | BEFORE: 3.18.2 | AFTER: 3.18.2 + 77b369a |
| --- | --- | --- | --- |
| 1. Proxy cache of docker.io: `paketobuildpacks/build:1.3.97-tiny` (71 M pulls) | `GET /v2/hub-*/paketobuildpacks/build/manifests/1.3.97-tiny`, docker pull's first request | 200, a Docker v2 manifest; the repository is created | **500 Internal Server Error** (HTML); Quay's log: `AttributeError: 'NoneType' object has no attribute 'id'` in `get_repo_tag` |
| the same, `tensorflow/build:latest-python3.11` (25 M pulls) | `GET …/manifests/latest-python3.11` | 200, an OCI index of 2 | **500** |
| the same, `cnbs/build:0.0.1-rc.1` (57 MB, a small real `*/build`) | `docker pull --platform linux/amd64 localhost:5012/hub-*/cnbs/build:0.0.1-rc.1` | pulled, `sha256:50edbf75…` | **fails**: `unexpected status from HEAD request to …/manifests/0.0.1-rc.1: 500 Internal Server Error` |
| the same, control `library/hello-world:latest` | `GET …/manifests/latest` | 200, an index of 20 | 200, an index of 20 |
| 2. Push to a new `foo/build` | `GET /v2/auth?…&scope=repository:rule-*/foo/build:pull,push`, then `oras push` | 200 `push,pull`; pushed | **400 `NAME_INVALID` "Repository name conflicts with a build route"**; oras: `Error response from registry: name invalid: Repository name conflicts with a build route` |
| `foo/build/bar`, `paketobuildpacks/build`, `tensorflow/build`, `foo/build/bar/logs`, `foo/build/bar/status` | the same | pushed | **400 `NAME_INVALID`**, the same text |
| controls `build/foo`, `foo/building/bar`, `foo/build/bar/extra` | the same | pushed | pushed |
| Lajstrom's names `oci/hub/paketobuildpacks/build`, `pypi/internal/build`, `npm/internal/netlify/build`, `raw/assets/build/app.tar.gz`, `raw/assets/ci/build` | the same | pushed | **400 `NAME_INVALID`** |
| The REST API's create of `api/build` | `POST /api/v1/repository` | 201 | **400** `invalid_request` "Repository name conflicts with a build route" |
| 3. `rule-before/foo/build`, `…/foo/build/bar`, `…/paketobuildpacks/build`, created on BEFORE | `oras manifest fetch :v1`, the token for push, `oras push :v2` | (created) | 200; 200 `push,pull`; pushed `v2` |
| `hub-before/paketobuildpacks/build`, `hub-before/cnbs/build`, cached on BEFORE | `GET …/manifests/1.3.97-tiny`; `docker pull` | (created) | 200; pulled |

So, once a release carries the commit:

- **Mirroring Docker Hub's `*/build` images through a Quay proxy cache breaks**, and not with
  a clean error: `docker pull` gets a 500. Docker Hub has 197 public `<namespace>/build`
  repositories (quay/quay#8070 lists them); only those cached before the upgrade keep working.
- **On a Quay backend, Lajstrom cannot create a package whose OCI name ends so**, below any
  prefix (the namespace is the organisation; the rule reads the rest): the PyPI package `build`
  (PyPA's build frontend, `pypi/<repo>/build`); npm's `build` and every `@<scope>/build`
  (`@netlify/build` is `npm/<repo>/netlify/build`); a raw file directly in a directory
  `build/` (`build/app.tar.gz`) or named `build` (`ci/build`); a Maven artifactId `build`; a
  Debian package `build`; and an `oci` repository mirroring any of the images above
  (`oci/<repo>/paketobuildpacks/build`, #442). A groupId such as `com.android.tools.build` is one
  component and is not hit. Through Lajstrom the token request's 400 is a refused request, as
  in item 1 (inference: not run through Lajstrom on AFTER). Packages created before the upgrade
  keep working.

The transcripts of both runs were kept outside the repository, for a comment on
quay/quay#8070; [Commands](#commands) has the requests.

### Measured again (2026-11-14, #662)

The same two builds, BEFORE and AFTER, on a copy of the compose stack under another project
name and port, by a push token per name (`GET /v2/auth?…&scope=repository:<org>/<name>:pull,push`
as `quayadmin`) in the organisations `rule-before` and `rule-after`. BEFORE granted all 38 names
and created each repository. AFTER refused 19 with `400 NAME_INVALID` "Repository name conflicts
with a build route" and granted the 19 others:

| Names below the namespace | AFTER | Why |
| --- | --- | --- |
| `foo/build`, `foo/bar/build`, `foo/build/build`, `a/b/c/d/build` | refused | a component `build`, not the first, and nothing after it |
| `foo/build/bar`, `foo/bar/build/baz`, `foo/build/logs`, `foo/build/status` | refused | one component after it |
| `foo/build/bar/logs`, `foo/build/bar/status` | refused | one component, then `logs` or `status` |
| `pypi/internal/build`, `npm/internal/netlify/build`, `npm/internal/build/cli` (npm's scope `@build`), `raw/assets/build/app.tar.gz`, `raw/assets/ci/build`, `raw/build/readme.txt` and `raw/build/logs` (a repo named `build`), `maven/releases/com.example/build`, `oci/hub/paketobuildpacks/build` | refused | Lajstrom's names, as above |
| `build`, `build/foo`, `build/foo/bar` | granted | `build` is the first component |
| `foo/building/bar`, `foo/mybuild`, `foo/builds`, `foo/rebuild/bar`, `foo/build-x`, `foo/build.x`, `foo/build_x`, `foo/x_build`, `foo/logs` | granted | no component is exactly `build` |
| `foo/build/bar/extra`, `foo/build/bar/baz`, `foo/build/bar/logs/x`, `foo/build/logs/x`, `raw/assets/build/linux/app.tar.gz` | granted | two components or more after it, but for one and `logs` or `status` |
| `raw/assets/buil_64/app.tar.gz`, `npm/internal/netlify/buil_64` | granted | the form an escape would write |

The names created on BEFORE in a third organisation, `rule-exist`, were granted again on AFTER,
38 of 38, and an image pushed to `rule-exist/foo/build` on BEFORE was fetched, and a new tag
pushed to it, on AFTER. A pull token for a new `foo/build/pull` is 200 and creates nothing:
`rule-after` held only the 19 granted names. The REST API's create of `api/build` is 400
`invalid_request` with the same message. The robot `lajstrom-it+lajstrom` is refused
`lajstrom-it/pypi/x/build` the same way, and granted `lajstrom-it/pypi/x/requests`. v3.18.3,
released on 2026-11-13, does not have the rule: its `util/names.py` has no
`conflicts_with_build_route`.

### What Lajstrom does (#662)

The vendor table says `capabilities.buildRoutes: true` for `vendor=quay`, and the start says so
on its capability line (`buildRoutes=true`). Every write path asks `Capabilities.CheckNew` of a
name its index does not have: an upload of a frontend, a streamed upload, a maven session, a
`/v2` manifest `PUT` and the start of a `/v2` blob upload are `400` (`NAME_INVALID` on `/v2`)
naming the rule, a proxy repo's file `403` with a `proxy.reject` row before the download, and an
`oci` proxy's pull `400 NAME_INVALID` before the upstream is asked. Nothing goes to Quay. A
package the index has (`index.PackageAt`, any earlier form) or an `oci` repository in `oci_repos`
keeps taking versions, as Quay keeps a repository that exists. The rule is not escaped: the
profile writes the same names on every registry, and ADR 0017 counts Quay's `build` among the
limits of a capability.

The default holds on every Quay, also on 3.18.x, which creates these names: nothing but the token
of a push tells a Quay with the rule from one without, and on one without that token creates the
repository. `registries[].capabilities.buildRoutes: false` lets a Quay that creates them take new
ones.

Run through `lajstrom serve` against the stack (a raw repo `assets`, `prefix: lajstrom-it/`, the
robot, SQLite; the bodies are a few bytes):

```text
BEFORE, buildRoutes: false   PUT /raw/assets/build/app.tar          201: Quay creates the name
BEFORE, the vendor table's   PUT /raw/assets/build/other.tar        400 "ingest: the OCI repository raw/assets/build/other.tar ends in a build
                                                                    route of the registry, …/build, …/build/<x> or …/build/<x>/logs|status,
                                                                    and the registry creates no new repository named so (capabilities.buildRoutes)"
                             PUT /raw/assets/build/app.tar, 2.0     201: the index has the package
                             PUT /raw/assets/build/linux/app.tar    201: no build route
AFTER, the vendor table's    PUT /raw/assets/build/app.tar, 3.0     201: the index has it, and Quay keeps the repository
                             PUT /raw/assets/build/new.tar          400, the same message; nothing reaches Quay
AFTER, buildRoutes: false    PUT /raw/assets/build/new.tar          502 Bad Gateway; the log: "generic: start upload raw/assets/build/new.tar:
                                                                    registry quay refused the request: 400 NAME_INVALID"
```

## Our tests against it

`LAJSTROM_TEST_REGISTRY=localhost:5012`, `LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom-it+lajstrom`,
the robot's token in `LAJSTROM_TEST_REGISTRY_PASSWORD`, `LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/`,
`LAJSTROM_TEST_POSTGRES_DSN` of the dev stack, `go test -race -count=1 -tags integration -skip
'TestIntegrationRun$' ./...` (the probe test, `TestIntegrationRun`, ran once: it hit its two-minute
bound in the lag row). Every failure, by cause:

| Cause | Tests that fail |
| --- | --- |
| The token endpoint refuses `delete` (item 1) | reindex `TestIntegrationFormats` (apt, pypi, maven; SQLite and Postgres), `TestIntegrationRepairReferrers`; ingest `TestIntegrationDeleteReferrerIndexRepair`, `TestIntegrationDeleteReferrerClientGone`; facade `TestIntegrationDelete`, `TestIntegrationDeleteReferrer`, `TestIntegrationCatalogAfterDelete` |
| The catalog reads empty (item 2) | reindex `TestIntegration` (`OCIRepositories:0`), `TestIntegrationNames`, `TestIntegrationSplit` (6 cells); `TestIntegrationRepositoryUnknown` fails its count (`Unchanged` 0, want 1), not followed up |
| No ranges (item 5) | provider `TestIntegration/open blob with a range` (the steps after it do not run), raw `TestIntegration`, facade `TestIntegrationBlobReads` |
| The status one byte ahead (item 4) | provider `TestIntegrationUploads/session across providers`, facade `TestIntegrationUploadAcrossReplicas` (`GET on B: 204, Range "0-38"`) |
| The harness | reindex `TestIntegrationNoCatalog/probe`: its front proxy is on another host than Quay's realm, which the provider refuses (#354); its `config` cell, `catalog: false`, passes |

Everything else passed: ingest's publishing tests, the deb, maven, npm and pypi format tests,
apt-get in three images, the facade's pass-through, raw push, referrers, tokens across replicas
and abandoned monolithic upload, api, server, auth, the Postgres store.

**The end-to-end scenarios** (`make e2e E2E='raw oras'` with `LAJSTROM_E2E_REGISTRY=localhost:5012`,
`LAJSTROM_E2E_REGISTRY_USER=lajstrom-it+lajstrom`, `LAJSTROM_E2E_REGISTRY_PASSWORD_FILE=deploy/compose/quay/.work/robot.token`,
`LAJSTROM_E2E_REGISTRY_PREFIX=lajstrom-it/`): `raw.sh` passes R-01 to R-03 (put, the same
bytes again, new bytes) and fails at the range (`got '200', want '206'`); `oras.sh` passes the
login, push, repeat, the refused old and conflicting manifests, the size policy, pull, tags and
manifest fetch, and fails at the raw `DELETE` (502, item 1). The other scenarios and the
conformance suite were not run.

**Again with #801 and #803** (2026-11-12, the same stack, `quayadmin`, an administrator of
`lajstrom-it`, in place of the robot; SQLite only), the delete tests: deletes reach Quay.
A signed and attested version deleted through the facade is 202, and its tag, its digest, its
tag-schema index and cosign's tag are 404 after it (`ingest: deleted … referrers=3`).
`TestIntegrationDeleteReferrer` and `TestIntegrationCatalogAfterDelete` pass. Still failing:

| Cause | Tests that fail |
| --- | --- |
| A referrer pushed by digest stays, its `DELETE` 404 ([Delete](#delete)) | facade `TestIntegrationDelete` (the bundle 200 by digest after the delete), `TestIntegrationDeleteInterleaved` (the same in an `oci` repo, and manifests pushed by digest there); ingest `TestIntegrationDeleteReferrerIndexRepair` ("the deleted signature resolves"); `TestIntegrationDeleteReferrerClientGone` too, its message not read |
| The catalog (item 2): a rebuild from an empty index finds no repository | reindex `TestIntegrationFormats` (apt, pypi, maven), past their deletes now |

reindex `TestIntegrationRepairReferrers` reported no failure. The e2e scenarios, the probe and
the Postgres cells were not run again.

**The build rule, with #662** (2026-11-14, the robot, on BEFORE and AFTER):
`TestIntegrationBuildRoutes` of the provider pushes a blob to three new names that end in a build
route and to a control: on AFTER each is `registry integration refused the request: 400
NAME_INVALID` (a `*provider.RejectedError`), on BEFORE each is created; the control is created
on both. `TestIntegrationBuildRoutes` of ingest publishes a new raw package `build`: with the
vendor table's capabilities it is `ErrInvalid` naming the rule on both, and nothing is written;
with `buildRoutes: false` AFTER refuses it, `ingest: push blob … registry local refused the
request: 400 NAME_INVALID`, and BEFORE creates it, after which a second version with the vendor
table's capabilities is published (the index has the package). Both pass on both builds.

## Configuration

```yaml
server:
  maxManifestSize: 1048576              # Quay's nginx refuses a manifest over 1 MiB
registries:
  - name: quay
    type: generic
    url: http://localhost:5012          # config.yaml's SERVER_HOSTNAME: the bearer realm is built from it
    prefix: lajstrom-it/                # required: repositories exist only inside an organisation
    auth:
      username: lajstrom-it+lajstrom    # a robot in a team with the role admin
      passwordEnv: LAJSTROM_QUAY_ROBOT_TOKEN
    # No capabilities: the vendor table sets catalog: false (until #802), referrers: false and
    # consistency: eventual within 130s (#803) and buildRoutes: true (#662), and a delete's
    # token asks push (#801). On 3.18.x, buildRoutes: false lets new packages end in build.
```

The startup line then says (2026-11-12, against the compose stack): `vendor=quay
createOnPush=true referrers=false catalog=false deleteManifest=true … consistency="eventual
within 2m10s"`, and `registry needs workarounds vendor=quay … pushForDelete=true`; since #662 it
ends in `maxNameDepth=0 buildRoutes=true` (2026-11-14).

## Commands

The stack as in [Run it](#run-it). With the robot's token in a variable (`R=$(cat
deploy/compose/quay/.work/robot.token)`), the probes:

```sh
curl -si localhost:5012/v2/
TOKEN=$(curl -s -u "lajstrom-it+lajstrom:$R" 'localhost:5012/v2/auth?service=localhost:5012&scope=repository:lajstrom-it/raw/a:pull,push' | jq -r .token)
curl -s -u "lajstrom-it+lajstrom:$R" 'localhost:5012/v2/auth?service=localhost:5012&scope=repository:lajstrom-it/raw/a:delete'   # 400
curl -si localhost:5012/v2/_catalog                                                                  # 200, empty
curl -si -H "Authorization: Bearer $(curl -s -u "lajstrom-it+lajstrom:$R" 'localhost:5012/v2/auth?service=localhost:5012' | jq -r .token)" 'localhost:5012/v2/_catalog?n=2'
# a blob: POST /v2/<name>/blobs/uploads/, then PUT <Location>?digest=sha256:<hex> with the bytes (the Location is absolute)
lajstrom registry probe --url http://localhost:5012 --registry-prefix lajstrom-it/ \
  --username lajstrom-it+lajstrom --password-file deploy/compose/quay/.work/robot.token -v
# the build rule: a proxy cache of docker.io, then a manifest through it (A: the superuser's token from .work/admin.token)
curl -s -H "Authorization: Bearer $A" -H 'Content-Type: application/json' -X POST localhost:5012/api/v1/organization/ -d '{"name":"hub","email":"hub@example.invalid"}'
curl -s -H "Authorization: Bearer $A" -H 'Content-Type: application/json' -X POST localhost:5012/api/v1/organization/hub/proxycache -d '{"upstream_registry":"docker.io"}'
```

The tests:

```sh
LAJSTROM_TEST_REGISTRY=localhost:5012 LAJSTROM_TEST_REGISTRY_USERNAME='lajstrom-it+lajstrom' LAJSTROM_TEST_REGISTRY_PASSWORD="$R" \
  LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/ \
  LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' \
  go test -race -count=1 -tags integration -skip 'TestIntegrationRun$' ./...
LAJSTROM_E2E_REGISTRY=localhost:5012 LAJSTROM_E2E_REGISTRY_USER='lajstrom-it+lajstrom' \
  LAJSTROM_E2E_REGISTRY_PASSWORD_FILE=deploy/compose/quay/.work/robot.token \
  LAJSTROM_E2E_REGISTRY_PREFIX=lajstrom-it/ make e2e E2E='raw oras'
```
