# JFrog Container Registry

What JFrog Container Registry (JCR), the free Artifactory edition for containers, answered on
2026-11-10, measured with `curl`, `oras`, `cosign`, the integration tests and the e2e scenarios
(issue #338), and again on 2026-11-13 and 14 after #813, with `lajstrom registry probe` too
(issue #947, [Again after #813](#again-after-813)). JCR is Artifactory with the Docker, OCI, Helm and Generic package types only; a
registry lives in a repository of Artifactory's, named by its key. The [summary
rows](README.md#jfrog-container-registry) are in the README; this page has the details.

**The answer.** With a local Docker repository as Lajstrom's prefix (`prefix: <repo-key>/`, the
repository path method), Lajstrom's manifest profile is taken, the OCI 1.1 referrers API works
(with the `artifactType` filter and `OCI-Subject`), ingest, raw, oras and cosign pass. It was not
a backend to recommend that day: **JCR does not check the digest of an uploaded blob, and drops
the bytes of a closing `PUT` that follows a `PATCH`**, so a blob can be served under a digest it
does not hash to ([Uploads and digests](#uploads-and-digests)); a blob `HEAD` answers 200 in an
image path where `GET` answers 404, which breaks ingest's read-back of content the repository
already has elsewhere ([Blob reads](#blob-reads-head-and-get)); and the free edition creates no
user, group or permission through its REST API, so Lajstrom runs with the admin's token. The
code works around the first three since (items 2 to 4 of
[What the provider must do differently](#what-the-provider-must-do-differently): #352, #368),
and **measured again on 2026-11-13 and 14 the three workarounds hold**: through Lajstrom a wrong
digest is `400 DIGEST_INVALID` and nothing is stored, a closing `PUT` after a `PATCH` keeps its
bytes, and the apt and ingest tests that failed on a second run pass twice in a row
([Again after #813](#again-after-813)). What is left: the admin's token; no catalog Lajstrom
uses, so a reindex from an empty index finds no OCI repository; JCR deletes an image path with
its blobs when its last manifest goes, and a re-pointed tag deletes the manifest it named; a
manifest naming a missing blob is `MANIFEST_INVALID`, not `MANIFEST_BLOB_UNKNOWN`; the
referrers list lags a write by milliseconds; and two answers of Lajstrom's own to JCR's upload
sessions (items 9 and 10). The rebuild is what keeps Quay a backend for trying too: until the
provider lists repositories through the per-key catalog (item 7), JCR is one for trying
Lajstrom, not for data.

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
- Again on 2026-11-13 and 14 ([Again after #813](#again-after-813)): the same image, still the
  newest tag (`latest` has the same index digest, `Last-Modified: Tue, 08 Sep 2026`); Go 1.27.4
  at 20ce9499. The container at 3.4 to 3.8 GiB of its 4 GiB; `jcr.sh up` with the image pulled
  in 87 s.

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
  Gitea's `scope="*"`, which #342, the spike of #120, fixed
  ([Uploads and digests](#uploads-and-digests)).
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

`lajstrom registry probe` measures every row of this table but the listing API, auth, rate
limits and the garbage collector, which still need a person;
[README.md](README.md#measuring-a-registry-lajstrom-registry-probe) says how.

Measured on 2026-11-10 by hand; the cells marked (probe) by `lajstrom registry probe` on
2026-11-13 ([Again after #813](#again-after-813)), which agreed with the others.

| Row of the matrix | JCR 7.161.26, a local docker repository as the prefix |
| --- | --- |
| Create on push | [yes, inside an existing repository key; a key is never created (404)](#create-on-push) |
| Where a repository may be created | [below `<repo-key>/`, at any depth (12 components; a component of 250 characters)](#create-on-push); [inside another repository too; 256 characters taken (probe)](#again-after-813) |
| Routing words in a name (`manifests`, `referrers`, `tags/list`) | [ok: 20 blob `HEAD`s in each name, all 200 (probe)](#again-after-813) |
| Referrers API | [yes, with the `artifactType` filter; no `n`; no tag schema](#referrers); [the list lags a referrer's push and its delete by about 10 ms](#again-after-813) |
| `OCI-Subject` on a push with `subject` | [yes](#referrers) |
| Catalog (`_catalog`) and its limits | [**200 with an empty body** at `/v2/_catalog`; per key at `/v2/<repo-key>/_catalog`, `n`/`last`/`Link`, lexical](#catalog-and-tags) |
| A listing API of its own | [`/artifactory/api/docker/<repo-key>/v2/_catalog`, and the storage API](#catalog-and-tags) |
| Tag list order and pagination | [lexical; `n`/`last`/`Link`](#catalog-and-tags) |
| Read-after-write lag | [none seen in 3 writes and 3 deletes: manifest by digest and by tag, blob, tag list (probe)](#again-after-813) |
| Monotonic reads | [none: 3 writes, a tag move, 3 deletes, each read 5 times more over new connections (probe)](#again-after-813) |
| Delete a manifest by digest | [yes, 202, with its tags; its referrers stay](#delete); [one an index lists too (probe); **the last manifest of an image path takes the path and its blobs**](#again-after-813) |
| Delete by tag | [yes, 202: the manifest stays while another tag holds it](#delete); [**the manifest too**, when it was pushed by that tag only (probe)](#again-after-813) |
| Delete a repository | [Artifactory's `DELETE /artifactory/<repo-key>/<path>`, 204](#delete) |
| Immutable tags, where the rule lives | [none: a tag re-points with 201; Artifactory's Delete/Overwrite permission (not measurable on JCR)](#immutable-tags); [**the manifest it named is deleted** when nothing else holds it (probe)](#again-after-813) |
| Blob redirect | [none on the filesystem store](#blob-reads-head-and-get) |
| `Range` and `Accept-Ranges` | [206, 416 past the end, multipart for two ranges; `Accept-Ranges` on `GET`, not on `HEAD`](#blob-reads-head-and-get) |
| Largest blob measured | [1 GiB in one `PUT`; 2 GiB not run (disk)](#blob-reads-head-and-get) |
| Manifest size limit | [none met: 8 MiB + 1 byte is 201](#blob-reads-head-and-get) |
| Lajstrom's manifest profile (`artifactType`, empty config, own media types) | [accepted](#media-types) |
| Auth | [Bearer from `/v2/token`; Basic straight on `/v2` too; an access token as the password](#version-tested) |
| Rate limits seen | [none](#our-tests-against-it) |
| Upload: chunk out of order | [416](#uploads-and-digests) |
| Upload: chunk size limit | [none met: 8 MiB in one `PATCH` (probe)](#again-after-813) |
| Upload: session status (`GET`) | [works, 204 with `Range`; `HEAD` too](#uploads-and-digests); [**404 `BLOB_UNKNOWN` before the first chunk**, and the session works](#again-after-813) |
| Upload: cancel (`DELETE`) | [**405**; the session stays](#uploads-and-digests); [through Lajstrom 501 (item 9)](#again-after-813), 204 since #1044 (not measured on JCR again) |
| Upload: wrong digest, empty blob | [**a wrong digest is 201 and stored**; the empty blob stored](#uploads-and-digests) |
| Upload: the closing `PUT` with a body after a `PATCH` | [**201, the body dropped**](#uploads-and-digests) |
| Upload: a chunk without `Content-Range` | [the first works; a later one is 416](#uploads-and-digests) |
| Upload continued by another replica | [works since #342 and #352 (item 2): the probe and the facade's test](#again-after-813); on 2026-11-10 [it failed: ours, the challenge's scope, then JCR's dropped body](#uploads-and-digests) |
| Mount from another repository | [**201 without a copy** when a manifest of the source names the blob (in the target `HEAD` 200, `GET` 404 until a manifest names it); 202, a new session, when the source only holds an upload](#again-after-813) |
| Blob `HEAD` and `GET` | [**`HEAD` 200, `GET` 404** in an image path where only another path has the blob](#blob-reads-head-and-get) |
| Garbage collector and an untagged referrer (#37) | *not measured*; [the blobs of an image path go with its last manifest](#again-after-813) |
| What the probe finds | [`vendor=jfrog referrers=true catalog=false` (probe)](#again-after-813); before the probe fix of item 1, [`catalog=true`, wrong](#what-the-provider-must-do-differently) |
| Overrides needed | [none; `prefix: <repo-key>/`; `uploads.spoolUnknownLength` for a chunk of unknown size after the first](#configuration) |

The probe reported, with the probe fix, `vendor=unknown createOnPush=true referrers=true
catalog=false deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0` (the
test log, with the prefix, before #352). On main before the fix it said `catalog=true`, and
every `ListRepositories` failed. On 2026-11-13 it says `vendor=jfrog`, the same capabilities and
`consistency=unknown`, with `blobExistsByGET=true` and the hint for
`uploads.spoolUnknownLength` ([Again after #813](#again-after-813)).

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
   with an empty `PUT`, on every registry. **Measured on JCR on 2026-11-14:** the facade's
   `TestIntegrationUploadAcrossReplicas` passes, and through Lajstrom a `PATCH` of `abc` and a
   closing `PUT` of `def` store `abcdef` ([Again after #813](#again-after-813)).
3. **Do not rely on the registry for the digest.** JCR stores whatever it is sent under the digest
   it is told, in a session or monolithically, and serves it so. Ingest computes its digests from
   the bytes it streams (ADR 0014), so what it pushes is right; the facade passed a client's
   digest through, so a client could store bytes under a digest they do not hash to, which every later reader gets.
   Done in #368: the facade hashes what it streams, with the sha256 state in the session row
   across replicas, and holds a blob's last byte back until the digest matches (threat model,
   [P-9](../security/threat-model.md)). **Measured on JCR on 2026-11-14:** a wrong digest through
   Lajstrom is `400 DIGEST_INVALID`, monolithic and after a `PATCH`, and the blob is not there.
4. **Check a blob's existence with `GET`, or push without checking, on JCR.** `HEAD` of a digest
   that a manifest anywhere in the repository key names is 200 in every image path, while `GET`
   there is 404 until a manifest of that path names it ([Blob reads](#blob-reads-head-and-get)).
   Ingest asks `HEAD`, skips the upload, then reads the blob back to validate it (apt reads the
   package, ingest the metadata layer) and fails `provider: not found`: the same `.deb` in two
   Lajstrom apt repos, or any content the JCR repository already has, is refused. A manifest that
   names such a blob without an upload is 201, so only the read before it fails. Validating the
   content as it streams, instead of reading it back, as [ghcr.md](ghcr.md) asks, fixes it
   too. **Since #352** the provider asks a JFrog registry with a `GET` of the blob's first byte (`Range: bytes=0-0`) for `StatBlob` and a
   blob's `Exists`, never `HEAD`: ingest uploads what this path does not serve, and the facade
   answers a client's `HEAD`, and checks a manifest's layers, as `GET` would. The quirk was set
   by the provider's first probe that succeeds, so a provider that had not probed yet still asked
   `HEAD` (#797: measured with Nexus's quirk on 2026-11-12); since #797 (PR #813) a blob's
   `StatBlob` and `Exists` probe before the provider's first request. **Measured on JCR on
   2026-11-14:** the apt tests and ingest's `TestSessionsIntegration` pass twice in a row on one
   JCR, the second run finding every `.deb` and metadata layer elsewhere in the key, and the
   facade's `TestIntegrationBlobReads` passes on its replica that never probed
   ([Again after #813](#again-after-813)).
5. **Hold the token for the repository's scope however the challenge spells it.** The challenge
   names the image without the key; #342 (Gitea's `scope="*"`) caches the token under the
   request's own scopes as well, and with it the facade's uploads pass on JCR but for item 2.
6. Optional: a chunk of unknown size after the first is 416 without `Content-Range`. The
   provider sends one so (`AppendBlobChunk(…, -1)`); docker sends a single streamed `PATCH`,
   which JCR takes. Spooling such a chunk to know its size would do. **Done in #425, behind a
   flag:** `registries[].uploads.spoolUnknownLength: true` spools it and sends it with
   `Content-Range`; off by default, with a hint at start (`vendor=jfrog`). **Measured on JCR on
   2026-11-13:** without it the provider's chunk of unknown size is
   `registry integration refused the request: 416`, with the hint; with it the step passes.
7. Optional: name the vendor. Every answer of JCR, the 200 of `GET /v2/` included, has
   `X-Jfrog-Version: Artifactory/7.161.26 86126900` and `X-Artifactory-Id`; a `jfrog` vendor
   could log what items 2 to 4 cost, and list repositories through `/v2/<repo-key>/_catalog`
   (with the key put back on the names) instead of having none. **Since #352** it is
   `vendor=jfrog` and carries item 4 (measured 2026-11-13); the per-key catalog is not done, so
   reindex from an empty index still finds no OCI repository ([Again after #813](#again-after-813)).
8. Nothing to do: the cancel of an upload session is 405 and the session stays until JCR purges
   it (when, not measured); the facade logs `facade: cancel backend upload session …
   unsupported (HTTP 405, )` and goes on. A client's own cancel is item 9.
9. **Done in #1044: a client's cancel through the facade was `501`.** It is `204` now: the
   facade drops its session row and leaves JCR's session to expire, and the message of any other
   failed cancel is the facade's own. Before, `cancelUpload`
   (internal/facade/uploads.go) answered the provider's `ErrUnsupported` for JCR's 405 as
   `501 UNSUPPORTED`, with the provider's error as the message, which names JCR's own session
   id, and kept the session row, while `cancelBackend` took the same 405 as harmless. The e2e
   scenario `rbac.sh` failed on it. GHCR answers the cancel 405 too
   ([Again after #813](#again-after-813), item 5).
10. **The status of a session before its first chunk ends it through Lajstrom.** JCR answers it
    `404 BLOB_UNKNOWN` and the session works; the facade takes the 404 for a session the
    registry no longer knows, as http-routing.md says, and ends its own
    ([Again after #813](#again-after-813), item 4). **Done in #1045:** on `vendor=jfrog` a 404 to
    the status of a session that holds no bytes is the session at offset 0 (the quirk
    `emptyStatus404`, set by the probe), so the facade answers `204` with `Range: 0-0` and the
    session goes on; a session JCR no longer knows meets its 404 at the next `PATCH` instead. Not
    measured on JCR again.
11. Nothing to do yet: a manifest naming a blob JCR does not have is `400 MANIFEST_INVALID`, so
    the retries of a manifest on `MANIFEST_BLOB_UNKNOWN` (#842) and the naming of the missing
    blob (#850) do not apply here; JCR showed no blob lag that the retries would cover
    ([Again after #813](#again-after-813), item 2).

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
`delete` steps passed. The probe of 2026-11-13 adds two answers: a tag of a manifest pushed by
that tag only takes the manifest with it (by digest 404), and the delete of an image path's last
manifest takes the path and its blobs ([Again after #813](#again-after-813), items 1 and 3).

## Immutable tags

```text
PUT another manifest as cat-3376/r2:v1          201   v1 is the new digest
PUT the same manifest again under v1            201
```

Measured by the probe on 2026-11-13: when the tag re-points, the manifest it named is gone by
digest (404) if no other tag holds it and it was not pushed by digest
([Again after #813](#again-after-813), item 3).

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

The runs of 2026-11-10; those of 2026-11-13 and 14, with the fixes since, are in
[Again after #813](#again-after-813).

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

## Again after #813

Measured on 2026-11-13 and 14 for #947 and item 1 of #850: the same image and `jcr.sh` (on
port 57170 with `LAJSTROM_JCR_PORT`, and as another compose project with `COMPOSE_PROJECT_NAME`,
which `jcr.sh`'s `docker compose` honours), Lajstrom at 20ce9499: #352's vendor table, #368's
digest check, #425's spooling, #813's quirks before the first read, #845's list priming and
delete lag, #564's monotonic reads. What ran ([Commands](#commands)):

- `lajstrom registry probe -v`, the column of [The row](#the-row) (its cells marked "probe");
- the integration tests of the provider, ingest, raw, facade, deb, reindex and probe packages,
  on SQLite (no `LAJSTROM_TEST_POSTGRES_DSN`: the Postgres variants skipped);
- the stock `test/e2e/run.sh` with `LAJSTROM_E2E_REGISTRY_USER`, `_PASSWORD_FILE` and `_PREFIX`
  (#353): `raw`, `oras`, `auth`, `rbac` and `cosign`;
- curl against JCR, and through a Lajstrom of the e2e harness, for what the tests found.

The probe's start, as `serve` logs it:

```text
registry capabilities registry=localhost:57170 vendor=jfrog createOnPush=true referrers=true catalog=false deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0 consistency=unknown maxNameDepth=0
registry needs workarounds registry=localhost:57170 vendor=jfrog blobsReadWhole=false blobExistsByGET=true pushForDelete=false
hint: the registry refuses a /v2 chunk of unknown length after the first write, and such an upload fails with its 4xx; registries[].uploads.spoolUnknownLength spools that chunk to $TMPDIR
```

**What changed since 2026-11-10: the workarounds hold.**

| | 2026-11-10 | 2026-11-13 and 14 |
| --- | --- | --- |
| A closing `PUT` with a body after a `PATCH` (item 2, #352) | the facade's `TestIntegrationUploadAcrossReplicas` failed, `mismatch Content-Length` | passes; through Lajstrom `PATCH abc`, then `PUT def ?digest=sha256(abcdef)` is 201 and the blob reads `abcdef`; the probe's "Upload continued by another replica": works |
| A wrong digest (item 3, #368) | the facade passed a client's digest through, and JCR stored the bytes under it | through Lajstrom `400 DIGEST_INVALID`, monolithic and after a `PATCH`, and the blob is 404; JCR itself still stores them (the probe) |
| `HEAD` 200 where `GET` is 404 (item 4, #352, #813) | `TestAptGet`, `TestAptGetInstall/registry`, `TestAptGetSigned` and ingest's `TestSessionsIntegration` failed on a second run | all pass, twice in a row on one JCR; the facade's `TestIntegrationBlobReads` passes on its replica that never probed (#813) |
| The challenge's scope (item 5, #342) | the facade's streamed uploads failed, not rewindable | the facade's uploads pass |
| A chunk of unknown size after the first (item 6, #425) | 416 | 416 without the flag, with the hint; passes with `uploads.spoolUnknownLength` |
| The vendor (item 7) | `vendor=unknown` | `vendor=jfrog`, `blobExistsByGET=true` |
| The probe's other rows | not run | routing words ok, no read-after-write lag in 3 writes and 3 deletes, monotonic reads, 8 MiB in one `PATCH`; [The row](#the-row) |

**New, from the probe, the tests and curl.**

1. **An image path goes with its last manifest, and its blobs with it**: the layers no other
   manifest of the key names, and the blobs uploaded there that no manifest names yet. A mount
   (201, below) copies nothing, so a manifest that relies on a mounted blob fails once the source
   path is gone. In `lajstrom/m-a-27800` and its neighbours:

   ```text
   m-a: the config, a layer, a manifest v1 naming them             201 201 201
   POST m-b/blobs/uploads/?mount=<layer>&from=lajstrom/m-a         201   then in m-b: HEAD 200, GET 404
   POST m-d/blobs/uploads/?mount=<blob>&from=lajstrom/m-c          202   (m-c holds the blob as an upload, no manifest)
   m-a: another blob uploaded, no manifest names it                201; HEAD 200
   DELETE m-a/manifests/<v1's digest>, its last manifest           202; tags/list 404
   HEAD, GET of the layer and of the other blob in m-a             404 404, 404 404
   HEAD of the layer in m-b                                        404
   PUT m-b/manifests/v1 naming the layer                           400 MANIFEST_INVALID "Failed to copy blob 7e6967b5… to m-b-27800/v1/sha256__7e6967b5…"
   ```

   Lajstrom leaves blobs to the registry's garbage collector; JCR collects at once, as Forgejo
   does. What fails on it: the facade's `TestIntegrationDeleteInterleaved`, 14 of its 32
   scenarios, where D, or a referrer of D, is pushed again after the delete of D's last manifest
   (1 across x's whole delete, 2b, 4 across the delete, both 5 after the lock or the listing):
   `400 MANIFEST_INVALID`; and ingest's `TestIntegrationPutReferrerSubjectDeleted`, whose late
   referrer is refused `ErrInvalid` as the test wants, but naming its own layer, which went with
   the path (`blob sha256:0890… was not uploaded to raw/it-…/notes`), not the subject, after
   the 30 s a registry not classified strong is read again for.

2. **A manifest naming a blob JCR does not have is `400 MANIFEST_INVALID`**, never
   `MANIFEST_BLOB_UNKNOWN`, for a digest pushed nowhere too: `"detail":{"description":"Failed to
   copy blob <hex> to <path>/<tag>/sha256__<hex>"}`. The facade passes it on, `registry it
   refused the request: 400 MANIFEST_INVALID`; the retries on `MANIFEST_BLOB_UNKNOWN` (#842) and
   the naming of the missing blob (#850) do not apply. No blob lag was seen for them to cover.

3. **A re-pointed tag deletes the manifest it named**, when no other tag holds it and it was not
   pushed by digest (the probe): JCR keeps a manifest in a folder per tag,
   `<image>/<tag>/manifest.json`, or `<image>/sha256:<hex>/` when pushed by digest, as
   Artifactory's storage API shows. Lajstrom's managed formats never re-point a tag; a client of
   an `oci` repo that pushes `app:latest` again deletes the image `latest` named, and its
   referrers stay without it. The facade's interleaving "3 D2 under v1 before find" fails so: D
   by digest is 404, and by digest its delete is 404 with D's referrer still listed.

4. **The status of an upload session before its first chunk is `404 BLOB_UNKNOWN`**
   (`"detail":{"blobSum":"<session id>"}`); after a `PATCH` it is 204 with `Range`, and the
   session works either way. Through Lajstrom the 404 ends the facade's session, as
   http-routing.md says of a session the registry no longer knows:

   ```text
   POST /v2/raw/scratch/status-12081.txt/blobs/uploads/     202
   GET  <the session>                                       404 BLOB_UPLOAD_UNKNOWN
   PATCH abc, Content-Range: 0-2                            404 BLOB_UPLOAD_UNKNOWN
   a second session: PATCH abc 0-2, then GET                202, 204 Range: 0-2
   ```

   A client that asks the status before its first chunk loses the session (item 10).

5. **A client's cancel through Lajstrom is `501`** (item 9). JCR answers the backend's `DELETE`
   405, the provider makes it `ErrUnsupported`, and the facade answers it with the provider's
   error, which names JCR's own session id; `rbac.sh` fails on it (`writer /v2 cancel of the
   upload: got '501', want '204'`):

   ```text
   DELETE /v2/raw/scratch/cancel-15829.txt/blobs/uploads/c18f84ab…   501 {"errors":[{"code":"UNSUPPORTED","message":"generic: cancel upload raw/scratch/cancel-15829.txt@03fe68de-e997-4dc3-9519-47dbf5402e3f.patch: provider: unsupported (HTTP 405, )"}]}
   ```

6. **The referrers list lags by milliseconds.** Manifests, tags and blobs answer at once (the
   probe), but the referrers API lists a new referrer up to 9 ms after its 201, and keeps
   listing a deleted one for about 10 ms after its 202, over one keep-alive connection: the first
   read after the 201 missed it in 18 of 30 tries; the first read after the delete still listed
   it in 22 of 40 (gone within 10.6 ms in the 20 tries timed); a `HEAD` of the referrer by digest
   was 200, and 404, at once. The probe's lag row does not
   read the referrers list. The facade's `TestIntegrationDeleteReferrer` reads it right after
   the 202 and failed in 5 of 16 runs. A push is covered: on a registry not classified strong
   (JCR has no consistency in the vendor table, `consistency=unknown`) ingest's visibility
   barrier reads the subject's referrers until they list it. A delete is not. Leave
   `capabilities.consistency` unset for JCR.

The integration tests, `-tags integration -race`, against `lajstrom/`:

| Test | Result |
| --- | --- |
| provider `TestIntegration`, `TestIntegrationNameLength` | pass; `list repositories` skipped (no catalog) |
| provider `TestIntegrationUploads` | without the flag **fail** at the chunk of unknown size, 416 (item 6). With `LAJSTROM_TEST_REGISTRY_SPOOL_UNKNOWN_LENGTH=1` the session across providers, the downloads, monolithic, chunks of unknown size, empty blob, chunk out of order and chunks split pass; **fail**: `wrong digest` (JCR stores it, so the provider has no refusal to report) and, each at its cancel, `cancel`, `mount` and `streamed writer` (405, `ErrUnsupported`). JCR's |
| ingest | pass, twice, but `TestIntegrationPutReferrerSubjectDeleted` (new item 1); the tag-schema tests skip (referrers API) |
| raw `TestIntegration` | pass |
| facade: delete, delete unsupported, catalog after delete, upload across replicas, monolithic abandoned, pass-through, referrers, token across replicas, raw push, blob reads, proxy repo push, spool budget, client body breaker | pass |
| facade `TestIntegrationDeleteReferrer` | **flaky**, 5 of 16 runs: the referrers list's lag (new item 6). JCR's |
| facade `TestIntegrationDeleteInterleaved` | **fail**, 14 of 32 scenarios (new items 1 and 3). JCR's |
| facade `TestIntegrationOCIProxy`, `TestIntegrationOCIGroup` | **fail** at once: the test seeds its upstream on the same registry without the instance prefix, 404 `Repository 'it-upstream-…' not found`, the test's assumption. With `LAJSTROM_TEST_UPSTREAM_REGISTRY=localhost:5001`, the dev stack's distribution as the upstream, they fail only in their counts of upstream blob `GET`s (3 for 4; 1 for 2; for the shared layer without mounts 0 for 1): JCR takes a manifest whose blobs another path of the key holds, and copies them, so the proxy fetched less. JCR's |
| deb `TestAptGet`, `TestAptGetInstall/registry`, `TestAptGetSigned` | pass, twice in a row |
| reindex `TestIntegrationNoCatalog`, `TestIntegrationRepositoryUnknown` | pass |
| reindex `TestIntegration`, `…Formats`, `…Names`, `…Split` | **fail**, as on 2026-11-10: from an empty index there is no catalog to find OCI repositories in (`list repositories …: no catalog: provider: unsupported`); ours, the per-key catalog of item 7 |
| probe `TestIntegrationRun` | **fail** on its "left behind" list alone: JCR answers a blob's `DELETE` 405, which the spec allows; every cell as the column says |

The e2e scenarios:

| Scenario | Result |
| --- | --- |
| `raw.sh`, `oras.sh`, `auth.sh` | pass |
| `rbac.sh` | **fail** at a writer's cancel of an upload: 501 (new item 5) |
| `cosign.sh` | pass, with the step that reads the backend (since #353) and `lajstrom reindex`, which found the OCI repository in `oci_repos` |

`cosign.sh` ran from a copy whose `docker run` names its containers, a rule of the machine it ran
on; nothing else changed. Not run: `oci-token`, `tokens` and `apt` (oras and apt-get in
containers; the backend parts are those of `oras` and the deb tests), the Postgres variants, the
suites on `lajstrom-oci/` and `lajstrom-virtual/`, a blob above 8 MiB, JCR's own cleanup of
upload sessions. No 429 in any run.

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
    uploads:
      spoolUnknownLength: true          # JCR refuses a chunk of unknown size after the first (416)
```

The probe needs no override; `uploads.spoolUnknownLength` is for clients that stream a chunk of
unknown size after their first (item 6), and writes that chunk to `$TMPDIR` first. Leave
`capabilities.consistency` unset: the referrers list lags (new item 6 of
[Again after #813](#again-after-813)). Items 2 to 4 hold on JCR since the measurement of
2026-11-13 and 14; what is left is in that section.

## Deviations from the specification

For the README's table; measured, each against distribution-spec v1.1.1:

| What JCR 7.161.26 does | What the spec says |
| --- | --- |
| The body of a closing `PUT` after `PATCH`es is dropped: 201, the blob short | "The final chunk MAY be uploaded using a `PATCH` request or it MAY be uploaded in the closing `PUT` request" |
| A blob is stored and served under any digest the client names (session or monolithic) | "A GET request to an existing blob URL MUST provide the expected blob"; the error table has `DIGEST_INVALID`, "provided digest did not match uploaded content", unbound to a status |
| `HEAD` of a blob 200 where `GET` is 404, after a mount's 201 too | "A HEAD request to an existing blob or manifest URL MUST return `200 OK`"; "A GET request to an existing blob URL MUST provide the expected blob" |
| The status of an upload session before its first chunk is 404 `BLOB_UNKNOWN`; the session works (2026-11-14) | "The response to an active upload `<location>` MUST be a `204 No Content` response code", with `Range` |
| The `Location` of a manifest `PUT` is an upload URL that answers 405 | "The `<location>` is a pullable manifest URL" |
| `GET /v2/_catalog` is 200 with an empty body | not a deviation: `_catalog` is not in the spec |
| The cancel of an upload session is 405; a mount is a new session (202) when the source holds the blob as an upload only | not deviations: the cancel is not in 1.1.1, and "the registry MAY" mount |
| A manifest naming a blob JCR does not have is 400 `MANIFEST_INVALID`, "Failed to copy blob …" (2026-11-14) | not a deviation in words: the error table has `MANIFEST_BLOB_UNKNOWN`, "manifest references a manifest or blob unknown to registry", but binds no case to it; a client that waits for that code misses this one |
| A blob's `DELETE` is 405; the blobs of an image path go with its last manifest (2026-11-14) | not deviations: a registry may refuse blob deletes with 405, and retention is not in the spec |

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

The probe, first ([Again after #813](#again-after-813)):

```sh
make build
bin/lajstrom registry probe --url http://localhost:5010 --registry-prefix lajstrom/ \
  --username admin --password-file "$LAJSTROM_JCR_DIR/lajstrom.token" -v > jcr-probe.md
```

The integration tests; `LAJSTROM_TEST_REGISTRY_SPOOL_UNKNOWN_LENGTH=1` turns on
`uploads.spoolUnknownLength` for the provider's upload steps, `LAJSTROM_TEST_POSTGRES_DSN` adds
the Postgres variants, and `LAJSTROM_TEST_UPSTREAM_REGISTRY=localhost:5001` gives the facade's oci
proxy tests an upstream of their own (they seed it without the prefix):

```sh
LAJSTROM_TEST_REGISTRY=localhost:5010 LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom/ \
LAJSTROM_TEST_REGISTRY_USERNAME=admin LAJSTROM_TEST_REGISTRY_PASSWORD="$(cat "$LAJSTROM_JCR_DIR/lajstrom.token")" \
LAJSTROM_TEST_REGISTRY_SPOOL_UNKNOWN_LENGTH=1 \
  go test -race -count=1 -tags integration ./internal/provider/generic/ ./internal/ingest/ \
    ./internal/format/raw/ ./internal/facade/ ./internal/format/deb/ ./internal/index/reindex/ \
    ./internal/provider/probe/
```

The e2e scenarios, with the stock `run.sh` since #353
([testing.md](../testing.md#a-registry-with-credentials-and-a-prefix)):

```sh
LAJSTROM_E2E_REGISTRY=localhost:5010 LAJSTROM_E2E_REGISTRY_USER=admin \
LAJSTROM_E2E_REGISTRY_PASSWORD_FILE="$LAJSTROM_JCR_DIR/lajstrom.token" LAJSTROM_E2E_REGISTRY_PREFIX=lajstrom/ \
  test/e2e/run.sh raw oras auth rbac cosign
deploy/compose/jfrog-jcr/jcr.sh down
```

`LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-virtual/` and `lajstrom-oci/` for the other two
repositories. On 2026-11-10 the e2e harness had no setting for a prefix or credentials of its
registry, so those runs used a copy of `test/e2e/run.sh` with `lib.sh` and `here` pointing at
`test/e2e/`, the registry `local` given the prefix and the `auth` above, and
`LAJSTROM_E2E_REGISTRY=` before the server starts, which left out cosign.sh's anonymous look at
the backend. The copy, for the record:

```sh
root=$PWD
sed -e "s|^\. \"\$(dirname \"\$0\")/lib.sh\"\$|. $root/test/e2e/lib.sh|" -e "s|^here=.*|here=$root/test/e2e|" \
    -e 's|^    url: "http://$registry"$|&\n    prefix: lajstrom/\n    auth: {username: admin, passwordEnv: LAJSTROM_E2E_REGISTRY_PASSWORD}|' \
    -e 's|^"$bin" migrate --config|LAJSTROM_E2E_REGISTRY=\n&|' test/e2e/run.sh >"$TMPDIR/jcr-run.sh"
LAJSTROM_E2E_REGISTRY=localhost:5010 LAJSTROM_E2E_REGISTRY_PASSWORD="$(cat "$LAJSTROM_JCR_DIR/lajstrom.token")" \
  bash "$TMPDIR/jcr-run.sh" oras cosign
```
