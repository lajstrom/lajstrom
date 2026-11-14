# Gitea container registry

What the container registry built into Gitea answered on 2026-11-10, measured with `curl`,
`oras`, the integration tests and the end-to-end scenarios (issue #120). Forgejo, the fork, was
run with the same setup and has a page of its own, [forgejo.md](forgejo.md). The registry is a
part of Gitea's package registry: `/v2/` on Gitea's own port, a repository is an image of an
owner (a user or an organisation), and every tag is a package version in Gitea's database. The
[summary column](README.md#capabilities) is in the README; this page has the details.

## Version tested

- Gitea **1.27.3**, the rootless image `gitea/gitea:1.27.3-rootless`, index
  `sha256:1c17ecaead42eb3b5391553d8708103a4beb0e86edf5b9ebc1eb269c318845f2` (linux/amd64,
  arm64, riscv64; built 2026-08-29). The binary says `gitea version 1.27.3 built with
  go1.26.7`. SQLite, packages on local storage (`/var/lib/gitea/packages`).
- On macOS with OrbStack, arm64; the tests with Go 1.27.3 at 6b3b7b7 and the provider fix of
  this page ([What the provider must do differently](#what-the-provider-must-do-differently),
  item 1).
- On 2026-11-13 the probe ran again, against 1.27.3 and against Gitea **28.3.0**,
  `gitea/gitea:28.3.0-rootless`, index
  `sha256:40c5a68a0463c0940e1ae32626c8300dd0572f3558e390073ad8379ca67c0e1d` (`gitea version
  28.3.0 built with go1.27.3`), with the image changed in a copy of compose.yaml
  ([The probe again](#the-probe-again-2026-11-13-850)). The pin stays 1.27.3.

## Run it

[deploy/compose/gitea/compose.yaml](../../deploy/compose/gitea/compose.yaml), a compose
project of its own (`lajstrom-gitea`) beside the dev stack, and
[gitea.sh](../../deploy/compose/gitea/gitea.sh), which sets it up with Gitea's CLI and REST API,
never the UI:

```sh
deploy/compose/gitea/gitea.sh up            # Gitea on http://localhost:5009, set up
deploy/compose/gitea/gitea.sh up forgejo    # the same with Forgejo on http://localhost:5014 (forgejo.md)
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
| Lajstrom | `prefix: lajstrom-it/`, `auth.username: lajstrom`, the token in the variable `passwordEnv` names; `uploads.spoolUnknownLength: true` for clients that stream more than one chunk ([item 4](#what-the-provider-must-do-differently)) |

`LAJSTROM_GITEA_CLEANUP_OLDER_THAN` (default `24h`) sets `[cron.cleanup_packages] OLDER_THAN`
for the [cleanup](#cleanup) experiment.

## The row

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Gitea | 1.27.3, local storage, `prefix: lajstrom-it/` (an organisation) | Bearer from `/v2/token` with the token; Basic straight on `/v2/` works too | yes, below an existing owner the user may write, at any depth; another owner 401, a missing one 404 | **no**: `/referrers/` is a plain-text 404; the `sha256-<digest>` tag schema | yes, `n`/`last`/`Link`, lexical; the user's view | yes, by digest, with every tag; by tag the tag, and the manifest with its last tag when it was pushed by tag only | **none**; tags are **case-insensitive and lower-cased** | none on local storage | pass but for ranges (Gitea serves none) and a streamed `PATCH` after the first chunk, with the fix of item 1; that `PATCH` passes with `uploads.spoolUnknownLength` (item 4) |
| Gitea | the same, no prefix | — | **no**: every repository needs an owner; `raw/…` is the owner `raw`, 404 | — | — | — | — | — | fail: nothing can be written |

`lajstrom registry probe` measures every row of this table but the listing API, auth, rate
limits and the garbage collector, which still need a person;
[README.md](README.md#measuring-a-registry-lajstrom-registry-probe) says how. Against Gitea
(2026-11-10, `--registry-prefix lajstrom-it/`) it gave the Gitea column cell for cell, and the
corrections of this page: the mount and the manifest limit ([Blob reads, ranges,
sizes](#blob-reads-ranges-sizes)), and that a digest delete takes the blobs only that manifest
used. It ran again on 2026-11-13, with the rows added since, on 1.27.3 and 28.3.0
([The probe again](#the-probe-again-2026-11-13-850)).

The probe reports `vendor=unknown createOnPush=true referrers=false catalog=true
deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0` (the test log and the
serve log of the e2e run, before #352). That is right; no override in
`registries[].capabilities` is needed. Since #352 it is `vendor=gitea` (item 6).

## The probe again, 2026-11-13 (#850)

The rows added to the probe since 2026-11-10 had not been measured on Gitea: the routing words
(#623), the read-after-write lag with the lists read before each write and the deletes (#845),
and the monotonic reads (#564). Lajstrom at 20ce9499, against Gitea 1.27.3, the pin, and then
against 28.3.0. Each Gitea came from a copy of gitea.sh and compose.yaml with another compose
project name, because other runs shared the machine. Gitea 28.3.0 needed the team fix of #1016,
which main has.

```sh
LAJSTROM_GITEA_PORT=57200 deploy/compose/gitea/gitea.sh up gitea
bin/lajstrom registry probe --url http://localhost:57200 --registry-prefix lajstrom-it/ \
  --username lajstrom --password-file deploy/compose/gitea/.work/gitea.token --max-manifest-size 32MiB -v
```

The provider's own probe said `vendor=gitea createOnPush=true referrers=false catalog=true
deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0 consistency=unknown
maxNameDepth=0`, with `serve`'s hint about `spoolUnknownLength`. Each run took 3 to 8 s, and
nothing was left behind. `--max-blob-size` stayed at its default of 8 MiB because the shared disk
had about 6.5 GiB free; the 512 MiB of 2026-11-10 still stands. Gitea 28.3.0 answered every cell
as 1.27.3 did:

| Row | Gitea 1.27.3 and 28.3.0 |
| --- | --- |
| Create on push | yes, with the first manifest |
| Where a repository may be created | below `lajstrom-it/`; outside it not tried; at any depth, inside another repository too; no length limit met: 256 characters, a component of 256 bytes |
| Routing words in a name (`manifests`, `referrers`, `tags/list`) | ok: 20 blob `HEAD`s in each name, all 200; `GET` 200 |
| Referrers API | no: a plain 404, the tag schema |
| `OCI-Subject` on a push with `subject` | no |
| Catalog (`_catalog`) and its limits | yes, `n`/`last`/`Link`, lexical, no largest `n` |
| A listing API of its own | *not measured by the probe* |
| Tag list order and pagination | **lower-cased, case-insensitive**; lexical; `n`/`last`/`Link` |
| Read-after-write lag | none: each read saw each of 3 writes at once (manifest by digest, manifest by tag, blob, tag list, catalog); each read saw each of 3 deletes at once (manifest by digest, manifest by tag, tag list) |
| Monotonic reads | none: no read saw a change undone; 3 writes, a tag move, 3 deletes, each read 5 times more over new connections after the first that saw it, from 1 address |
| Delete a manifest by digest | yes, 202, with its tags **and the blobs only it used**; its referrers stay |
| Delete by tag | the tag; **the manifest too**, when it was pushed by that tag only |
| Delete a repository | no `/v2` API; it leaves `_catalog` with its last manifest, its tag list 200 |
| Immutable tags, where the rule lives | none: a tag re-points with 201, and **the manifest it named is deleted** |
| Blob redirect | none: the bytes |
| `Range` and `Accept-Ranges` | **none: 200 with the whole blob**; no `Accept-Ranges` |
| Largest blob measured | 8 MiB in one `PUT` and in one `PATCH`; no limit met |
| Manifest size limit | 8 MiB taken; 16 MiB is 413 `MANIFEST_INVALID` |
| Lajstrom's manifest profile (`artifactType`, empty config, own media types) | accepted, stored as pushed |
| Auth | *not measured by the probe* |
| Rate limits seen | *not measured by the probe* |
| Upload: chunk out of order | 416 `BLOB_UPLOAD_INVALID` |
| Upload: chunk size limit | none met: 8 MiB in one `PATCH` |
| Upload: session status (`GET`) | works, 204 with `Range` |
| Upload: cancel (`DELETE`) | works: 204, then its `GET` 404 `BLOB_UPLOAD_UNKNOWN` |
| Upload: mount (`POST ?mount=&from=`) | works, 201 |
| Upload: wrong digest, empty blob | 400 `DIGEST_INVALID`, the session stays; the empty blob stored |
| Upload continued by another replica | works (the provider: one starts the session, another streams a chunk and closes it) |
| Garbage collector and an untagged referrer (#37) | *not measured by the probe* |
| What the probe finds | `referrers=false catalog=true` |
| Overrides needed | none |

The routing words, name by name: `x`, `x/manifests/y`, `x/manifests`, `x/referrers/y` and
`x/tags/list` each got `HEAD` 20×200 and `GET` 200.

What is new against the [column](README.md#capabilities):

- **Routing words, read-after-write lag, monotonic reads**: measured for the first time. Gitea
  routes the words right. The lists read before each write did not go stale, a deleted manifest
  was gone from every read at once, and no read over a new connection undid a change. As on
  Forgejo, the `none` is a measurement of three writes and three deletes, not a promise of
  Gitea's. The vendor table's entry for Gitea leaves its consistency unknown, so a publish
  reads back its tag and blobs too, and that is right.
- **Where a repository may be created**: a name of 256 characters and a component of 256 bytes
  are taken, as on Forgejo. The probe does not try `--` or `__`, which Gitea refuses
  ([quirks.md](quirks.md#per-registry)).
- **Delete a repository**: an emptied repository leaves `_catalog`, as before, and its tag list
  answers 200 with `"tags":[]`, not 404.
- **Largest blob, chunk size, manifest size**: only the 8 MiB of the default and the ladder up
  to 16 MiB. They agree with [Blob reads, ranges, sizes](#blob-reads-ranges-sizes), and the
  larger measurements there stand.
- Every other cell is the column's, on both versions.

**One finding is ours, not Gitea's.** In the `-v` log, Gitea answers nearly every request 401
before it answers it with the token: 527 of the 1,135 requests of the 1.27.3 run were such
401s, and 79 fetched a token. The provider asks for no new token after the 401; it sends the
request again with a token it already has. oras-go's auth client caches a token under the
request's scopes merged with the challenge's `scope="*"`, but looks a request up by its own
scopes before the challenge, as item 1 says. `prime` adds that alias for a streamed upload
only (`aliasToken`, internal/provider/generic/uploads.go). So every other request to Gitea
costs one more round trip. Nothing fails, and Forgejo's challenge is the same. **Done in
#1042:** the provider's auth client adds the alias after every challenge that names a scope the
request does not (`aliasClient`, internal/provider/generic/client.go), so only the first
request with a given set of scopes meets the 401; `TestTokenAfterChallenge` counts it. Not
measured with the probe again.

## What the provider must do differently

1. **Done in #342**, the spike of #120: keep the primed token when the challenge names extra
   scopes. Gitea's challenge always says `scope="*"`, which oras-go's auth client adds to the key of the token
   it caches, but not to the key it looks a request up by before it is challenged. The token
   that `prime` (the bodyless `GET` before a streamed body) fetched was never found, the
   streamed `PATCH` or `PUT` went out without it, got the 401 and could not be sent again:
   `request body is not rewindable`. Every blob upload through the facade (`POST ?digest=`,
   `PUT` of a session) and every upload continued by another replica failed with 500. `prime`
   now caches the token under the request's own scopes too; `TestUploadAcrossProviders` runs
   with a challenge scope of `*` (commit `fix(provider): keep the primed token …`).
2. Know that tags are case-insensitive. Gitea stores a tag lower-cased and resolves it in any
   case: `5.4.Final` is listed as `5.4.final`, and a push to `5.4.final` re-points it. Maven
   versions (`-SNAPSHOT`, `.Final`, `-RC1`) carry capitals; pypi normalises to lower case.
   - **Reindex, done in #844.** `tags/list` gives other tags than Lajstrom wrote: `1.4.0-m1` for
     the `1.4.0-M1` that ingest pushed. Reindex now matches a listed tag to the tag of the
     manifest's annotations without case. It does so only when the registry resolves the
     annotations' tag to the same manifest, so a registry that keeps case is not affected. The
     version and the row's tag come from the annotations, as ingest wrote them, and nothing
     decodes the listed tag. A later run finds the row by the listed tag's digest and counts it
     unchanged. The cost is one `HEAD` per run for each tag whose case differs from its
     version's, and a lookup by digest in the index for each tag the index does not have. It is
     tested on a fake that folds case (`TestRebuildFoldedTags`); the integration tests have not
     been run against Gitea for it.
   - **Two versions that differ only in case are one tag.** Through Lajstrom the second one is a
     409, and nothing is overwritten: `ingest.Pipeline.checkTag` resolves its tag before the
     push, and Gitea answers with the first version's manifest, which has another digest
     (`TestFoldedTagCaseVariant`). The message names the tag and that digest, not the case. A
     reindex finds the one manifest the tag holds and indexes it with the version and tag of its
     annotations. A version the index has whose tag a write straight to the registry re-pointed
     is reported as drift and kept, as any re-pointed tag is (`TestFoldedTagRepointed`). Gitea
     deletes the manifest that the tag named before.
   - **Refusing such a version at ingest** with a clear message needs a vendor-table capability
     ("tags fold case"). That is a contract question, #426.
3. Expect no ranges from Gitea (Forgejo has them,
   [forgejo.md](forgejo.md#where-it-differs-from-gitea)). A `Range` on a blob is answered 200
   with the whole blob and no `Accept-Ranges`; the facade and `/raw/` pass that on, which RFC
   9110 allows, but a client that resumes a download starts over. Nothing to change unless
   Lajstrom should serve the range itself.
4. Do not stream a chunk of unknown size into a session that already holds bytes: Gitea and
   Forgejo answer 400 `BLOB_UPLOAD_INVALID` "Stream uploads after first write are not allowed".
   The provider does it for `FinishBlobUpload` with a last chunk of unknown size
   (`TestIntegrationUploads/session across providers`, the "chunk of unknown size" step). A
   docker client sends one streamed `PATCH`, which works. **Done in #425, behind a flag:** with
   `registries[].uploads.spoolUnknownLength: true` the provider spools such a chunk to `$TMPDIR`
   for its request and sends it with `Content-Range`; the first chunk still streams. The flag is
   off by default, and the vendor table leaves it so: Lajstrom runs on ephemeral disks. With it
   off, `serve` logs a hint at start (`vendor=gitea`), and such a chunk fails with the 400,
   which the facade passes on as `400 BLOB_UPLOAD_INVALID` with a hint that names the flag
   (#449).
5. Read a size-limit refusal as one: over `[packages] LIMIT_SIZE_CONTAINER` the closing `PUT`
   is 403 with an empty body, which the provider makes `ErrUnauthorized` (bad credentials), as
   it does Harbor's quota (harbor.md, item 1): the same contract question.
6. **Done in #352:** name the vendor. `GET /v2/` challenges `Bearer
   realm="<ROOT_URL>v2/token",service="container_registry",scope="*"`, which makes it
   `vendor=gitea`, Forgejo too ([forgejo.md, item 6](forgejo.md#what-the-provider-must-do-differently)).
   The entry changes nothing else: the probe is right. Refusing capitals at ingest (item 2) is
   still a contract question.

Nothing else: the token service, the tag schema, catalog, tags, delete, mounts, the monolithic
`POST`, the cancel and the digest check work as the provider expects.

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

A digest delete takes every tag with the manifest, and the blobs only that manifest used: the
empty config of the probe's manifest answered `HEAD` 404 after it. In Gitea a manifest pushed
by tag is that tag's package version; one pushed by digest is a version of its own, named by the
digest, and stays when its tags go. `/v2` has no repository delete; the REST API's package delete was not
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
  there, until a mount (`POST ?mount=&from=` from that image, and `?mount=` alone) answers 201
  and the blob 200 in its new image (`lajstrom registry probe` and curl, 2026-11-10; this page
  said 202, a session, before).
- 256 MiB and 512 MiB in one monolithic `PUT`: 201 in about 1 s each, and 512 MiB in one
  `PATCH` (`lajstrom registry probe`, 2026-11-10); Gitea sets no limit by
  default (`[packages] LIMIT_SIZE_CONTAINER = -1`). Larger blobs were not tried: the machine's
  disk was nearly full. With `LIMIT_SIZE_CONTAINER=1M` a monolithic `PUT` of 2 MiB is 403
  with an empty body, while `PATCH`es past the limit are 202.
- Manifests: 4 MiB, 4 MiB + 1 and 10 MiB are 201; 16 MiB (the probe's ladder) and 32 MiB are
  413 `MANIFEST_INVALID` "Manifest exceeds maximum size": the limit is between 10 and 16 MiB.

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

Forgejo has a page of its own since #816: [forgejo.md](forgejo.md), with [where it differs from
Gitea](forgejo.md#where-it-differs-from-gitea) (ranges work, the upload status is one byte
ahead) and its tests.

## Our tests against it

`LAJSTROM_TEST_REGISTRY=localhost:5009`, `LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom`, the token
in `LAJSTROM_TEST_REGISTRY_PASSWORD`, `LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/`,
`LAJSTROM_TEST_POSTGRES_DSN` of the dev stack, `go test -race -count=1 -tags integration`: the
whole suite against Gitea, before and after the fix of item 1 (2026-11-10). The apt tests run
apt-get in `ubuntu:22.04`, `ubuntu:24.04` and `debian:12`. Forgejo's results are on
[forgejo.md](forgejo.md#our-tests-against-it).

| Test | Gitea, without the fix | Gitea, with the fix |
| --- | --- | --- |
| provider `TestIntegration` | **fail**: open blob with a range, 200 for 206 (the steps after it do not run) | the same |
| provider `TestIntegrationUploads` | **fail**: session across providers, `request body is not rewindable` | **fail**: the same step, the chunk of unknown size: 400 "Stream uploads after first write are not allowed"; **pass** with `uploads.spoolUnknownLength` (#425, 2026-11-10) |
| ingest `TestIntegration` | pass | pass |
| raw `TestIntegration` | **fail**: the range, 200 | **fail**: the range |
| reindex `TestIntegration` | pass | pass |
| facade: delete, delete referrer, pass-through, raw push, referrers, uploads across replicas, tokens across replicas, abandoned monolithic | **fail**: every blob upload 500, not rewindable | pass |
| facade `TestIntegrationBlobReads` | **fail** (the upload) | **fail**: the range |
| `TestAptGet`, `TestAptGetInstall/registry`, `TestAptGetSigned` | pass | pass |

Every other package of the suite passed against Gitea, the Postgres tests included.

**The end-to-end scenarios** (`test/e2e/run.sh`, against Gitea with the fix): `run.sh` had no
registry credentials or prefix then, so a copy of it with `prefix:` and `auth:` in its
configuration ran (`$TMPDIR/spike-e2e/run.sh`); since #353 `run.sh` takes them (below).
`oras`, `auth`, `rbac`, `oci-token`, `tokens`, `apt`, `pypi`, `maven`: pass.
`raw.sh`: **fail** at the range (`got '200', want '206'`), the steps after it do not run.
`cosign.sh`: sign, verify and `oras discover` through Lajstrom pass; it fails at its last step,
which asks the backend for the tag schema without credentials or prefix (401): the script's
limit, not Gitea's. `ha.sh` skipped.

The failures, by owner:

- **Gitea's:** no ranges (item 3), the streamed `PATCH` after the first write (item 4).
- **Ours, the provider:** the challenge scope, fixed here (item 1).
- **Ours, the harness:** `run.sh` without registry credentials, `cosign.sh`'s anonymous check of
  the backend. Both done in #353: `run.sh` takes the user, a file with its token and the prefix
  ([testing.md](../testing.md#a-registry-with-credentials-and-a-prefix)), and `cosign.sh` asks
  the backend below the prefix with them. On 2026-11-10 `oras`, `auth`, `rbac`, `oci-token`,
  `cosign` and `tokens` passed against Gitea 1.27.3 so, the stock `run.sh`. CI runs these six
  against the Gitea of `gitea.sh` on every pull request that touches code: the job
  `e2e (gitea)` of [ci.yml](../../.github/workflows/ci.yml), not required (#353).

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

`LAJSTROM_TEST_REGISTRY_SPOOL_UNKNOWN_LENGTH=1` turns `uploads.spoolUnknownLength` on in the
provider's integration tests ([item 4](#what-the-provider-must-do-differently)); without it
`TestIntegrationUploads` fails at the chunk of unknown size, with it it passes.
