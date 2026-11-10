# Zot

What Zot answered on 2026-11-09, measured with `curl`, `oras` and the integration tests (issue
#18). Zot is the dev stack's registry `local` and the one the integration job runs against on
every pull request, so this page also has the exact configuration Lajstrom needs from it. The
[summary row](README.md) is in the README; this page has the details.

## Version tested

- Zot **v2.1.23** (released 2026-11-02, the latest release on the day), the minimal build:
  `ghcr.io/project-zot/zot-minimal:v2.1.23`, image index
  `sha256:59cb5740832970818903c5ce104425762664c7880835ee566506aad397cd3458`, the pin of
  [Versions tested](README.md#versions-tested). On macOS arm64 (OrbStack) it runs
  linux/arm64/v8, `sha256:d8ec72e9…0571f3`; the binary is `zot-linux-arm64-minimal`, and its
  startup line says `"Commit":"v2.1.23-d7915e82e6693255338ef7fa9bb15300c82db020"`,
  `"GoVersion":"go1.27.3"`, `"BinaryType":"minimal"`. The index also carries freebsd/arm64
  and freebsd/amd64.
- The minimal build has none of Zot's extensions (search, UI, sync, scrub, lint, image trust);
  the referrers API, access control, the S3 driver and garbage collection are in the core, and
  in it. Each extension logs at startup that "given zot binary doesn't include this feature".
- The dev stack's Zot (`lajstrom-dev-zot-1`, up 34 hours, image id the index above) with
  [deploy/compose/zot/config.json](../../deploy/compose/zot/config.json), and scratch Zots of
  the same image for what that configuration does not show ([Run it](#run-it)). curl 8.7.1,
  oras 1.3.6, Lajstrom at c9405e4 with Go 1.27.3. Everything pushed to the dev stack's Zot went
  under `spike18/`, and was deleted afterwards ([Commands](#commands)).

## Run it

The dev stack's Zot, `make dev-up`, on `localhost:5000`: anonymous, filesystem storage, `dedupe`,
`gc` every 10 minutes with a `gcDelay` of an hour. For the options it does not set, scratch
containers of the same image, each with its configuration file outside the repository
([Configuration](#configuration) has them):

| Container | Port | What it adds |
| --- | --- | --- |
| `spike18-zot-fs` | 5101 | nothing: the dev stack's configuration, for the tests without a prefix and the names that leave junk behind |
| `spike18-zot-imm` | 5102 | `http.accessControl`, `anonymousPolicy` without `update` |
| `spike18-zot-auth` | 5103 | htpasswd, and a policy for the user `lajstrom`: `read`, `create`, `delete`, no `update` |
| `spike18-minio` | 5104 | MinIO 2025-07-23 (the dev stack's image), bucket `zot` |
| `spike18-zot-s3` | 5105 | the `s3` storage driver on that MinIO, `redirectBlobURL` off (the default) |
| `spike18-zot-s3r` | 5106 | the same, `redirectBlobURL: true` |
| `spike18-zot-gc` | 5108 | the dev stack's configuration with `gcDelay: 5s` and `gcInterval: 10s` |

The two S3 Zots run in MinIO's network namespace (`--network container:spike18-minio`), as
distribution-s3 does in the dev stack, so that the presigned URLs they sign for
`localhost:5104` work on the machine too ([dev-stack.md](../dev-stack.md#the-s3-profile)).

## The row

| Row of the matrix | Zot v2.1.23 |
| --- | --- |
| Create on push | [yes: the first upload session creates the repository](#create-on-push) |
| Where a repository may be created | [anywhere; a name component of more than 255 bytes is 500](#create-on-push) |
| Referrers API | [yes, with the `artifactType` filter; no tag schema](#referrers) |
| `OCI-Subject` on a push with `subject` | [yes](#referrers) |
| Catalog (`_catalog`) and its limits | [yes, `n`/`last`/`Link`, component by component (`/` first), no largest `n`](#catalog) |
| A listing API of its own | [none in the minimal build](#catalog) |
| Tag list order and pagination | [lexical; `n`/`last`/`Link`](#tags) |
| Delete a manifest by digest | [yes, 202, its tags with it; **405** for a manifest an index lists](#delete) |
| Delete by tag | [yes: the manifest stays, untagged](#delete) |
| Delete a repository | [no API; an empty one goes with the collector](#delete) |
| Immutable tags, where the rule lives | [none by default; access control without `update`: 403 `DENIED`](#immutable-tags) |
| Blob redirect | [none on the filesystem; on S3 with `redirectBlobURL`, 307 to a presigned URL](#blob-redirect) |
| `Range` and `Accept-Ranges` | [206, 416 past the end; no `Accept-Ranges` on `GET`](#blob-reads-ranges-sizes) |
| Largest blob measured | [1 GiB in one `PUT` and in two `PATCH`es; no limit set](#blob-reads-ranges-sizes) |
| Manifest size limit | [4 MiB; one byte more is 413](#blob-reads-ranges-sizes) |
| Lajstrom's manifest profile | [accepted; Docker types refused (415)](#media-types) |
| Auth | [none; Basic (htpasswd), with access control](#our-tests-against-it) |
| Rate limits seen | [none](#blob-reads-ranges-sizes) |
| Upload: chunk out of order | [416 `BLOB_UPLOAD_INVALID`](#uploads) |
| Upload: chunk size limit | [none met: 512 MiB a `PATCH`](#uploads) |
| Upload: session status (`GET`) | [204 with `Range`](#uploads) |
| Upload: cancel (`DELETE`) | [204; on S3, 503 after a refused digest](#uploads) |
| Upload: wrong digest, empty blob | [a digest mismatch; stored (the test)](#our-tests-against-it) |
| Upload continued by another replica | [works (the test)](#our-tests-against-it) |
| Garbage collector and an untagged referrer (#37) | [kept, with or without its subject; an untagged manifest goes after `gcDelay`](#garbage-collection) |
| What the probe finds | [`referrers=true catalog=true`](#the-row) |
| Overrides needed | [none; `immutableTags: true` behind a policy without `update`](#configuration) |

`lajstrom registry probe` measures every row of this table but the listing API, auth, rate
limits and the garbage collector, which still need a person;
[README.md](README.md#measuring-a-registry-lajstrom-registry-probe) says how.

The probe logged the same line against all four Zots the suite ran on (the dev stack, the dev
configuration without a prefix, S3 with redirects, htpasswd without `update`):

```text
level=INFO msg="registry capabilities" registry=integration vendor=unknown createOnPush=true referrers=true catalog=true deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0
```

Against what was measured:

- `createOnPush`, `referrers`, `catalog`, `maxBlobSize=0` (no limit is set or met): right.
- `deleteManifest=true` is right and stays true: Zot answers 405 `DENIED` for the delete of a
  manifest that an image index lists, though every other delete works, and the provider reads
  that 405 as a refusal of the one delete, `provider.ErrConflict` (#153, item 1 below).
- `immutableTags=false` is right for the default configuration and wrong behind an access
  control policy without `update`, which the probe cannot see: set it in the configuration.
- `blobRedirect=true` is the operator's switch, not a finding: Zot redirects only on S3 with
  `redirectBlobURL`, and never a `HEAD` or a range; where it does not, the provider streams.
- `vendor=unknown`: Zot sends nothing that names it but `X-ZOT-API-CLIENT` in
  `Access-Control-Allow-Headers` of every answer.

## What the provider must do differently

1. Done in #153: read Zot's 405 `DENIED` to a delete as a refusal of that manifest or blob,
   not as "this registry does not delete". Zot keeps a manifest that an image index lists
   ("manifest is part of an index image, don't allow index manipulations" in its source) and
   a blob that a manifest uses, and answers 405 with the code `DENIED`, where distribution with
   deletes disabled answers 405 `UNSUPPORTED`. Before #153 the provider turned
   `DeleteManifest` false at the first such answer ([Delete](#delete)), and the raw route then
   refused every delete until a restart. Now a 405 `DENIED` (without `UNSUPPORTED`) is
   `provider.ErrConflict`, 409 at `/raw/`, and changes nothing else; `UNSUPPORTED` and a 405
   without `DENIED` still turn `DeleteManifest` false.
2. Nothing else in the code. The rest is configuration: `capabilities: {immutableTags: true}`
   behind a policy without `update`, whose 403 `DENIED` the provider reads as bad credentials
   (`provider: unauthorized`) and nothing in Lajstrom but the reindex test's drift meets
   ([Immutable tags](#immutable-tags)); `redirectBlobURL` on S3 for redirects
   ([Blob redirect](#blob-redirect)). Referrers of a deleted manifest stay on Zot, listed under
   its digest, unless a retention policy says `deleteReferrers`
   ([Garbage collection](#garbage-collection)): whoever deletes a version with signatures
   deletes them first, as the integration test does.

## Create on push

A repository is created by the first upload session, before a byte or a manifest arrives:

```text
GET  /v2/spike18/cop/a/tags/list        404 {"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known to registry",…,"name":"spike18/cop/a"}}]}
POST /v2/spike18/cop/a/blobs/uploads/   202 Location: /v2/spike18/cop/a/blobs/uploads/1e479050-…  Range: 0-0
GET  /v2/spike18/cop/a/tags/list        200 {"name":"spike18/cop/a","tags":[]}
GET  /v2/_catalog                       200 … "spike18/cop/a" …
```

So the catalog lists repositories that have no manifest yet: abandoned upload sessions, a facade
client that never pushed its manifest. The referrers probe does not create one: after it,
`spike18/lajstrom-probe` is still 404. Names, on the scratch Zot with the dev configuration:

```text
POST …/spike18/cop/a/b/c/d/e/f/g/h/i/j/blobs/uploads/       202, its manifest v1 201
POST …/spike18/cop/docs/hello.txt/blobs/uploads/            202, its manifest v1 201
POST …/spike18/cop/a__b/c--d/e.f_g/blobs/uploads/           202
POST …/spike18/cop/Upper/blobs/uploads/                     404 text/plain "404 page not found"
POST …/spike18/cop/a..b/blobs/uploads/                      404 "404 page not found"
POST …/spike18/<a component of 255 bytes>/blobs/uploads/    202
POST …/spike18/<a component of 256 bytes>/blobs/uploads/    500, no body; log "failed to create blobs subdir … mkdir …: file name too long"
POST …/spike18/<5 components of 200 bytes, 1012 in all>/…   202, its manifest 201
POST …/spike18/<20 components of 200 bytes, 4027 in all>/…  202
```

No limit on the whole name; a component is a directory on the filesystem, which ends at 255
bytes. Lajstrom never comes near it: [manifest-profile.md](../spec/manifest-profile.md) cuts a
component at 64 bytes and a name at 255. A name outside the OCI grammar is the router's plain
404, not `NAME_INVALID`. Behind an access control policy a repository may be created only where
the user has `create`.

## Referrers

The OCI 1.1 referrers API. A subject `S` tagged `v1` in `spike18/ref/pkg`, then three referrers
by digest: one by `oras attach`, one with `artifactType`, one with only a config media type:

```text
$ oras attach --plain-http --artifact-type application/vnd.lajstrom.test.signature.v1 localhost:5000/spike18/ref/pkg:v1 sig.txt:application/vnd.lajstrom.test.sig.v1
Attached to [registry] localhost:5000/spike18/ref/pkg@sha256:fcb5cba71b723d508da076d51f5018ddc9814c887bed7fcbca8985450be63176
Digest: sha256:1de374d30a6af277efc57b3fd3b2cf09c032394bb4176ee65c0d48296c209ec9

PUT manifests/sha256:e45d5e47…   201  Oci-Subject: sha256:fcb5cba7…   (artifactType application/vnd.lajstrom.test.sbom.v1)
PUT manifests/sha256:35a66efe…   201  Oci-Subject: sha256:fcb5cba7…   (config application/vnd.lajstrom.test.cfg.v1+json, no artifactType)
GET referrers/sha256:fcb5cba7…   200  Content-Type: application/vnd.oci.image.index.v1+json
    {"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
      {"artifactType":"application/vnd.lajstrom.test.signature.v1","digest":"sha256:1de374d3…","size":753,"annotations":{"org.opencontainers.image.created":…}},
      {"artifactType":"application/vnd.lajstrom.test.sbom.v1","digest":"sha256:e45d5e47…","size":795,"annotations":{"lajstrom.format":"raw",…}},
      {"artifactType":"application/vnd.lajstrom.test.cfg.v1+json","digest":"sha256:35a66efe…","size":554,"annotations":null}]}
GET …?artifactType=application/vnd.lajstrom.test.sbom.v1                          200  Oci-Filters-Applied: artifactType, the sbom alone
GET …?artifactType=application/vnd.lajstrom.test.cfg.v1%2Bjson                    200  the third alone
GET …?artifactType=…sbom.v1&artifactType=…signature.v1                            200  Oci-Filters-Applied: artifactType, both
GET …?artifactType=application/x-none                                            200  Oci-Filters-Applied: artifactType, "manifests":[]
GET …?n=1                                                                         200  all three, no Link
GET referrers/<zero digest> in spike18/ref/pkg                                    200  {"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}
GET referrers/<zero digest> in spike18/lajstrom-probe (no such repository)        200  the same empty index
GET referrers/sha256:abc, referrers/v1                                            400, no body
PUT a referrer of sha256:1111… (no such manifest)                                 201  Oci-Subject: sha256:1111…; listed under that digest
```

**No `sha256-<digest>` tag is written**: after the attach `tags/list` is `["v1"]`, and `oras
discover` lists the three through the API. The artifact type of a referrer without one is its
config media type, as the spec asks; several `artifactType` parameters are an "or". `n` is
ignored. The empty index for a repository that does not exist is what the probe reads as the
referrers API. A referrer whose subject was deleted stays listed under the subject's digest
([Delete](#delete)).

## Catalog

160 repositories `spike18/cat/r001` to `r160` pushed into the dev stack's Zot, which then held
376:

```text
GET /v2/_catalog                                    200  all 376, sorted, no Link
GET /v2/_catalog?n=2                                200  Link: </v2/_catalog?n=2&last=apt/e2e/e2e-put-20261107230333-6724>; rel="next"
                                                         {"repositories":["apt/e2e/e2e-oras-20261107230333-6724","apt/e2e/e2e-put-20261107230333-6724"]}
GET /v2/_catalog?last=spike18/cat/r050&n=3          200  Link: </v2/_catalog?n=3&last=spike18/cat/r053>; rel="next"   ["spike18/cat/r051","spike18/cat/r052","spike18/cat/r053"]
GET <that Link>                                     200  Link: …last=spike18/cat/r056…   ["spike18/cat/r054","spike18/cat/r055","spike18/cat/r056"]
GET …?last=spike18/cat/r050x&n=2                    200  ["spike18/cat/r051","spike18/cat/r052"]   (a last that is no repository)
GET …?last=spike18/cat/r157&n=5                     200  the five that are left, no Link
GET …?last=spike18/cat/r157&n=4                     200  four, Link: </v2/_catalog?n=4&last=spike18/cop/a>; rel="next"
GET …?last=zzzz&n=2                                 200  {"repositories":[]}
GET …?n=0                                           200  {"repositories":[]}
GET …?n=-1, ?n=abc                                  200  all 376
GET …?n=100000                                      200  all 376
GET …?last=spike18/cat/r150                         200  the 12 after it
walk with n=100, following Link                     4 pages, 376 names, no duplicates
```

Ordered component by component, `/` first: names that share a prefix up to a `/` are listed
before the names that continue it with another character. The 160 names above do not show it;
`lajstrom registry probe` (2026-11-10, against the dev stack's Zot) pushed `pre`, `pre/sub`,
`pre-x`, `pre.y` and `prea` below one prefix:

```text
GET /v2/_catalog?last=<prefix>ord&n=100    … "<prefix>ord/pre","<prefix>ord/pre/sub","<prefix>ord/pre-x","<prefix>ord/pre.y","<prefix>ord/prea" …
GET /v2/_catalog?last=<prefix>ord/pre&n=2  Link: </v2/_catalog?n=2&last=<prefix>ord/pre-x>; rel="next"   ["<prefix>ord/pre/sub","<prefix>ord/pre-x"]
```

That is the order of distribution on a filesystem ([distribution.md](distribution.md#catalog)),
not byte order (which puts `pre-x` and `pre.y` before `pre/sub`); this page said "lexical" before
the probe. `last` is a position in it, not a name that must exist; a `Link` only when more
remain; no largest `n`. The probe's `n=1` gets 200: `catalog=true`, and `ListRepositories`
works. The catalog lists every repository, those without a manifest too
([Create on push](#create-on-push)). The minimal build has no listing API of its own: the
search extension is not in it (`/v2/_zot/ext/search` is 404), and `/v2/_oci/ext/discover`
answers `{"extensions":[]}`.

## Tags

One manifest under 163 tags in `spike18/tags/pkg` (`1` to `150`, `v1`, `v2`, `v10`, `V1`,
`1.0.0`, `1.9.0`, `1.10.0`, `latest`, `_x`, `sha256-abc`, `Z9`, `a.b-c_d`, `first`):

```text
GET tags/list                   200  all 163: "1","1.0.0","1.10.0","1.9.0","10","100",… "99","V1","Z9","_x","a.b-c_d","first","latest","sha256-abc","v1","v10","v2"
GET tags/list?n=3               200  Link: </v2/spike18/tags/pkg/tags/list?n=3&last=1.10.0>; rel="next"   {"tags":["1","1.0.0","1.10.0"]}
GET tags/list?n=2&last=1.0.0    200  Link: …last=1.9.0…   ["1.10.0","1.9.0"]
GET tags/list?n=2&last=99a      200  Link: …last=Z9…      ["V1","Z9"]   (a last that is no tag)
GET tags/list?n=3&last=v1       200  ["v10","v2"], no Link
GET tags/list?n=2&last=v2       200  {"tags":[]}
GET tags/list?n=0               200  {"tags":[]}
GET tags/list?n=-1, ?n=abc      400, no body
GET tags/list?n=100000          200  all 163
walk with n=50, following Link  4 pages, 163 tags, no duplicates
GET /v2/spike18/tags/nope/tags/list   404 NAME_UNKNOWN
```

Byte order (digits, upper case, `_`, lower case), as the spec asks; `last` is a position; no
largest `n`. A bad `n` is a 400 here and "everything" in the catalog. A repository with no
tag answers `{"tags":[]}`, not `null`.

## Delete

In `spike18/del/pkg`: `M1` tagged `t1` and `t1b`, `M2` tagged `t2`, `M3` tagged `t3` and `t3b`,
`M4` tagged `t4` and listed by an index tagged `sha256-<M4>`:

```text
DELETE manifests/sha256:<M1>       202   t1 404, t1b 404, M1 404; tags ["t2","t3","t3b"]
DELETE the same digest again       404 {"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown",…}]}
DELETE manifests/t2                202   t2 404, M2 by digest 200 (untagged); tags ["t3","t3b"]
DELETE manifests/t3                202   t3 404, t3b 200
DELETE manifests/nope              404 MANIFEST_UNKNOWN
DELETE manifests/sha256:<M4>       405 {"errors":[{"code":"DENIED","message":"requested access to the resource is denied","detail":{…,"reference":"sha256:691aa930…"}}]}   t4 and M4 still 200
DELETE blobs/<a layer of M3>       405 the same DENIED; the blob still 200
DELETE blobs/<a blob nothing uses> 202
```

A digest delete takes every tag of the manifest with it; a tag delete takes the tag and leaves
the manifest, untagged, for the collector ([Garbage collection](#garbage-collection)). A manifest
an index lists and a blob a manifest uses are refused with **405 `DENIED`**; the index goes
first, then the manifest (202, 202). The provider, against `spike18-zot-fs`, with a manifest
tagged `v1` and an index tagged `list` that names it (a test written for the spike, not kept):

```text
level=WARN msg="registry refuses manifest deletes; DeleteManifest is now false" registry=integration
Delete(manifest listed by an index) = generic: delete raw/spike18-565385de/pkg@sha256:ffd102b5…: provider: unsupported (HTTP 405, DENIED)
after: DeleteManifest=false
Delete(index) = <nil>
Delete(manifest) = <nil>
```

Item 1 of [What the provider must do differently](#what-the-provider-must-do-differently),
fixed by #153. The provider's integration test now deletes such a manifest
(`delete a manifest that an index lists`), against the dev stack's Zot:

```text
the registry keeps a manifest that an index lists: generic: delete raw/it-61d72647/indexed@sha256:0399c2dd…: provider: conflict (HTTP 405, DENIED)
--- PASS: TestIntegration/delete_a_manifest_that_an_index_lists
```

`DeleteManifest` stays true, and the index, then the manifest, are deleted. Deleting a subject
leaves its referrers:

```text
DELETE spike18/ref/pkg/manifests/sha256:<S>   202   v1 404, S 404; the referrers R1, R2 200
GET referrers/sha256:<S>                      200   all three still listed
```

There is no repository delete. When its last manifest goes, the repository stays in the catalog
with `"tags":[]` on the dev configuration, until the collector has removed its blobs and the
repository with them ([Garbage collection](#garbage-collection)); on `spike18-zot-auth`
(htpasswd, which makes Zot keep a metadata database) it was gone at once: `tags/list` 404
`NAME_UNKNOWN`, the catalog empty.

## Immutable tags

None by default. On the dev stack's Zot, `A` tagged `v1`, then:

```text
PUT B as v1                     201   v1 is B; A still 200 by digest
PUT A as v1 again               201   v1 is A
PUT C by digest ?tag=v3         201   v3 created
PUT C by digest ?tag=v1         201   v1 is C
```

(`PUT …/manifests/<digest>?tag=<t>`, several tags in one push, is not in distribution-spec
1.1.1; Zot takes it.)

The rule can live in Zot's access control: `create` lets a user push a new tag or a digest,
`update` re-point an existing tag, `delete` delete. A policy without `update`, the same steps:

```text
                                   htpasswd, user lajstrom: read, create, delete     anonymousPolicy: read, create, delete
PUT B as v1                        403 {"errors":[{"code":"DENIED","message":"requested access to the resource is denied",…}]}   401 UNAUTHORIZED, Www-Authenticate: Basic realm="Authorization Required"
PUT A as v1 again (same bytes)     403 DENIED                                         401
PUT A as v2 (new tag)              201                                                201
PUT A as v2 again                  403 DENIED                                         401
PUT C by digest                    201                                                201
PUT C by digest ?tag=v3 / ?tag=v1  201 / 403 DENIED                                   201 / 401
DELETE v2, DELETE by digest        202                                                202 (without delete in the policy: 401)
then: DELETE v1, PUT B as v1       202, 201: v1 is B
```

So the same manifest pushed again under its own tag is refused, not idempotent, as on GitLab
and Harbor; Lajstrom's ingest resolves the tag first and writes nothing on a repeat. The 403
carries no reason, and the provider reads it as `provider: unauthorized`. With `delete` granted
(Lajstrom's raw deletes need it) a tag can still be deleted and pushed again: the policy stops a
re-point, not a delete and a create. Zot v2.1.23 also has CEL `conditions` on a policy (its
example: `req.referenceType == "digest"`), not tried. The probe sees none of this:
`immutableTags: true` goes in the configuration ([Configuration](#configuration)).

## Blob redirect

Zot redirects a blob download only with `storage.redirectBlobURL: true`, which it takes for the
`s3`, `gcs` and `azure` drivers alone. On the filesystem it refuses to start:

```text
{"level":"error","message":"invalid storage config, redirectBlobURL is supported only for s3/gcs/azure storage","error":"invalid server config","redirectBlobURL":true,"storageDriver":"local",…}
```

The same 1 MiB blob in `spike18/s3/pkg` of both S3 Zots on MinIO:

```text
                                  spike18-zot-s3 (redirectBlobURL off)   spike18-zot-s3r (redirectBlobURL: true)
GET blobs/sha256:88dc6391…        200, the bytes, no Accept-Ranges       307 Location: http://localhost:5104/zot/zot-r/zot-r/spike18/s3/pkg/blobs/sha256/88dc6391…?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=…&X-Amz-Date=20261109T061354Z&X-Amz-Expires=1200&X-Amz-SignedHeaders=host&X-Amz-Signature=…
                                                                         Docker-Content-Digest: sha256:88dc6391…
HEAD blobs/sha256:88dc6391…       200, Accept-Ranges: bytes              200, Accept-Ranges: bytes: no redirect
GET with Range: bytes=0-9         206 Content-Range: bytes 0-9/1048576   206 from Zot: no redirect
curl -L                           the blob's digest                      the blob's digest
the Location, no credentials                                             200, Accept-Ranges: bytes; with a Range 206
the Location, HEAD                                                       403 (signed for GET)
the Location, signature changed                                          403 SignatureDoesNotMatch
```

The presigned URL names the endpoint Zot talks to (`regionendpoint`), lives 20 minutes
(`X-Amz-Expires=1200`, the default of distribution's s3 driver, which Zot uses), and its path
holds the driver's `rootdirectory` twice (`zot-r/zot-r`), which works. Only a whole `GET` is
redirected: a `HEAD` and a range are answered by Zot. Through Lajstrom,
`TestIntegrationS3Redirect` passes against `spike18-zot-s3r`: the facade's and `/raw/`'s `GET`
are 307 to MinIO, the bytes behind the `Location` have the digest, a range after the redirect is
206, `HEAD` 200, and `policy.blobRedirect: never` streams; the provider's `OpenBlob` returned
"a redirect to http://localhost:5104/zot/zot-r/zot-r/raw/it-ff168bb8/…".

## Blob reads, ranges, sizes

On the dev stack's Zot:

```text
POST blobs/uploads/?digest=sha256:15f907e6… with the bytes   201 Location: /v2/spike18/misc/pkg/blobs/sha256:15f907e6…
GET  blobs/sha256:15f907e6…        200 Content-Length: 13, Content-Type: application/octet-stream, Docker-Content-Digest; no Accept-Ranges
HEAD blobs/sha256:15f907e6…        200 the same and Accept-Ranges: bytes
GET  Range: bytes=0-4              206 Content-Range: bytes 0-4/13
GET  Range: bytes=100-200          416 Content-Range: bytes */13
GET  blobs/sha256:2222…            404 BLOB_UNKNOWN
```

- No redirect on the filesystem: the first answer is the bytes. `Content-Type` of a blob is
  `application/octet-stream`, whatever the layer's media type.
- **1 GiB** (sparse zeros, `sha256:49bc20df…`), on the dev stack's Zot: one `PUT` of the whole
  blob after the `POST`, 201 in 2 s; then, deleted (202) and pushed again as two `PATCH`es of
  512 MiB with `Content-Range` and a closing `PUT` without a body, 202 (`Range: 0-536870911`),
  202 (`Range: 0-1073741823`), 201, in 2 s. `HEAD` says `Content-Length: 1073741824`; a `GET`
  gave the digest back in 2 s; the delete freed the GiB on the VM's disk at once. Nothing larger
  was pushed: the VM had 3.4 GiB free, shared with the session measuring distribution. Zot sets
  no blob limit; its quotas (`storage.maxRepos`, `storage.maxRepoBytes`) are 0, off, by default.
- Manifests: **4 MiB**. A manifest of 4194304 bytes is 201, of 4194305 bytes 413
  `{"errors":[{"code":"MANIFEST_INVALID","message":"manifest invalid",…}]}` with the reason in
  the detail: Lajstrom's `server.maxManifestSize` default.
- `GET /v2/` is 200 with `Docker-Distribution-Api-Version: registry/2.0`,
  `Access-Control-Allow-Origin: *` and `Access-Control-Allow-Headers:
  Authorization,content-type,X-ZOT-API-CLIENT`, and no body.
- No rate limit: no 429 in the curl runs or the four runs of the suite. Zot has one
  (`http.ratelimit`), off unless configured.

## Uploads

On the dev stack's Zot:

```text
PATCH abc, Content-Range: 0-2            202 Range: 0-2
PATCH y,   Content-Range: 7-7            416 {"errors":[{"code":"BLOB_UPLOAD_INVALID","message":"blob upload invalid",…}]}
GET  <session>                           204 Location: <session>, Range: 0-2
DELETE <session>                         204; its GET then 404
POST /v2/spike18/misc/other/blobs/uploads/?mount=sha256:15f907e6…&from=spike18/misc/pkg   201
two PATCHes of 512 MiB                   202, 202 (above)
```

`TestIntegrationUploads` passes on the filesystem ([Our tests](#our-tests-against-it)). On S3 it
fails at one step: the cancel of a session after a refused digest is **503** with no body (500
on distribution on S3), and the client's retry gets the same; Zot logs "failed to delete blob
upload due to storage failure … s3aws: append to zero-size path
/zot-r/raw/it-12c9dc8d/uploads/.uploads/e3a3f7b7-… unsupported". It is the s3 driver of
distribution that both use, as in the README's
[Deviations](README.md#deviations-from-the-specification). The facade's tests pass there.

## Media types

Nothing in Lajstrom's profile is refused: `artifactType`, the empty config with its `data`,
`application/vnd.lajstrom.*` layers, annotations. The raw and deb pushes of the quick starts:

```text
$ oras push --plain-http --artifact-type application/vnd.lajstrom.raw.v1 --annotation lajstrom.schema=1 … \
    localhost:5000/spike18/raw/assets/docs/hello.txt:ec5ba820a095a4d6 docs/hello.txt:application/vnd.lajstrom.raw.file.v1
Pushed [registry] localhost:5000/spike18/raw/assets/docs/hello.txt:ec5ba820a095a4d6
ArtifactType: application/vnd.lajstrom.raw.v1
$ oras push --plain-http --artifact-type application/vnd.lajstrom.deb.v1 … localhost:5000/spike18/apt/ubuntu/world:2.0-1__jammy_2fmain_2famd64 \
    world_2.0-1_amd64.deb:application/vnd.lajstrom.deb.package.v1 metadata.json:application/vnd.lajstrom.deb.metadata.v1+json
Pushed [registry] localhost:5000/spike18/apt/ubuntu/world:2.0-1__jammy_2fmain_2famd64
ArtifactType: application/vnd.lajstrom.deb.v1
```

The stored manifest has the config as pushed, `data` and all. The rest:

```text
PUT a Docker v2 manifest (application/vnd.docker.distribution.manifest.v2+json)   415 MANIFEST_INVALID
PUT a manifest whose layer was never pushed                                       400 MANIFEST_BLOB_UNKNOWN
PUT an OCI manifest, config application/vnd.lajstrom.unknown.config.v1+json, no artifactType, no layers   201
PUT an OCI manifest with Content-Type: application/vnd.oci.image.index.v1+json     400 MANIFEST_INVALID
```

Docker's media types need `http.compat` (not set in the dev stack; [dev-stack.md](../dev-stack.md#what-runs)).
The integration job pushes the whole profile to this Zot on every pull request.

## Garbage collection

The question of #37, on `spike18-zot-gc` (the dev configuration with `gcDelay: 5s`, `gcInterval:
10s`; without `retention`, the delay for untagged manifests is the `gcDelay`, and the default
policy deletes untagged manifests and keeps referrers): in `spike18/gc/pkg`, `M` tagged `v1`;
`R`, a referrer of `M`, untagged; `N`, a manifest by digest, no tag, no subject; `X`, a referrer
of `Y`, which was tagged and then deleted by digest; `U`, a blob nothing uses.

| | pushed | 20 s later | 40 s later |
| --- | --- | --- | --- |
| `M`, tagged | 200 | 200 | 200 |
| `R`, untagged referrer of `M` | 200 | **200**, and listed by `referrers/<M>` | 200 |
| `N`, untagged, no subject | 200 | 404 | 404 |
| `X`, referrer of a deleted subject | 200 | **200** | 200 |
| `U`, unreferenced blob | 200 | 404 | 404 |

```text
{"level":"info","message":"removed untagged manifest","module":"gc","repository":"spike18/gc/pkg","reference":"sha256:274dbeb3…","decision":"delete","reason":"deleteUntagged",…}
{"level":"info","message":"garbage collected blobs","module":"gc","repository":"spike18/gc/pkg","count":4,…}
```

**An untagged referrer survives Zot's collector**, and so does one whose subject is gone: Zot
counts a manifest with `subject` as referenced, and removes referrers of a missing subject only
under a retention policy with `deleteReferrers: true` (default false). On the dev stack an
untagged manifest without a subject goes an hour after its push (`gcDelay`; the retention delay
defaults to it), at the next 10-minute run; every manifest ingest writes is tagged, and a
referrer pushed through the facade is kept. Emptied of its manifests, the repository went at the next passes: about 35 s after the
last delete, `tags/list` was 404 `NAME_UNKNOWN` and the catalog empty.

## Our tests against it

`go test -race -count=1 -tags integration -v ./...` from c9405e4, without
`LAJSTROM_TEST_POSTGRES_DSN`:

| Run | Result |
| --- | --- |
| the dev stack's Zot, `LAJSTROM_TEST_REGISTRY=localhost:5000`, `LAJSTROM_TEST_REGISTRY_PREFIX=spike18/` | **pass**: every package; skipped: `TestIntegrationS3Redirect`, `TestAptGetInstall/s3`, `TestConformance`, `TestConcurrentMigrate`, `TestScale`, `TestWriteFixtures` (their variables not set) |
| `spike18-zot-fs`, the dev configuration, no prefix | **pass**, the same skips |
| `spike18-zot-s3r`, S3 on MinIO with `redirectBlobURL`, also as `LAJSTROM_TEST_REGISTRY_S3` | pass but two: provider `TestIntegrationUploads/wrong digest`, the cancel's 503 ([Uploads](#uploads)); deb `TestAptGetInstall/registry`, which wants the downloads streamed (200) from `LAJSTROM_TEST_REGISTRY` and got 307s: the test's assumption, since `/s3` passes against the same Zot |
| `spike18-zot-auth`, htpasswd, user `lajstrom` without `update` | pass but one: reindex `TestIntegration`, whose drift step re-points a tag behind Lajstrom's back when `immutableTags` is false: `generic: push raw/it-8ef2e97f/bin/tool@961a561a9250d768: provider: unauthorized (HTTP 403, DENIED)`. The ingest, raw, apt and facade tests pass: Lajstrom never re-points a tag itself |

Without credentials the htpasswd Zot answers `GET /v2/` with 401 and `Www-Authenticate: Basic
realm="Authorization Required"`; with them the provider and oras-go use Basic on every request.

## Configuration

What Lajstrom needs from Zot is its default: the dev stack's configuration, as it is, serves
every row above. `dedupe` and `gc` are options; `gc` removes untagged manifests after `gcDelay`,
which leaves Lajstrom's tagged manifests and its referrers alone:

```json
{
  "distSpecVersion": "1.1.1",
  "storage": {"rootDirectory": "/var/lib/registry", "dedupe": true, "gc": true, "gcDelay": "1h", "gcInterval": "10m"},
  "http": {"address": "0.0.0.0", "port": "5000"},
  "log": {"level": "info"}
}
```

Blob downloads straight from S3: the `s3` driver and `redirectBlobURL`. The endpoint must be a
host clients reach under the same name, since the signature covers it. `dedupe: false` here: with
`dedupe` on S3 Zot wants a cache (a local BoltDB, or DynamoDB or Redis with `remoteCache`), not
tried.

```json
{
  "distSpecVersion": "1.1.1",
  "storage": {
    "rootDirectory": "/var/lib/registry",
    "dedupe": false,
    "redirectBlobURL": true,
    "gc": true, "gcDelay": "1h", "gcInterval": "10m",
    "storageDriver": {
      "name": "s3", "rootdirectory": "/zot-r", "region": "us-east-1",
      "regionendpoint": "http://localhost:5104", "bucket": "zot", "forcepathstyle": true, "secure": false,
      "accesskey": "lajstrom", "secretkey": "lajstrom-dev"
    }
  },
  "http": {"address": "0.0.0.0", "port": "5106"},
  "log": {"level": "info"}
}
```

Tags that cannot be re-pointed: htpasswd and a policy without `update` (`spike18-zot-auth`;
for anonymous access, `"anonymousPolicy": ["read", "create", "delete"]` without `auth`, as
`spike18-zot-imm`):

```json
"http": {
  "address": "0.0.0.0", "port": "5000",
  "auth": {"htpasswd": {"path": "/etc/zot/htpasswd"}},
  "accessControl": {"repositories": {"**": {"policies": [{"users": ["lajstrom"], "actions": ["read", "create", "delete"]}]}}}
}
```

and in Lajstrom:

```yaml
registries:
  - name: local
    type: generic
    url: http://localhost:5000          # the dev stack's Zot
    # auth: {username: lajstrom, passwordEnv: LAJSTROM_REGISTRY_PASSWORD}   # behind htpasswd
    # capabilities: {immutableTags: true}   # behind a policy without update: the probe cannot see it
    # capabilities: {blobRedirect: false}   # on S3 with redirectBlobURL, when clients cannot reach the store
```

## Commands

The helpers, in a scratch directory: `R=localhost:5000`, `dg() { echo "sha256:$(shasum -a 256
"$1" | cut -d' ' -f1)"; }`, a blob push as `POST …/blobs/uploads/` then `PUT <Location>?digest=`,
and a manifest as below. Responses are trimmed: `Date`, `Content-Length: 0` and the CORS headers
are left out.

```sh
# A manifest of the profile (layer.txt pushed as a blob first, the empty config {} too).
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.lajstrom.raw.v1",
  "config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa3…","size":2,"data":"e30="},
  "layers":[{"mediaType":"application/vnd.lajstrom.raw.file.v1","digest":"%s","size":%s}]}' "$(dg layer.txt)" "$(stat -f %z layer.txt)" > m.json
curl -si -X PUT -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' --data-binary @m.json http://$R/v2/spike18/ref/pkg/manifests/v1
curl -si "http://$R/v2/spike18/ref/pkg/referrers/<digest>?artifactType=application/vnd.lajstrom.test.sbom.v1"
curl -si "http://$R/v2/_catalog?n=3&last=spike18/cat/r050"
curl -si "http://$R/v2/spike18/tags/pkg/tags/list?n=3"
curl -si -X DELETE http://$R/v2/spike18/del/pkg/manifests/<digest or tag>
# 1 GiB without the disk: a sparse file.
mkfile -n 1g big1g.bin
curl -si -X PUT -H 'Content-Type: application/octet-stream' -T big1g.bin "http://$R<Location>?digest=$(dg big1g.bin)"
```

The scratch Zots (each `config.json` as in [Configuration](#configuration), with the
differences of [Run it](#run-it)), and MinIO with the S3 Zots in its network namespace:

```sh
docker run -d --name spike18-zot-fs -p 127.0.0.1:5101:5000 -v "$PWD/zot-fs/config.json:/etc/zot/config.json:ro" ghcr.io/project-zot/zot-minimal:v2.1.23
htpasswd -Bbn lajstrom "$PASSWORD" > zot-auth/htpasswd
docker run -d --name spike18-zot-auth -p 127.0.0.1:5103:5000 -v "$PWD/zot-auth/config.json:/etc/zot/config.json:ro" \
  -v "$PWD/zot-auth/htpasswd:/etc/zot/htpasswd:ro" ghcr.io/project-zot/zot-minimal:v2.1.23
docker run -d --name spike18-minio -p 127.0.0.1:5104:5104 -p 127.0.0.1:5105:5105 -p 127.0.0.1:5106:5106 \
  -e MINIO_ROOT_USER=lajstrom -e MINIO_ROOT_PASSWORD=lajstrom-dev -e MINIO_DEFAULT_BUCKETS=zot -e MINIO_API_PORT_NUMBER=5104 \
  bitnamilegacy/minio:2025.7.23-debian-12-r5
docker run -d --name spike18-zot-s3r --network container:spike18-minio \
  -v "$PWD/zot-s3r/config.json:/etc/zot/config.json:ro" ghcr.io/project-zot/zot-minimal:v2.1.23
```

The tests:

```sh
LAJSTROM_TEST_REGISTRY=localhost:5000 LAJSTROM_TEST_REGISTRY_PREFIX=spike18/ go test -race -count=1 -tags integration -v ./...
LAJSTROM_TEST_REGISTRY=localhost:5101 go test -race -count=1 -tags integration -v ./...
LAJSTROM_TEST_REGISTRY=localhost:5106 LAJSTROM_TEST_REGISTRY_S3=localhost:5106 go test -race -count=1 -tags integration -v ./...
LAJSTROM_TEST_REGISTRY=localhost:5103 LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom LAJSTROM_TEST_REGISTRY_PASSWORD="$PASSWORD" \
  go test -race -count=1 -tags integration -v ./...
```

Cleaning up: every manifest under `spike18/` of the dev stack's Zot, read from the
repositories' `index.json` through a read-only mount of its volume, deleted by digest (189 in
one pass, none refused), then the scratch containers:

```sh
docker run --rm -v lajstrom-dev_zot-data:/data:ro alpine:3 sh -c \
  'cd /data && find spike18 -name index.json -not -path "*/blobs/*" | while read f; do
     grep -o "\"digest\":\"sha256:[0-9a-f]*\"" "$f" | cut -d\" -f4 | sed "s|^|${f%/index.json} |"; done' |
  sort -u | while read -r repo d; do curl -s -o /dev/null -X DELETE "http://localhost:5000/v2/$repo/manifests/$d"; done
docker rm -f spike18-zot-fs spike18-zot-imm spike18-zot-auth spike18-zot-s3 spike18-zot-s3r spike18-zot-gc spike18-minio
```

The emptied repositories (199) stay in the dev stack's catalog until the collector, an hour after
their last blob, removes them.
