# Nexus Repository

What Sonatype Nexus Repository Community Edition answered on 2026-11-10 as an OCI backend,
measured with `curl`, `oras`, the integration tests and the end-to-end scenarios (issue #119).
Nexus serves a registry from a hosted repository on an HTTP connector port of its own. 3.98.1
has two formats for that: `docker`, the one the issue names, and `oci`, which is newer and
has the referrers API. Both were run; they differ enough to be two columns. The
[summary columns](README.md#capabilities) are in the README; this page has the details.

## Version tested

- `sonatype/nexus3:3.98.1`, index
  `sha256:91a9af44af777c353db11a5ee73850c7eb24b03c997ff4cc62a3500ad38a1332` (linux/amd64 and
  arm64, built 2026-11-05; the label says `3.98.1-01`). `Server: Nexus/3.98.1-01 (COMMUNITY)`
  on every answer: the image is the Community Edition, the successor of Nexus Repository
  OSS, which has an EULA to accept (`GET /service/rest/v1/system/eula` says
  `"accepted":false` on a new volume); `nexus.sh` accepts it before anything else.
- The embedded H2 database and the blob store `default` on the data volume; the heap set to
  1 GiB (the image's default is 2.7 GiB).
- On macOS with OrbStack, arm64; the tests with Go 1.27.3 at 6b3b7b7 and the provider fix of
  this page ([What the provider must do differently](#what-the-provider-must-do-differently),
  item 1).

## Run it

[deploy/compose/nexus/compose.yaml](../../deploy/compose/nexus/compose.yaml), a compose
project of its own (`lajstrom-nexus`) beside the dev stack, and
[nexus.sh](../../deploy/compose/nexus/nexus.sh), which sets it up through the REST API, never
the UI:

```sh
deploy/compose/nexus/nexus.sh up      # about 25 s from an empty volume, the image pulled
deploy/compose/nexus/nexus.sh down    # with the volume
LAJSTROM_NEXUS_WRITE_POLICY=allow_once deploy/compose/nexus/nexus.sh up   # both repositories without redeploy
```

| What | How |
| --- | --- |
| Admin | the generated password of `/nexus-data/admin.password` replaced by `PUT /service/rest/v1/security/users/admin/change-password` |
| EULA | `GET /service/rest/v1/system/eula`, then `POST` it back with `"accepted":true` |
| Anonymous access | off: `PUT /service/rest/v1/security/anonymous` |
| Realms | `NexusAuthenticatingRealm`, `DockerToken`, `OciBearerToken` |
| Repositories | `lajstrom`, `POST /service/rest/v1/repositories/docker/hosted`, connector 8082 on `localhost:5008`; `lajstrom-oci`, `POST /service/rest/v1/repositories/oci/hosted`, connector 8083 on `localhost:5015`; both `forceBasicAuth`, blob store `default`, `writePolicy` from `LAJSTROM_NEXUS_WRITE_POLICY` (`allow`) |
| Role and user | role `lajstrom` with `nx-repository-view-docker-lajstrom-*` and `nx-repository-view-oci-lajstrom-oci-*`; user `lajstrom` with that role only |
| Lajstrom | `url: http://localhost:5008` or `:5015`, no prefix, `auth.username: lajstrom` |

User tokens are a Pro feature; the Community Edition has the user's password. The UI and REST
API stay on `localhost:8081`.

## The row

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Nexus, docker | 3.98.1 CE, `docker` hosted, `writePolicy: allow` | Basic (`forceBasicAuth`); `/v2/token` too | yes, anywhere, at any depth | **no**: an HTML 404; the `sha256-<digest>` tag schema | yes, `n`/`last`/`Link` (absolute) | **only the digest**: the tags that name the manifest stay and serve it | `writePolicy: allow_once`: an existing tag is 409 `REDEPLOY_DISABLED`, the same manifest too | none: blob store on the file system | **fail**: the digest delete; a `PATCH` after the cancel is 500 |
| Nexus, OCI | 3.98.1 CE, `oci` hosted, `writePolicy: allow` | the same | the same | **yes**: the referrers API with the `artifactType` filter, `OCI-Subject`; and Nexus writes the tag schema itself | yes, `n` and a `cursor` in `Link`; `last` ignored | yes, with its tags | the same, 409 `REDEPLOY_DISABLED` | none | **fail**: every `GET` with `Range` is 500, which breaks pypi uploads |

The probe reports `vendor=unknown createOnPush=true referrers=false catalog=true
deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0` for the docker format
and the same with `referrers=true` for the OCI format (the test log). That is right but for
the docker format's `deleteManifest`, which says true while a delete leaves the tags.

## What the provider must do differently

1. **Done in this PR:** ask for a blob's own bytes. Go's transport offers `Accept-Encoding:
   gzip`, and the docker format takes the offer for a blob that looks like text
   (`Content-Encoding: gzip`, chunked); the transport unzips and the size is lost. The facade
   then served such blobs without `Content-Length` (`TestIntegrationBlobReads`). The
   provider's transport no longer offers compression (commit `fix(provider): ask the registry
   for a blob's own bytes, not gzip`).
2. On the docker format, delete the tags as well as the digest, or do not use the format. A
   `DELETE /v2/<name>/manifests/<digest>` answers 202 and removes the manifest under its
   digest only: every tag that named it still answers 200 with it, `tags/list` still lists
   them, and reindex finds them again (`TestIntegration` of reindex: "the rows of the deleted
   manifest are still there"; flaky, it passed in one of two runs). A delete by tag removes
   that tag. Through Lajstrom a package or version deleted on this format is back after the
   next reindex. The provider would need to delete by each tag it knows of the manifest,
   which `Repository.Delete` by descriptor does not carry: a contract question. The OCI
   format deletes the tags with the digest, as the spec says. **Since #352** the vendor
   table sets `deleteManifest: false` for a Nexus without the referrers API, the docker
   format: Lajstrom refuses a delete there instead of leaving the tags.
3. On the OCI format, survive a 500 to a ranged `GET`. Every `GET` of a blob with `Range`
   (one range, an open range, a suffix, two ranges, past the end) is 500 `UNKNOWN`
   (`IllegalArgumentException: Paths must start with a slash` in Nexus's log), though `HEAD`
   says `Accept-Ranges: bytes`. Ingest reads a wheel back by ranges (`fetchedBlob.ReadAt`), so
   **every twine upload is 400** (`pypi.sh`), and a client's range through the facade and
   `/raw/` is 500. The provider could retry such a `GET` without `Range` and skip to the
   offset, as ingest already does with a 200, or a capability `ranges: false` from the vendor
   table could; either is a change to file. The docker format serves ranges (206, 416).
   **Since #352** the provider asks a Nexus with the referrers API, the OCI format, for whole
   blobs: no `Range` goes out, a client's range is answered with the whole blob (200) and no
   blob says `Accept-Ranges`, and ingest's `ReadAt` reads up to its block, as on Gitea. The
   unit tests answer as Nexus did; not run against Nexus again.
4. Expect 429 for a wrong password, and for the right one after it. Three failed logins of a
   user make Nexus answer that user 429 "Too many authentication attempts" with
   `Retry-After: 29`, the right password included, until it expires. The provider retries a
   429 with its `Retry-After`; a misconfigured `passwordEnv`, or anyone guessing the service
   account's password elsewhere, keeps Lajstrom out. Worth a line in the operations guide.
5. **Done in #352:** name the vendor. `Server: Nexus/<version> (COMMUNITY)` on `GET /v2/` or
   the challenge `BASIC realm="Sonatype Nexus Repository Manager"` make it `vendor=nexus`; the
   referrers probe tells the formats apart, and the table carries items 2 and 3.

Nothing else: Basic auth, create on push, the manifest profile, tags, catalog, uploads across
replicas and the digest check work as the provider expects.

## Connectors and create on push

The `GET /v2/` of either connector answers 401 with `WWW-Authenticate: BASIC realm="Sonatype
Nexus Repository Manager"` (`forceBasicAuth`; without it the bearer realms would answer) and
`Docker-Distribution-Api-Version: registry/2.0`; with Basic auth 200. `GET /v2/token` with
Basic auth answers a token too. On the main port, `/v2/` is 404 and
`/repository/lajstrom/v2/` is 200: Nexus routes by path there, but oras-go and every OCI
client address `/v2/` at the root of the host, so a connector port (or the subdomain
connector, Pro) is the address Lajstrom needs.

A repository is created by its first manifest, anywhere and at any depth:

```text
PUT manifests/v1 in one, raw/one, raw/a/b/c, maven/repo/org/example/group/artifact/sub/1/2/3      201 each
    Docker-Content-Digest: sha256:…
    Location: http://localhost:5008/v2/one/manifests/v1/repository/lajstrom/v2/one/manifests/v1
    the body: the manifest itself
```

The `Location` of a manifest `PUT` is two paths glued together, on both formats, and the 201
carries the manifest as its body. Neither costs Lajstrom anything: oras-go reads neither. On
the docker format a repository that holds only manifests pushed by digest does not exist for
`tags/list` (404 `NAME_UNKNOWN`) until a tag is pushed.

## Media types

Both formats take every manifest of the profile, by tag, and give the same bytes back:

```text
all-known              201   no artifactType, empty config, application/octet-stream layer
artifactType-custom    201   artifactType application/vnd.lajstrom.raw.v1
layer-custom           201   layer application/vnd.lajstrom.raw.file.v1
config-custom          201   config application/vnd.lajstrom.deb.metadata.v1+json
profile-raw            201   the raw profile
profile-deb            201   the deb profile
an image index         201
GET profile-deb        200   Content-Type: application/vnd.oci.image.manifest.v1+json; the bytes pushed (no Accept: the same)
```

## Referrers

**docker format:** no referrers API. `GET /v2/<name>/referrers/<digest>` is 404 with Nexus's
HTML error page (`text/html`), the probe sets `referrers=false`, a manifest with `subject` is
taken without `OCI-Subject`, and `oras attach` writes the tag schema; two attachments, both
listed by `oras discover`, both readable by digest.

**OCI format:** the OCI 1.1 referrers API.

```text
PUT referrer by digest (subject tagged)          201   OCI-Subject: sha256:<subject>
GET referrers/<subject>                          200   application/vnd.oci.image.index.v1+json, the referrer with its artifactType and annotations
GET referrers/<subject>?artifactType=…           200   OCI-Filters-Applied: artifactType
GET referrers/<zero digest> in lajstrom-probe    200   {"schemaVersion":2,"manifests":[],"mediaType":"application/vnd.oci.image.index.v1+json"}
PUT a referrer whose subject does not exist      201   no OCI-Subject
a referrer pushed at once after its subject      OCI-Subject at once (three runs); listed at once
```

And Nexus writes the tag schema as well: after a referrer push the repository has the tag
`sha256-<subject>`, an index of the referrers, though the client (`oras attach -d`: one `PUT`
of the referrer, `OCI-Subject` in the answer, no further request) wrote no tag. With
`writePolicy: allow_once` that index keeps the first referrer only (its re-pointing is a
redeploy), while the API lists all. Lajstrom reads the API on this format.

## Catalog

```text
docker:  GET /v2/_catalog?n=2               Link: <http://localhost:5008/v2/_catalog?n=2&last=cat%2Fr1>; rel="next"   ["case/pkg","cat/r1"]
         GET /v2/_catalog?last=cat/r2&n=2   ["cat/r3","cat/r4"];   n=0 and n=100000: every repository
OCI:     GET /v2/_catalog?n=2               Link: </v2/_catalog?n=2&cursor=Y2F0L3IxfGxhanN0cm9tLW9jaXwxNzk0…>; rel="next"   ["case/pkg","cat/r1"]
         GET <that Link>                    the next two, and a Link with the next cursor
         GET /v2/_catalog?last=cat/r2&n=2   {"repositories":[]} and a Link: last is not read
         n=0: []; n=100000: every repository
```

The provider follows `Link` from the first page, which both answer; a client that builds
`last` itself gets nothing from the OCI format. Lexical order on both.

## Tags

Both formats: tags keep their case (`5.4.Final` and `5.4.final` are two tags), byte order
(`["1.0","V10","_5f9","v1","v2","v3","v4","v5"]`), `n`/`last` with an absolute `Link`, and 404
`NAME_UNKNOWN` for a repository that does not exist. With `writePolicy: allow` a tag re-points
(201) and the manifest it named stays readable by digest.

## Delete

```text
                                                  docker                         OCI
DELETE manifests/t1 (a under t1 and t2)           202; t2 200, a by digest 200   the same
DELETE manifests/sha256:<b> (b under t2 and t3)   202; t2 200, t3 200            202; t2 404, t3 404
    tags/list afterwards                          ["t2","t3"], b's bytes          NAME_UNKNOWN: the repository went with it
    GET by digest afterwards                      404                            404 (with a Docker-Content-Digest header)
DELETE the same digest again                      404 MANIFEST_UNKNOWN           404
DELETE the last tag                               202; the repository goes       202
DELETE blobs/sha256:<layer>                       202; GET 404                   202; GET 404
DELETE tags/reference/t1                          404                            404 "no handler matched request path"
```

On the docker format the tags stayed after a digest delete in every order tried: pushed by
tag only, by tag and then by digest, with a `latest` tag, after another tag's delete. One
sequence after refused redeploys (`allow_once`) deleted the tag as well; not followed up.

## Immutable tags

The repository's `writePolicy`: `allow` (default), `allow_once` ("disable redeploy"), `deny`.
With `allow_once`, on both formats:

```text
PUT a as v1                  201
PUT a as v1 again            409 {"errors":[{"code":"REDEPLOY_DISABLED","message":"lajstrom/v2/imm/pkg-…/manifests/v1 -  cannot be updated as asset already exists and redeploy is not allowed"}]}
PUT b as v1                  409 REDEPLOY_DISABLED;   v1 still a
PUT a, then b by digest      201, 201
PUT b as v2                  201
a blob uploaded again        201
DELETE v2, DELETE a          202, 202
```

The same manifest again under its own tag is refused, not idempotent; Lajstrom's ingest
resolves the tag first and writes nothing on a repeat. `latestPolicy: true` would exempt
`latest`. The provider reads the 409 as `ErrConflict`, which is right; `immutableTags: true` in
the configuration says so, but nothing in Lajstrom reads it yet.

## Blob reads, ranges, sizes

- **docker format:** `GET` 200 with `Content-Length`, `Docker-Content-Digest`, `ETag`,
  `Last-Modified`, and a `Content-Type` sniffed from the bytes (`text/plain` for text). With
  `Accept-Encoding: gzip` a text-like blob comes back `Content-Encoding: gzip`, chunked; a
  random one does not (item 1). `Range: bytes=0-4` 206 `Content-Range: bytes 0-4/32`; past the
  end 416 `bytes */32` with a JSON `UNKNOWN` body; `HEAD` has `Accept-Ranges: bytes`. A mount
  answers 202 (a session); a blob of the repository is readable under any image name.
- **OCI format:** `GET` 200, `Content-Type: application/octet-stream`, `Content-Length`,
  never gzip; every `Range` 500 (item 3). A mount answers 201 with the blob's `Location`.
- No redirect on either: the blob store is on the file system. An S3 blob store was not run.
- 256 MiB in one monolithic `PUT`: 201 in 1 to 2 s on each format; no limit is set. Larger
  blobs were not tried: the machine's disk was nearly full (once it was full, and the
  container runtime stopped). The same `PUT` streamed from stdin by `curl -T -` was 400 on
  both, and 201 with its length known (`--data-binary @-`); the provider sends the length.
- Manifests: 4 MiB, 4 MiB + 1 and 10 MiB are 201 on both. 32 MiB: the OCI format 413
  `MANIFEST_INVALID` `{"size":33554432,"maxSize":10485760}`, a limit of 10 MiB; the docker
  format 400 `MANIFEST_INVALID` "String value length (20051112) exceeds the maximum allowed",
  its JSON parser's limit on one string.

## Uploads

```text
                                           docker                                      OCI
POST blobs/uploads/                        202 Range: 0-0, Docker-Upload-UUID           the same
PATCH 0-4, GET the session                 202 Range: 0-4, 204 Range: 0-4               the same
PATCH 7-11 (out of order)                  416 RANGE_NOT_SATISFIABLE "Expected Range: 5-9"   416 RANGE_NOT_SATISFIABLE, expectedStart 5
two streamed PATCHes (no Content-Range)    202 0-4, 202 0-9                             the same
POST ?digest= with the bytes               201                                          201
PUT ?digest= of other bytes                400 DIGEST_INVALID; the session stays         400 DIGEST_INVALID; the session is gone (404)
the empty blob                             201                                          201
DELETE the session, then GET, PATCH        204, 404 BLOB_UPLOAD_UNKNOWN, **500**         204, 404, 404
HEAD the session                           404                                          400
```

`RANGE_NOT_SATISFIABLE` is not a code of the spec's table; the provider reads the 416.

## Garbage collection

The question of #37, on each format: `M` tagged `v1`; `R`, a referrer of `M` by digest,
untagged; `N`, untagged, no subject; `T`, a referrer by `oras attach`; `U`, a blob no manifest
uses; an open upload session. The tasks were created by `POST /service/rest/v1/tasks` from
their templates and run by `POST /service/rest/v1/tasks/<id>/run`: "Docker - Delete unused
manifests and images" (`repository.docker.gc`, `deployOffset` 0 hours) and "Docker - Delete
incomplete uploads" (`repository.docker.upload-purge`, age 0).

```text
docker, before:  M 200  v1 200  R 200  N 200  T 200  sha256-<M> 200  U 200  session 204
docker, after:   M 200  v1 200  R 404  N 404  T 200  sha256-<M> 200  U 404  session 404   (N's layer 404)
OCI:             the docker GC task FAILED for an oci repository; the upload purge ran and left the session; everything stayed
```

On the docker format the collector deletes an untagged referrer pushed by digest, as GitLab's
online collector does; one listed by a tag-schema index stays. Lajstrom writes the tag schema
on this format, which keeps its referrers. The task is not scheduled by default; its
`deployOffset` (24 h by default) spares what is younger. The OCI format of the Community
Edition has no collector task among the templates (only "OCI - Rebuild Referrers index" and
"OCI - Rebuild Blob Reference index"); cleanup policies, by component age or name, were not
tried.

## Our tests against it

`LAJSTROM_TEST_REGISTRY=localhost:5008` (docker) or `localhost:5015` (OCI),
`LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom`, `LAJSTROM_TEST_REGISTRY_PASSWORD=lajstrom`, no
prefix, `LAJSTROM_TEST_POSTGRES_DSN` of the dev stack, `go test -race -count=1 -tags
integration -run 'TestIntegration|TestAptGet' ./internal/...`. The apt tests run apt-get in
`ubuntu:22.04`, `ubuntu:24.04` and `debian:12`.

| Test | docker | OCI |
| --- | --- | --- |
| provider `TestIntegration` | **fail**: delete a manifest that an index lists, 404 to the digest delete (the steps after it do not run) | **fail**: open blob with a range, 500 (the steps after it do not run) |
| provider `TestIntegrationUploads` | **fail**: cancel, a `PATCH` after it 500 for 404; the steps before pass, the session across providers among them | pass |
| ingest `TestIntegration` | pass | pass |
| raw `TestIntegration` | pass | **fail**: the range, 500 |
| reindex `TestIntegration` | **fail** once of two runs: the deleted manifest's rows stay | pass |
| facade `TestIntegrationDelete` | **fail**: a referrer of the deleted package still answers 200 by digest | pass |
| facade `TestIntegrationBlobReads` | **fail** without the fix of item 1 (`Content-Length -1`); pass with it | **fail**: the range, 500 |
| facade: delete referrer, pass-through, raw push, referrers, uploads and tokens across replicas, abandoned monolithic | pass | pass |
| `TestAptGet`, `TestAptGetInstall/registry`, `TestAptGetSigned` | pass | pass |

**The end-to-end scenarios** (`test/e2e/run.sh` against each, with the fix): `run.sh` has no
registry credentials, so a copy of it with `auth:` in its configuration ran
(`$TMPDIR/spike-e2e/run.sh`; worth an option of its own in `run.sh`, a follow-up).

| Scenario | docker | OCI |
| --- | --- | --- |
| `raw.sh` | pass | **fail**: range status 500 |
| `oras.sh` | **fail**: after the digest delete, `HEAD …/manifests/one` 200, want 404 | pass |
| `cosign.sh` | sign, verify, discover pass; its last step asks the backend without credentials (the script) | the same |
| `pypi.sh` | pass | **fail**: twine upload 400; ingest's range read of the wheel, 500 from Nexus four times |
| `auth`, `rbac`, `oci-token`, `tokens`, `apt`, `maven` | pass | pass |

The failures, by owner:

- **Nexus's, the docker format:** the digest delete that leaves the tags (item 2), the 500 to
  a `PATCH` of a cancelled session.
- **Nexus's, the OCI format:** the 500 to every range (item 3).
- **Ours, the provider:** the gzip offer, fixed here (item 1).
- **Ours, the harness:** `run.sh` without registry credentials, `cosign.sh`'s anonymous check of
  the backend.

The conformance suite was not run.

## Commands

The stack as in [Run it](#run-it). The probes, with the docker format on `:5008` and the OCI
format on `:5015`:

```sh
curl -si localhost:5008/v2/
curl -si -u lajstrom:lajstrom 'localhost:5015/v2/_catalog?n=2'
curl -si -u lajstrom:lajstrom localhost:5015/v2/lajstrom-probe/referrers/sha256:0000000000000000000000000000000000000000000000000000000000000000
# a blob: POST /v2/<name>/blobs/uploads/, then PUT <Location>?digest=sha256:<hex> with the bytes
curl -si -u lajstrom:lajstrom -X PUT -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
  --data-binary @m.json localhost:5008/v2/curl/b1/manifests/v1
curl -si -u lajstrom:lajstrom -H 'Range: bytes=0-4' localhost:5015/v2/curl/b1/blobs/sha256:<hex>      # 500
curl -si -u lajstrom:lajstrom -H 'Accept-Encoding: gzip' localhost:5008/v2/curl/b1/blobs/sha256:<hex> # gzip for text
# a task: its template, POSTed with the properties set, then run
curl -s -u admin:lajstrom-admin localhost:8081/service/rest/v1/tasks/templates/repository.docker.gc
```

The tests:

```sh
LAJSTROM_TEST_REGISTRY=localhost:5015 LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom LAJSTROM_TEST_REGISTRY_PASSWORD=lajstrom \
  LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' \
  go test -race -count=1 -tags integration -run 'TestIntegration|TestAptGet' ./internal/...
```
