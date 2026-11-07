# Registries tested with Lajstrom

## What this is

The matrix of the backend registries Lajstrom was run against: what each one did, measured with
`curl`, `oras`, the integration tests and the conformance suite, not remembered. Every cell links
to the section that measured it: a page of its own for the GitLab container registry
([gitlab.md](gitlab.md)), GHCR ([ghcr.md](ghcr.md)) and Harbor ([harbor.md](harbor.md)), and the
[Notes](#notes) below for distribution and Zot, whose spikes (#17, #18) have no page yet. A cell
that was not measured says *not measured*. Every measurement here was made on 2026-11-07; the
[Versions tested](#versions-tested) table has the date of each registry, and
[Keeping the matrix current](#keeping-the-matrix-current) says when it changes. A registry that is
not here is not tested.

What the provider makes of a registry, `provider.Capabilities`, comes from its probe and from
a vendor table for what the probe cannot measure ([configuration.md](../configuration.md#registry-capabilities));
the last rows of [Capabilities](#capabilities) say what the probe found and what the
configuration has to add.

## Versions tested

| Registry or store | Image or installer | Pinned in | Last verified | Tier |
| --- | --- | --- | --- | --- |
| distribution 3.1.2 | `registry:3.1.2`, index `sha256:38907510915c3f1604c7e83d6ab602ab3020d092f73bdaed8696773693ff768c` | `compose.yaml` (`distribution`, `distribution-s3`, `distribution-s3-minio`), `ci.yml` | each PR with code; by hand 2026-11-07 | `CI` on the filesystem; `manual` on S3 (the `s3` and `s3-minio` profiles) |
| Zot v2.1.23 | `ghcr.io/project-zot/zot-minimal:v2.1.23`, index `sha256:59cb5740832970818903c5ce104425762664c7880835ee566506aad397cd3458` | `compose.yaml` (`zot`), `ci.yml` | each PR with code; by hand 2026-11-07 | `CI` |
| GitLab container registry v4.43.0, A (metadata on the filesystem) | `registry.gitlab.com/gitlab-org/build/cng/gitlab-container-registry:v4.43.0-gitlab`, index `sha256:fe9c67416a605ffaa5a053c17a6d3b1eab0668a33a58b7bd938a476088f8222e` (CNG rebuilds the tag in place: the digest is the pin) | `compose.yaml` (`gitlab`, profile `gitlab`) | 2026-11-07 | `manual` |
| GitLab container registry v4.43.0, B (metadata database, as GitLab.com) | the same, and `postgres:17.11-alpine`, index `sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac` | `compose.yaml` (`gitlab-db`, `gitlab-postgres`, profile `gitlab`) | 2026-11-07 | `manual` |
| GHCR | `ghcr.io`, no version published (`registry/2.0`) | nothing: a hosted service | 2026-11-07 | `hosted`: an account in the test organisation `lajstrom-extras` and a token |
| Harbor v2.15.4 | `harbor-online-installer-v2.15.4.tgz`, sha256 `eeb899cf7b626c925790adba87db9008dd1d73a693a72442651b5bdece9805c6` (images linux/amd64 only) | `harbor.sh` | 2026-11-07 | `manual` |
| MinIO 2025-07-23, behind distribution | `bitnamilegacy/minio:2025.7.23-debian-12-r5`, index `sha256:6dabb4a2088c9a79908de3bc05f4586c23ad2182c8908e7e3acbf61c1467fb20` | `compose.yaml` (`minio`, profile `s3-minio`) | 2026-11-07 | `manual` |
| SeaweedFS 4.52, behind distribution | `chrislusf/seaweedfs:4.52`, index `sha256:57877e32bb90073b37dc109b438ba66e84fd5ed9ced5f5c04f56444ef243ce59` | `compose.yaml` (`seaweedfs`, `seaweedfs-bucket`, profile `s3`) | 2026-11-07 | `manual` |

`compose.yaml` is [deploy/compose/compose.yaml](../../deploy/compose/compose.yaml), `harbor.sh`
[deploy/compose/harbor/harbor.sh](../../deploy/compose/harbor/harbor.sh), `ci.yml` the
`integration` job of [.github/workflows/ci.yml](../../.github/workflows/ci.yml). `CI`: the
integration job runs every test against it on every pull request that touches code. `manual`: a
profile of the dev stack or a script, run by hand. `hosted`: a service, an account and a token.

**Untested.** No cell of the matrix is about these; what is written for ECR was written from
memory before the spikes and is kept as it was, not measured:

| Registry | Issue | Tier |
| --- | --- | --- |
| Amazon ECR | spike #19 (needs an AWS account; not run), adapter #36 | `untested`. From memory: a repository is created on push only through a creation template with `CREATE_ON_PUSH`, otherwise by `CreateRepository` from the adapter; the OCI 1.1 referrers API since 2024-06, but pushing referrers with oras returned 405 (aws/containers-roadmap#2783); listing by `DescribeRepositories`, no `_catalog`; manifest delete yes; immutable tags a per-repository option that must be respected; blob `GET` a 307 to S3; an image count limit per repository and a 12-hour token |
| Nexus Repository OSS | spike #119 | `untested` |
| Gitea container registry | spike #120 | `untested` |
| Quay | none | `untested` |
| Azure Container Registry | none | `untested` |
| Docker Hub | none | `untested` |

## Capabilities

One row per capability or behaviour, one column per registry. GitLab A and B are the same
registry with its metadata on the filesystem and in its database. *not measured* cells of
distribution and Zot wait for their spikes, #17 and #18.

| | distribution 3.1.2 | Zot v2.1.23 | GitLab A | GitLab B | GHCR | Harbor v2.15.4 |
| --- | --- | --- | --- | --- | --- | --- |
| Create on push | [yes](#the-integration-test) | [yes](#the-integration-test) | [yes](gitlab.md#the-row) | [yes](gitlab.md#the-row) | [yes, with the first manifest](ghcr.md#create-on-push) | [yes, inside an existing project](harbor.md#projects-robots-and-create-on-push) |
| Where a repository may be created | [anywhere](#the-integration-test) | [anywhere](#the-integration-test) | [anywhere; behind GitLab, project plus two levels (read)](gitlab.md#behind-gitlab-read-not-measured) | [anywhere; behind GitLab, project plus two levels (read)](gitlab.md#behind-gitlab-read-not-measured) | [below `<org>/`; elsewhere not tried](ghcr.md#the-probe) | [below `<project>/`, at any depth](harbor.md#projects-robots-and-create-on-push) |
| Referrers API | [no: a plain 404, the tag schema](#catalog-and-referrers) | [yes](#catalog-and-referrers) | [no: a plain 404, the tag schema](gitlab.md#referrers) | [no: a plain 404, the tag schema](gitlab.md#referrers) | [no: 404 `MANIFEST_UNKNOWN`, the tag schema](ghcr.md#referrers) | [yes, with the `artifactType` filter](harbor.md#referrers) |
| `OCI-Subject` on a push with `subject` | *not measured* | *not measured* | [no](gitlab.md#referrers) | [no](gitlab.md#referrers) | [no](ghcr.md#referrers) | [yes](harbor.md#referrers) |
| Catalog (`_catalog`) and its limits | [yes, `n`/`last`; limits not measured](#catalog-and-referrers) | [yes; pagination not measured](#catalog-and-referrers) | [yes, `n`/`last`/`Link`, `n` at most 1000](gitlab.md#catalog) | [yes, `n`/`last`/`Link`, `n` at most 1000](gitlab.md#catalog) | [200 with all of GHCR, 1000 a page](ghcr.md#catalog) | [system admins with Basic only](harbor.md#catalog-and-listing) |
| A listing API of its own | *not measured* | *not measured* | [none: its `/gitlab/v1/` tag list is 404](gitlab.md#referrers) | [`/gitlab/v1/`: tag lists with referrers](gitlab.md#referrers) | [packages REST API, per organisation](ghcr.md#catalog) | [REST API per project, `page_size` at most 100](harbor.md#catalog-and-listing) |
| Tag list order and pagination | [listed; order not measured](#the-integration-test) | [listed; order not measured](#the-integration-test) | [listed; pagination not measured](gitlab.md#our-tests-against-it) | [listed; pagination not measured](gitlab.md#our-tests-against-it) | [**creation order; `last` a position**](ghcr.md#tags) | [`n`/`last`/`Link`, as distribution](harbor.md#catalog-and-listing) |
| Delete a manifest by digest | [yes; 405 with deletes disabled](#delete) | [yes](#delete) | [yes, 202](gitlab.md#delete) | [yes, 202](gitlab.md#delete) | [no: 405 `UNSUPPORTED`](ghcr.md#delete) | [yes; its referrers go with it](harbor.md#delete) |
| Delete by tag | *not measured* | *not measured* | [yes: the manifest stays, untagged](gitlab.md#delete) | [yes: the manifest stays, untagged](gitlab.md#delete) | [no: 405](ghcr.md#delete) | [400 through `/v2`; the REST API does](harbor.md#delete) |
| Delete a repository | *not measured* | *not measured* | *not measured* | *not measured* | [REST API: the package](ghcr.md#packages-left-in-lajstrom-extras) | [REST API](harbor.md#delete) |
| Immutable tags, where the rule lives | *not measured* | *not measured* | [none; GitLab's patterns need the database (read)](gitlab.md#immutable-tags) | [GitLab's tokens: 401 `IMMUTABLE_TAG`](gitlab.md#immutable-tags) | [none: a tag re-points](ghcr.md#immutable-tags) | [project rules: 412 `PRECONDITION`](harbor.md#immutable-tags) |
| Blob redirect | [none on the filesystem; 307 on S3](#object-stores-behind-distribution) | [none on the filesystem; S3 not tested](#blob-reads-and-ranges) | [none on the filesystem; object storage not tested](gitlab.md#blob-redirect-ranges-blob-size-headers) | [none on the filesystem; object storage not tested](gitlab.md#blob-redirect-ranges-blob-size-headers) | [307 to its CDN, 5 to 10 min](ghcr.md#blob-redirect) | [none on the filesystem; S3 not run](harbor.md#blob-redirect-ranges-sizes-headers) |
| `Range` and `Accept-Ranges` | [206; `Accept-Ranges: bytes`](#blob-reads-and-ranges) | [206; no `Accept-Ranges` on `GET`](#blob-reads-and-ranges) | [206; `Accept-Ranges: bytes`](gitlab.md#blob-redirect-ranges-blob-size-headers) | [206; `Accept-Ranges: bytes`](gitlab.md#blob-redirect-ranges-blob-size-headers) | [206 from the CDN; `accept-ranges: bytes`](ghcr.md#blob-redirect) | [206, 416 past the end; `Accept-Ranges: bytes`](harbor.md#blob-redirect-ranges-sizes-headers) |
| Largest blob measured | *not measured* | *not measured* | [2 GiB in one `PUT`; no limit set](gitlab.md#blob-redirect-ranges-blob-size-headers) | [2 GiB in one `PUT`; no limit set](gitlab.md#blob-redirect-ranges-blob-size-headers) | [100 MiB; 10 GB documented](ghcr.md#size) | [2 GiB in one `PUT`](harbor.md#blob-redirect-ranges-sizes-headers) |
| Manifest size limit | *not measured* | *not measured* | [none by default (configuration read, not pushed)](gitlab.md#blob-redirect-ranges-blob-size-headers) | [none by default (configuration read, not pushed)](gitlab.md#blob-redirect-ranges-blob-size-headers) | *not measured* | [4 MiB; one byte more is 413](harbor.md#blob-redirect-ranges-sizes-headers) |
| Lajstrom's manifest profile (`artifactType`, empty config, own media types) | [accepted](#the-integration-test) | [accepted](#the-integration-test) | [accepted](gitlab.md#media-types) | [**refused**: unknown `artifactType`, unless a flag](gitlab.md#media-types) | [accepted](ghcr.md#create-on-push) | [accepted](harbor.md#media-types) |
| Auth | [none; Basic (htpasswd)](#authentication) | [none; Basic (htpasswd)](#authentication) | [none](gitlab.md#version-tested) | [none; Bearer from a test token server](gitlab.md#bearer-tokens) | [Bearer from `/token`: the PAT in base64](ghcr.md#token) | [Bearer from Harbor's token service](harbor.md#projects-robots-and-create-on-push) |
| Rate limits seen | *not measured* | *not measured* | *not measured* | *not measured* | [none on `/v2`; REST API 5000 an hour](ghcr.md#rate-limits) | [none](harbor.md#blob-redirect-ranges-sizes-headers) |
| Upload: chunk out of order | [416 `RANGE_INVALID`](harbor.md#uploads) | [refused (the test)](#upload-sessions) | [refused (the test)](gitlab.md#our-tests-against-it) | [refused (the test)](gitlab.md#our-tests-against-it) | [416](ghcr.md#upload-sessions) | [**appended**, no 416](harbor.md#uploads) |
| Upload: chunk size limit | *not measured* | *not measured* | *not measured* | *not measured* | [4 MiB a `PATCH`, then 416](ghcr.md#upload-sessions) | *not measured* |
| Upload: session status (`GET`) | [works; 500 on S3 after a refused digest](#upload-sessions) | [works (the test)](#upload-sessions) | [works (the test)](gitlab.md#our-tests-against-it) | [works (the test)](gitlab.md#our-tests-against-it) | [**303 to a web page**](ghcr.md#upload-sessions) | [works (the test)](harbor.md#uploads) |
| Upload: cancel (`DELETE`) | [works; 500 on S3 after a refused digest](#upload-sessions) | [works (the test)](#upload-sessions) | [works (the test)](gitlab.md#our-tests-against-it) | [works (the test)](gitlab.md#our-tests-against-it) | [405 `UNSUPPORTED`](ghcr.md#upload-sessions) | [works (the test)](harbor.md#uploads) |
| Upload: wrong digest, empty blob | [a digest mismatch; stored (the test)](#upload-sessions) | [a digest mismatch; stored (the test)](#upload-sessions) | [a digest mismatch; stored (the test)](gitlab.md#our-tests-against-it) | [a digest mismatch; stored (the test)](gitlab.md#our-tests-against-it) | [404 `BLOB_UPLOAD_INVALID`; empty blob **refused**](ghcr.md#upload-sessions) | [a digest mismatch; stored (the test)](harbor.md#uploads) |
| Upload continued by another replica | [works (the test)](#upload-sessions) | [works (the test)](#upload-sessions) | [works (the test)](gitlab.md#our-tests-against-it) | [works; behind a token server fails: ours](gitlab.md#bearer-tokens) | [fails: ours, priming](ghcr.md#our-tests) | [works, with tokens](harbor.md#uploads) |
| Garbage collector and an untagged referrer (#37) | *not measured* | *not measured* | [kept; `--delete-untagged` deletes it](gitlab.md#garbage-collection) | [**deleted 24 h after its push**](gitlab.md#garbage-collection) | *not measured* | [kept; it goes with its subject](harbor.md#garbage-collection) |
| What the probe finds | [`referrers=false catalog=true`](#catalog-and-referrers) | [`referrers=true catalog=true`](#catalog-and-referrers) | [`referrers=false catalog=true`](gitlab.md#the-row) | [`referrers=false catalog=true`](gitlab.md#the-row) | [`catalog=true`, wrong; vendor table: `false`](ghcr.md#the-probe) | [`referrers=true catalog=false`](harbor.md#the-row) |
| Overrides needed | [none](#catalog-and-referrers) | [none](#catalog-and-referrers) | [none; behind GitLab with rules, `deleteManifest: false`](gitlab.md#the-row) | [the flag on the registry; behind GitLab, as A](gitlab.md#media-types) | [none since the vendor table](ghcr.md#the-probe) | [none; `prefix: <project>/`](harbor.md#the-row) |

## Object stores behind distribution

distribution 3.1.2 on its `s3` storage driver, the profiles `s3` and `s3-minio` of the
[dev stack](../dev-stack.md#the-s3-profile), side by side (#27, #118). The third column is for
the next store.

| | SeaweedFS 4.52 | MinIO 2025-07-23 | next store |
| --- | --- | --- | --- |
| Profile and ports | `s3`: the S3 gateway on `localhost:9000`, distribution on `localhost:5002` | `s3-minio`: MinIO on `localhost:9001`, distribution on `localhost:5006` | *not measured* |
| Image and platforms | 270 MB to pull, 1 GB on disk; linux/amd64 and arm64 | the Bitnami build (`minio/minio` left Docker Hub); size *not measured*; linux/amd64 and arm64 | *not measured* |
| Bucket creation | none by the image: the one-shot `seaweedfs-bucket` runs `weed shell` | the image, from `MINIO_DEFAULT_BUCKETS` | *not measured* |
| Start | about 15 s from an empty volume, 8 s after | *not measured* | *not measured* |
| Blob `GET` and `HEAD` | 307 to a presigned URL | 307 to a presigned URL | *not measured* |
| The presigned URL's host | the endpoint distribution talks to: `localhost:9000` | the endpoint distribution talks to: `localhost:9001` | *not measured* |
| Its lifetime | 20 minutes, `X-Amz-Expires=1200` (the s3 driver's default) | 20 minutes, `X-Amz-Expires=1200` | *not measured* |
| Expiry enforced | yes: past it 403 "Request has expired"; a tampered signature `SignatureDoesNotMatch` | *not measured* | *not measured* |
| `Range` through the redirect | 206 | 206 | *not measured* |
| `HEAD` | 307; the provider follows it, 200 (`StatBlob`) | 307; the provider follows it, 200 | *not measured* |
| Cancel of an upload session after a refused digest | 500 `s3aws: append to zero-size path … unsupported`; its status `GET` 500 too | the same 500s: distribution's s3 driver, not the store | *not measured* |
| Tests | `TestIntegrationS3Redirect`, `TestIntegration` with and without the prefix, `TestAptGetInstall/s3`: pass; `TestIntegrationUploads` but for the cancel | the same | *not measured* |

A presigned URL names the endpoint distribution signs for, and the signature covers the host:
the dev stack runs distribution in the store's network namespace so that `localhost:<port>`
works inside the registry's container and on the machine alike
([dev-stack.md](../dev-stack.md#the-s3-profile)). `TestIntegrationS3Redirect` (`internal/facade`,
`LAJSTROM_TEST_REGISTRY_S3`) checks the 307 of the facade and of `/raw/`, that the bytes behind
the `Location` have the blob's digest, a range after the redirect, `HEAD`, and that
`policy.blobRedirect: never` streams. CI does not run these profiles.

## Deviations from the specification

Where a registry's protocol does not do what distribution-spec v1.1.1 or image-spec v1.1.1 says,
measured: the list from which we report upstream. Each row was checked against the spec's text;
what the spec allows is in the second table, not here. "To file" means nobody from Lajstrom has
reported it yet.

| Registry, version | What it does (measured) | What the spec says | What it costs Lajstrom | Our handling today | Upstream |
| --- | --- | --- | --- | --- | --- |
| GitLab container registry v4.43.0, B (metadata database) | Refuses a manifest whose `artifactType` or `config.mediaType` is not in its table of known types: 400 `MANIFEST_INVALID` "unknown media type", every Lajstrom manifest among them ([gitlab.md](gitlab.md#media-types)) | image-spec 1.1.1, manifest, `artifactType`: "Implementations storing or copying image manifests MUST NOT error on encountering an `artifactType` that is unknown to the implementation"; the same for `config.mediaType` | Nothing Lajstrom writes can be stored; GitLab.com runs this variant | `REGISTRY_FF_DYNAMIC_MEDIA_TYPES=true` on the registry; since #35 `serve` warns at startup when `GET /v2/` says `Gitlab-Container-Registry-Database-Enabled: true`; the profile is not changed (a contract question) | [gitlab-org/container-registry](https://gitlab.com/gitlab-org/container-registry/-/issues): to file (accept unknown types, or seed `application/vnd.lajstrom.*`) |
| GitLab container registry v4.43.0, B, with GitLab's rules in the token (a test token server played GitLab) | A push to an immutable tag is 401 `IMMUTABLE_TAG`; every digest delete in a repository a pattern applies to is 401 `PROTECTED_MANIFEST` ([gitlab.md](gitlab.md#immutable-tags)) | Error codes: "The `code` field MUST be one of the following", and neither code is in the list; Deleting Manifests: "If manifest deletion is disabled, the registry MUST respond with either a `400 Bad Request` or a `405 Method Not Allowed`" | A policy refusal reads as bad credentials: `ErrUnauthorized`, 502 at `/raw/`; `deleteManifest` stays true | `IMMUTABLE_TAG` becomes `ErrConflict` by its message; `capabilities: {deleteManifest: false}` behind a GitLab with rules; reading `PROTECTED_MANIFEST` as `ErrUnsupported` is item 1 of gitlab.md's list, no issue yet | [gitlab-org/container-registry](https://gitlab.com/gitlab-org/container-registry/-/issues): to file |
| GHCR, 2026-11-07 | In a repository without a manifest, a blob answers `HEAD` 200 and `GET` 404 `BLOB_UNKNOWN`, seconds later too ([ghcr.md](ghcr.md#create-on-push)) | Checking if content exists: "A HEAD request to an existing blob or manifest URL MUST return `200 OK`"; Pulling blobs: "A GET request to an existing blob URL MUST provide the expected blob, with a response code that MUST be `200 OK`" | Ingest reads the metadata layer back before it pushes the manifest, so the first version of every package fails; raw `PUT` answers 400 | none; ingest should check the layer from its spool (ghcr.md), no issue yet | GitHub has no public tracker: [Community, Packages](https://github.com/orgs/community/discussions/categories/packages): to file |
| GHCR, 2026-11-07 | The empty blob `sha256:e3b0c442…` cannot be pushed (404 `BLOB_UNKNOWN` to `PUT` and `POST ?digest=`), yet `HEAD` and `GET` answer 200 with 0 bytes, in a repository that never existed too ([ghcr.md](ghcr.md#upload-sessions)) | Checking if content exists: "If the blob or manifest is not found in the repository, the response code MUST be `404 Not Found`"; Pushing Manifests: "An attempt to pull a nonexistent repository MUST return response code `404 Not Found`" | A client's push of the empty blob through the facade fails, though a manifest with it as a layer is taken | none; never upload the 0-byte blob to GHCR (ghcr.md), no issue yet | [Community, Packages](https://github.com/orgs/community/discussions/categories/packages): to file |
| GHCR, 2026-11-07 | `GET` of an upload session is `303 See Other` to a github.com web page that answers 404, with no challenge; `HEAD` is 405 ([ghcr.md](ghcr.md#upload-sessions)) | `GET` of the session (end-13): "The response to an active upload `<location>` MUST be a `204 No Content` response code", with `Location` and `Range` | No offset from the registry; a replica that primes its token with this `GET` gets none, and its streamed `PATCH` fails ("request body is not rewindable") | none; prime with a request GHCR challenges and keep the offset in Lajstrom's session (ghcr.md), no issue yet | [Community, Packages](https://github.com/orgs/community/discussions/categories/packages): to file |
| GHCR, 2026-11-07 | `tags/list` is in creation order; `last` is a position, and a `last` that is no tag gives `"tags":null` ([ghcr.md](ghcr.md#tags)) | Listing Tags: "the tags MUST be in lexical order"; with `last`, "up to `<int>` tags *after* `<tagname>`"; the body's `tags` is a list | None today: the provider follows `Link` and never makes up a `last` | oras-go follows `Link` | [Community, Packages](https://github.com/orgs/community/discussions/categories/packages): to file |
| Harbor v2.15.4 | Appends a chunk at the end of the session whatever its `Content-Range` says: 202 and a longer `Range` ([harbor.md](harbor.md#uploads)) | Pushing a blob in chunks: "If a chunk is uploaded out of order, the registry MUST respond with a `416 Requested Range Not Satisfiable` code" | A client resuming at a wrong offset is caught only by the digest of the closing `PUT`; `TestIntegrationUploads/chunk out of order` fails on Harbor | nothing needed: the closing digest refuses the blob | [goharbor/harbor](https://github.com/goharbor/harbor/issues): to file. Harbor's registry is distribution v2.8.3, whose `PATCH` ignores `Content-Range` (a TODO in `registry/handlers/blobupload.go`); distribution 3 answers 416 `RANGE_INVALID` (measured on 3.1.2), so Harbor on distribution 3 (goharbor/harbor#20815, open) fixes it |
| Harbor v2.15.4; distribution v3.1.2 has the same code (read in `registry/handlers/manifests.go`, not measured) | A manifest `GET` or `HEAD` without an OCI type in `Accept` (none, `*/*`) is 404 `MANIFEST_UNKNOWN` "OCI manifest found, but Accept header does not support OCI manifests" ([harbor.md](harbor.md#blob-redirect-ranges-sizes-headers)) | Pulling manifests: "The client SHOULD include an `Accept` header"; "A GET request to an existing manifest URL MUST provide the expected manifest, with a response code that MUST be `200 OK`"; 404 is for a manifest "not found in the repository" | None through Lajstrom: oras-go and the provider send OCI types; a client of the facade without one gets the 404 | nothing; the facade passes the client's `Accept` | [distribution/distribution](https://github.com/distribution/distribution/issues): to file (the code is distribution's; #4385 asked about it and was closed as a Docker Hub question); goharbor/harbor through it |
| Harbor v2.15.4 | Error codes outside the spec's list on `/v2`: `BAD_REQUEST` (a repository outside a project, a malformed digest), `NOT_FOUND` (tags of a missing repository, a deleted manifest), `PRECONDITION` (412, an immutable tag), `REQUEST_ENTITY_TOO_LARGE` (413) ([harbor.md](harbor.md)) | Error codes: "The `code` field MUST be one of the following" (`NAME_INVALID`, `NAME_UNKNOWN`, `MANIFEST_UNKNOWN`, `DENIED`, …) | The provider reads the status, so 404 is `ErrNotFound`; a 412 is `ErrConflict` only because Harbor's message says "configured as immutable"; the referrers probe's 400 for a name outside a project says nothing about the cause | `classifyResponse` by status, and by the message for 412 (`mentionsImmutable`); harbor.md items 3 and 4; the Harbor adapter, #73 | [goharbor/harbor](https://github.com/goharbor/harbor/issues): to file |
| distribution 3.1.2, `s3` storage driver (on SeaweedFS 4.52 and MinIO 2025-07-23) | After the closing `PUT` of a session is refused for its digest, the status `GET` and the cancel of that session answer 500 `s3aws: append to zero-size path … unsupported` ([Notes](#upload-sessions)) | `GET` of the session (end-13): 204 for an active session, 404 otherwise; the cancel is not in 1.1.1 (Docker's API: 204, or 404 for an unknown session). A 500 is neither: a bug | The session stays in the bucket until distribution purges old uploads; a log line | the facade cancels the backend session itself after the refused digest, logs the 500 (`facade: cancel backend upload session`), and answers the client's `DELETE` 404 `BLOB_UPLOAD_UNKNOWN` (#24) | [distribution/distribution](https://github.com/distribution/distribution/issues): to file |

**Not deviations**: measured, and allowed by the spec or outside it. Some are still worth a
word upstream, and the row says so; where one costs Lajstrom something, the row says that too.

| Registry | What it does | Why it is not a deviation |
| --- | --- | --- |
| distribution 3.1.2, GitLab v4.43.0, GHCR | No referrers API (a 404); GitLab and GHCR send no `OCI-Subject` | Optional: "If the referrers API returns a 404, the client MUST fallback to pulling the referrers tag schema" (Listing Referrers, Backwards Compatibility). `OCI-Subject` is for "a registry implementation that supports the referrers API" |
| GHCR, Harbor, GitLab | GHCR's `_catalog` is 200 with every repository of GHCR as the token sees it, other users' among them; Harbor's is for system administrators with Basic auth; GitLab's refuses `n` above 1000 | `_catalog` is not in distribution-spec 1.1.1 at all (it is Docker's registry API). A catalog of what the token may push is still worth asking GitHub for |
| GHCR; distribution with deletes disabled; Harbor | Every delete through `/v2` is 405 `UNSUPPORTED` (GHCR); a manifest delete 405 (distribution); a tag delete 400 (Harbor) | Content Management: "Registries MAY implement deletion or they MAY disable it"; disabled is "either a `400 Bad Request` or a `405 Method Not Allowed`" |
| GHCR | The cancel of an upload session is 405 | The cancel is not in distribution-spec 1.1.1 |
| GHCR | A `PATCH` above 4 MiB is 416 "the request body is too large" | The spec sets no largest chunk (only `OCI-Chunk-Min-Length`, a smallest); 416 is the status it gives an out-of-order chunk, and no header announces the limit: worth a word upstream |
| GHCR | A `PATCH` without `Content-Range` starts the session over | 1.1.1 defines a chunk `PATCH` with `Content-Range` only |
| GHCR | A wrong digest on the closing `PUT` is 404 `BLOB_UPLOAD_INVALID` | The error table has `DIGEST_INVALID`, "provided digest did not match uploaded content", for exactly this, but the spec binds no code and no status to the condition; worth asking. The provider reads it as `ErrNotFound`, not `content.ErrMismatchedDigest` |
| GHCR | The bearer token is the PAT in base64: no expiry, not narrowed by the scope | Authentication is "out of the purview of this specification" (Determining Support). It means the token cache and any log line with a bearer carry the PAT |
| Zot v2.1.23 | No `Accept-Ranges` on a blob `GET` (its `HEAD` has one); a range is 206 | "A registry SHOULD support the `Range` request header in accordance with RFC 9110", which Zot does; in RFC 9110 (§14.3) a server that supports ranges "can send" `Accept-Ranges`, and "a client MAY generate range requests regardless" |
| Harbor v2.15.4 | `POST …/blobs/uploads/?digest=` with the bytes opens a session (202) | Single POST: "Registries that do not support single request monolithic uploads SHOULD return a `202 Accepted`" |
| Harbor v2.15.4 | Over a project's quota, 403 `DENIED` on the blob and manifest `PUT` | Quotas are outside the spec, and `DENIED` is in its table. The provider reads it as bad credentials (502); a sentinel of its own is a contract question |
| Harbor v2.15.4 | The referrers API ignores `n`, and `X-Total-Count` counts before the filter | The referrers API has no `n` and no count |
| GitLab v4.43.0 | `GET /v2/` advertises `Gitlab-Container-Registry-Features: tag_delete`, but `DELETE /v2/<name>/tags/reference/<tag>` is a plain 404 | A GitLab extension, not the spec; the OCI tag delete on `/manifests/<tag>` works. Worth a word to GitLab: the header and its docs promise a route v4.43.0 does not have |
| GitLab v4.43.0, B | The online garbage collector deletes an untagged manifest, a referrer with `subject` among them, 24 h after its push | The spec says nothing about retention. Lajstrom writes the tag schema on GitLab (no referrers API), which survives (#37) |

## Configuration

One `registries[]` entry per registry, from the pages; the keys are in
[configuration.md](../configuration.md#registries). `prefix` is optional unless the registry
needs one.

distribution and Zot, on the filesystem or on S3: the probe finds everything.

```yaml
registries:
  - name: local
    type: generic
    url: http://localhost:5000          # Zot of the dev stack; distribution is :5001, distribution on S3 :5002
    auth:                               # for htpasswd; leave auth out for an anonymous registry
      username: lajstrom
      passwordEnv: LAJSTROM_REGISTRY_PASSWORD
    # capabilities: {blobRedirect: false}   # on S3, when clients cannot reach the object store: Lajstrom streams
```

The GitLab container registry, both variants:

```yaml
registries:
  - name: gitlab
    type: generic
    url: http://localhost:5004          # B; A is :5003
    # B, with its metadata database (as GitLab.com), refuses every Lajstrom manifest unless the
    # registry runs with REGISTRY_FF_DYNAMIC_MEDIA_TYPES=true (LAJSTROM_GITLAB_DYNAMIC_MEDIA_TYPES=true
    # in the dev stack); serve warns at startup when GET /v2/ says the database is on.
    # Behind GitLab (read, not measured): one GitLab project per Lajstrom repo,
    # <group>/<format>/<repo>, and with tag protection or immutability rules no digest deletes:
    # prefix: <group>/
    # capabilities: {deleteManifest: false}
```

GHCR ([ghcr.md](ghcr.md#the-probe)):

```yaml
registries:
  - name: ghcr
    type: generic
    url: https://ghcr.io                # https only: port 80 answers 301
    prefix: <org>/
    auth:
      username: <user>
      passwordEnv: LAJSTROM_GHCR_TOKEN  # a classic PAT with write:packages
    capabilities:
      catalog: false                    # the catalog is all of GHCR; the vendor table sets it since #35
      deleteManifest: false             # /v2 deletes are 405 (the REST API: #74); the vendor table too
      referrers: false                  # what the probe finds; saves the request
      maxBlobSize: 10000000000          # documented: 10 GB per layer; measured up to 100 MiB
```

Harbor ([harbor.md](harbor.md#run-it)):

```yaml
registries:
  - name: harbor
    type: generic
    url: http://localhost:5005
    prefix: <project>/                  # required: repositories exist only inside a project
    auth:
      username: 'robot$<project>+<name>'  # a project robot; its permissions are in harbor.md
      passwordEnv: LAJSTROM_HARBOR_ROBOT_SECRET
```

## Keeping the matrix current

The versions are pinned in three places: `deploy/compose/compose.yaml` (distribution, Zot, the
GitLab container registry and its Postgres, SeaweedFS, MinIO), `deploy/compose/harbor/harbor.sh`
(Harbor's version and the installer's sha256) and `.github/workflows/ci.yml` (distribution and
Zot of the integration job, the same as in `compose.yaml`). **Whoever changes a pin re-runs the
commands of that registry's page (or of [Commands](#commands) for distribution and Zot),
updates the cells that changed and the "last verified" date, and says so in the PR.** GHCR has
no pin: it is re-measured when something on its page stops being true, or quarterly. A registry
that joins gets a page like [gitlab.md](gitlab.md), a row in [Versions tested](#versions-tested),
a column in [Capabilities](#capabilities), and its deviations.

The commands per registry:

- [ ] **distribution and Zot** (`compose.yaml` and `ci.yml`, together): the integration job runs
      on the pull request; by hand, [Commands](#commands): `TestIntegration` with and without
      the prefix, with htpasswd, distribution with deletes disabled.
- [ ] **distribution on S3, SeaweedFS, MinIO** (`compose.yaml`, profiles `s3` and `s3-minio`):
      `make dev-up PROFILES=s3,s3-minio`, then the commands of
      [dev-stack.md](../dev-stack.md#the-s3-profile) against both stores:
      `TestIntegrationS3Redirect`, `TestIntegration`, `TestIntegrationUploads`, `TestAptGetInstall`.
- [ ] **GitLab container registry** (`compose.yaml`, profile `gitlab`; a new CNG build of the same
      tag is a new digest): [gitlab.md, Commands](gitlab.md#commands): the curl probes, the tests
      on A and B with and without dynamic media types and the prefix, the media-type table, the
      garbage collection.
- [ ] **Harbor** (`harbor.sh`): [harbor.md, Commands](harbor.md#commands): the project and the
      robot, the curl probes, the tests with the prefix, without it and with the immutability
      rule, the quota, the garbage collection, the conformance suite.
- [ ] **GHCR** (no pin; quarterly, or when the page stops being true):
      [ghcr.md, Measurements](ghcr.md#measurements) and [Our tests](ghcr.md#our-tests) in
      `lajstrom-extras`, then the deletion of the packages it leaves.
- [ ] **Every registry**: the `registry capabilities` line of the test log or of `serve`
      (`vendor=… createOnPush=… referrers=… catalog=…`) against the rows "What the probe finds"
      and "Overrides needed", and each row of [Deviations](#deviations-from-the-specification)
      against the new version.

## Commands

distribution and Zot ran in containers (OrbStack on macOS, arm64):

```sh
docker run -d --name lajstrom-dist9 -p 55000:5000 -e REGISTRY_STORAGE_DELETE_ENABLED=true registry:3
docker run -d --name lajstrom-zot9 -p 55001:5000 ghcr.io/project-zot/zot-minimal-linux-arm64:v2.1.23

# Basic auth: an htpasswd file with bcrypt, mounted read-only.
htpasswd -Bbn lajstrom "$PASSWORD" > auth/htpasswd
docker run -d --name lajstrom-dist9-auth -p 55002:5000 -v "$PWD/auth:/auth:ro" \
  -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=lajstrom \
  -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd -e REGISTRY_STORAGE_DELETE_ENABLED=false registry:3
# auth/zot-config.json: {"distSpecVersion": "1.1.1", "storage": {"rootDirectory": "/var/lib/registry"},
#   "http": {"address": "0.0.0.0", "port": "5000", "auth": {"htpasswd": {"path": "/auth/htpasswd"}}}}
docker run -d --name lajstrom-zot9-auth -p 55003:5000 -v "$PWD/auth/htpasswd:/auth/htpasswd:ro" \
  -v "$PWD/auth/zot-config.json:/etc/zot/config.json:ro" ghcr.io/project-zot/zot-minimal-linux-arm64:v2.1.23
```

The s3 profile: `make dev-up PROFILES=s3` (`deploy/compose/compose.yaml`), then
`LAJSTROM_TEST_REGISTRY=localhost:5002` as below and
`LAJSTROM_TEST_REGISTRY_S3=localhost:5002 go test -race -count=1 -tags integration -run TestIntegrationS3Redirect -v ./internal/facade/`;
`PROFILES=s3-minio` and `localhost:5006` for the fallback on MinIO.

The test, once per registry:

```sh
LAJSTROM_TEST_REGISTRY=localhost:55000 go test -race -count=1 -tags integration -run 'TestIntegration$' -v ./internal/provider/generic/
LAJSTROM_TEST_REGISTRY=localhost:55000 LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/ go test -race -count=1 -tags integration -run 'TestIntegration$' -v ./internal/provider/generic/
LAJSTROM_TEST_REGISTRY=localhost:55002 LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom LAJSTROM_TEST_REGISTRY_PASSWORD="$PASSWORD" \
  go test -race -count=1 -tags integration -run TestIntegration -v ./internal/provider/generic/
```

`LAJSTROM_TEST_REGISTRY_TLS=1` switches to HTTPS. The test does not clean up what the backend
cannot delete; remove the containers afterwards:

```sh
docker rm -f lajstrom-dist9 lajstrom-zot9 lajstrom-dist9-auth lajstrom-zot9-auth
```

## Notes

What was measured on distribution and Zot, which have no page yet (#17, #18), and what holds
for every registry.

### The integration test

`TestIntegration` (`internal/provider/generic/integration_test.go`) pushes a blob, pushes a
manifest with a tag in one `PUT` (`artifactType` `application/vnd.lajstrom.test.v1`, the empty
config, a layer), resolves, fetches, checks existence, lists tags, attaches a referrer (a
manifest with `subject`) and lists it, `OpenBlob` (the first answer of one `GET`: a redirect to
follow, or the bytes; with redirect off, the bytes; a range, 206; a range past the end, 416),
`StatBlob`, lists repositories under a random prefix, and deletes the referrer and the manifest.
Each run works in `raw/it-<random>/…`, once without and once with the instance prefix
`lajstrom-it/`. The CI job runs it with the rest of the integration suite (ingest, the raw and apt
frontends, the facade, `TestIntegrationUploads`) on Zot and on distribution, so Lajstrom's
manifest profile is pushed to both on every pull request. The runs by hand on 2026-11-07:

| Registry | Configuration | Auth | Result |
| --- | --- | --- | --- |
| distribution 3.1.2 | `registry:3`, filesystem storage, deletes enabled | none | pass, with and without the prefix |
| distribution 3.1.2 | filesystem, `REGISTRY_STORAGE_DELETE_ENABLED=false`, htpasswd | Basic | pass; the delete step skipped ([Delete](#delete)) |
| distribution 3.1.2 | the `s3` driver on SeaweedFS (profile `s3`) and on MinIO (profile `s3-minio`), deletes enabled | none | pass, with and without the prefix |
| Zot v2.1.23 | `zot-minimal-linux-arm64:v2.1.23` (`sha256:d8ec72e9…0571f3`), its default configuration | none | pass, with and without the prefix |
| Zot v2.1.23 | the same, htpasswd | Basic | pass, with the prefix |
| Zot v2.1.23 | the dev stack: the `zot-minimal` index (arm64 as above), `dedupe`, `gc` every 10 minutes ([deploy/compose/zot/config.json](../../deploy/compose/zot/config.json)) | none | pass, with and without the prefix |

The [dev stack](../dev-stack.md) runs distribution as in the first row (plus
`REGISTRY_LOG_LEVEL=info`) on `localhost:5001` and Zot as in the last row on `localhost:5000`;
both passed there on 2026-11-07. The `registry:3` image enables deletes in its default
configuration (`/etc/distribution/config.yml`), unlike distribution's own default. Zot takes OCI
media types only, not Docker's ([dev-stack.md](../dev-stack.md#what-runs)).

### Catalog and referrers

Both answer `_catalog` with 200 and `n`/`last` pagination; neither was asked for more than a
page, and the 150 repositories of #17 were not tried. distribution 3.1.2 has no referrers API:
the routes do not exist, so the probe and oras-go get a plain-text 404 and use the
`sha256-<digest>` tag schema; Lajstrom lists referrers either way. Zot answers the probe with an
empty OCI image index for a repository that does not exist: the referrers API. The probe logs
`referrers=false catalog=true` for distribution and `referrers=true catalog=true` for Zot, and
`createOnPush=true deleteManifest=true immutableTags=false blobRedirect=true` for both; no
override is needed.

### Delete

Both delete a manifest by digest (the test's delete step). distribution with
`REGISTRY_STORAGE_DELETE_ENABLED=false` answered the first manifest delete with 405
`UNSUPPORTED`: `DeleteManifest` turned false and the step was skipped.

### Blob reads and ranges

On filesystem storage neither registry redirects a blob download: the first answer is the
bytes, streamed as they come, so a download through `format.ServeBlob` is one request to the
registry. distribution on S3 redirects `GET` and `HEAD` to the object store
([Object stores](#object-stores-behind-distribution)); Zot on S3 was not tested. One range is 206
with `Content-Range`; several are 206 `multipart/byteranges`, which `format.ServeBlob` does not
ask for (it sends one range, or none); a range past the end is 416 `bytes */<size>`, and a
malformed one is 416 too. distribution sends `Accept-Ranges: bytes`; Zot sends it on a `HEAD`
and not on a `GET`.

### Upload sessions

`TestIntegrationUploads` (#24) passes on both on the filesystem, in CI on every pull request: a
session continued by a second provider and its status, a chunk of unknown size, the monolithic
`PUT`, the empty blob, a wrong digest refused as a digest mismatch, a chunk out of order refused
(distribution: 416 `RANGE_INVALID`, [harbor.md](harbor.md#uploads)), the cancel and the 404 of
the session after it, and the mount. No chunk size limit was looked for. On
distribution on S3 it passes but for one step: the cancel of a session after a refused digest
answers 500 (`s3aws: append to zero-size path … unsupported`), and so does the status `GET` of
that session; the steps after it pass. SeaweedFS and MinIO, side by side (#118), give the same
answer and message: the cause is distribution's s3 driver, not the store. Through Lajstrom the
facade cancels the backend session itself after the refused digest, logs the 500
(`facade: cancel backend upload session`), and the client's `DELETE` is 404
`BLOB_UPLOAD_UNKNOWN`.

### Authentication

Without credentials, both registries with htpasswd answer the probe with 401 and a Basic
challenge; `Capabilities` then fails with `provider.ErrUnauthorized` (no credentials
configured), which `serve` logs at startup. Bearer tokens are covered by unit tests against a
fake token endpoint, and against real token services by GitLab B with a test token server
([gitlab.md](gitlab.md#bearer-tokens)) and by Harbor ([harbor.md](harbor.md#projects-robots-and-create-on-push)).

### The other registries

Harbor runs from its installer as a compose project of its own on `localhost:5005`, beside the
dev stack and not part of it (`deploy/compose/harbor/harbor.sh up` and `down`); the GitLab
container registry is the `gitlab` profile of the dev stack on `localhost:5003` (A) and
`localhost:5004` (B). The packages the GHCR tests left in `lajstrom-extras`, and their deletion,
are in [ghcr.md](ghcr.md#packages-left-in-lajstrom-extras).
