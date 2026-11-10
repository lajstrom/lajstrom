# JFrog Container Registry

What JFrog Container Registry (JCR), the free Artifactory edition for containers, answered on
2026-11-10, measured with `curl`, `oras`, `cosign`, the integration tests and the e2e scenarios
(issue #338). JCR is Artifactory with the Docker, OCI, Helm and Generic package types only; a
registry lives in a repository of Artifactory's, named by its key. The [summary
rows](README.md#jfrog-container-registry) are in the README; this page has the details.

**The answer.** With a local Docker repository as Lajstrom's prefix (`prefix: <repo-key>/`, the
repository path method), Lajstrom's manifest profile is taken, the OCI 1.1 referrers API works
(with the `artifactType` filter and `OCI-Subject`), ingest, raw, oras and cosign pass. It is not
a backend to recommend yet: **JCR does not check the digest of an uploaded blob, and drops the
bytes of a closing `PUT` that follows a `PATCH`**, so a blob can be served under a digest it
does not hash to ([Uploads and digests](#uploads-and-digests)); a blob `HEAD` answers 200 in an
image path where `GET` answers 404, which breaks ingest's read-back of content the repository
already has elsewhere ([Blob reads](#blob-reads-head-and-get)); and the free edition creates no
user, group or permission through its REST API, so Lajstrom runs with the admin's token.

## Version tested

- JFrog Container Registry **7.161.26** (`GET /artifactory/api/system/version`: `"version" :
  "7.161.26"`, `"revision" : "86126900"`, `"license" : "JFrog Container Registry"`, `"addons" : [
  "docker", "properties" ]`), the newest tag on the day: `releases-docker.jfrog.io/jfrog/artifactory-jcr:7.161.26`,
  index `sha256:fd8d64416384c2ede4478c89bfa85ba7e9193db4db0dc36db3a307a0b9a5128b`, linux/arm64
  `sha256:df0244fb…a846d` and linux/amd64 `sha256:c9abe47e…7489a`, built 2026-09-07. Its
  Access service says 7.191.16. The image is 2.1 GB to pull and 5.4 GB on disk.
- PostgreSQL 17.11 (`postgres:17.11-alpine`, the dev stack's image): on its embedded Derby this
  Artifactory does not start, "Cannot start the application with a database other than
  PostgreSQL".
- On macOS with OrbStack, arm64, natively. **Memory**: the container settles at 3.2 to 3.5 GiB
  under its `mem_limit: 4g` (a JVM, the router, Access, metadata, frontend, observability and
  event services in one container); Postgres 110 MiB. `jcr.sh up` from nothing, setup
  included, in 90 s; the JCR container alone healthy after 78 s.
- The tests with Go 1.27.3 at 6b3b7b7, and with the probe fix that came with this page,
  "fix(provider): count a catalog only when the probe's 200 has one" (#338).

## Run it

[deploy/compose/jfrog-jcr/](../../deploy/compose/jfrog-jcr/): JCR and its Postgres as the
compose project `lajstrom-jcr` on `http://localhost:5010` (`LAJSTROM_JCR_PORT`), with their own
network and volumes, beside the dev stack and not part of it (`make dev-down` does not touch
it). Only the router's port 8082 is published, on 127.0.0.1: the UI, the REST APIs under
`/artifactory` and `/access`, and `/v2`.

```sh
deploy/compose/jfrog-jcr/jcr.sh up      # compose up --wait, then setup
deploy/compose/jfrog-jcr/jcr.sh setup   # the REST setup again; each step is idempotent
deploy/compose/jfrog-jcr/jcr.sh down    # compose down -v, and remove the secrets
```

`setup` does everything through the REST API; nothing is done in the UI. The secrets go to
files under `LAJSTROM_JCR_DIR` (default `deploy/compose/jfrog-jcr/.work`, ignored by git), mode
600, and never reach the terminal: `admin.password`, and `lajstrom.token`, the password of the
user `admin` for Lajstrom and the tests. The steps, with what answered:

| Step | Request | Answer |
| --- | --- | --- |
| Admin password | `POST /artifactory/api/security/users/authorization/changePassword` with `admin:password` (the default) | 200 `Password has been successfully changed`; the old password is 401 afterwards |
| EULA | `POST /artifactory/ui/jcr/eula/accept`, the UI's endpoint (JCR has no other) | 200, empty body |
| An admin token for setup | `POST /access/api/v1/tokens` with Basic, `scope=applied-permissions/admin`, `expires_in=600` | 200; without [access.config.patch.yml](../../deploy/compose/jfrog-jcr/access.config.patch.yml), 401 `Unsupported authentication method Basic` |
| Repositories | `PATCH /artifactory/api/system/configuration`, `Content-Type: application/yaml` | 200 `3 changes to config merged successfully` |
| | `PUT /artifactory/api/repositories/<key>` (the repositories API) | 400 `This REST API is available only in Artifactory Pro` |
| A user, a group | `POST /access/api/v2/users`, `POST /access/api/v2/groups` (admin token) | **403** `{"errors":[{"code":"FORBIDDEN","message":"Forbidden"}]}`, and to `GET` too; `PUT /artifactory/api/security/users/<name>`: 400, Pro only |
| A permission | `POST /access/api/v2/permissions` (admin token) | **403**; `/artifactory/api/v2/security/permissions`: 400, Pro only |
| A token for a transient user | `POST /access/api/v1/tokens`, `username=lajstrom`, `scope=applied-permissions/groups:readers` | 200; the user does not exist and is not created; its push is 403 |
| Lajstrom's token | `POST /access/api/v1/tokens` `{"scope":"applied-permissions/user","expires_in":0}` (admin token) | 200: a token of the admin that never expires |

Access accepts Basic credentials for its token API only with `token.allow-basic-auth: true`
(`access.config.template.yml` in the container: "when enabled, allow basic authentication for
create token api"), which the compose file puts in `/bootstrap/access/`; the image's entrypoint
copies `/bootstrap` into `var/etc` on each start, without overwriting. Without it the only way
to an admin token is the UI. The old token API, `POST /artifactory/api/security/token`, takes
Basic but its tokens are for Artifactory alone: Access answers them 401 `Invalid token,
audience`, and it refuses `scope=applied-permissions/admin` (`Unaccepted scope`).

The repositories, in YAML:

| Key | Class, package type | What for |
| --- | --- | --- |
| `lajstrom` | local, docker, `dockerApiVersion: V2` | Lajstrom's prefix: `prefix: lajstrom/` |
| `lajstrom-oci` | local, oci | the same measurements with the OCI package type |
| `lajstrom-virtual` | virtual, docker, `repositories: [lajstrom]`, `defaultDeploymentRepo: lajstrom` | reads through a virtual repository |

## The repository model and Lajstrom's prefix

JCR has local, remote (a proxy of another registry) and virtual (several behind one URL)
repositories, each with a key, and three Docker access methods; the configuration descriptor
says `<dockerReverseProxyMethod>path</dockerReverseProxyMethod>`, the default:

- **Repository path** (measured): the OCI name is `<repo-key>/<image path>`, on the router's
  host and port, no reverse proxy. Lajstrom's name `raw/scratch/notes` is
  `lajstrom/raw/scratch/notes` with `prefix: lajstrom/`; in Artifactory's tree it is the folder
  `lajstrom/raw/scratch/notes/<tag>/` with a `manifest.json` and the blobs as
  `sha256__<hex>`. One JCR repository holds every Lajstrom repo of the registry entry.
- **Subdomain** and **port** (read in JFrog's documentation, not measured): the host or the port
  names the repository and the OCI name has no key, so Lajstrom's prefix would be empty and its
  URL the repository's host. Both need a reverse proxy in front (a wildcard DNS name, or a port
  per repository).
- **The challenge drops the key.** A request to `/v2/lajstrom/raw/x/blobs/uploads/` is
  challenged with `scope="repository:raw/x:pull,push"`, the name without the repository key;
  the token service grants either spelling. oras-go caches the token under the request's scope
  and the challenge's together, which the provider's streamed requests miss: the same failure as
  Gitea's `scope="*"`, which #342 fixes ([Uploads and digests](#uploads-and-digests)).
- **Without a prefix** the first component of every Lajstrom name is taken for a repository key:
  `POST /v2/raw/scratch/x/blobs/uploads/` is 404 `Repository 'raw' not found`, and the referrers
  probe's 404 makes `referrers=false`. A key is never created on push.
- **A virtual repository** (`lajstrom-virtual`) serves reads: a manifest by tag, its blobs, tag
  lists, the referrers API and the catalog of its members. A push through it lands in its
  default deployment repository. It is not Lajstrom's prefix: a digest `DELETE` through it is 400
  (the raw frontend answers 500), and a range past the end of a blob is 404 `BLOB_UNKNOWN`
  instead of 416. A virtual is for clients that read straight from JCR.
- **The OCI package type** (`lajstrom-oci`) answered the measurements of this page as the docker
  one did: create on push, referrers, catalog, tags, delete, re-tag, ranges, uploads and digests,
  `HEAD` and `GET`, the `Location`, and the integration tests.

## The row

| Row of the matrix | JCR 7.161.26, a local docker repository as the prefix |
| --- | --- |
| Create on push | [yes, inside an existing repository key; a key is never created (404)](#create-on-push) |
| Where a repository may be created | [below `<repo-key>/`, at any depth (12 components; a component of 250 characters)](#create-on-push) |
| Referrers API | [yes, with the `artifactType` filter; no `n`; no tag schema](#referrers) |
| `OCI-Subject` on a push with `subject` | [yes](#referrers) |
| Catalog (`_catalog`) and its limits | [**200 with an empty body** at `/v2/_catalog`; per key at `/v2/<repo-key>/_catalog`, `n`/`last`/`Link`, lexical](#catalog-and-tags) |
| A listing API of its own | [`/artifactory/api/docker/<repo-key>/v2/_catalog`, and the storage API](#catalog-and-tags) |
| Tag list order and pagination | [lexical; `n`/`last`/`Link`](#catalog-and-tags) |
| Delete a manifest by digest | [yes, 202, with its tags; its referrers stay](#delete) |
| Delete by tag | [yes, 202: the manifest stays](#delete) |
| Delete a repository | [Artifactory's `DELETE /artifactory/<repo-key>/<path>`, 204](#delete) |
| Immutable tags, where the rule lives | [none: a tag re-points with 201; Artifactory's Delete/Overwrite permission (not measurable on JCR)](#immutable-tags) |
| Blob redirect | [none on the filesystem store](#blob-reads-head-and-get) |
| `Range` and `Accept-Ranges` | [206, 416 past the end, multipart for two ranges; `Accept-Ranges` on `GET`, not on `HEAD`](#blob-reads-head-and-get) |
| Largest blob measured | [1 GiB in one `PUT`; 2 GiB not run (disk)](#blob-reads-head-and-get) |
| Manifest size limit | [none met: 8 MiB + 1 byte is 201](#blob-reads-head-and-get) |
| Lajstrom's manifest profile (`artifactType`, empty config, own media types) | [accepted](#media-types) |
| Auth | [Bearer from `/v2/token`; Basic straight on `/v2` too; an access token as the password](#version-tested) |
| Rate limits seen | [none](#our-tests-against-it) |
| Upload: chunk out of order | [416](#uploads-and-digests) |
| Upload: chunk size limit | *not measured* |
| Upload: session status (`GET`) | [works, 204 with `Range`; `HEAD` too](#uploads-and-digests) |
| Upload: cancel (`DELETE`) | [**405**; the session stays](#uploads-and-digests) |
| Upload: wrong digest, empty blob | [**a wrong digest is 201 and stored**; the empty blob stored](#uploads-and-digests) |
| Upload: the closing `PUT` with a body after a `PATCH` | [**201, the body dropped**](#uploads-and-digests) |
| Upload: a chunk without `Content-Range` | [the first works; a later one is 416](#uploads-and-digests) |
| Upload continued by another replica | [fails: ours (the challenge's scope, #342), then JCR's dropped body](#uploads-and-digests) |
| Mount from another repository | [202: a new session, no mount](#uploads-and-digests) |
| Blob `HEAD` and `GET` | [**`HEAD` 200, `GET` 404** in an image path where only another path has the blob](#blob-reads-head-and-get) |
| Garbage collector and an untagged referrer (#37) | *not measured* |
| What the probe finds | [`referrers=true catalog=true`, wrong; with the probe fix `catalog=false`](#what-the-provider-must-do-differently) |
| Overrides needed | [none with the probe fix; `prefix: <repo-key>/`](#configuration) |

The probe reports, with the probe fix, `vendor=unknown createOnPush=true referrers=true
catalog=false deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0` (the
test log, with the prefix). On main it said `catalog=true`, and every `ListRepositories` failed.

## What the provider must do differently

1. **Done with this page: count a catalog only when the probe's 200 has one.** `GET
   /v2/_catalog?n=1` is 200 with `Content-Length: 0` on JCR's router; the probe took the 200 for
   a catalog and `ListRepositories` failed with `failed to decode response: EOF`
   (`TestIntegration/list_repositories`, reindex). The probe now wants a JSON object with
   `repositories`.
2. **Never put bytes in the closing `PUT` after a `PATCH`.** JCR keeps the `PATCH`es and drops
   the body of the closing `PUT` ([Uploads and digests](#uploads-and-digests)): 201, and the blob
   is short, under the digest of the whole. The provider's `FinishBlobUpload` with a last chunk
   should send it as a `PATCH` and close with an empty `PUT` when the session already holds
   bytes; the facade's `TestIntegrationUploadAcrossReplicas` fails on this (`mismatch
   Content-Length` on the read-back). A single `PUT` after the `POST` keeps its body. **Done in
   #352:** `FinishBlobUpload` puts a last chunk in the closing `PUT` only when the session holds
   no bytes yet; otherwise it sends the chunk as a `PATCH` with its `Content-Range` and closes
   with an empty `PUT`, on every registry. The unit tests check the requests; not run against
   JCR again.
3. **Do not rely on the registry for the digest.** JCR stores whatever it is sent under the digest
   it is told, in a session or monolithically, and serves it so. Ingest computes its digests from
   its spool, so what it pushes is right; the facade passed a client's digest through, so a
   client could store bytes under a digest they do not hash to, which every later reader gets.
   Done in #368: the facade hashes what it streams, with the sha256 state in the session row
   across replicas, and holds a blob's last byte back until the digest matches (threat model,
   [P-9](../security/threat-model.md)).
4. **Check a blob's existence with `GET`, or push without checking, on JCR.** `HEAD` of a digest
   that a manifest anywhere in the repository key names is 200 in every image path, while `GET`
   there is 404 until a manifest of that path names it ([Blob reads](#blob-reads-head-and-get)).
   Ingest asks `HEAD`, skips the upload, then reads the blob back to validate it (apt reads the
   package, ingest the metadata layer) and fails `provider: not found`: the same `.deb` in two
   Lajstrom apt repos, or any content the JCR repository already has, is refused. A manifest that
   names such a blob without an upload is 201, so only the read before it fails. Validating from
   the spool, as [ghcr.md](ghcr.md) asks, fixes it too. **Since #352** the provider asks a JFrog
   registry with a `GET` of the blob's first byte (`Range: bytes=0-0`) for `StatBlob` and a
   blob's `Exists`, never `HEAD`: ingest uploads what this path does not serve, and the facade
   answers a client's `HEAD`, and checks a manifest's layers, as `GET` would. The unit tests
   answer as JCR did; not run against JCR again.
5. **Hold the token for the repository's scope however the challenge spells it.** The challenge
   names the image without the key; #342 (Gitea's `scope="*"`) caches the token under the
   request's own scopes as well, and with it the facade's uploads pass on JCR but for item 2.
6. Optional: a chunk of unknown size after the first is 416 without `Content-Range`. The
   provider sends one so (`AppendBlobChunk(…, -1)`); docker sends a single streamed `PATCH`,
   which JCR takes. Spooling such a chunk to know its size would do. **Done in #425, behind a
   flag:** `registries[].uploads.spoolUnknownLength: true` spools it and sends it with
   `Content-Range`; off by default, with a hint at start (`vendor=jfrog`). The unit tests answer
   416 as JCR did; not run against JCR again.
7. Optional: name the vendor. Every answer of JCR, the 200 of `GET /v2/` included, has
   `X-Jfrog-Version: Artifactory/7.161.26 86126900` and `X-Artifactory-Id`; a `jfrog` vendor
   could log what items 2 to 4 cost, and list repositories through `/v2/<repo-key>/_catalog`
   (with the key put back on the names) instead of having none. **Since #352** it is
   `vendor=jfrog` and carries item 4; the per-key catalog is not done.
8. Nothing to do: the cancel of an upload session is 405 and the session stays until JCR purges
   it (when, not measured); the facade logs `facade: cancel backend upload session …
   unsupported (HTTP 405, )` and goes on.

## Create on push

```text
POST /v2/nosuchkey/x/blobs/uploads/          404 {"errors":[{"status":404,"message":"Repository 'nosuchkey' not found"}]}
POST /v2/lajstrom/blobs/uploads/             404 {"errors":[{"status":404,"message":"Not Found"}]}
POST /v2/raw/scratch/x/blobs/uploads/        404 … "Repository 'raw' not found"      (no prefix)
POST /v2/lajstrom/maven/repo/org/example/group/artifact/sub/1/2/3/4/5/blobs/uploads/   202; PUT 201; manifest 201
PUT  /v2/lajstrom/long/<250 × a>/manifests/v1                                          201
```

An image path is created by its first manifest, at any depth below the key; a key is not. The
404s carry Artifactory's error body (`status` and `message`, no OCI `code`); the provider reads
the status. Lajstrom's provider with `prefix: ""` fails `TestIntegration` at its first push,
`provider: not found (HTTP 404, )`.

## Referrers

The OCI 1.1 referrers API, in `lajstrom/ref-3376/pkg`: a subject `S` tagged `v1`, then two
referrers pushed by digest, without a tag, one with `artifactType` and one with only a config
media type:

```text
PUT manifests/sha256:5a63f680…   201  OCI-Subject: sha256:ce794b4a…   (artifactType application/vnd.lajstrom.test.signature.v1)
PUT manifests/sha256:7841b81f…   201  OCI-Subject: sha256:ce794b4a…   (config application/vnd.lajstrom.test.sbom.v1+json)
GET referrers/sha256:ce794b4a…   200  Content-Type: application/vnd.oci.image.index.v1+json
    [{"artifactType":"application/vnd.lajstrom.test.signature.v1","digest":"sha256:5a63f680…","size":620},
     {"artifactType":"application/vnd.lajstrom.test.sbom.v1+json","digest":"sha256:7841b81f…","size":555}]
GET referrers/…?artifactType=application/vnd.lajstrom.test.signature.v1   200  OCI-Filters-Applied: artifactType, one manifest
GET referrers/…?n=1              200, both
GET /v2/lajstrom/lajstrom-probe/referrers/<zero digest>   200 {"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}
GET /v2/nosuchrepo/x/referrers/<zero digest>              404
PUT a referrer of sha256:1111… (no such subject)          201  OCI-Subject: sha256:1111…, and listed
tags/list of the image           {"tags":["v1"]}: no sha256-<digest> tag
```

The artifact type of a referrer without one is its config media type. `cosign sign` and `oras
discover` through Lajstrom used the API (the e2e below).

## Catalog and tags

```text
GET /v2/_catalog?n=2                                    200  Content-Length: 0
GET /v2/lajstrom/_catalog?n=2                           200  Link: </v2/lajstrom/_catalog?last=apt/it-8b050f8f/hello-data&n=2>; rel="next"
    {"repositories":["apt/it-8b050f8f/hello","apt/it-8b050f8f/hello-data"]}
GET /artifactory/api/docker/lajstrom/v2/_catalog?n=2    200  Link: </v2/_catalog?last=apt/it-8b050f8f/hello-data&n=2>; rel="next"
GET …?n=2&last=apt/it-8b050f8f/hello-data               200  {"repositories":["cat-3376/r1","cat-3376/r2"]}
GET …?n=0                                               200, every image (33)
GET /artifactory/api/docker/lajstrom-virtual/v2/_catalog?n=3   200, the members' images
GET /v2/lajstrom/cat-3376/r1/tags/list?n=2              200  Link: </v2/lajstrom/cat-3376/r1/tags/list?last=v2&n=2>; rel="next"   {"name":"cat-3376/r1","tags":["v1","v2"]}
GET …?n=2&last=v2                                       200  Link: …last=v4…   ["v3","v4"]
GET /v2/lajstrom/nope-3376/x/tags/list                  404 {"errors":[{"code":"NAME_UNKNOWN","message":"Repository name not known to registry.","detail":{"name":"nope-3376/x"}}]}
```

The router's `/v2/_catalog` answers 200 with an empty body, with Basic credentials and with a
Bearer token for `registry:catalog:*` alike. The catalog is per repository key, and its names
leave the key out. Artifactory's Docker API (`/artifactory/api/docker/<key>/v2/_catalog`)
paginates with a `Link` to `/v2/_catalog`, the empty one; `/v2/<key>/_catalog` links to itself.
Order: lexical, for images and tags.

## Delete

```text
DELETE manifests/v2 (by tag)                    202   v2 404, v3 and the digest 200
DELETE manifests/sha256:<m>                     202   every tag and the digest 404; tags/list 404 NAME_UNKNOWN
DELETE the same digest again                    404
DELETE manifests/sha256:<S> (a subject)         202   its two referrers still listed and 200
DELETE blobs/sha256:<…>                         405
DELETE through lajstrom-virtual                 400
DELETE /artifactory/lajstrom/raw/it-1bc815ae/bin/tool    204   tags/list 404, gone from the catalog
```

Lajstrom deletes a package's referrers itself before its manifest; the integration tests'
`delete` steps passed.

## Immutable tags

```text
PUT another manifest as cat-3376/r2:v1          201   v1 is the new digest
PUT the same manifest again under v1            201
```

No option of a local Docker repository makes tags immutable. Artifactory stops a re-point when
the deployer lacks the Delete/Overwrite permission (`enforceStrictTagOverwrite` in the
configuration descriptor is false); on JCR no user other than the admin can be made through the
REST API, and the admin may overwrite, so it was not measured. Lajstrom's ingest refuses a
second digest under a tag itself (oras e2e: `tag invalid: the tag is taken`).

## Uploads and digests

In `lajstrom/curl/up-18721` and `curl/dg-15391`, with curl, each line one request:

```text
POST blobs/uploads/                             202  Location: …/blobs/uploads/03d408a1-….patch
PATCH 'chunk one ', Content-Range: 0-9          202  Range: 0-9
PATCH 'chunk two ', chunked, no Content-Range   416
GET  <session>                                  204  Range: 0-9   (HEAD the same)
PATCH 'chunk two ', Content-Range: 10-19        202  Range: 0-19
PUT  'end', Content-Range: 20-22, ?digest=sha256("chunk one chunk two end")
                                                201  Docker-Content-Digest: sha256:d2109701…
GET  blobs/sha256:d2109701…                     "chunk one chunk two " (20 bytes): the body of the PUT dropped
PATCH abc 0-2, then x 1-1                       202, 416      (out of order)
DELETE <session>                                405, and the session still answers GET 204
PATCH 'abc', PUT ?digest=sha256("other")        201  Docker-Content-Digest: sha256(other); GET serves "abc"
POST, PUT 'p15391' ?digest=sha256("q15391")     201; GET serves "p15391" under the digest of "q15391"
POST ?digest=sha256("x15391") with 'y15391'     201; GET serves 0 bytes
POST, PATCH chunked without Content-Range (the first chunk), PUT ?digest   201, right
POST, PUT 'whole' ?digest=sha256("whole")       201, right
PUT ?digest=sha256("") , empty body             201; HEAD 200, Content-Length: 0
POST ?mount=<digest>&from=lajstrom/curl/a-16298 202: a new session, no mount
```

The same on `lajstrom-oci`. A manifest naming a blob stored under a wrong digest is 201, and
Artifactory's own storage API shows the difference: `GET
/artifactory/api/storage/lajstrom/curl/dg2-17428/v1/sha256__d20f57a5…` says `"sha256" :
"c34a7eeb…"`, the hash of the bytes. Through Lajstrom on main, the provider's streamed requests
after a session started also go out without the token (item 5): `request body is not
rewindable` on the facade's uploads. With #342's fix applied, the facade's suite passes but for
`TestIntegrationUploadAcrossReplicas` (`mismatch Content-Length`, item 2), and
`TestIntegrationUploads` stops at the chunk of unknown size (416, item 6).

## Blob reads, HEAD and GET

A blob that a manifest names in one image path, `HEAD` and `GET` in others:

```text
curl/a-14736: upload the config 201, the blob 201, a manifest naming it 201
curl/b-14736 (nothing pushed):        HEAD 200, GET 404
other/c-14736 (nothing pushed):       HEAD 200, GET 404
curl/b-14736: a manifest naming it, no upload   201; then HEAD 200, GET 200
a digest pushed nowhere:              HEAD 404
a blob uploaded, no manifest yet:     HEAD and GET 200 in its own path; 404 and 404 in another
```

The apt tests show the cost: `TestAptGet` passes on a fresh JCR; the next tests push the same
`.deb` to new paths, and JCR's `artifactory-request.log` has `HEAD
/api/docker/lajstrom/v2/apt/it-797f5869/hello/blobs/sha256:46b00a18…|200` then the `GET` of it
`|404` (`deb: read the package: ingest: fetch sha256:46b00a18… … provider: not found`). The
ingest tests fail the same way at their metadata layer on a later run, once it exists elsewhere.

The rest of a blob read:

- No redirect: the filesystem store streams, one request.
- `GET` 200 with `Accept-Ranges: bytes` and `Docker-Content-Digest`; `HEAD` the same without
  `Accept-Ranges`; `Range: bytes=0-4` 206 `Content-Range: bytes 0-4/13`; past the end 416
  `bytes */13`; two ranges 206 `multipart/byteranges`; a missing blob 404.
- 1 GiB of zeros in one streamed `PUT`: 201 in 5 s, `GET` hashes right in 2 s. 2 GiB was not
  pushed: the Docker VM, shared with other containers, had 4 GB free.
- Manifests of 4194305 and 8388609 bytes: 201. Lajstrom's `server.maxManifestSize` (4 MiB)
  refuses a larger one first.
- A manifest `GET` needs an OCI type in `Accept`: none, `*/*` or the Docker v2 type alone is
  404, as on Harbor and distribution.
- The `Location` of a manifest `PUT` is `…/v2/<name>/blobs/uploads/sha256:<manifest digest>`,
  which answers `GET` 405: not a manifest URL. Nothing in Lajstrom reads it.

## Media types

Nothing in the profile is refused: `artifactType`, the empty config with `data`,
`application/vnd.lajstrom.*` layers, annotations; the manifest comes back byte for byte. The
provider's `TestIntegration`, ingest's, raw's and the oras and cosign e2e push Lajstrom's
manifests of raw, deb, maven-like paths and OCI images, and cosign's bundle and legacy
signatures.

## Our tests against it

`LAJSTROM_TEST_REGISTRY=localhost:5010`, `LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom/`,
`LAJSTROM_TEST_REGISTRY_USERNAME=admin`, the token in `LAJSTROM_TEST_REGISTRY_PASSWORD`, the dev
stack's Postgres; on a fresh JCR, `go test -race -count=1 -tags integration` of the provider,
ingest, raw, facade, deb and reindex packages:

| Test | main | main + the probe fix | + #342's token fix |
| --- | --- | --- | --- |
| provider `TestIntegration` | **fail**: `list repositories`, EOF | pass; `list repositories` skipped (no catalog) | pass |
| provider `TestIntegrationUploads` | **fail**: not rewindable | the same | **fail**: the chunk of unknown size, 416 |
| ingest | pass on a fresh JCR; on a later run `TestSessionsIntegration` fails (`HEAD` 200, `GET` 404) | the same | the same |
| raw `TestIntegration` | pass | pass | pass |
| facade: delete, pass-through, raw push, blob reads, referrers, tokens | **fail**: 500, not rewindable | the same | pass |
| facade `TestIntegrationUploadAcrossReplicas` | **fail** | **fail** | **fail**: `mismatch Content-Length` (the dropped `PUT` body) |
| `TestAptGet` | pass on a fresh JCR, fails on a later run (`HEAD` 200, `GET` 404) | the same | the same |
| `TestAptGetInstall/registry`, `TestAptGetSigned` | **fail**: the same `.deb` already in the key | the same | the same |
| reindex `TestIntegration` | **fail**: EOF | **fail**: no catalog, no OCI repository found (#319) | the same |
| provider `TestIntegration` through `lajstrom-virtual/` | **fail**: a range past the end 404 | not run | not run |
| raw `TestIntegration` through `lajstrom-virtual/` | **fail**: `DELETE` 500 (JCR's 400) | not run | not run |
| the suite on `lajstrom-oci/` | the same failures as on `lajstrom/` | not run | not run |

The e2e scenarios with `LAJSTROM_E2E_REGISTRY=localhost:5010`, the registry `local` of the run
given `prefix: lajstrom/` and `auth: {username: admin, passwordEnv:
LAJSTROM_E2E_REGISTRY_PASSWORD}` ([Commands](#commands)):

| Scenario | main | + #342's token fix |
| --- | --- | --- |
| `oras.sh` | pass | pass |
| `cosign.sh` | pass | pass |

`cosign.sh` reads the backend directly at one step, without credentials and without the prefix
(its 401 is the harness's, not JCR's); the runs left that step out, and the referrers API on
JCR listed the bundle. No 429 or other rate limit in any run.

## Configuration

```yaml
registries:
  - name: jcr
    type: generic
    url: http://localhost:5010
    prefix: lajstrom/                   # required: the key of a local docker (or oci) repository
    auth:
      username: admin                   # JCR creates no other user through its REST API
      passwordEnv: LAJSTROM_JCR_TOKEN   # an access token (jcr.sh: lajstrom.token)
```

With the probe fix the probe needs no override. Until items 2 to 4 are done, JCR is a backend for
trying Lajstrom, not for data.

## Deviations from the specification

For the README's table; measured, each against distribution-spec v1.1.1:

| What JCR 7.161.26 does | What the spec says |
| --- | --- |
| The body of a closing `PUT` after `PATCH`es is dropped: 201, the blob short | "The final chunk MAY be uploaded using a `PATCH` request or it MAY be uploaded in the closing `PUT` request" |
| A blob is stored and served under any digest the client names (session or monolithic) | "A GET request to an existing blob URL MUST provide the expected blob"; the error table has `DIGEST_INVALID`, "provided digest did not match uploaded content", unbound to a status |
| `HEAD` of a blob 200 where `GET` is 404 | "A HEAD request to an existing blob or manifest URL MUST return `200 OK`"; "A GET request to an existing blob URL MUST provide the expected blob" |
| The `Location` of a manifest `PUT` is an upload URL that answers 405 | "The `<location>` is a pullable manifest URL" |
| `GET /v2/_catalog` is 200 with an empty body | not a deviation: `_catalog` is not in the spec |
| The cancel of an upload session is 405; a mount is a new session (202) | not deviations: the cancel is not in 1.1.1, and "the registry MAY" mount |

## Commands

JCR as in [Run it](#run-it), with `LAJSTROM_JCR_DIR` pointing at a directory for the secrets.
curl reads the token from a config file, never from its command line:

```sh
export LAJSTROM_JCR_DIR=$TMPDIR/jcr-state
deploy/compose/jfrog-jcr/jcr.sh up
printf 'user = "admin:%s"\n' "$(cat "$LAJSTROM_JCR_DIR/lajstrom.token")" > "$LAJSTROM_JCR_DIR/curlrc"
J=http://localhost:5010; c() { curl -K "$LAJSTROM_JCR_DIR/curlrc" -s "$@"; }
c -i "$J/v2/_catalog?n=2"
c -i "$J/v2/lajstrom/_catalog?n=2"
c -i "$J/v2/lajstrom/<image>/referrers/<digest>?artifactType=<type>"
L=$(c -i -X POST "$J/v2/lajstrom/curl/up/blobs/uploads/" | tr -d '\r' | awk -F': ' 'tolower($1)=="location"{print $2}')
c -i -X PATCH -H 'Content-Range: 0-2' --data-binary abc "$L"
c -i -X PUT --data-binary def "$L?digest=sha256:$(printf abcdef | shasum -a 256 | cut -d' ' -f1)"
c "$J/v2/lajstrom/curl/up/blobs/sha256:$(printf abcdef | shasum -a 256 | cut -d' ' -f1)"   # abc
c "$J/artifactory/api/storage/lajstrom/<image>/<tag>/sha256__<hex>"                       # the real sha256
curl -s -i "$J/v2/lajstrom/raw/x/blobs/uploads/" -X POST | grep -i www-authenticate        # the scope without the key
```

The integration tests:

```sh
LAJSTROM_TEST_REGISTRY=localhost:5010 LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom/ \
LAJSTROM_TEST_REGISTRY_USERNAME=admin LAJSTROM_TEST_REGISTRY_PASSWORD="$(cat "$LAJSTROM_JCR_DIR/lajstrom.token")" \
LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' \
  go test -race -count=1 -tags integration ./internal/provider/generic/ ./internal/ingest/ \
    ./internal/format/raw/ ./internal/facade/ ./internal/format/deb/ ./internal/index/reindex/
```

`LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-virtual/` and `lajstrom-oci/` for the other two
repositories. The e2e harness has no setting for a prefix or credentials of its registry, so
the runs used a copy of `test/e2e/run.sh` with `lib.sh` and `here` pointing at `test/e2e/`, the
registry `local` given the prefix and the `auth` above, and `LAJSTROM_E2E_REGISTRY=` before the
server starts, which leaves out cosign.sh's anonymous look at the backend:

```sh
root=$PWD
sed -e "s|^\. \"\$(dirname \"\$0\")/lib.sh\"\$|. $root/test/e2e/lib.sh|" -e "s|^here=.*|here=$root/test/e2e|" \
    -e 's|^    url: "http://$registry"$|&\n    prefix: lajstrom/\n    auth: {username: admin, passwordEnv: LAJSTROM_E2E_REGISTRY_PASSWORD}|' \
    -e 's|^"$bin" migrate --config|LAJSTROM_E2E_REGISTRY=\n&|' test/e2e/run.sh >"$TMPDIR/jcr-run.sh"
LAJSTROM_E2E_REGISTRY=localhost:5010 LAJSTROM_E2E_REGISTRY_PASSWORD="$(cat "$LAJSTROM_JCR_DIR/lajstrom.token")" \
  bash "$TMPDIR/jcr-run.sh" oras cosign
deploy/compose/jfrog-jcr/jcr.sh down
```
