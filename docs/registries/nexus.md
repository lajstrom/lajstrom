# Nexus Repository

What Sonatype Nexus Repository Community Edition answered on 2026-11-10 as an OCI backend,
measured with `curl`, `oras`, the integration tests and the end-to-end scenarios (issue #119).
Nexus serves a registry from a hosted repository on an HTTP connector port of its own. 3.98.1
has two formats for that: `docker`, the one the issue names, and `oci`, which is newer and
has the referrers API. Both were run; they differ enough to be two columns. The
[summary columns](README.md#capabilities) are in the README; this page has the details. Both
were measured again on 2026-11-12 with the vendor table of #352 (#781):
[Again after #352](#again-after-352); the tests of reindex and the e2e scenarios on 2026-11-14
(#800): [Again on 2026-11-14](#again-on-2026-11-14).

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
  item 1). Again on 2026-11-12, the same image on a new volume, at d44b112e
  ([Again after #352](#again-after-352)).

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
| Repositories | `lajstrom`, `POST /service/rest/v1/repositories/docker/hosted`, connector 8082 on `localhost:5008`; `lajstrom-oci`, `POST /service/rest/v1/repositories/oci/hosted`, connector 8083 on `localhost:5015`; both `forceBasicAuth`, blob store `default`, `writePolicy` from `LAJSTROM_NEXUS_WRITE_POLICY` (`allow`); `strictContentTypeValidation` off on `lajstrom` since #800 ([below](#again-after-352)), on on `lajstrom-oci` |
| Role and user | role `lajstrom` with `nx-repository-view-docker-lajstrom-*` and `nx-repository-view-oci-lajstrom-oci-*`; user `lajstrom` with that role only |
| Lajstrom | `url: http://localhost:5008` or `:5015`, no prefix, `auth.username: lajstrom` |

User tokens are a Pro feature; the Community Edition has the user's password. The UI and REST
API stay on `localhost:8081`.

## The row

`lajstrom registry probe` measures every row of this table but the listing API, auth, rate
limits and the garbage collector, which still need a person;
[README.md](README.md#measuring-a-registry-lajstrom-registry-probe) says how.

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Nexus, docker | 3.98.1 CE, `docker` hosted, `writePolicy: allow` | Basic (`forceBasicAuth`); `/v2/token` too | yes, anywhere, at any depth | **no**: an HTML 404; the `sha256-<digest>` tag schema | yes, `n`/`last`/`Link` (absolute) | **only the digest**: the tags that name the manifest stay and serve it | `writePolicy: allow_once`: an existing tag is 409 `REDEPLOY_DISABLED`, the same manifest too | none: blob store on the file system | **fail**: the digest delete; a `PATCH` after the cancel is 500 |
| Nexus, OCI | 3.98.1 CE, `oci` hosted, `writePolicy: allow` | the same | the same | **yes**: the referrers API with the `artifactType` filter, `OCI-Subject`; and Nexus writes the tag schema itself | yes, `n` and a `cursor` in `Link`; `last` ignored | yes, with its tags | the same, 409 `REDEPLOY_DISABLED` | none | **fail**: every `GET` with `Range` is 500, which breaks pypi uploads |

The probe reports `vendor=unknown createOnPush=true referrers=false catalog=true
deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0` for the docker format
and the same with `referrers=true` for the OCI format (the test log, before #352). That is
right but for the docker format's `deleteManifest`, which says true while a delete leaves the
tags. Since #352 it is `vendor=nexus`, with `deleteManifest=false` on the docker format and the
blobs of the OCI format read whole (items 2, 3 and 5), as the start logged on 2026-11-12
([Again after #352](#again-after-352)).

## What the provider must do differently

1. **Done in #341:** ask for a blob's own bytes. Go's transport offers `Accept-Encoding:
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
   format: Lajstrom refuses a delete there instead of leaving the tags. Measured on
   2026-11-12: the facade answers 405 `UNSUPPORTED`, `/raw/` and the management API 501, and
   nothing is deleted ([Again after #352](#again-after-352)).
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
   unit tests answer as Nexus did. Measured on 2026-11-12: a range through the facade and
   `/raw/` is 200 with the whole blob, and `pypi.sh` passes; but only on a provider that had
   probed, #797 ([Again after #352](#again-after-352)). Since #797 a blob read probes before
   its first request; not measured against Nexus again.
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
  answers 202 (a session); a blob of the repository is readable under any image name. Under
  `strictContentTypeValidation: true` a blob whose content type Nexus cannot tell is 400
  `BLOB_UPLOAD_INVALID` (2026-11-12, [Again after #352](#again-after-352)).
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
`ubuntu:22.04`, `ubuntu:24.04` and `debian:12`. The tables of this section are the run of
2026-11-10, before #352, kept as they were; the run of 2026-11-12 is in
[Again after #352](#again-after-352).

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

**The end-to-end scenarios** (`test/e2e/run.sh` against each, with the fix): `run.sh` had no
registry credentials then, so a copy of it with `auth:` in its configuration ran
(`$TMPDIR/spike-e2e/run.sh`). Since #353 `run.sh` takes them
([testing.md](../testing.md#a-registry-with-credentials-and-a-prefix)), and the stock `run.sh`
ran against Nexus on 2026-11-12 ([Again after #352](#again-after-352)).

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
  the backend; both done in #353.

The conformance suite was not run.

## Again after #352

Measured on 2026-11-12 for #781: the same image and `nexus.sh` (`Nexus/3.98.1-01 (COMMUNITY)`,
`writePolicy: allow`), Lajstrom at d44b112e with the vendor table of #352; the probe against
both connectors, the integration tests of [Our tests against it](#our-tests-against-it), the
stock `test/e2e/run.sh` with `LAJSTROM_E2E_REGISTRY_USER` and `_PASSWORD_FILE` (#353), and a
`serve` with an `oci` and a `raw` repo on each format. The start logs what the vendor table
made of each connector:

```text
registry capabilities registry=nxd vendor=nexus createOnPush=true referrers=false catalog=true deleteManifest=false …
registry capabilities registry=nxo vendor=nexus createOnPush=true referrers=true catalog=true deleteManifest=true …
registry needs workarounds registry=nxo vendor=nexus blobsReadWhole=true blobExistsByGET=false
```

**The docker format: deletes are refused, and nothing changes** (item 2). Through Lajstrom:

```text
DELETE /v2/raw/assets/<path>/manifests/<tag>           405 {"errors":[{"code":"UNSUPPORTED","message":"the registry \"it\" of raw/assets does not delete manifests; nothing was deleted"}]}
DELETE /raw/nxd/<path>                                 501 Not Implemented; a GET afterwards 200
DELETE /api/v1/repos/raw/scratch/packages/…/versions/… 501 {"error":{"code":"UNAVAILABLE","message":"the registry \"local\" of raw/scratch does not delete manifests; nothing was deleted"}}
```

No tag is left serving a deleted manifest any more; a version leaves this format only when it
is deleted in Nexus itself, and then reindex's prune decides about its rows (not measured).

**The OCI format: blobs are read whole** (item 3). Through a `serve` whose probe at start
succeeded, a blob of 2500 bytes:

```text
                                  /v2/oci/nxo/reads/blobs/<digest>                        /raw/nxo/<path>
GET                               200, 2500 bytes                                         200, 2500 bytes
GET, Range: bytes=1000-1999       200, 2500 bytes, no Content-Range, no Accept-Ranges     200, 2500 bytes
GET, Range: bytes=2500-           200, 2500 bytes                                         200, 2500 bytes
GET, Range: bytes=-10             200, 2500 bytes
HEAD                              200, Content-Length 2500, no Accept-Ranges
```

The docker format through the same `serve`: 206 `bytes 1000-1999/2500`, 416 past the end, 206
for the suffix. Nexus still answers every `Range` 500 itself (the probe: "500 to a `Range`"), but
Lajstrom no longer sends one, and `pypi.sh` passes: twine uploads, which were 400, take.

**Only once the provider has probed.** The quirk is set by the provider's first probe that
succeeds, and the read path never probes. A provider that has not, the second replica of the
facade's `TestIntegrationBlobReads`, which only reads, or a `serve` whose probe at start found
Nexus down, sends the client's `Range` and answers 500. One provider, one blob:

```text
before the probe: OpenBlob with a Range = … response status code 500: unknown: unknown
after the probe:  OpenBlob with a Range = status 200, size 2800, acceptRanges false
```

Ours: #797. Since its fix `OpenBlob`, `StatBlob` and a blob's `Exists` probe before the
provider's first request, or wait for the probe under way, at most `registries[].timeout`; the
unit tests answer as Nexus did, and a fresh provider's first read with a `Range` is the whole
blob, 200. Not measured against Nexus again.

**New since the first run**, from the probe, which had not been run against Nexus, and the
tests:

- **The docker format refuses some blobs by their content.** With `strictContentTypeValidation:
  true`, nexus.sh's setting, a blob that starts with eight zero bytes followed by random ones is
  400 `BLOB_UPLOAD_INVALID` "Content type could not be determined"; random bytes alone, zeros
  alone, text and gzip are 201. The probe's blobs start so (a block index of eight bytes), so it
  measured no blob read, blob size or chunk size on this format. Through Lajstrom such a raw
  file is 502 (`registry nxd refused the request: 400 BLOB_UPLOAD_INVALID` in the log). With
  the setting off the same push is 201 (set back to true afterwards). The OCI format took
  every one of them with the setting on. For Lajstrom on the docker format, turn
  `strictContentTypeValidation` off ([configuration.md](../configuration.md#registry-capabilities)).
  Since #800 nexus.sh does so on the docker repository, so the probe measures its blob cells;
  the runs on this page were made with the setting on.
- **The OCI format refuses names the specification allows.** A component with `--`, `---` or
  `__` is 400 `NAME_INVALID` "Repository name does not match OCI grammar" (`a--b.txt`,
  `h0__bc37`, `a__b__c`; `a-b.txt`, `a_b` and `a.b` are 201): the 1.0 grammar, as on Gitea. The
  docker format takes all seven. Since ADR 0017 Lajstrom writes neither (`h0_h…`, `_2d_2d`);
  names written before it cannot be stored on this format (reindex's `TestIntegrationSplit`,
  which writes them).
- **A blob a manifest uses is 409 `BLOB_REFERENCED`** to its delete, on both formats (the
  probe's cleanup of the empty blob): another code outside the spec's list.
- The probe's other cells agree with this page and fill rows it had left: the routing words
  are ok (20 blob `HEAD`s in each name, all 200), no read-after-write lag in three writes, and
  on the OCI format 8 MiB in one `PUT` and in one `PATCH`. An emptied repository's tag list is
  404 `NAME_UNKNOWN`, as [Delete](#delete) says, but it stays in `_catalog`.

The integration tests, `-run 'TestIntegration|TestAptGet' ./internal/...`:

| Test | docker | OCI |
| --- | --- | --- |
| provider `TestIntegration` | pass; the delete steps skip (`deleteManifest=false`) | **fail**, one step: open blob with a range is 200 with the whole blob, as #352 meant, and the test wants 206; the steps after it pass |
| provider `TestIntegrationUploads` | **fail**: cancel, a `PATCH` after it 500 | pass |
| ingest | **fail**: `TestIntegrationDeleteReferrerIndexRepair` and `…ClientGone`, the delete refused | pass |
| raw `TestIntegration` | pass | **fail**: a range is 200 with the whole blob; the test wants 206 |
| reindex `TestIntegration` | pass | pass |
| reindex, the others | **fail**: `TestIntegrationRepositoryUnknown` (a repository Nexus never had answers `MANIFEST_UNKNOWN`, and the delete of the version is refused), `TestIntegrationRepairReferrers` (the test deletes the `sha256-<hex>` index by digest, and its tag stays) | **fail**: `TestIntegrationSplit` (`NAME_INVALID`), `TestIntegrationFormats/apt` (the rebuild finds no OCI repository for the package whose only version was deleted; the index had one). Zot passes both |
| facade `TestIntegrationDelete`, `…DeleteReferrer`, `…CatalogAfterDelete` | **fail**: 405 `UNSUPPORTED`, the refusal of #352 | pass |
| facade `TestIntegrationBlobReads` | pass (item 1) | **fail**: 500 to a range, on the replica that never probed (#797) |
| facade, the rest | pass | pass |
| the apt tests | pass | pass |
| probe `TestIntegrationRun` | **fail**: the blobs refused for their content; blobs left behind, 409 `BLOB_REFERENCED` | **fail**: blobs left behind, 409 `BLOB_REFERENCED` |

The e2e scenarios:

| Scenario | docker | OCI |
| --- | --- | --- |
| `raw.sh` | **fail** at the version delete through the API: 501 | **fail**: a range is 200, want 206 |
| `oras.sh` | **fail** at the delete through `/raw/`: 501 | pass |
| `cosign.sh` | sign, verify, discover, the tag schema in the backend and reindex pass; **fail** at its delete: 501 | pass |
| `pypi.sh` | pass | **pass** (400 on 2026-11-10) |

The failures, by owner:

- **Nexus's:** the 500 to a `PATCH` of a cancelled session (docker), the 500 to every range
  (OCI, worked around), the names (OCI), the blobs refused for their content (docker, a
  setting), `BLOB_REFERENCED`.
- **Ours, the code:** the quirks before the first probe, #797 (fixed since, not measured again).
- **Ours, the tests:** the tests that delete, and those that want a 206, assume a registry that
  deletes and serves ranges. On these formats Lajstrom refuses deletes (docker) and answers a
  range with the whole blob (OCI) on purpose, so they fail where they could skip or take the
  200; so does `TestIntegrationRepositoryUnknown` on a registry that answers `MANIFEST_UNKNOWN`
  for a repository it never had and cannot delete. **Since #800** they read the provider's
  capabilities: the delete tests of ingest, reindex and the facade skip, or take the `405`
  and check that nothing changed, where `deleteManifest` is false; the range tests of the
  provider and raw take the `200` with the whole blob where the provider reads blobs whole;
  reindex's `TestIntegrationSplit` skips a name the registry answers `NAME_INVALID`, and
  `TestIntegrationFormats` no longer wants the repository of a package whose versions were all
  deleted to stay in `_catalog`. Not run against Nexus again: against Zot behind a front that
  answers as each format does (`Server: Nexus/…`; no referrers API, or a `500` to a range).
  The reindex tests and the e2e scenarios ran against Nexus on 2026-11-14:
  [Again on 2026-11-14](#again-on-2026-11-14).

## Again on 2026-11-14

For #800: the same image on a new volume, `nexus.sh` of #1012 (`strictContentTypeValidation`
off on the docker repository, on on the oci one). The tests of reindex at main 20ce9499, with
those of #940; the e2e scenarios that need no image of their own at 696c7394 with #1031 and
#1032 (only docs changed between the two), the others later the same day at a58a3f47 with #1104,
on another new volume; and `curl` and `oras` by hand. The start logged the vendor table as on
2026-11-12: `deleteManifest=false` on the docker format, `blobsReadWhole=true` on the oci one.

**Reindex: the two failures of the oci format are the tests', not Lajstrom's.**
`-run TestIntegration ./internal/index/reindex/` over SQLite and Postgres passes on both
formats since #940; `TestIntegrationRepairReferrers` skips on both, on the docker format for
`deleteManifest` false, on the oci one for its referrers API. With the checks of before #940
back:

- `TestIntegrationFormats/apt` deletes `goodbye`, the only version of its package, through the
  write path, drops the derived rows and rebuilds them from the registry, then wants the
  `oci_repos` of before. The rebuilt rows lacked `apt/it-…/goodbye`, and nothing else. Nexus
  drops a repository with its last manifest, on both formats; Zot keeps it, empty. A manifest
  pushed with `oras push` to a new repository, then deleted by digest:

  ```text
                                     Nexus docker           Nexus oci              Zot
  in _catalog, at once, 5 s, 30 s    no                     no                     yes
  GET tags/list                      404 NAME_UNKNOWN       404 NAME_UNKNOWN       200 {"tags":[]}
  ```

  Ingest's delete keeps the `oci_repos` row of a managed repo, which goes after reconcile's
  grace age ([index-schema.md](../spec/index-schema.md)); a rebuild finds the repositories the
  registry has, and reindex takes a tag list `404 NAME_UNKNOWN` for a repository that is gone.
  Right on both sides: the test now leaves out a repository that holds no version and that the
  rebuild did not find, and wants every other.
- `TestIntegrationSplit` pushes a raw file straight to the OCI name a Lajstrom before ADR 0017
  wrote. On the oci format two of its three names are `400 NAME_INVALID` "Repository name does
  not match OCI grammar": `a--b.txt`, and `h0__bc37…`, the earlier form of `.gitignore` (the 1.0
  grammar, [above](#again-after-352)). Nothing at such a name can be in this format, so the
  test skips them; `k8s/manifests/app.yaml` passes. The docker format takes all three.

This corrects "an emptied repository's tag list is 404 `NAME_UNKNOWN` … but it stays in
`_catalog`" of the probe's run [above](#again-after-352): the repository itself leaves
`_catalog` with its last manifest. The probe's cell looks for any repository below its scratch
prefix, where its cleanup had left blobs (`BLOB_REFERENCED`); not followed up.

**The e2e scenarios** take the answers of the vendor table since #800: `run.sh` reads
`deleteManifest` and `blobsReadWhole` from the server's log and passes them on
([testing.md](../testing.md#a-registry-that-deletes-nothing-or-reads-blobs-whole)).
`test/e2e/run.sh` with the scenarios below; each passes on each format:

| Scenario | docker (`deleteManifest=false`) | oci (`blobsReadWhole=true`) |
| --- | --- | --- |
| `raw.sh` | pass: the version delete `501 UNAVAILABLE` naming the registry, R-07 and the cleanup `501`, every version still listed and served | pass: the range `200` with all 3000 bytes |
| `oras.sh` | pass: the deletes through `/raw/` `501`, `oras manifest delete` refused with `UNSUPPORTED`, the delete by one of two tags `405`; the manifests and tags still there | pass |
| `cosign.sh` | pass: the delete `501`; the signature still a referrer, the file, the signature, the legacy `.sig` and the tag-schema index still served | pass |
| `auth.sh`, `rbac.sh` | pass: the maintainer's and the admin's deletes `501` on `/raw/`, `405 UNSUPPORTED` on `/v2`; the others' refusals as on Zot | pass |
| `retention.sh` | pass: `--apply` exits 1, the version an error naming the registry, nothing deleted | pass |
| `first-admin.sh` | pass; before #1032 its servers had no registry credentials and were never ready | the same |
| `apt.sh` | pass: both version deletes through the API `501 UNAVAILABLE` naming the registry; the next Packages, `apt-get update` and `apt-cache show` still have both packages | pass |
| `rpm.sh` | pass: the version delete `501 UNAVAILABLE` naming the registry; primary, filelists and other the same bytes after it, `dnf list` still has the package; the reindex rebuilds the repo with both | pass |
| `npm.sh` | pass: the maintainer's `npm unpublish` of a version and of a package fail with `E501`; every version and dist-tag stays, and the package is there after the reindex | pass |
| `docker.sh` | pass; before #1104 D-07 wanted no `sha256-<hex>` tag in the reader's `crane ls`, where the facade writes the referrers tag schema | pass; before #1104 the same, with the tags Nexus writes itself |
| `ociproxy.sh` | pass, the upstream another registry (a Zot, anonymous), containerd in a local busybox image instead of `debian:12` (the disk limit below); before #1104 it read the backend without credentials | the same |
| `oci-token.sh`, `tokens.sh` | pass | pass |

`apt.sh`, `rpm.sh` and `npm.sh` failed on the docker format at their deletes before #1104, which
makes them want the refusals. npm retries a `501` twice, 10 and 60 s apart, so an `npm unpublish`
on this format takes more than a minute to fail; `npm.sh` passes `--fetch-retries=0`.

Not run: `pypi` and `maven` (the disk was below the limit for pulling their client images; `pypi.sh`
passed on both formats on 2026-11-12, [above](#again-after-352)), `migrate-npm` (no Verdaccio),
and `oidc` and `ha`, which #800 did not list.

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

The probe and the e2e scenarios of [Again after #352](#again-after-352), the password in a file:

```sh
printf lajstrom > nexus.pw
bin/lajstrom registry probe --url http://localhost:5008 --url http://localhost:5015 --username lajstrom --password-file nexus.pw
LAJSTROM_E2E_REGISTRY=localhost:5015 LAJSTROM_E2E_REGISTRY_USER=lajstrom LAJSTROM_E2E_REGISTRY_PASSWORD_FILE=nexus.pw \
  test/e2e/run.sh raw oras cosign pypi
```
