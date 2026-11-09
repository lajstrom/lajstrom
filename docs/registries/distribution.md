# distribution

What distribution 3.1.2 (`registry:3`, the CNCF Distribution project's registry) answered on
2026-11-09, measured with `curl`, `oras` and the integration tests (issue #17), on its filesystem
storage and on its `s3` driver against SeaweedFS and MinIO. It is the registry the dev stack and
CI run, and the code base of Harbor's registry (an older build, [harbor.md](harbor.md)) and of
the GitLab container registry (a fork, [gitlab.md](gitlab.md)). The [summary row](README.md) is
in the README; this page has the details.

## Version tested

- distribution **3.1.2** (released 2026-09-24), the image `registry:3.1.2`, index
  `sha256:38907510915c3f1604c7e83d6ab602ab3020d092f73bdaed8696773693ff768c`, the pin of the
  [Versions table](README.md#versions-tested). The running containers have that image, and
  `registry:3` still points at the same index (`crane digest registry:3`, 2026-11-09).
  `registry --version` says `github.com/distribution/distribution/v3 3.1.2`; the log says
  `go.version=go1.26.8`.
- The three of the dev stack: `distribution` on `localhost:5001`, filesystem storage with the
  image's configuration (`/etc/distribution/config.yml`: deletes enabled) and
  `REGISTRY_LOG_LEVEL=info`; `distribution-s3` on `localhost:5002`, the `s3` driver on
  SeaweedFS 4.52 (profile `s3`); `distribution-s3-minio` on `localhost:5006`, on MinIO
  2025-07-23 (profile `s3-minio`).
- What the dev stack does not run, as scratch containers of the same image with a
  configuration of their own ([Run it](#run-it)): deletes disabled; an empty registry, for exact
  counts of repositories and tags; one registry per 1 GiB blob; htpasswd; and on both stores
  `storage.redirect.disable` and the driver's `redirectendpoint`.
- On macOS with OrbStack, arm64; curl 8.7.1, oras 1.3.6; the tests with Go 1.27.3 at c9405e4.

## Run it

The dev stack ([dev-stack.md](../dev-stack.md)) runs `distribution`; the services of the two S3
profiles start and stop alone, without touching the rest of the stack:

```sh
make dev-up                                         # zot, distribution (localhost:5001), postgres
docker compose -f deploy/compose/compose.yaml --profile s3 up -d --wait seaweedfs seaweedfs-bucket distribution-s3
docker compose -f deploy/compose/compose.yaml --profile s3-minio up -d --wait minio distribution-s3-minio
docker compose -f deploy/compose/compose.yaml stop seaweedfs distribution-s3 minio distribution-s3-minio
```

Each profile was healthy in under 10 s, the images pulled and the volumes there from earlier
runs. The other variants are `registry:3.1.2` with a configuration mounted on
`/etc/distribution/config.yml`, which replaces the image's. On the filesystem, the image's own
configuration without its debug server and at log level `info`:

```yaml
version: 0.1
log:
  level: info
storage:
  delete:
    enabled: true            # false for the variant without deletes
  cache:
    blobdescriptor: inmemory
  filesystem:
    rootdirectory: /var/lib/registry
  tag:
    concurrencylimit: 5
http:
  addr: :5000
```

On S3, on the dev stack's network, reaching the store by its service name, with a root
directory of its own in the same bucket:

```yaml
version: 0.1
log:
  level: info
storage:
  delete:
    enabled: true
  redirect:
    disable: true                             # the variant that streams
  s3:
    region: us-east-1
    regionendpoint: http://seaweedfs:9000     # http://minio:9001 on MinIO
    # redirectendpoint: http://localhost:9000 # the other variant, without redirect.disable
    forcepathstyle: true
    secure: false
    bucket: distribution
    accesskey: lajstrom
    secretkey: lajstrom-dev
    rootdirectory: /spike17
http:
  addr: :5000
```

```sh
docker run -d --name spike17-fs -p 127.0.0.1:5150:5000 -v "$PWD/config.yml:/etc/distribution/config.yml:ro" registry:3.1.2
docker run -d --name spike17-seaweedfs-noredirect --network lajstrom-dev_default -p 127.0.0.1:5152:5000 \
  -v "$PWD/s3-seaweedfs-noredirect.yml:/etc/distribution/config.yml:ro" registry:3.1.2
docker rm -fv spike17-fs spike17-seaweedfs-noredirect   # -v: the image declares a volume on /var/lib/registry
```

| Container | Port | Storage | For |
| --- | --- | --- | --- |
| `spike17-fs` | 5150 | filesystem, deletes enabled, empty at the start | [Catalog](#catalog), [Tags](#tags), [Sizes](#sizes), [Garbage collection](#garbage-collection) |
| `spike17-fs-nodelete` | 5151 | filesystem, `delete.enabled: false` | [Delete](#delete) |
| `spike17-big` | 5156 | filesystem, a new one per blob | [Sizes](#sizes) |
| `spike17-auth` | 5157 | filesystem, htpasswd | [Authentication](#authentication) |
| `spike17-seaweedfs-noredirect`, `spike17-minio-noredirect` | 5152, 5153 | `s3`, `redirect.disable: true` | [Blob redirect](#blob-redirect), [Sizes](#sizes) |
| `spike17-seaweedfs-redirectendpoint`, `spike17-minio-redirectendpoint` | 5154, 5155 | `s3`, `redirectendpoint` | [Blob redirect](#blob-redirect) |

Every repository pushed was below `spike17/`.

## The row

The cells of the [Capabilities](README.md#capabilities) table. S3 is the same on SeaweedFS and on
MinIO unless the cell says otherwise; deletes are enabled unless it says otherwise.

| | Filesystem (the dev stack) | S3 (SeaweedFS, MinIO) |
| --- | --- | --- |
| Create on push | [yes, with the first manifest; blobs alone make no repository](#create-on-push) | [yes](#our-tests-against-it) |
| Where a repository may be created | [anywhere, inside another one too; at most 255 characters](#create-on-push) | [anywhere](#catalog) |
| Referrers API | [no: a plain-text 404; oras and Lajstrom use the tag schema](#referrers) | [the same](#referrers) |
| `OCI-Subject` on a push with `subject` | [no](#referrers) | [no](#referrers) |
| Catalog (`_catalog`) and its limits | [yes, `n`/`last`/`Link`: 100 without `n`, `n` at most 1000; 150 repositories paged](#catalog) | [yes, `n`/`last`/`Link`; order by object key](#catalog) |
| A listing API of its own | [none](#catalog) | [none](#catalog) |
| Tag list order and pagination | [byte order; all without `n`, `n` above 1000 clamped with `Link`](#tags) | [the same; a missing repository is 200 `[]`](#tags) |
| Delete a manifest by digest | [yes, 202, its tags with it; 405 `UNSUPPORTED` with deletes disabled](#delete) | [yes, 202, its tags with it](#delete) |
| Delete by tag | [yes, 202: the manifest stays; 405 with deletes disabled](#delete) | [the same](#delete) |
| Delete a repository | [no API; it stays in `_catalog` without tags](#delete) | [no API; it leaves `_catalog` with its last manifest](#delete) |
| Immutable tags, where the rule lives | [none: no option; a tag re-points with 201](#immutable-tags) | [the same](#immutable-tags) |
| Blob redirect | [none: the bytes](#blob-redirect) | [307 to a URL presigned for 20 min; `redirect.disable` streams](#blob-redirect) |
| `Range` and `Accept-Ranges` | [206; 416 past the end; `Accept-Ranges: bytes`](#blob-redirect) | [206 from the store after the 307; two ranges 200](#blob-redirect) |
| Largest blob measured | [1 GiB: one `PUT`, four `PATCH`es, one streamed `PATCH`; no limit](#sizes) | [1 GiB in one `PUT`; the driver: 10 000 parts of 10 MiB (read)](#sizes) |
| Manifest size limit | [4 MiB; one byte more is 400 `MANIFEST_INVALID`](#sizes) | *not measured* |
| Lajstrom's manifest profile (`artifactType`, empty config, own media types) | [accepted](#media-types) | [accepted](#media-types) |
| Auth | [none; Basic (htpasswd)](#authentication) | [none](#version-tested) |
| Rate limits seen | [none](#our-tests-against-it) | [none](#our-tests-against-it) |
| Upload: chunk out of order | [416 `RANGE_INVALID`](#uploads) | [416 `RANGE_INVALID`](#uploads) |
| Upload: chunk size limit | [none met: 256 MiB a `PATCH`, and 1 GiB in one](#sizes) | *not measured* |
| Upload: session status (`GET`) | [204 with `Range`](#uploads) | [204; 500 after a refused digest](#uploads) |
| Upload: cancel (`DELETE`) | [204, then 404](#uploads) | [204; 500 after a refused digest](#uploads) |
| Upload: wrong digest, empty blob | [a digest mismatch; stored (the test)](#our-tests-against-it) | [the same (the test)](#our-tests-against-it) |
| Upload continued by another replica | [works (the test)](#our-tests-against-it) | [works (the test)](#our-tests-against-it) |
| Garbage collector and an untagged referrer (#37) | [kept; with `--delete-untagged`, kept while a tag-schema index lists it](#garbage-collection) | *not measured* |
| What the probe finds | [`referrers=false catalog=true`](#the-probe) | [`referrers=false catalog=true`](#the-probe) |
| Overrides needed | [none; with deletes disabled, `deleteManifest: false` saves one refused delete](#the-probe) | [none; `blobRedirect: false` when clients cannot reach the store](#the-probe) |

## The probe

`TestIntegration` of the provider, with `-v`, logged the same line on the dev stack's three, the
registry without deletes, the one with htpasswd (and the credentials) and the one on SeaweedFS
with redirects off:

```text
level=INFO msg="registry capabilities" registry=integration vendor=unknown createOnPush=true referrers=false catalog=true deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0
```

Against the measurements:

| Capability | The probe | Measured | |
| --- | --- | --- | --- |
| `createOnPush` | true, not probed | true, everywhere | agrees |
| `referrers` | false: the plain 404 has no `NAME_UNKNOWN` | no referrers API | agrees |
| `catalog` | true: `_catalog?n=1` is 200 | 200 with `Link` pagination | agrees |
| `deleteManifest` | true, not probed | false with `delete.enabled: false` | **disagrees until the first delete**: its 405 turns it false (the log warns "registry refuses manifest deletes; DeleteManifest is now false") |
| `immutableTags` | false, not probed | no option; a tag re-points | agrees |
| `blobRedirect` | true, not probed | 307 on S3, none on the filesystem | agrees: it means "may return a redirect", the operator's switch |
| `maxBlobSize` | 0, unknown | no limit met at 1 GiB | agrees: nothing tells it |

`vendor=unknown`: distribution says nothing about itself on `GET /v2/`
(`Docker-Distribution-Api-Version: registry/2.0` only), and the vendor table has nothing for
it. No override is needed. For a distribution with deletes disabled,
`capabilities: {deleteManifest: false}` makes the first delete through Lajstrom answer "the
backend registry cannot delete manifests" at once instead of after a refused request.

## What the provider must do differently

Nothing is required: the whole integration suite passes on the dev stack's distribution, and on
S3 everything but the cancel of a session after a refused digest, which the facade handles
since #24 ([Uploads](#uploads)). Three things are worth knowing:

1. Optional: delete the referrers of a manifest it deletes, where there is no referrers API.
   distribution deletes a subject alone: its referrers and the `sha256-<digest>` index that
   lists them stay, tagged, and no garbage collection takes them ([Delete](#delete),
   [Garbage collection](#garbage-collection)). The raw delete deletes the version's manifest by
   digest only. Harbor deletes the referrers with their subject; here they are left behind.
2. Optional: do not read an empty tag list as "the repository exists" on S3. `tags/list` of a
   repository that does not exist is 200 `{"tags":[]}` on the `s3` driver and 404 `NAME_UNKNOWN`
   on the filesystem ([Tags](#tags)). The facade's `tags/list` answers `NAME_UNKNOWN` when the
   provider says not found, so on S3 it answers an empty list for a path that was never pushed
   (read in `internal/facade/lists.go`, not run through Lajstrom). Reindex lists repositories
   from `_catalog`, which on S3 drops a repository with its last manifest, so it is not misled.
3. Optional: a `maxBlobSize` for S3. The `s3` driver uploads a blob as one multipart upload of
   `chunksize` parts (10 MiB by default), and S3 allows 10 000 parts: about 97.7 GiB a blob
   with the default (read in `registry/storage/driver/s3-aws/s3.go`, not pushed). The probe
   cannot know; `capabilities.maxBlobSize` can say it.

## Create on push

On the dev stack, a repository ten levels deep that did not exist:

```text
GET  /v2/spike17/create/a/b/c/d/e/f/g/h/i/j/tags/list   404 {"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known to registry",…}]}
POST /v2/spike17/blobonly/x/blobs/uploads/?digest=sha256:31eb9630…   (with the 14 bytes)
     202  Docker-Upload-Uuid, Location: …/blobs/uploads/01a1beed-…?_state=…, Range: 0-0
POST, then PUT <Location>&digest=sha256:31eb9630…   201
     _catalog: no spike17/blobonly/x;  tags/list: 404 NAME_UNKNOWN;  HEAD the blob: 200, Content-Type: application/octet-stream
POST + PUT the empty config and a layer to spike17/create/a/b/c/d/e/f/g/h/i/j   201, 201
PUT  /v2/spike17/create/a/b/c/d/e/f/g/h/i/j/manifests/v1   (Content-Type: application/vnd.oci.image.manifest.v1+json)
     201  Docker-Content-Digest: sha256:6c8b448c…, Location: …/manifests/sha256:6c8b448c…   (no OCI-Subject)
     _catalog: ["spike17/create/a/b/c/d/e/f/g/h/i/j"];  tags/list: {"name":…,"tags":["v1"]}
```

A repository is created by its first manifest, at any depth, and inside another one too
(`spike17/ord/pre` and `spike17/ord/pre/sub` both, [Catalog](#catalog)). Blobs alone make no
repository: it is not in `_catalog` and `tags/list` is 404, though the blob answers `HEAD`.
**A single `POST ?digest=` with the bytes opens a session (202) and ignores them**: distribution
has no monolithic `POST`, so the blob is uploaded with `POST` then `PUT`, as oras-go does.

Names: 255 characters are taken, 256 are 400 `NAME_INVALID` "repository name must not be more
than 255 characters" (with the error object written twice in the body, 845 bytes: two JSON
documents one after the other); a name outside the grammar (`spike17/Upper`, `spike17/-a`,
`spike17/a..b`) is a plain-text `404 page not found`, the route does not match. Lajstrom's
profile makes names lowercase and at most 255 characters before they reach the registry
([manifest-profile.md](../spec/manifest-profile.md)).

A manifest `GET` or `HEAD` needs an OCI type in `Accept`, as on Harbor: without one, with
`*/*` or with Docker's v2 type alone, 404 `MANIFEST_UNKNOWN` "OCI manifest found, but accept
header does not support OCI manifests", by tag and by digest. With
`Accept: application/vnd.oci.image.manifest.v1+json`, 200 with `Content-Type`,
`Docker-Content-Digest` and an `Etag` of the digest.

## Referrers

oras pushes the raw profile and attaches a signature; distribution has no referrers routes:

```text
$ oras push --plain-http --artifact-type application/vnd.lajstrom.raw.v1 localhost:5001/spike17/ref/pkg:v1 hello.txt:application/vnd.lajstrom.raw.file.v1
Pushed [registry] localhost:5001/spike17/ref/pkg:v1
Digest: sha256:2092fb59…
$ oras attach --plain-http --debug --artifact-type application/vnd.lajstrom.test.signature.v1 localhost:5001/spike17/ref/pkg:v1 sig2.txt:…
  HEAD manifests/v1                         200
  HEAD blobs/<config>, blobs/<layer>        200, 404;  POST blobs/uploads/ 202;  PUT 201
  PUT  manifests/sha256:b04e5f05…           201   (no OCI-Subject)
  GET  manifests/sha256-2092fb59…           200   the index of the tag schema
  PUT  manifests/sha256-2092fb59…           201   the index with the new referrer
$ oras repo tags --plain-http localhost:5001/spike17/ref/pkg
sha256-2092fb598702388ee7cf94f5ac896cb7f110537bb3eedde0d6f611c20b83af6e
v1
$ oras discover --plain-http --debug localhost:5001/spike17/ref/pkg:v1
  GET manifests/v1                          200
  GET referrers/sha256:2092fb59…            404
  GET manifests/sha256-2092fb59…            200
└── application/vnd.lajstrom.test.signature.v1
    ├── sha256:1c113f61…
    └── sha256:b04e5f05…

GET /v2/spike17/ref/pkg/referrers/sha256:2092fb59…                       404  Content-Type: text/plain; charset=utf-8  "404 page not found"
GET /v2/spike17/ref/pkg/referrers/sha256:2092fb59…?artifactType=…        404, the same
GET /v2/lajstrom-probe/referrers/sha256:0000…0000   (the probe's request)  404, the same
PUT a referrer by curl, subject sha256:2092fb59…                          201, no OCI-Subject; the index is not updated
```

The same on SeaweedFS and on MinIO: the plain 404s, no `OCI-Subject`, the tag `sha256-<hex>`
written by oras. The registry neither serves referrers nor maintains the index: a client that
pushes a manifest with `subject` and does not update the index itself (the curl referrer above)
leaves a referrer nobody lists. oras-go, and so the provider, updates it; the provider's
`TestIntegration` attaches and lists a referrer on all three.

## Catalog

On `spike17-fs`, empty at the start (`{"repositories":[]}`), 150 repositories
`spike17/cat/r001` to `r150`, each with the raw profile's manifest (750 requests, 31 s):

```text
GET /v2/_catalog                      200  Link: </v2/_catalog?last=spike17%2Fcat%2Fr100&n=100>; rel="next"   100: r001 … r100
GET /v2/_catalog?n=50                 200  Link: </v2/_catalog?last=spike17%2Fcat%2Fr050&n=50>; rel="next"    50: r001 … r050
GET <Link>                            200  Link: …last=spike17%2Fcat%2Fr100&n=50…                             50: r051 … r100
GET <Link>                            200  no Link                                                            50: r101 … r150
GET /v2/_catalog?n=60, then the Links 60, 60, then 30 without Link
GET /v2/_catalog?n=0                  200  {"repositories":[]}
GET /v2/_catalog?n=1000               200  all 150, no Link
GET /v2/_catalog?n=1001               400  {"errors":[{"code":"PAGINATION_NUMBER_INVALID","message":"invalid number of results requested","detail":{"n":1001}}]}
GET /v2/_catalog?n=-1, ?n=abc         400  PAGINATION_NUMBER_INVALID
GET /v2/_catalog?n=2&last=spike17/cat/r075     r076, r077;  last=spike17/cat/r075x (no such repository): the same
GET /v2/_catalog?n=2&last=zzz         200  {"repositories":[]}
```

Without `n` a page is 100, `catalog.maxentries` (1000 by default) is the largest `n`, and more
is an error, not a shorter page. `Link` is relative, with `last` and `n`; a last page that is
exactly full has no `Link`. `last` need not be a repository. The provider asks for pages of 100
and follows `Link` ([provider.go](../../internal/provider/generic/provider.go)), so 150
repositories are two requests.

The order is not byte order, and it is not the same on both drivers. Eight names around `/`,
each paged with `n=1` from `last=spike17/ord`:

```text
filesystem   pre, pre/sub, pre/sub/deeper, pre-x, pre.y, pre0, prea, prefix     (component by component: "/" sorts first)
SeaweedFS    pre-x, pre.y, pre, pre/sub, pre/sub/deeper, pre0, prea, prefix     (by object key: "pre-x/" < "pre.y/" < "pre/")
MinIO        the same as SeaweedFS
```

On every driver the pages followed one another with no repeat and no gap. The provider walks the
whole catalog and sorts the names itself, so the order does not matter to it. distribution
has no listing API besides `_catalog`.

## Tags

`spike17/tags/pkg` on `spike17-fs`, one manifest under 1006 tags: `t0001` to `t1000`, `1.10`,
`1.2`, `A`, `Z`, `_x`, `a` (39 s):

```text
GET tags/list                       200  1006 tags, no Link: ["1.10","1.2","A","Z","_x","a","t0001",…,"t1000"]
GET tags/list?n=100                 200  Link: </v2/spike17/tags/pkg/tags/list?last=t0094&n=100>; rel="next"   100 tags
    following Link                  11 pages, 1006 tags; the last one 6 tags, no Link
GET tags/list?n=1001                200  Link: </v2/spike17/tags/pkg/tags/list?last=t0994&n=1000>; rel="next"  1000 tags
GET tags/list?n=503                 200  Link: …last=t0497&n=503…
GET tags/list?n=503&last=t0497      200  Link: …last=t1000&n=503…   503 tags, through t1000
GET tags/list?last=t1000            200  {"name":"spike17/tags/pkg","tags":[]}
GET tags/list?n=0                   200  {"tags":[]}, no Link
GET tags/list?n=3&last=t0005x       t0006, t0007, t0008 (last need not be a tag)
GET tags/list?n=-1, ?n=x            400  PAGINATION_NUMBER_INVALID
GET /v2/spike17/tags/nope/tags/list 404  {"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known to registry",…}]}
```

Tags are in byte order: upper case before lower case, `_` between them, where distribution-spec
1.1.1 asks for "lexical order (i.e. case-insensitive alphanumeric order)". Lajstrom's tags can
hold upper case (`Escape` keeps `A–Z`: `1.0-SNAPSHOT`), but nothing in Lajstrom reads the
backend's order. Without `n` the answer
is every tag, however many; with `n` above `tags.maxtags` (1000 by default) the page is clamped
and `Link` follows. A page that is exactly full links to one more, empty page. The provider
follows `Link` with pages of 100; the facade sorts the tags it serves in byte order too
([lists.go](../../internal/facade/lists.go)).

On SeaweedFS and MinIO, 16 tags paged by 4, the same (order, `Link`, the empty page after a full
one), with one difference:

```text
GET /v2/spike17/tags/nope/tags/list   200  {"name":"spike17/tags/nope","tags":[]}     (both stores; filesystem: 404 NAME_UNKNOWN)
```

## Delete

On the dev stack (deletes enabled), `M` under the tags `t1`, `t2`, `t3` and `M2` under `u1`, in
`spike17/del/pkg`:

```text
DELETE manifests/t1                   202   GET t1: 404, M by digest: 200, t2: 200; tags ["t2","t3","u1"]
DELETE manifests/t1 again             404 {"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown","detail":"filesystem: Path not found: /docker/registry/v2/repositories/spike17/del/pkg/_manifests/tags/t1"}]}
DELETE manifests/sha256:<M>           202   M, t2, t3: 404; u1: 200; tags ["u1"]
DELETE manifests/sha256:<M> again     404 {"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}
DELETE manifests/sha256:0000…0000     404 MANIFEST_UNKNOWN
DELETE manifests/sha256:abc           400 {"errors":[{"code":"DIGEST_INVALID","message":"provided digest did not match uploaded content","detail":"invalid checksum digest length"}]}
HEAD   blobs/<M's layer>              200   (a manifest delete leaves its blobs)
DELETE blobs/<M's layer>              202;  HEAD: 404;  again: 404 BLOB_UNKNOWN
DELETE manifests/sha256:<M2>          202   tags/list: 200 {"tags":[]};  still in _catalog
DELETE /v2/spike17/del/pkg/           404 text/plain "404 page not found"   (no repository delete)
```

A digest delete removes the manifest and every tag that points at it; a tag delete removes
the tag and leaves the manifest, reachable by digest. The detail of a missing tag names the
storage path. On SeaweedFS and MinIO the same answers (the detail says `s3aws: Path not
found: …`), but **the repository leaves `_catalog` with its last manifest**: S3 has no empty
directories.

A subject with referrers in the tag schema (`spike17/ref/pkg` of [Referrers](#referrers)),
deleted by digest:

```text
DELETE manifests/sha256:2092fb59…   202   tags ["sha256-2092fb59…"]: v1 is gone, the index stays
GET manifests/sha256-2092fb59…      200   GET each referrer: 200
```

With `delete.enabled: false` (`spike17-fs-nodelete`), every delete is the same 405, the
manifest by tag, by digest, a digest that does not exist, a blob:

```text
DELETE manifests/t1                 405 {"errors":[{"code":"UNSUPPORTED","message":"The operation is unsupported."}]}
DELETE manifests/sha256:<M>         405 the same
DELETE blobs/<layer>                405 the same
DELETE <upload session>             204, then GET of it: 404   (the cancel is not a delete)
```

The provider's `TestIntegration` against it: the delete step is skipped, "the registry refused
the delete and DeleteManifest is now false: … provider: unsupported (HTTP 405, UNSUPPORTED)".

## Immutable tags

distribution has no option for them (none in the configuration reference of 3.1.2). In
`spike17/imm/pkg`, `a` pushed as `v1`, then `b`:

```text
PUT manifest b as v1      201   Docker-Content-Digest: sha256:6bea3e2b… (b);  v1 is now b
GET a by digest           200   (untagged, kept)
PUT b as v1 again         201;  PUT a as v1 again: 201, v1 is a again
```

The same on SeaweedFS and MinIO. Lajstrom's tags are immutable by its own rule: ingest resolves
the tag first and answers 409 on another digest, whatever the registry does.

## Blob redirect

The filesystem driver never redirects; the `s3` driver redirects `GET` and `HEAD` of a blob,
not of a manifest. A 300 000-byte layer in `spike17/redir/pkg`:

```text
localhost:5001 (filesystem)
GET  blobs/sha256:a19f0caf…   200  Accept-Ranges: bytes, Content-Length: 300000, Docker-Content-Digest, Etag, Cache-Control: max-age=31536000
HEAD blobs/sha256:a19f0caf…   200  the same headers
Range: bytes=0-9              206  Content-Range: bytes 0-9/300000
GET  blobs/sha256:0000…0000   404  BLOB_UNKNOWN

localhost:5002 (SeaweedFS)
GET  blobs/sha256:f3554cc6…   307  Location: http://localhost:9000/distribution/docker/registry/v2/blobs/sha256/f3/f3554cc6…/data
                                     ?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=lajstrom%2F20261109%2Fus-east-1%2Fs3%2Faws4_request
                                     &X-Amz-Date=20261109T061526Z&X-Amz-Expires=1200&X-Amz-SignedHeaders=host&X-Amz-Signature=<64 hex>
HEAD blobs/sha256:f3554cc6…   307  the same, signed for HEAD (another signature)
GET  <Location>               200  300000 bytes, the blob's digest
GET  <Location>, Range: bytes=0-9          206  Content-Range: bytes 0-9/300000
curl -L, Range: bytes=0-9 on the registry  206, one redirect
<Location> with another signature          403 SignatureDoesNotMatch
GET  manifests/v1             200  (no redirect)

localhost:5006 (MinIO): the same, Location http://localhost:9001/distribution/docker/registry/v2/blobs/…
```

The URL names the endpoint distribution signs for, `regionendpoint` (the dev stack's trick that
makes it `localhost:<port>` for both, [dev-stack.md](../dev-stack.md#the-s3-profile)), and
lives 20 minutes, `X-Amz-Expires=1200`: fixed in the driver (`RedirectURL`), not configurable.
Both stores enforce it: the URLs above answered 200 at 19 min 57 s and 403 `AccessDenied`
"Request has expired" at 20 min 13 s. Through the redirect a range is the store's: one range
206, past the end 416 `InvalidRange` (XML), two ranges or a malformed one 200 with the whole
blob (the filesystem: 206 `multipart/byteranges`, and 416). `format.ServeBlob` asks for one
range or none.

**`storage.redirect.disable: true`** (`spike17-seaweedfs-noredirect`, `spike17-minio-noredirect`):
`GET` and `HEAD` are 200 with the bytes and `Accept-Ranges: bytes`, a range 206, as on the
filesystem. Lajstrom's `capabilities: {blobRedirect: false}` gets the same for its clients
without touching the registry: the provider follows the 307 and streams.

**`redirectendpoint`**, in the driver's documentation since 3.1.2
(`spike17-seaweedfs-redirectendpoint`, `spike17-minio-redirectendpoint`): distribution talks to
`http://seaweedfs:9000` (or `minio:9001`) on the compose network and signs its redirects for
`http://localhost:9000` (or `9001`):

```text
GET blobs/sha256:334d6afc…   307  Location: http://localhost:9000/distribution/spike17/docker/registry/v2/blobs/sha256/33/334d6afc…/data?…X-Amz-Expires=1200…
GET <Location> from the host 200, the digest; Range 206; another signature 403 SignatureDoesNotMatch
```

The store checks the signature against the host the client names, and the client names the host
distribution signed for, so the URL works though distribution never talks to that host. This is
the option for a registry that reaches its store on an internal address while clients reach it
on a public one; the dev stack could use it instead of sharing the store's network namespace.

## Sizes

Blobs of zeros, 1 GiB (1073741824 bytes, `sha256:49bc20df…`), streamed from `/dev/zero` by curl
with `Content-Length`; each on a new `spike17-big`, removed after, for the disk's sake:

```text
POST, then PUT <Location>&digest=…, 1 GiB in the body           201 in 3 s;  HEAD: Content-Length: 1073741824;  GET back: the digest, 3 s
POST, then PATCH 0-268435455, 268435456-536870911, …, 805306368-1073741823 (Content-Range, 256 MiB each)
                                                                202, Range: 0-268435455 … 0-1073741823;  PUT ?digest=: 201, 3 s in all
POST, then one PATCH of 1 GiB, chunked transfer, no Content-Range   202, Range: 0-1073741823;  PUT ?digest=: 201
the same PUT of 1 GiB to spike17-minio-noredirect               201 in 8 s;  GET back: the digest
the same PUT of 1 GiB to spike17-seaweedfs-noredirect           201 in 7 s;  GET back: the digest
```

No limit was met: distribution sets none on a blob or a chunk. On S3 the driver writes a blob as
one multipart upload in parts of `chunksize`, 10 MiB by default (the minimum is 5 MiB), and S3
allows 10 000 parts, about 97.7 GiB with the default (read, not pushed). Larger blobs were not
tried: the machine had 3 GiB of disk left.

Manifests: 4 MiB. A manifest of 4194304 bytes (an annotation filled up) is 201, of 4194305 bytes
400 `{"errors":[{"code":"MANIFEST_INVALID","message":"manifest invalid","detail":"http: request
body too large"}]}`, where Harbor answers 413 and the spec says a registry SHOULD. The same
limit as Lajstrom's
`server.maxManifestSize` default, which refuses a larger manifest before it reaches the registry.

## Uploads

```text
POST blobs/uploads/                 202  Location (with _state), Docker-Upload-Uuid, Range: 0-0; no OCI-Chunk-Min-Length
PATCH abc, Content-Range: 0-2       202  Range: 0-2
PATCH x,   Content-Range: 1-1       416  {"errors":[{"code":"RANGE_INVALID","message":"invalid content range"}]}
PATCH x,   Content-Range: 3-3       202  Range: 0-3
GET  <session>                      204  Range: 0-3
PUT  <session>&digest=sha256(abcx)  201
POST blobs/uploads/?mount=<digest>&from=spike17/ref/pkg   201  Location of the blob in the new repository
another session, PATCH abc, then DELETE <session>        204;  its GET after: 404
```

The same on SeaweedFS and MinIO. Each `PATCH` answers a new `Location` (the `_state` changes);
a `PUT` to an older one is 416. On S3, after the closing `PUT` of a session is refused for its
digest, the session's `GET` and its cancel answer 500 `s3aws: append to zero-size path …
unsupported`, on both stores and with redirects off too: `TestIntegrationUploads/wrong_digest`
fails there, as in [Object stores behind distribution](README.md#object-stores-behind-distribution)
since #118. The facade cancels the backend session itself and answers its client 404
`BLOB_UPLOAD_UNKNOWN`.

## Media types

Nothing in Lajstrom's manifest profile is refused, on the filesystem or on either store:
`artifactType` `application/vnd.lajstrom.raw.v1`, the empty config `application/vnd.oci.empty.v1+json` with
`"data":"e30="`, the layer `application/vnd.lajstrom.raw.file.v1` with
`org.opencontainers.image.title`, the `lajstrom.*` annotations, a referrer with `subject` and
`artifactType` `application/vnd.lajstrom.test.signature.v1`. The manifest is stored as pushed:

```text
GET manifests/v1 (Accept: application/vnd.oci.image.manifest.v1+json)   200  Content-Type: application/vnd.oci.image.manifest.v1+json
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.lajstrom.raw.v1",
 "config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa3…","size":2,"data":"e30="},
 "layers":[{"mediaType":"application/vnd.lajstrom.raw.file.v1",…,"annotations":{"org.opencontainers.image.title":"hello.txt"}}],
 "annotations":{"lajstrom.format":"raw",…}}
```

A blob's `Content-Type` is `application/octet-stream` whatever its media type. The integration
suite, on every pull request in CI and [below](#our-tests-against-it), pushes the profile of raw
and apt, and OCI images through the facade.

## Garbage collection

The question of #37: does a referrer, a manifest with `subject` and no tag, survive the
collector? distribution's is offline, `registry garbage-collect`, run here with `--dry-run`
in `spike17-fs`. In `spike17/gc/pkg`: `M` tagged `v1`; `R1` and `R3` attached to `M` by oras, so
listed by the index tagged `sha256-<M>`, which the second attach replaced (the first index
stays, untagged); `R2`, a referrer of `M` pushed by digest with no index listing it; `N`, a
manifest by digest, no tag, no subject:

```text
registry garbage-collect --dry-run /etc/distribution/config.yml
    16 blobs marked, 0 blobs and 0 manifests eligible for deletion
registry garbage-collect --dry-run --delete-untagged /etc/distribution/config.yml
    manifest eligible for deletion: {spike17/gc/pkg sha256:262c2ada… […]}   the first index
    manifest eligible for deletion: {spike17/gc/pkg sha256:32b3a086… […]}   N
    manifest eligible for deletion: {spike17/gc/pkg sha256:df41d193… […]}   R2
    11 blobs marked, 5 blobs and 3 manifests eligible for deletion            M, the index, R1, R3 and their layers kept
```

Without `--delete-untagged` everything stays. With it, **an untagged referrer survives while a
tagged index lists it**, which is how the provider (oras-go) writes referrers on a registry
without the referrers API; a referrer that no index lists goes, as any untagged manifest. The
collector does not run by itself.

## Authentication

`spike17-auth`, htpasswd with bcrypt (`REGISTRY_AUTH=htpasswd`):

```text
GET /v2/                   401  Www-Authenticate: Basic realm="lajstrom"   {"errors":[{"code":"UNAUTHORIZED","message":"authentication required","detail":null}]}
GET /v2/, Basic            200
```

The provider's `TestIntegration` without credentials fails at once: "Capabilities: generic: ping
integration: provider: unauthorized (no credentials configured)"; with them `TestIntegration`
and `TestIntegrationUploads` pass, every step. No token auth was run on distribution.

## Our tests against it

`go test -race -count=1 -tags integration`, Go 1.27.3 at c9405e4, with
`LAJSTROM_TEST_REGISTRY_PREFIX=spike17/`:

| Registry | Tests | Result |
| --- | --- | --- |
| the dev stack, `localhost:5001` | the whole suite (`./...`; no Postgres DSN, so the Postgres tests skip) | pass, every package |
| `spike17-fs-nodelete`, `localhost:5151` | provider `TestIntegration` | pass; the delete step skipped (405) |
| `spike17-auth`, `localhost:5157`, Basic | provider `TestIntegration`, `TestIntegrationUploads` | pass |
| SeaweedFS `localhost:5002`, MinIO `localhost:5006`, SeaweedFS without redirects `localhost:5152` | provider `TestIntegration`, `TestIntegrationUploads` | `TestIntegration` pass; `TestIntegrationUploads` fails at `wrong_digest`, the cancel's 500 ([Uploads](#uploads)) |

No answer was a 429, in the tests or the curl runs: distribution has no rate limits.
`TestIntegrationS3Redirect` and `TestAptGetInstall/s3` were not run: they push below
`lajstrom-it/`; the redirects are measured in [Blob redirect](#blob-redirect) instead, and the
README has their last run.

## Commands

The helpers of the curl runs (`R` the registry, `N` the repository):

```sh
sha() { echo "sha256:$(shasum -a 256 "$1" | cut -d' ' -f1)"; }
# a blob: POST, then PUT <Location>&digest= with the bytes (a single POST ?digest= is a session)
put_blob() { loc=$(curl -s -o /dev/null -w '%header{location}' -X POST "http://$R/v2/$N/blobs/uploads/")
  curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/octet-stream' --data-binary @"$1" "$loc&digest=$(sha "$1")"; }
put_manifest() { curl -s -o /dev/null -w '%{http_code} %header{docker-content-digest}\n' -X PUT \
  -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' --data-binary @"$2" "http://$R/v2/$N/manifests/$1"; }
# a large blob, streamed with its length (curl -T - is chunked unless Transfer-Encoding is emptied)
head -c 1073741824 /dev/zero | curl -X PUT -T - -H 'Transfer-Encoding:' -H 'Content-Length: 1073741824' "$loc&digest=$D"
```

The rest:

```sh
curl -s "http://localhost:5150/v2/_catalog?n=50"                      # then the Link, until there is none
curl -si "http://localhost:5150/v2/spike17/tags/pkg/tags/list?n=100"
curl -si -X DELETE "http://localhost:5001/v2/spike17/del/pkg/manifests/t1"
curl -s -D - -o /dev/null "http://localhost:5002/v2/spike17/redir/pkg/blobs/$D"   # the 307 and its Location
oras attach --plain-http --debug --artifact-type application/vnd.lajstrom.test.signature.v1 localhost:5001/spike17/ref/pkg:v1 sig.txt:application/vnd.lajstrom.test.signature.v1
oras discover --plain-http --format tree localhost:5001/spike17/ref/pkg:v1
docker exec spike17-fs registry garbage-collect --dry-run --delete-untagged /etc/distribution/config.yml
LAJSTROM_TEST_REGISTRY=localhost:5001 LAJSTROM_TEST_REGISTRY_PREFIX=spike17/ go test -race -count=1 -tags integration ./...
LAJSTROM_TEST_REGISTRY=localhost:5002 LAJSTROM_TEST_REGISTRY_PREFIX=spike17/ go test -race -count=1 -tags integration \
  -run 'TestIntegration$|TestIntegrationUploads' -v ./internal/provider/generic/
```

What was pushed was deleted afterwards, manifests by digest and their blobs. On the dev stack's
filesystem the `spike17/` repositories stay in `_catalog` without tags (distribution has no
repository delete); on S3 they left it with their last manifest. The `/spike17` root directory
of the scratch registries was removed from both buckets (`mc rm --recursive`, `weed shell`
`fs.rm -r`), and the scratch containers with their volumes.
