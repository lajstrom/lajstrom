# Gitea container registry

What the container registry built into Gitea answered on 2026-11-10, measured with `curl`,
`oras`, the integration tests and the end-to-end scenarios (issue #120); Forgejo, the fork, was
run with the same setup and differs in three places ([Forgejo](#forgejo)). The registry is a
part of Gitea's package registry: `/v2/` on Gitea's own port, a repository is an image of an
owner (a user or an organisation), and every tag is a package version in Gitea's database. The
[summary column](README.md#capabilities) is in the README; this page has the details.

## Version tested

- Gitea **1.27.3**, the rootless image `gitea/gitea:1.27.3-rootless`, index
  `sha256:1c17ecaead42eb3b5391553d8708103a4beb0e86edf5b9ebc1eb269c318845f2` (linux/amd64,
  arm64, riscv64; built 2026-08-29). The binary says `gitea version 1.27.3 built with
  go1.26.7`. SQLite, packages on local storage (`/var/lib/gitea/packages`).
- Forgejo **17.0.1**, `codeberg.org/forgejo/forgejo:17.0.1-rootless`, index
  `sha256:5ef02fbcf22beef56b8c844952ffad58cfe177b6f98e46e6abb5df90b88dc8d3` (built
  2026-10-26); `/api/v1/version` says `17.0.1+gitea-1.22.0`.
- On macOS with OrbStack, arm64; the tests with Go 1.27.3 at 6b3b7b7 and the provider fix of
  this page ([What the provider must do differently](#what-the-provider-must-do-differently),
  item 1).

## Run it

[deploy/compose/gitea/compose.yaml](../../deploy/compose/gitea/compose.yaml), a compose
project of its own (`lajstrom-gitea`) beside the dev stack, and
[gitea.sh](../../deploy/compose/gitea/gitea.sh), which sets it up with Gitea's CLI and REST API,
never the UI:

```sh
deploy/compose/gitea/gitea.sh up            # Gitea on http://localhost:5009, set up
deploy/compose/gitea/gitea.sh up forgejo    # the same with Forgejo on http://localhost:5014
deploy/compose/gitea/gitea.sh down          # both, with their volumes and the token files
```

| What | How |
| --- | --- |
| Configuration | environment only (`GITEA__section__KEY`): `INSTALL_LOCK`, SQLite, registration off, packages on, `ROOT_URL` the published address: the registry's Bearer realm is `<ROOT_URL>v2/token` |
| Site admin | `lajstrom-admin`, by `gitea admin user create` in the container: the REST API needs a first admin |
| User | `lajstrom`, by `POST /api/v1/admin/users` |
| Organisation | `lajstrom-it`, private, by `POST /api/v1/orgs` |
| Team | `packages` in the organisation: permission write, the one unit `repo.packages`; the user in it |
| Token | the user's, scope `write:package`, by `POST /api/v1/users/lajstrom/tokens`, written to `deploy/compose/gitea/.work/<service>.token` (ignored by git) |
| Lajstrom | `prefix: lajstrom-it/`, `auth.username: lajstrom`, the token in the variable `passwordEnv` names |

`LAJSTROM_GITEA_CLEANUP_OLDER_THAN` (default `24h`) sets `[cron.cleanup_packages] OLDER_THAN`
for the [cleanup](#cleanup) experiment.

## The row

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Gitea | 1.27.3, local storage, `prefix: lajstrom-it/` (an organisation) | Bearer from `/v2/token` with the token; Basic straight on `/v2/` works too | yes, below an existing owner the user may write, at any depth; another owner 401, a missing one 404 | **no**: `/referrers/` is a plain-text 404; the `sha256-<digest>` tag schema | yes, `n`/`last`/`Link`, lexical; the user's view | yes, by digest, with every tag; by tag the tag, and the manifest with its last tag when it was pushed by tag only | **none**; tags are **case-insensitive and lower-cased** | none on local storage | pass but for ranges (Gitea serves none) and a streamed `PATCH` after the first chunk, with the fix of item 1 |
| Gitea | the same, no prefix | — | **no**: every repository needs an owner; `raw/…` is the owner `raw`, 404 | — | — | — | — | — | fail: nothing can be written |
| Forgejo | 17.0.1, the same setup | the same | the same | the same: a 404 with an empty body | the same | the same | the same | none | as Gitea, but ranges work and the upload status is off by one byte |

The probe reports `vendor=unknown createOnPush=true referrers=false catalog=true
deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0` (the test log and the
serve log of the e2e run). That is right; no override in `registries[].capabilities` is needed.

## What the provider must do differently

1. **Done in this PR:** keep the primed token when the challenge names extra scopes. Gitea's
   challenge always says `scope="*"`, which oras-go's auth client adds to the key of the token
   it caches, but not to the key it looks a request up by before it is challenged. The token
   that `prime` (the bodyless `GET` before a streamed body) fetched was never found, the
   streamed `PATCH` or `PUT` went out without it, got the 401 and could not be sent again:
   `request body is not rewindable`. Every blob upload through the facade (`POST ?digest=`,
   `PUT` of a session) and every upload continued by another replica failed with 500. `prime`
   now caches the token under the request's own scopes too; `TestUploadAcrossProviders` runs
   with a challenge scope of `*` (commit `fix(provider): keep the primed token …`).
2. Know that tags are case-insensitive. Gitea stores a tag lower-cased and resolves it in any
   case: `5.4.Final` is listed as `5.4.final`, and a push to `5.4.final` re-points it. Through
   Lajstrom nothing is overwritten (ingest resolves the tag first: the second version that
   differs only in case is a 409), but such a version cannot be published, and `tags/list`
   gives other tags than Lajstrom wrote, which reindex looks up in the index by the listed tag
   (read, not run). Maven versions (`-SNAPSHOT`, `.Final`, `-RC1`) carry capitals; pypi
   normalises to lower case. A vendor-table capability ("tags fold case") would let ingest
   refuse with a clear message; a contract question.
3. Expect no ranges from Gitea (Forgejo has them). A `Range` on a blob is answered 200 with
   the whole blob and no `Accept-Ranges`; the facade and `/raw/` pass that on, which RFC 9110
   allows, but a client that resumes a download starts over. Nothing to change unless Lajstrom
   should serve the range itself.
4. Do not stream a chunk of unknown size into a session that already holds bytes: Gitea and
   Forgejo answer 400 `BLOB_UPLOAD_INVALID` "Stream uploads after first write are not allowed".
   The provider does it for `FinishBlobUpload` with a last chunk of unknown size
   (`TestIntegrationUploads/session across providers`, the "chunk of unknown size" step). A
   docker client sends one streamed `PATCH`, which works.
5. Read a size-limit refusal as one: over `[packages] LIMIT_SIZE_CONTAINER` the closing `PUT`
   is 403 with an empty body, which the provider makes `ErrUnauthorized` (bad credentials), as
   it does Harbor's quota (harbor.md, item 1): the same contract question.
6. **Done in #352:** name the vendor. `GET /v2/` challenges `Bearer
   realm="<ROOT_URL>v2/token",service="container_registry",scope="*"`, which makes it
   `vendor=gitea`, Forgejo too. The entry changes nothing else: the probe is right, and item 2
   is still a contract question.

Nothing else: the token service, the tag schema, catalog, tags, delete, mounts (refused with
202), the monolithic `POST`, the cancel and the digest check work as the provider expects.

## Owners, tokens and create on push

`GET /v2/` answers 401 with `Www-Authenticate: Bearer
realm="http://localhost:5009/v2/token",service="container_registry",scope="*"`. The token
endpoint takes Basic auth with the user's password or a token, and an anonymous request too:

```text
GET /v2/token?service=container_registry&scope=repository:lajstrom-it/raw/a/b:pull,push   (Basic, the token)
    200 {"token":"<204 characters>"}   claims {"exp":…,"nbf":…,"UserID":2,"Scope":"write:package",…}: exp - nbf = 24 h
GET /v2/token?service=container_registry                                                  (anonymous)
    200 a token with "UserID":-1
GET /v2/   with that bearer 200;   with Basic (the token) 200
```

The token is the user's for 24 hours, whatever the requested scope; its `Scope` is the
access token's. A repository is `<owner>/<image>`, and the image may have any number of
components:

```text
POST+PUT blobs, PUT manifests/v1 in lajstrom-it/raw/one                                          201 201, 201
… in lajstrom-it/raw/a/b/c                                                                       201 201, 201
… in lajstrom-it/maven/repo/org/example/group/artifact/sub/1/2/3                                 201 201, 201
POST /v2/lajstrom/x/blobs/uploads/            (the user's own namespace)                         202
POST /v2/lajstrom-admin/x/blobs/uploads/      (another user)                                     401 reqPackageAccess   (plain text)
POST /v2/nosuchowner/x/blobs/uploads/                                                            404 Not found.         (plain text)
POST /v2/toplevel/blobs/uploads/              (one component: no image)                          404 Not found.
```

The manifest `PUT` answers `Location: /v2/<name>/manifests/<tag>` and `Docker-Content-Digest`.
Without a prefix the first component of every Lajstrom repository (`raw`, `apt`, `pypi`) is
taken for an owner that does not exist: Lajstrom needs `prefix: <owner>/`.

## Media types

Every manifest of the profile is taken; one per field, by tag:

```text
all-known              201   no artifactType, empty config, application/octet-stream layer
artifactType-custom    201   artifactType application/vnd.lajstrom.raw.v1
layer-custom           201   layer application/vnd.lajstrom.raw.file.v1
config-custom          201   config application/vnd.lajstrom.deb.metadata.v1+json
profile-raw            201   the raw profile
profile-deb            201   the deb profile: artifactType and config of Lajstrom's
an image index         201
```

A manifest `GET` without `Accept` is 200 with the stored media type.

## Referrers

No referrers API: `GET /v2/<name>/referrers/<digest>` is 404 `text/plain` `404 page not found`
for a subject that exists, for one that does not, and with `?artifactType=`; in a repository
without an owner the 404 says `Not found.`. A manifest with `subject` is taken (201) without
`OCI-Subject`, also when the subject does not exist. The probe sets `referrers=false`, and
`oras attach` twice writes the tag schema: the tag `sha256-<subject>` names an index of both
referrers, `oras discover` lists them, and both stay readable by digest after the index's
tag moved to the second one.

## Catalog

`_catalog` answers the user's view of the registry, lexical, with `n`/`last` and a relative
`Link`; five repositories `cat/r1`…`cat/r5` among others:

```text
GET /v2/_catalog?n=2                               Link: </v2/_catalog?last=lajstrom-it%2Fcat%2Fr1&n=2>; rel="next"   ["lajstrom-it/case/pkg","lajstrom-it/cat/r1"]
GET <that Link>                                    Link: …last=lajstrom-it%2Fcat%2Fr3…   ["lajstrom-it/cat/r2","lajstrom-it/cat/r3"]
GET /v2/_catalog?last=lajstrom-it/cat/r2&n=2       ["lajstrom-it/cat/r3","lajstrom-it/cat/r4"]
GET /v2/_catalog?n=0, ?n=100000                    every repository, no largest n
GET /v2/_catalog   anonymous                       401 UNAUTHORIZED;   with the anonymous token 200 {"repositories":[]}
```

The site admin's view was the same list. Whether a user sees another owner's packages was not
measured (the admin's push to its own namespace failed for a reason not followed up).

Gitea's REST API lists an owner's packages, `GET /api/v1/packages/lajstrom-it?type=container`:
one entry per image and version, where a version is a tag or, for a manifest pushed by
digest, the digest (`raw/it-…/hello.txt:sha256-feef0c02…`, `…:sha256:c0d4646e…`).

## Tags

```text
PUT a as 5.4.Final                     201   Location: …/manifests/5.4.Final
GET tags/list                          {"tags":["5.4.final"]}
HEAD 5.4.Final, 5.4.final, 5.4.FINAL   200, the digest of a
PUT b as 5.4.final                     201;   HEAD 5.4.Final: the digest of b;   a by digest: 404
GET tags/list?n=2                      Link: </v2/lajstrom-it/tags/pkg/tags/list?last=_5f9&n=2>; rel="next"   ["1.0","_5f9"]
GET tags/list?n=2&last=v2              ["v3","v4"]
GET tags/list (tags v1…v5, V10, _5f9, 1.0)   ["1.0","_5f9","v1","v10","v2","v3","v4","v5"]
GET tags/list of a missing repository  404 {"errors":[{"code":"NAME_UNKNOWN","message":""}]}
```

Tags are lower-cased on the way in and compared without case; the listing is lexical over the
lower-cased tags.

## Delete

```text
DELETE manifests/t1   (a, pushed as t1 only)          202   a by digest: 404: the manifest went with its only tag
a under t1 and t2; DELETE manifests/t1                202   t2 200, a by digest 200
a by digest, then as v1; DELETE manifests/v1          202   a by digest 200, untagged
DELETE manifests/sha256:<b>  (b under t2 and t3)      202   t2 404, t3 404, tags []
DELETE the same digest again                          404 MANIFEST_UNKNOWN
DELETE blobs/sha256:<layer>                           202   GET afterwards 404
DELETE tags/reference/t1                              404 404 page not found
```

A digest delete takes every tag with the manifest. In Gitea a manifest pushed by tag is that
tag's package version; one pushed by digest is a version of its own, named by the digest, and
stays when its tags go. `/v2` has no repository delete; the REST API's package delete was not
tried.

## Immutable tags

None: a tag re-points with 201. When it does, the manifest it named is deleted if nothing
else holds it (no other tag, never pushed by digest), with the layers only it used:

```text
PUT x as v1, then a as v1     201 201
x by digest                   404;   x's own layer 404;   the shared empty config 200
PUT x as v2 again             404 BLOB_UNKNOWN: its layer is gone from the image
```

Lajstrom never re-points a tag but the tag schema's `sha256-<subject>`, whose old index is
garbage. The re-pointing of `5.4.final` above deleted `a` the same way.

## Blob reads, ranges, sizes

- `GET` of a blob: 200, `Content-Length`, `Docker-Content-Digest`, `Etag: "<digest>"`, and a
  `Content-Type` sniffed from the bytes (`text/plain; charset=utf-8` for a text file). No
  `Accept-Ranges`; `Range: bytes=0-4` is 200 with the whole blob, and so is a range past the
  end. No redirect on local storage; `[storage.packages] SERVE_DIRECT` with MinIO or Azure
  storage redirects (read in Gitea's configuration cheat sheet, not run).
- A blob is visible in its image only: the same digest in another image of the owner is 404
  there, and a mount (`POST ?mount=&from=`) answers 202 and opens a session.
- 256 MiB and 512 MiB in one monolithic `PUT`: 201 in about 1 s each; Gitea sets no limit by
  default (`[packages] LIMIT_SIZE_CONTAINER = -1`). Larger blobs were not tried: the machine's
  disk was nearly full. With `LIMIT_SIZE_CONTAINER=1M` a monolithic `PUT` of 2 MiB is 403
  with an empty body, while `PATCH`es past the limit are 202.
- Manifests: 4 MiB, 4 MiB + 1 and 10 MiB are 201; 32 MiB is 413 `MANIFEST_INVALID` "Manifest
  exceeds maximum size" (the limit between was not bisected).

## Uploads

```text
POST blobs/uploads/                         202   Location: /v2/<name>/blobs/uploads/<id>, Docker-Upload-Uuid; no Range
PATCH Content-Range 0-4                     202   Range: 0-4
GET the session                             204   Range: 0-4
PATCH Content-Range 7-11 (out of order)     416 BLOB_UPLOAD_INVALID;   the session still 0-4
PATCH 5-9, PUT ?digest=                     202 Range: 0-9, 201
PATCH without Content-Range (streamed)      202 as the first write;   400 BLOB_UPLOAD_INVALID "Stream uploads after first write are not allowed" after any write
POST ?digest= with the bytes                201: a monolithic upload in one request
PUT ?digest= of other bytes                 400 DIGEST_INVALID;   the session stays (204)
the empty blob                              201;   HEAD 200, Content-Length: 0
DELETE the session                          204;   GET and PATCH afterwards 404 BLOB_UPLOAD_UNKNOWN
HEAD the session                            404
```

## Cleanup

Gitea's cron task `cleanup_packages` (daily) removes what is older than `OLDER_THAN` (24 h).
With `LAJSTROM_GITEA_CLEANUP_OLDER_THAN=1s` and the task run by
`POST /api/v1/admin/cron/cleanup_packages`, in one repository: `M` tagged `v1`; `R`, a referrer
of `M` by digest, untagged; `N`, untagged, no subject; `T`, a referrer through the tag schema
(`oras attach`); `U`, a blob no manifest uses; an open upload session.

```text
before:  M 200  v1 200  R 200  N 200  T 200  sha256-<M> 200  U 200  the session 204
after:   M 200  v1 200  R 200  N 200  T 200  sha256-<M> 200  U 404  the session 404
```

An untagged manifest, a referrer among them, survives the cleanup; an unreferenced blob and an
old session go (the question of #37). The owners' cleanup rules (keep the last N versions,
remove by pattern), which an owner sets in the UI or the API, delete versions, that is tags; not
tried.

## Forgejo

The same compose file (profile `forgejo`) and `gitea.sh up forgejo`; every command of this page
was run against it. Where it differs:

- **Ranges work:** `Range: bytes=0-4` is 206 with `Content-Range: bytes 0-4/32`; past the end
  416 `bytes */32`. `TestIntegration` of the provider passes whole.
- **The upload status is off by one byte:** after `PATCH 0-4` answered `Range: 0-4`, `GET`
  of the session answers `Range: 0-5` (the facade's `TestIntegrationUploadAcrossReplicas`: `GET
  on B: 204, Range "0-38"` for 38 bytes). A client that resumes from it sends a chunk one byte
  late and gets 416.
- `POST` of a session answers `Range: 0-0`; the referrers 404 has an empty body.

Everything else measured the same: the token and its claims, owners, case-folded tags, the
deletes, re-pointing, media types, catalog, the manifest size limit, the streamed `PATCH`.

## Our tests against it

`LAJSTROM_TEST_REGISTRY=localhost:5009`, `LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom`, the token
in `LAJSTROM_TEST_REGISTRY_PASSWORD`, `LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/`,
`LAJSTROM_TEST_POSTGRES_DSN` of the dev stack, `go test -race -count=1 -tags integration`: the
whole suite against Gitea, before and after the fix of item 1, and `-run
'TestIntegration|TestAptGet' ./internal/...` against Forgejo (`localhost:5014`). The apt tests
run apt-get in `ubuntu:22.04`, `ubuntu:24.04` and `debian:12`.

| Test | Gitea, without the fix | Gitea, with the fix | Forgejo, with the fix |
| --- | --- | --- | --- |
| provider `TestIntegration` | **fail**: open blob with a range, 200 for 206 (the steps after it do not run) | the same | pass |
| provider `TestIntegrationUploads` | **fail**: session across providers, `request body is not rewindable` | **fail**: the same step, the chunk of unknown size: 400 "Stream uploads after first write are not allowed" | the same as Gitea |
| ingest `TestIntegration` | pass | pass | pass |
| raw `TestIntegration` | **fail**: the range, 200 | **fail**: the range | pass |
| reindex `TestIntegration` | pass | pass | pass |
| facade: delete, delete referrer, pass-through, raw push, referrers, uploads across replicas, tokens across replicas, abandoned monolithic | **fail**: every blob upload 500, not rewindable | pass | pass but uploads across replicas: the status `Range` |
| facade `TestIntegrationBlobReads` | **fail** (the upload) | **fail**: the range | pass |
| `TestAptGet`, `TestAptGetInstall/registry`, `TestAptGetSigned` | pass | pass | pass |

Every other package of the suite passed against Gitea, the Postgres tests included.

**The end-to-end scenarios** (`test/e2e/run.sh`, against Gitea with the fix): `run.sh` has no
registry credentials or prefix, so a copy of it with `prefix:` and `auth:` in its
configuration ran (`$TMPDIR/spike-e2e/run.sh`; worth an option of its own in `run.sh`, a
follow-up). `oras`, `auth`, `rbac`, `oci-token`, `tokens`, `apt`, `pypi`, `maven`: pass.
`raw.sh`: **fail** at the range (`got '200', want '206'`), the steps after it do not run.
`cosign.sh`: sign, verify and `oras discover` through Lajstrom pass; it fails at its last step,
which asks the backend for the tag schema without credentials or prefix (401): the script's
limit, not Gitea's. `ha.sh` skipped.

The failures, by owner:

- **Gitea's:** no ranges (item 3), the streamed `PATCH` after the first write (item 4);
  Forgejo's status offset.
- **Ours, the provider:** the challenge scope, fixed here (item 1).
- **Ours, the harness:** `run.sh` without registry credentials, `cosign.sh`'s anonymous check of
  the backend.

The conformance suite was not run.

## Commands

The stack as in [Run it](#run-it). With the token in a variable (`T=$(cat
deploy/compose/gitea/.work/gitea.token)`), the probes:

```sh
curl -si localhost:5009/v2/
TOKEN=$(curl -s -u "lajstrom:$T" 'localhost:5009/v2/token?service=container_registry&scope=repository:lajstrom-it/raw/a:pull,push' | jq -r .token)
curl -si -H "Authorization: Bearer $TOKEN" 'localhost:5009/v2/_catalog?n=2'
curl -si -H "Authorization: Bearer $TOKEN" localhost:5009/v2/lajstrom-it/lajstrom-probe/referrers/sha256:0000000000000000000000000000000000000000000000000000000000000000
# a blob: POST /v2/<name>/blobs/uploads/, then PUT <Location>?digest=sha256:<hex> with the bytes
curl -si -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
  --data-binary @m.json localhost:5009/v2/lajstrom-it/curl/b1/manifests/v1
curl -s -u lajstrom-admin:lajstrom-admin -X POST localhost:5009/api/v1/admin/cron/cleanup_packages
```

The tests:

```sh
LAJSTROM_TEST_REGISTRY=localhost:5009 LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom LAJSTROM_TEST_REGISTRY_PASSWORD="$T" \
  LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/ \
  LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' \
  go test -race -count=1 -tags integration ./...
```
