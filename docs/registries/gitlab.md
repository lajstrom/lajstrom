# GitLab container registry

What the GitLab container registry answered on 2026-11-07, measured with `curl`, `oras`, the
integration tests and the conformance suite through the facade (issue #117). It is a fork of
distribution with a metadata database and an online garbage collector; GitLab.com runs it with
the database. The [summary row](README.md) is in the README; this page has the details. A
delete through Lajstrom's `/v2` of a manifest that referrers name was measured again on
2026-11-12 (#781): [A delete through Lajstrom's `/v2`](#a-delete-through-lajstroms-v2).

## Version tested

- Image `registry.gitlab.com/gitlab-org/build/cng/gitlab-container-registry:v4.43.0-gitlab`,
  the registry as GitLab's cloud-native chart ships it: index
  `sha256:fe9c67416a605ffaa5a053c17a6d3b1eab0668a33a58b7bd938a476088f8222e`, linux/amd64
  `sha256:89c8084a…b5e25b` and linux/arm64 `sha256:107bde4f…7ab4e0`. The binary says
  `v4.43.0-gitlab` (released 2026-10-23), go1.27.3. CNG rebuilds the tag in place: this digest
  was built on 2026-11-07. It runs as `git` (uid 1000) and reads
  `/etc/docker/registry/config.yml`.
- On macOS with OrbStack, arm64; the tests with Go 1.27.3 at c8384ad.
- **Variant A**, metadata on the filesystem: [deploy/compose/gitlab/config.yml](../../deploy/compose/gitlab/config.yml),
  filesystem storage, `storage.delete.enabled: true`, no auth.
- **Variant B**, metadata in the registry's own Postgres (17.11): [deploy/compose/gitlab/config-db.yml](../../deploy/compose/gitlab/config-db.yml),
  the same storage. It runs standalone, without GitLab: the container applies the schema
  migrations (`registry database migrate up`, 200 pre- and 20 post-deployment migrations in
  11 s on an empty database) and starts. GitLab's own features (tag protection, immutability,
  project paths) come from GitLab's tokens and are not there without GitLab; one experiment
  below plays GitLab's part with a test token server.

## Run it

The `gitlab` profile of the dev stack ([deploy/compose/compose.yaml](../../deploy/compose/compose.yaml),
[dev-stack.md](../dev-stack.md#the-gitlab-profile)):

```sh
make dev-up PROFILES=gitlab      # with the rest of the stack; PROFILES=s3,gitlab adds the s3 profile too
make dev-down RESET=1            # every profile, and the volumes
```

`make dev-up` returns in about 15 s, most of it the migrations. Variant A is on `localhost:5003`
(`LAJSTROM_GITLAB_PORT`), variant B on `localhost:5004` (`LAJSTROM_GITLAB_DB_PORT`); its
Postgres (`gitlab-postgres`, user, password and database `registry`) is not published. Two
settings of variant B come from the environment, at the registry's defaults:

| Variable | Default | Sets |
| --- | --- | --- |
| `LAJSTROM_GITLAB_DYNAMIC_MEDIA_TYPES` | `false` | `REGISTRY_FF_DYNAMIC_MEDIA_TYPES`: `true` adds unknown media types on push instead of refusing the manifest ([Media types](#media-types)) |
| `LAJSTROM_GITLAB_GC_REVIEWAFTER` | `24h` | `gc.reviewafter`: `-1` lets the online garbage collector review at once ([Garbage collection](#garbage-collection)) |

With the defaults, variant B refuses every manifest Lajstrom writes. To run Lajstrom against it,
`LAJSTROM_GITLAB_DYNAMIC_MEDIA_TYPES=true` on the `make dev-up` (or in `deploy/compose/.env`); the types it adds stay in the database
after the variable goes back to `false` (`down -v` forgets them).

## The row

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| GitLab, variant A | v4.43.0, metadata on the filesystem | none | yes | **no**: `/referrers/` is a plain-text 404; the `sha256-<digest>` tag schema | yes, `n`/`last`/`Link`; `n` at most 1000 | yes, by digest; by tag removes the tag | no option in the registry | no redirect: streamed, one request; `Accept-Ranges: bytes`, a range 206 | pass, with and without prefix |
| GitLab, variant B | v4.43.0, metadata database | none | yes | no (tag schema) | yes, the same | yes, the same | no option in the registry | no redirect, the same | **fail**: every Lajstrom manifest is refused, 400 `MANIFEST_INVALID` `unknown media type: application/vnd.lajstrom.<format>.v1` |
| GitLab, variant B | v4.43.0, `REGISTRY_FF_DYNAMIC_MEDIA_TYPES=true` | none | yes | no (tag schema) | yes | yes | no option | no redirect | pass, with and without prefix |
| GitLab, variant B | v4.43.0, dynamic media types, token auth from a test token server | Bearer | yes | no (tag schema) | yes | yes | — | no redirect | pass, but for the cross-replica upload ([Bearer tokens](#bearer-tokens)) |
| GitLab, variant B | the same, the token carries `tag_immutable_patterns: [".*"]` | Bearer | yes | no (tag schema) | yes | **no**: every digest delete answers 401 `PROTECTED_MANIFEST`, which the provider reads as bad credentials | **yes, from the token**: an existing tag is 401 `IMMUTABLE_TAG` | no redirect | fail: the delete steps |

`lajstrom registry probe` measures every row of this table but the listing API, auth, rate
limits and the garbage collector, which still need a person;
[README.md](README.md#measuring-a-registry-lajstrom-registry-probe) says how. Its run is
[below](#lajstrom-registry-probe).

The probe reports the same capabilities for every variant: `createOnPush=true referrers=false
catalog=true deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0` (the serve
log and the test log). That is right for A and B; no override in `registries[].capabilities` is
needed for a standalone registry. Behind GitLab with protection or immutability rules,
`capabilities: {deleteManifest: false}` until the provider reads `PROTECTED_MANIFEST`.

## What the provider must do differently

1. Read GitLab's policy refusals as policy, not credentials. With tag protection or
   immutability in the token, the registry answers every digest delete with 401
   `PROTECTED_MANIFEST`; the provider makes it `ErrUnauthorized` ("the registry rejected
   Lajstrom's own credentials", measured) and `DeleteManifest` stays true. It should be
   `ErrUnsupported`, with `DeleteManifest` turning false as after a 405. The same holds for 401
   `PROTECTED_TAG` (a push or delete denied by tag protection; read in the source, not run). A
   push to an immutable tag, 401 `IMMUTABLE_TAG` "tag is immutable", already becomes
   `ErrConflict` through its message.
2. Hold a token before streaming a request body. A replica that continues an upload session
   without a token for the repository sends its `PATCH`, gets the 401 challenge, and oras-go
   cannot resend: `request body is not rewindable`. Not GitLab's doing: any registry with token
   auth; measured here because nothing else in the matrix has a token server.
3. Optional: say at startup what variant B will do. `GET /v2/` answers
   `Gitlab-Container-Registry-Database-Enabled: true`; the provider could log that a GitLab
   registry with the database refuses unknown artifact types unless
   `REGISTRY_FF_DYNAMIC_MEDIA_TYPES` is on or the types are known to it. The refusal itself
   cannot be probed without pushing a manifest. Done in #35: the probe names the vendor
   (`vendor=gitlab`) and warns when the database is on.

Nothing else: catalog, tag listing, the referrers fallback, delete, uploads, mounts and ranges
work as on distribution.

## Media types

With the database, the registry keeps a table of known media types (`media_types`, 108 rows
after the migrations, the list of `docs/supported-media-types.md`) and refuses a manifest whose
`artifactType` or `config.mediaType` is not in it. Layer media types are not checked. Measured
with one manifest per field, by tag, on a fresh database:

```text
all-known                    201   no artifactType, empty config, application/octet-stream layer
artifactType-custom          400   {"code":"MANIFEST_INVALID","message":"manifest invalid","detail":"unknown media type: application/vnd.lajstrom.raw.v1"}
artifactType-known           201   artifactType application/vnd.oci.empty.v1+json
layer-custom                 201   layer application/vnd.lajstrom.raw.file.v1
config-custom                400   ... "detail":"unknown media type: application/vnd.lajstrom.deb.metadata.v1+json"
profile-raw                  400   ... "detail":"unknown media type: application/vnd.lajstrom.raw.v1"
```

Variant A takes all six. The manifest profile always sets `artifactType`
(`application/vnd.lajstrom.<format>.v1`), so every manifest Lajstrom writes is refused, and so
is the oras push of the [quick start](../quickstart/oras.md) straight to the registry:

```text
$ oras push --plain-http --artifact-type application/vnd.lajstrom.raw.v1 … localhost:5004/raw/assets/docs/hello.txt:772a78f6f48ec5cf docs/hello.txt:application/vnd.lajstrom.raw.file.v1
Uploaded  772a78f6f48e docs/hello.txt
Error response from registry: manifest invalid: manifest invalid: unknown media type: application/vnd.lajstrom.raw.v1
```

With `REGISTRY_FF_DYNAMIC_MEDIA_TYPES=true` the six are 201 and the new types are inserted into
`media_types`, where they stay: after the variable went back to `false`, the same database took
`application/vnd.lajstrom.*` and the conformance suite's types. A self-managed GitLab can
therefore turn the flag on, or on for one push of each format. On GitLab.com users set nothing,
and the registry's guide advises public registries against the flag; the table is seeded by
migrations, and it carries other projects' types (zarf, timoni, flux, wasm, helm), so adding
`application/vnd.lajstrom.*` upstream is a merge request to GitLab. Changing the profile
is a contract question and is not proposed here.

## Referrers

No referrers API in either variant: the route does not exist, so `GET /v2/<name>/referrers/<digest>`
is a plain-text 404 (`404 page not found`), the probe sets `referrers=false`, and oras-go,
oras and Lajstrom use the `sha256-<digest>` tag schema. A manifest with `subject` is accepted
(201) with no `OCI-Subject` header in the answer. `oras attach` writes the tag schema
(`tags: ["772a78f6f48ec5cf", "sha256-0418bc86…"]`) and `oras discover` lists the referrer, on
both variants. Variant B also lists referrers of tagged manifests in GitLab's own API,
`GET /gitlab/v1/repositories/<path>/tags/list/?referrers=true` (`"referrers":[{"artifactType":…,"digest":…}]`),
not an OCI endpoint; variant A answers it 404.

## Catalog

`_catalog` answers 200 on both, with `Link` pagination by `n` and `last`; five repositories
`cat/r1`…`cat/r5`, `n=2`:

```text
GET /v2/_catalog?n=2                     Link: </v2/_catalog?last=cat%2Fr2&n=2>; rel="next"   {"repositories":["cat/r1","cat/r2"]}
GET /v2/_catalog?last=cat%2Fr2&n=2       Link: </v2/_catalog?last=cat%2Fr4&n=2>; rel="next"   {"repositories":["cat/r3","cat/r4"]}
GET /v2/_catalog?n=0                     all repositories, no Link
GET /v2/_catalog?n=100000                400 {"code":"PAGINATION_NUMBER_INVALID",…,"detail":"n must be no larger than 1000"}
```

The provider asks for 100 per page. A repository appears with its first manifest; blobs alone
do not list it.

## Delete

Both variants, `storage.delete.enabled: true`:

```text
DELETE /v2/curl/del/manifests/t1                    202   (by tag: removes the tag; the manifest stays, untagged)
DELETE /v2/curl/del/manifests/sha256:<digest of t3> 202   GET manifests/t3 afterwards: 404 MANIFEST_UNKNOWN
DELETE the same digest again                        404   MANIFEST_UNKNOWN
DELETE /v2/curl/del/blobs/sha256:<layer of t3>      202   GET afterwards: 404
DELETE /v2/curl/del/tags/reference/t2               404   404 page not found
```

The tag route of `docs/spec/docker/v2/api.md` and `docs/upstream-differences.md` does not exist
in v4.43.0 (`registry/api/v2/routes.go`), although `/v2/` advertises
`Gitlab-Container-Registry-Features: tag_delete`; the tag delete is the OCI one on
`/manifests/<tag>`. Without `delete.enabled` the registry answers deletes with
`UNSUPPORTED` (read in `HandleDeleteManifest`, not run), which the provider already reads.

## Immutable tags

The registry has no option for them. GitLab's tag protection and immutability rules live in
GitLab: Rails puts the patterns that apply into the token it signs
(`access[].meta.tag_immutable_patterns`, `tag_deny_access_patterns`), and the registry enforces
them (`registry/handlers/manifests.go`). To see that without GitLab, a throwaway token server
(not committed) signed ES256 tokens with an `x5c` chain that variant B trusted through
`auth.token.rootcertbundle`, every repository scope with `tag_immutable_patterns: [".*"]`:

```text
PUT  manifest a as v1            201
PUT  manifest a as v1 again      401 {"code":"IMMUTABLE_TAG","message":"tag is immutable","detail":"tag \"v1\" is immutable"}
PUT  manifest b as v1            401 IMMUTABLE_TAG
PUT  manifest b as v2            201
DELETE manifests/sha256:<b>      401 {"code":"PROTECTED_MANIFEST","message":"insufficient permissions to delete manifest due to tag protection or immutability policies"}
DELETE manifests/v1              401 IMMUTABLE_TAG
```

The same manifest pushed again under its own tag is refused, not idempotent; Lajstrom's ingest
resolves the tag first and writes nothing on a repeat, and the repeat of the ingest test passed
here. A digest delete is refused for every manifest of a repository as soon as any
pattern applies to it, tagged or not. In the source, immutability is checked with the
database only (`dbTagManifest`) and the delete refusal in both modes; only variant B was run.

## Bearer tokens

The same token server without patterns is the first registry with token auth these tests ran
against: the provider's ping, challenge and token fetch work, and every test passes but the
two that continue an upload on a second provider, `TestIntegrationUploads/session_across_providers`
(`append to upload …: request body is not rewindable`) and the facade's
`TestIntegrationUploadAcrossReplicas` (`PATCH on B: 500`). The realm had to be
`http://localhost:5005/token` for a registry at `localhost:5006`: oras-go 2.7 refuses a
loopback realm unless it is the registry's own host name (`bearer realm host "127.0.0.1" is
127.0.0.1, in blocked prefix 127.0.0.0/8`); private addresses are allowed.

## Blob redirect, ranges, blob size, headers

- No redirect on filesystem storage (the registry logs `storage backend redirection enabled`
  anyway): `GET` of a blob is 200 with the bytes, `Accept-Ranges: bytes`, `Docker-Content-Digest`;
  `Range: bytes=0-4` is 206 with `Content-Range: bytes 0-4/22`. Object storage was not tested
  (`storage.redirect.disable`, default false, redirects with S3, GCS and Azure).
- A 2 GiB blob in one monolithic `PUT` was 201 on both (3.0 s and 3.7 s); the registry sets no
  blob limit. `validation.manifests.payloadsizelimit` (default 0, none) is not the only limit on
  manifests: the HTTP handler takes 4 MiB, as distribution's, and a manifest of 4 MiB + 1 byte is
  400 `MANIFEST_INVALID` "http: request body too large" on both ([the probe](#lajstrom-registry-probe);
  this page said "none by default", from the configuration read, before it).
- `Docker-Content-Digest` on the blob `PUT`, the manifest `PUT`, `HEAD` and `GET`. No
  `OCI-Subject` anywhere. `GET /v2/` adds `Gitlab-Container-Registry-Version: 4.43.0-gitlab`,
  `Gitlab-Container-Registry-Features: tag_delete` and
  `Gitlab-Container-Registry-Database-Enabled: false` (A) or `true` (B).
- No `http.secret` in the configurations: the registry generates one and warns, which matters
  only for several registry replicas behind one address.

## `lajstrom registry probe`

The probe (#339) against both variants of the `gitlab` profile, on fresh volumes and with the
defaults (`REGISTRY_FF_DYNAMIC_MEDIA_TYPES` off), on 2026-11-10; A once more with
`--max-blob-size 2GiB --max-manifest-size 32MiB`:

| Row | A | B |
| --- | --- | --- |
| Create on push | yes, with the first manifest | the same |
| Where a repository may be created | anywhere; at any depth, inside another repository too; at most 255 characters (256: **500 `UNKNOWN`**) | the same |
| Referrers API | no: a plain 404, the tag schema | the same |
| `OCI-Subject` | no | no |
| Catalog | yes, `n`/`last`/`Link`, component by component (`/` first), `n` at most 1000 (1001: 400 `PAGINATION_NUMBER_INVALID`) | the same, but lexical |
| Tag list | lexical, byte by byte; **`n` and `last` ignored**: the whole list, no `Link` | lexical, byte by byte; `n`/`last`/`Link` |
| Delete by digest | yes, 202, with its tags; its referrers stay | yes, 202, with its tags; **a manifest with a referrer: 500 `UNKNOWN`**; **409 `MANIFEST_REFERENCED`** for a manifest an index lists |
| Delete by tag | yes, 202: the manifest stays, untagged | the same |
| Delete a repository | no `/v2` API; an emptied repository stays in `_catalog`, its tag list 200 | no `/v2` API; it leaves `_catalog` with its last manifest, its tag list 200 |
| Immutable tags | none: a tag re-points with 201 | the same |
| Blob redirect, `Range` | none: the bytes; 206, 416 past the end; `Accept-Ranges: bytes` | the same |
| Largest blob, chunk size | 2 GiB in one `PUT` and in one `PATCH`; no limit met | 8 MiB, the default bound (the 2 GiB run filled the machine's disk before it reached B) |
| Manifest size | 4 MiB; one byte more is 400 `MANIFEST_INVALID` | the same |
| Profile | accepted, stored as pushed | **refused**: 400 `MANIFEST_INVALID` "unknown media type: application/vnd.lajstrom.raw.v1" |
| Uploads | 416 `CONTENT_RANGE_INVALID` out of order; the session's `GET` 204 with `Range`; the cancel 204, then 404 `BLOB_UPLOAD_UNKNOWN`; a mount 201; a wrong digest 400 `DIGEST_INVALID`, the session stays; the empty blob stored; a session continued by another provider works | the same |
| What the probe finds, overrides | `referrers=false catalog=true`; none | the same; **no override helps**: the profile is refused |

New against the rest of this page, which had not measured them:

- **A ignores `n` and `last` on `tags/list`**: `?n=2` answers every tag and no `Link`, and so
  does `?n=1&last=a` (curl: `{"tags":["a","b","c"]}` for both). B pages as the spec says. The
  provider follows `Link` and reads a whole list in one page, so nothing breaks.
- **B cannot delete a manifest that a referrer names**: 500 `UNKNOWN`, and the registry's log
  says why: `deleting manifest: ERROR: insert or update on table "gc_manifest_review_queue"
  violates foreign key constraint "fk_gc_manifest_review_queue_tp_lvl_nspc_id_rp_id_mfst_id_mnfsts"`.
  The referrer deleted first, the subject goes (the probe's cleanup). Lajstrom's deletes delete
  what refers to a version before the version (`internal/ingest`), so they do not meet it; a
  client that deletes a subject through the facade's `/v2` did, until c1a223fc sent that delete
  through ingest too: measured on 2026-11-12, it is a 202 and nothing is left, but for a
  referrer outside the tag schema ([A delete through Lajstrom's `/v2`](#a-delete-through-lajstroms-v2)).
  A manifest an image index lists is 409 `MANIFEST_REFERENCED` (Zot: 405 `DENIED`).
- A name over 255 characters is 500 `UNKNOWN` on both, `{"Name":…,"Reason":{}}` in its detail,
  where distribution answers 400 `NAME_INVALID`. Lajstrom's names stop at 255.
- The catalog's order is the storage's: the filesystem's (`/` first) on A, lexical from the
  database on B.

## A delete through Lajstrom's `/v2`

The 500 above, measured again on 2026-11-12 for #781 now that the facade's `DELETE` goes through
`ingest.Delete` (c1a223fc), which deletes the referrers first. Lajstrom at d44b112e, `bin/lajstrom
serve` with SQLite and an `oci` repo on each variant (B refuses the managed profile, so only an
`oci` repo can be measured there); B with `REGISTRY_FF_DYNAMIC_MEDIA_TYPES` off, its 108 known
types. The tag `v4.43.0-gitlab` had been rebuilt in place again: index `sha256:2f1dcea7…`,
built 2026-11-10, the binary still `v4.43.0-gitlab`.

Into `oci/<repo>/app`, all through Lajstrom: an image of standard types (an image config and a
gzip layer, `oras push`), signed with cosign v3.1.4 and a key of the run, once with a sigstore
bundle (a referrer with `subject`; without the referrers API the provider writes the
`sha256-<hex>` index) and once with `--new-bundle-format=false` (cosign's legacy
`sha256-<hex>.sig`). B took every manifest: the bundle's `application/vnd.dev.sigstore.bundle.v0.3+json`
and cosign's legacy types are among its known types. Then the image deleted by digest through
Lajstrom's `/v2`, as an admin:

```text
                                               A                     B
the backend's tags before                      v1, sha256-<hex>, sha256-<hex>.sig   the same
DELETE /v2/oci/<repo>/app/manifests/<digest>   202                   202
the backend's tags after                       {"tags":null}         {"tags":null}
the image, the index, the .sig, the bundle     404 each              404 each
```

Ingest sent four deletes, each 202: the `.sig` manifest, the `sha256-<hex>` index, the bundle,
the image. **What Lajstrom wrote no longer meets the 500.**

What still meets it: a referrer the tag schema does not list, which Lajstrom cannot find on a
registry without the referrers API. An image through Lajstrom, then a referrer of it (`subject`
set, no tag) pushed straight to the backend with curl, no `sha256-<hex>` index written:

```text
                                               A                          B
DELETE through Lajstrom                        202                        500 {"errors":[{"code":"UNKNOWN","message":"Internal Server Error"}]}
the image after                                404, tags {"tags":null}    200, tags ["v1"]
the referrer after, by digest                  200: an orphan             200
```

On B ingest found no referrer (the index, `.sig`, `.att` and `.sbom` 404), sent the image's
delete and got B's 500 four times (the provider retries a 500 three times; 1.5 s in all), with
the foreign-key error of `gc_manifest_review_queue` in B's log. Nothing changed, and the client
gets 500 `UNKNOWN` with the cause only in the serve log (`ingest: delete … response status code
500: unknown: unknown error`). Such a referrer comes from a client that pushes to the backend
past Lajstrom: Lajstrom's own referrer pushes write the index. Its delete first, then the
image's, works. A, like distribution, deletes the image and leaves the referrer.

## Garbage collection

The question of #37: does a referrer stored as cosign stores it on a registry with the
referrers API, a manifest with `subject` and no tag, survive the collector? Pushed into one
repository: `M`, a manifest tagged `v1`; `R`, a referrer of `M` by digest, no tag; `N`, a
manifest by digest, no tag, no subject; `T`, a referrer of `M` through the tag schema
(`oras attach`: `T` and the index tagged `sha256-<M>`); `U`, a blob no manifest refers to.

**Variant B, online GC.** Always on: every manifest pushed and every blob uploaded is queued for
review (`gc_manifest_review_queue`, `gc_blob_review_queue`) `gc.reviewafter` later, 24 h by
default (`gc_review_after_defaults`: `1 day` for all seven events). With
`LAJSTROM_GITLAB_GC_REVIEWAFTER=-1` (no delay; the registry adds up to 60 s of jitter), within
a minute of the push:

```text
manifest_upload M  "the manifest is not dangling, deleting task"
manifest_upload R  "the manifest is dangling, deleting"   "manifest deleted" digest=sha256:af9c6022…
manifest_upload N  "the manifest is dangling, deleting"   "manifest deleted" digest=sha256:d3d49adc…
manifest_upload T  "the manifest is not dangling, deleting task"
blob_upload     U  "the blob is dangling"   "blob deleted"
layer_delete    R's layer  "the blob is dangling"   "blob deleted"
```

M, T and the index answer 200 afterwards; R, N and U 404. A manifest is dangling when no tag,
no manifest list and no referrer points at it (`docs/spec/gitlab/online-garbage-collection.md`):
a subject with referrers is kept, an untagged referrer is not. **An untagged referrer does not
survive variant B: it goes 24 h after its push**, with no setting to keep it but
`gc.manifests.disabled: true`, which stops collecting every untagged manifest (not tried). The
registry's own guide says it: "all untagged images will be removed automatically by online
garbage collection". Lajstrom is not exposed today: without the referrers API the provider
writes referrers through the tag schema, which survives. It would be if GitLab added the API,
the case #37's tag-schema insurance covers. Also: a `gc.reviewafter` shorter than the longest
gap between a blob upload and its manifest (an upload session of the facade lives an hour
after its last request, a maven publication 30 minutes) lets the collector delete blobs before
their manifest arrives.

**Variant A, offline GC** (`registry garbage-collect`, the registry stopped):

```text
garbage-collect                     U deleted; R and N kept   ("mark stage complete" manifests_to_delete=0)
garbage-collect --delete-untagged   R and N deleted with their layers; M, T and the index kept
```

On variant B the offline command refuses: `the garbage-collect command is not compatible with
database metadata, please use online garbage collection instead` (exit 1). On A it warns that
offline GC and filesystem metadata are deprecated.

## Our tests against it

`LAJSTROM_TEST_REGISTRY=localhost:5003` (A) or `=localhost:5004` (B), with
`LAJSTROM_TEST_POSTGRES_DSN` of the dev stack, `go test -race -count=1 -tags integration ./...`,
once without and once with `LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/`; `TestAptGet` and
`TestAptGetSigned` run apt-get in `ubuntu:22.04`, `ubuntu:24.04` and `debian:12` against the
same registry. The token columns ran without the prefix.

| Test | A | B | B, dynamic media types | B, token auth | B, token, immutable `.*` |
| --- | --- | --- | --- | --- | --- |
| provider `TestIntegration` | pass | **fail**: push manifest, 400 `unknown media type: application/vnd.lajstrom.test.v1` | pass | pass | **fail**: delete, `provider: unauthorized (HTTP 401, PROTECTED_MANIFEST)` |
| provider `TestIntegrationUploads` | pass | pass | pass | **fail**: session across providers, not rewindable | the same |
| ingest `TestIntegration` | pass | **fail**: 400 on `application/vnd.lajstrom.raw.v1` | pass | pass | pass |
| raw `TestIntegration` | pass | **fail**: `PUT` 500 | pass | pass | **fail**: `DELETE` 502 |
| facade: uploads across replicas, abandoned monolithic | pass | pass | pass | **fail**: across replicas, `PATCH on B: 500` | the same |
| facade: pass-through, raw push | pass | **fail**: the manifest `PUT` 500 | pass | pass | pass |
| `TestAptGet`, `TestAptGetSigned` | pass | **fail**: 400 on `application/vnd.lajstrom.deb.v1` | pass | pass | pass |
| conformance v1.1.1 through the facade, an `oci` repo | pull 23, push 26 pass; discovery 10 pass, 5 fail | the same numbers; the references setup fails at the manifest `PUT`: 500 | as A | not run | not run |

The whole suite ran against A without the prefix and passed, the Postgres tests included; the
other runs selected `TestIntegration*` and `TestAptGet*`, the tests that use the registry.
Every failure of column B is the registry's, the media types. The others:

- **Ours, the facade:** the five discovery failures are those of Zot and distribution, the
  referrers API of #28 and `OCI-Subject`, which this registry does not send either. The facade
  serves both since #28; the suite was not run against GitLab again.
- **Ours, the facade, the raw route and ingest:** a backend's 400 `MANIFEST_INVALID` reaches the
  client as 500 `UNKNOWN` (`PUT` of a manifest, `PUT /raw/…`); the reason is only in the serve
  log (`response status code 400: manifest invalid: … unknown media type: …`).
- **Ours, the provider:** `PROTECTED_MANIFEST` read as credentials (the raw route then answers
  502), and the unrewindable `PATCH`: the first two items of
  [What the provider must do differently](#what-the-provider-must-do-differently).

## Behind GitLab (read, not measured)

Behind GitLab, Rails issues the tokens, and it grants a repository only below an existing
project: `ContainerRegistry::Path` in `gitlab-org/gitlab` (`lib/container_registry/path.rb`,
master on 2026-11-07) looks for the project among the three longest prefixes of the path
(`LEVELS_SUPPORTED = 3`), so a repository is a project path plus at most two components.
Lajstrom's repositories are `<prefix><format>/<repo>/<package path>`, three or more components
below the prefix: with `prefix: <group>/<project>/`, none would get a token. One GitLab project
per Lajstrom repo, `<group>/<format>/<repo>`, with `prefix: <group>/`, leaves the package path:
one component for pypi and deb, two for maven, raw paths of at most two components. Since #599
Lajstrom's vendor table knows this depth: a challenge whose realm ends in `/jwt/auth`, GitLab's
token service, sets `maxNameDepth: 4`, and a publish into a deeper name is a `400` that names
the limit; behind a proxy that rewrites the realm, set `capabilities.maxNameDepth`
([configuration.md](../configuration.md#registry-capabilities)). Not tried:
it needs GitLab itself, as do GitLab.com and its rate limits, deploy tokens and the cleanup
policies, which delete tags by rules set in the project.

## Commands

The stack as in [Run it](#run-it); variant B recreated with other settings:

```sh
LAJSTROM_GITLAB_DYNAMIC_MEDIA_TYPES=true docker compose -f deploy/compose/compose.yaml --profile gitlab up -d --wait gitlab-db
LAJSTROM_GITLAB_GC_REVIEWAFTER=-1 LAJSTROM_GITLAB_DYNAMIC_MEDIA_TYPES=true docker compose … up -d --wait gitlab-db
docker exec lajstrom-dev-gitlab-postgres-1 psql -U registry -d registry -c 'select event, value from gc_review_after_defaults'
```

The settings reach the database a jitter of up to a minute after the start (`online GC settings
updated successfully` in the log). The probes with curl:

```sh
curl -si localhost:5003/v2/
curl -si 'localhost:5003/v2/_catalog?n=1'
curl -si localhost:5003/v2/lajstrom-probe/referrers/sha256:0000000000000000000000000000000000000000000000000000000000000000
# a blob: POST /v2/<name>/blobs/uploads/, then PUT <Location>&digest=sha256:<hex> with the bytes
curl -si -X PUT -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' --data-binary @m.json localhost:5004/v2/curl/b1/manifests/v1
```

The tests and the conformance suite:

```sh
LAJSTROM_TEST_REGISTRY=localhost:5003 LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' \
  go test -race -count=1 -tags integration ./...
LAJSTROM_TEST_REGISTRY=localhost:5003 LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/ go test -race -count=1 -tags integration -run 'TestIntegration|TestAptGet' -v ./...
# lajstrom.example.yaml with the registry "local" at http://localhost:5003, then:
bin/lajstrom serve --config lajstrom-gitlab.yaml &
bin/lajstrom admin repo create oci/conformance --registry local --config lajstrom-gitlab.yaml
test/conformance/run.sh
```

The delete through `/v2` (#781): a configuration with the registries `a` at
`http://localhost:5003` and `b` at `http://localhost:5004`, and an `oci` repo on each
(`{name: vb, format: oci, registry: b}`); cosign in `ghcr.io/sigstore/cosign/cosign:v3.1.4`
with `--network host`, as `test/e2e/cosign.sh` runs it:

```sh
docker compose -f deploy/compose/compose.yaml --profile gitlab up -d --wait gitlab gitlab-db
oras push --plain-http localhost:8080/oci/vb/app:v1 --config config.json:application/vnd.oci.image.config.v1+json \
  layer.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip
cosign sign --key cosign.key --tlog-upload=false --use-signing-config=false -y --allow-http-registry localhost:8080/oci/vb/app@<digest>
cosign sign --key cosign.key --tlog-upload=false --use-signing-config=false --new-bundle-format=false -y --allow-http-registry localhost:8080/oci/vb/app@<digest>
curl -s -u <user>:<password> -X DELETE localhost:8080/v2/oci/vb/app/manifests/<digest>
curl -s localhost:5004/v2/oci/vb/app/tags/list
```

The garbage collection, variant A, the registry stopped:

```sh
docker compose -f deploy/compose/compose.yaml --profile gitlab stop gitlab
docker compose -f deploy/compose/compose.yaml --profile gitlab run --rm --no-deps -T gitlab \
  /bin/registry garbage-collect --delete-untagged /etc/docker/registry/config.yml
docker compose -f deploy/compose/compose.yaml --profile gitlab start gitlab
```
