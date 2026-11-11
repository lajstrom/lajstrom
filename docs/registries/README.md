# Registries tested with Lajstrom

## What this is

The matrix of the backend registries Lajstrom was run against: what each one did, measured with
`curl`, `oras`, the integration tests, the conformance suite and, since #339,
[`lajstrom registry probe`](#measuring-a-registry-lajstrom-registry-probe), not remembered.
Every cell links to the section that measured it, on the registry's page: distribution
([distribution.md](distribution.md)), Zot ([zot.md](zot.md)), the GitLab container registry
([gitlab.md](gitlab.md)), GHCR ([ghcr.md](ghcr.md)), Harbor ([harbor.md](harbor.md)), Nexus
Repository ([nexus.md](nexus.md)) and Gitea's container registry, with Forgejo's
([gitea.md](gitea.md)). A cell
that was not measured says *not measured*. distribution's and Zot's measurements were made on
2026-11-09, Nexus's and Gitea's on 2026-11-10, the others' on 2026-11-07; the [Versions tested](#versions-tested) table has the date
of each registry, and [Keeping the matrix current](#keeping-the-matrix-current) says when it
changes. A registry that is not here is not tested.

How registries differ beyond these cells, the untested ones too (ECR, ACR, Google Artifact
Registry, Docker Hub, Quay): their repository models and names, whether `a/b` and `a/b/c` can
both be repositories, uploads, deletes, retention rules that read tags, and what each means for
Lajstrom, from the vendors' documentation and a few measurements: [quirks.md](quirks.md).

What the provider makes of a registry, `provider.Capabilities`, comes from its probe and from
a vendor table for what the probe cannot measure ([configuration.md](../configuration.md#registry-capabilities));
the last rows of [Capabilities](#capabilities) say what the probe found and what the
configuration has to add.

## Versions tested

| Registry or store | Image or installer | Pinned in | Last verified | Tier |
| --- | --- | --- | --- | --- |
| distribution 3.1.2 | `registry:3.1.2`, index `sha256:38907510915c3f1604c7e83d6ab602ab3020d092f73bdaed8696773693ff768c` | `compose.yaml` (`distribution`, `distribution-s3`, `distribution-s3-minio`), `ci.yml` | each PR with code; by hand 2026-11-09; the probe 2026-11-10 | `CI` on the filesystem; `manual` on S3 (the `s3` and `s3-minio` profiles) |
| Zot v2.1.23 | `ghcr.io/project-zot/zot-minimal:v2.1.23`, index `sha256:59cb5740832970818903c5ce104425762664c7880835ee566506aad397cd3458` | `compose.yaml` (`zot`), `ci.yml` | each PR with code; by hand 2026-11-09; the probe 2026-11-10 | `CI` |
| GitLab container registry v4.43.0, A (metadata on the filesystem) | `registry.gitlab.com/gitlab-org/build/cng/gitlab-container-registry:v4.43.0-gitlab`, index `sha256:fe9c67416a605ffaa5a053c17a6d3b1eab0668a33a58b7bd938a476088f8222e` (CNG rebuilds the tag in place: the digest is the pin) | `compose.yaml` (`gitlab`, profile `gitlab`) | 2026-11-07; the probe 2026-11-10 | `manual` |
| GitLab container registry v4.43.0, B (metadata database, as GitLab.com) | the same, and `postgres:17.11-alpine`, index `sha256:3540c0362c809df084ffac0ae69fe794515f9de92692f02178bcdce0ef0302ac` | `compose.yaml` (`gitlab-db`, `gitlab-postgres`, profile `gitlab`) | 2026-11-07; the probe 2026-11-10 | `manual` |
| GHCR | `ghcr.io`, no version published (`registry/2.0`) | nothing: a hosted service | 2026-11-07 | `hosted`: an account in the test organisation `lajstrom-extras` and a token |
| JFrog Container Registry 7.161.26 | `releases-docker.jfrog.io/jfrog/artifactory-jcr:7.161.26`, index `sha256:fd8d64416384c2ede4478c89bfa85ba7e9193db4db0dc36db3a307a0b9a5128b`, and `postgres:17.11-alpine` (it needs 4 GiB of memory) | [`jfrog-jcr/compose.yaml`](../../deploy/compose/jfrog-jcr/compose.yaml), set up by `jcr.sh` | 2026-11-10 | `manual` |
| Harbor v2.15.4 | `harbor-online-installer-v2.15.4.tgz`, sha256 `eeb899cf7b626c925790adba87db9008dd1d73a693a72442651b5bdece9805c6` (images linux/amd64 only) | `harbor.sh` | 2026-11-07 | `manual` |
| Nexus Repository 3.98.1, Community Edition, a `docker` and an `oci` hosted repository | `sonatype/nexus3:3.98.1`, index `sha256:91a9af44af777c353db11a5ee73850c7eb24b03c997ff4cc62a3500ad38a1332` | `nexus/compose.yaml`, set up by `nexus.sh` | 2026-11-10 | `manual` |
| Gitea 1.27.3, its container registry | `gitea/gitea:1.27.3-rootless`, index `sha256:1c17ecaead42eb3b5391553d8708103a4beb0e86edf5b9ebc1eb269c318845f2` | `gitea/compose.yaml`, set up by `gitea.sh` | 2026-11-10, by hand and by the probe | `manual` |
| Forgejo 17.0.1, its container registry | `codeberg.org/forgejo/forgejo:17.0.1-rootless`, index `sha256:5ef02fbcf22beef56b8c844952ffad58cfe177b6f98e46e6abb5df90b88dc8d3` | `gitea/compose.yaml` (profile `forgejo`), set up by `gitea.sh up forgejo` | 2026-11-10 | `manual`; where it differs from Gitea, on gitea.md |
| MinIO 2025-07-23, behind distribution | `bitnamilegacy/minio:2025.7.23-debian-12-r5`, index `sha256:6dabb4a2088c9a79908de3bc05f4586c23ad2182c8908e7e3acbf61c1467fb20` | `compose.yaml` (`minio`, profile `s3-minio`) | 2026-11-09 | `manual` |
| SeaweedFS 4.52, behind distribution | `chrislusf/seaweedfs:4.52`, index `sha256:57877e32bb90073b37dc109b438ba66e84fd5ed9ced5f5c04f56444ef243ce59` | `compose.yaml` (`seaweedfs`, `seaweedfs-bucket`, profile `s3`) | 2026-11-09 | `manual` |

`compose.yaml` is [deploy/compose/compose.yaml](../../deploy/compose/compose.yaml), `harbor.sh`
[deploy/compose/harbor/harbor.sh](../../deploy/compose/harbor/harbor.sh), `nexus/compose.yaml`
and `nexus.sh` [deploy/compose/nexus/](../../deploy/compose/nexus/), `gitea/compose.yaml`
and `gitea.sh` [deploy/compose/gitea/](../../deploy/compose/gitea/), `ci.yml` the
`integration` job of [.github/workflows/ci.yml](../../.github/workflows/ci.yml) (and its `e2e`
job for Zot). `CI`: the
integration job runs every test against it on every pull request that touches code. `manual`: a
profile of the dev stack or a script, run by hand. `hosted`: a service, an account and a token.

**Untested.** No cell of the matrix is about these; what is written for ECR was written from
memory before the spikes and is kept as it was, not measured:

| Registry | Issue | Tier |
| --- | --- | --- |
| Amazon ECR | spike #19 (needs an AWS account; not run), adapter #36 ([ecr.md](ecr.md): tested against moto, the AWS mock, with distribution for `/v2`; live ECR pending #19) | `untested`. From memory: a repository is created on push only through a creation template with `CREATE_ON_PUSH`, otherwise by `CreateRepository` from the adapter; the OCI 1.1 referrers API since 2024-06, but pushing referrers with oras returned 405 (aws/containers-roadmap#2783); listing by `DescribeRepositories`, no `_catalog`; manifest delete yes; immutable tags a per-repository option that must be respected; blob `GET` a 307 to S3; an image count limit per repository and a 12-hour token |
| Quay | none | `untested` |
| Azure Container Registry | none | `untested` |
| Docker Hub | none | `untested` |

## Capabilities

One row per capability or behaviour, one column per registry. distribution's cells are its
filesystem storage, and S3 where that differs; its page has both columns. GitLab A and B are the
same registry with its metadata on the filesystem and in its database. Nexus's two columns are
one Nexus with a hosted repository of each of its two registry formats, `docker` and `oci`.
Gitea's column holds for Forgejo too, but where a cell names Forgejo.

| | distribution 3.1.2 | Zot v2.1.23 | GitLab A | GitLab B | GHCR | Harbor v2.15.4 | Nexus 3.98.1, docker | Nexus 3.98.1, OCI | Gitea 1.27.3 (Forgejo 17.0.1) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Create on push | [yes, with the first manifest](distribution.md#create-on-push) | [yes: the first upload session creates the repository](zot.md#create-on-push) | [yes](gitlab.md#the-row) | [yes](gitlab.md#the-row) | [yes, with the first manifest](ghcr.md#create-on-push) | [yes, inside an existing project](harbor.md#projects-robots-and-create-on-push) | [yes, with the first manifest](nexus.md#connectors-and-create-on-push) | [yes, with the first manifest](nexus.md#connectors-and-create-on-push) | [yes, below an existing owner](gitea.md#owners-tokens-and-create-on-push) |
| Where a repository may be created | [anywhere; at most 255 characters](distribution.md#create-on-push) | [anywhere; a name component of more than 255 bytes is 500](zot.md#create-on-push) | [anywhere; over 255 characters **500 `UNKNOWN`**; behind GitLab, project plus two levels (read)](gitlab.md#lajstrom-registry-probe) | [anywhere; over 255 characters **500 `UNKNOWN`**; behind GitLab, project plus two levels (read)](gitlab.md#lajstrom-registry-probe) | [below `<org>/`; elsewhere not tried](ghcr.md#the-probe) | [below `<project>/`, at any depth](harbor.md#projects-robots-and-create-on-push) | [anywhere, at any depth; a connector port per Nexus repository](nexus.md#connectors-and-create-on-push) | [anywhere, at any depth; a connector port per Nexus repository](nexus.md#connectors-and-create-on-push) | [`<owner>/` and any depth below; another owner 401, a missing one 404](gitea.md#owners-tokens-and-create-on-push) |
| Referrers API | [no: a plain 404, the tag schema](distribution.md#referrers) | [yes, with the `artifactType` filter; no tag schema](zot.md#referrers) | [no: a plain 404, the tag schema](gitlab.md#referrers) | [no: a plain 404, the tag schema](gitlab.md#referrers) | [no: 404 `MANIFEST_UNKNOWN`, the tag schema](ghcr.md#referrers) | [yes, with the `artifactType` filter](harbor.md#referrers) | [no: an HTML 404, the tag schema](nexus.md#referrers) | [yes, with the `artifactType` filter; and Nexus writes the tag schema itself](nexus.md#referrers) | [no: a plain 404, the tag schema](gitea.md#referrers) |
| `OCI-Subject` on a push with `subject` | [no](distribution.md#referrers) | [yes](zot.md#referrers) | [no](gitlab.md#referrers) | [no](gitlab.md#referrers) | [no](ghcr.md#referrers) | [yes](harbor.md#referrers) | [no](nexus.md#referrers) | [yes](nexus.md#referrers) | [no](gitea.md#referrers) |
| Catalog (`_catalog`) and its limits | [yes, `n`/`last`/`Link`: 100 a page, `n` at most 1000; the order depends on the driver](distribution.md#catalog) | [yes, `n`/`last`/`Link`, component by component (`/` first), no largest `n`](zot.md#catalog) | [yes, `n`/`last`/`Link`, `n` at most 1000; `/` first](gitlab.md#lajstrom-registry-probe) | [yes, `n`/`last`/`Link`, `n` at most 1000; lexical](gitlab.md#lajstrom-registry-probe) | [200 with all of GHCR, 1000 a page](ghcr.md#catalog) | [system admins with Basic only](harbor.md#catalog-and-listing) | [yes, `n`/`last`/`Link` (absolute), no largest `n`](nexus.md#catalog) | [yes, `n` and a `cursor` in `Link`; **`last` ignored**](nexus.md#catalog) | [yes, `n`/`last`/`Link`, lexical, no largest `n`; the user's view](gitea.md#catalog) |
| A listing API of its own | [none](distribution.md#catalog) | [none in the minimal build](zot.md#catalog) | [none: its `/gitlab/v1/` tag list is 404](gitlab.md#referrers) | [`/gitlab/v1/`: tag lists with referrers](gitlab.md#referrers) | [packages REST API, per organisation](ghcr.md#catalog) | [REST API per project, `page_size` at most 100](harbor.md#catalog-and-listing) | [REST API `search`: a component per tag](nexus.md#delete) | *not measured* | [REST API: the owner's packages, a version per tag or untagged digest](gitea.md#catalog) |
| Tag list order and pagination | [lexical, byte by byte; `n`/`last`/`Link`, `n` clamped at 1000; on S3 a missing repository is 200 `[]`](distribution.md#tags) | [lexical; `n`/`last`/`Link`](zot.md#tags) | [lexical; **`n` and `last` ignored**: every tag, no `Link`](gitlab.md#lajstrom-registry-probe) | [lexical; `n`/`last`/`Link`](gitlab.md#lajstrom-registry-probe) | [**creation order; `last` a position**](ghcr.md#tags) | [`n`/`last`/`Link`, as distribution](harbor.md#catalog-and-listing) | [byte order, case kept; `n`/`last`/`Link` (absolute)](nexus.md#tags) | [byte order, case kept; `n`/`last`/`Link` (absolute)](nexus.md#tags) | [**lower-cased, case-insensitive**; lexical; `n`/`last`/`Link`](gitea.md#tags) |
| Read-after-write lag | [none seen in 3 writes, on S3 too](#consistency) | [none seen in 3 writes](#consistency) | [none seen in 3 writes](#consistency) | [none seen in 3 writes](#consistency) | *not measured*; [`eventual` by the vendor table](#consistency) | *not measured* | *not measured* | *not measured* | *not measured* |
| Delete a manifest by digest | [yes, 202, with its tags; 405 `UNSUPPORTED` with deletes disabled; its referrers stay](distribution.md#delete) | [yes, 202; **405** for a manifest an index lists](zot.md#delete) | [yes, 202, with its tags; its referrers stay](gitlab.md#lajstrom-registry-probe) | [yes, 202, with its tags; **a manifest with a referrer: 500**; one an index lists: 409 `MANIFEST_REFERENCED`](gitlab.md#lajstrom-registry-probe) | [no: 405 `UNSUPPORTED`](ghcr.md#delete) | [yes; its referrers go with it](harbor.md#delete) | [**202, but its tags stay and serve it**](nexus.md#delete) | [yes, 202, with its tags](nexus.md#delete) | [yes, 202, with its tags and the blobs only it used](gitea.md#delete) |
| Delete by tag | [yes: the manifest stays; 405 with deletes disabled](distribution.md#delete) | [yes: the manifest stays, untagged](zot.md#delete) | [yes: the manifest stays, untagged](gitlab.md#delete) | [yes: the manifest stays, untagged](gitlab.md#delete) | [no: 405](ghcr.md#delete) | [400 through `/v2`; the REST API does](harbor.md#delete) | [yes: the manifest stays](nexus.md#delete) | [yes: the manifest stays](nexus.md#delete) | [the tag; **the manifest too**, when it was pushed by that tag only](gitea.md#delete) |
| Delete a repository | [no API; on S3 it leaves `_catalog` with its last manifest](distribution.md#delete) | [no API; an empty one goes with the collector](zot.md#delete) | [no API; it stays in `_catalog`](gitlab.md#lajstrom-registry-probe) | [no API; it leaves `_catalog` with its last manifest](gitlab.md#lajstrom-registry-probe) | [REST API: the package](ghcr.md#packages-left-in-lajstrom-extras) | [REST API](harbor.md#delete) | [no `/v2` API; it goes with its last tag](nexus.md#delete) | [no `/v2` API; it goes with its last manifest](nexus.md#delete) | [no `/v2` API; it leaves `_catalog` with its last manifest; the REST API not measured](gitea.md#delete) |
| Immutable tags, where the rule lives | [none: a tag re-points with 201](distribution.md#immutable-tags) | [none by default; access control without `update`: 403 `DENIED`](zot.md#immutable-tags) | [none; GitLab's patterns need the database (read)](gitlab.md#immutable-tags) | [GitLab's tokens: 401 `IMMUTABLE_TAG`](gitlab.md#immutable-tags) | [none: a tag re-points](ghcr.md#immutable-tags) | [project rules: 412 `PRECONDITION`](harbor.md#immutable-tags) | [the repository's `writePolicy: allow_once`: 409 `REDEPLOY_DISABLED`](nexus.md#immutable-tags) | [the same](nexus.md#immutable-tags) | [none: a tag re-points, and **the manifest it named is deleted** when nothing else holds it](gitea.md#immutable-tags) |
| Blob redirect | [none on the filesystem; 307 on S3, 20 min, enforced; `redirect.disable` streams](distribution.md#blob-redirect) | [none on the filesystem; 307 on S3 with `redirectBlobURL`](zot.md#blob-redirect) | [none on the filesystem; object storage not tested](gitlab.md#blob-redirect-ranges-blob-size-headers) | [none on the filesystem; object storage not tested](gitlab.md#blob-redirect-ranges-blob-size-headers) | [307 to its CDN, 5 to 10 min](ghcr.md#blob-redirect) | [none on the filesystem; S3 not run](harbor.md#blob-redirect-ranges-sizes-headers) | [none on the file system; S3 not run](nexus.md#blob-reads-ranges-sizes) | [none on the file system; S3 not run](nexus.md#blob-reads-ranges-sizes) | [none on local storage; `SERVE_DIRECT` on MinIO or Azure not run](gitea.md#blob-reads-ranges-sizes) |
| `Range` and `Accept-Ranges` | [206, 416 past the end; `Accept-Ranges: bytes`; on S3 the store's](distribution.md#blob-redirect) | [206; no `Accept-Ranges` on `GET`](zot.md#blob-reads-ranges-sizes) | [206; `Accept-Ranges: bytes`](gitlab.md#blob-redirect-ranges-blob-size-headers) | [206; `Accept-Ranges: bytes`](gitlab.md#blob-redirect-ranges-blob-size-headers) | [206 from the CDN; `accept-ranges: bytes`](ghcr.md#blob-redirect) | [206, 416 past the end; `Accept-Ranges: bytes`](harbor.md#blob-redirect-ranges-sizes-headers) | [206, 416 past the end; `Accept-Ranges: bytes` on `HEAD`; **gzip when offered**](nexus.md#blob-reads-ranges-sizes) | [**500 to every `Range`**; `Accept-Ranges: bytes` on `HEAD`](nexus.md#blob-reads-ranges-sizes) | [**none: 200 with the whole blob**; Forgejo 206, 416](gitea.md#blob-reads-ranges-sizes) |
| Largest blob measured | [1 GiB in one `PUT` and in `PATCH`es; on S3 in one `PUT`; no limit set](distribution.md#sizes) | [1 GiB in one `PUT` and in `PATCH`es; no limit set](zot.md#blob-reads-ranges-sizes) | [2 GiB in one `PUT`; no limit set](gitlab.md#blob-redirect-ranges-blob-size-headers) | [2 GiB in one `PUT`; no limit set](gitlab.md#blob-redirect-ranges-blob-size-headers) | [100 MiB; 10 GB documented](ghcr.md#size) | [2 GiB in one `PUT`](harbor.md#blob-redirect-ranges-sizes-headers) | [256 MiB in one `PUT`; no limit set](nexus.md#blob-reads-ranges-sizes) | [256 MiB in one `PUT`; no limit set](nexus.md#blob-reads-ranges-sizes) | [512 MiB in one `PUT`; no limit set (`LIMIT_SIZE_CONTAINER`: 403)](gitea.md#blob-reads-ranges-sizes) |
| Manifest size limit | [4 MiB; one byte more is 400 `MANIFEST_INVALID`](distribution.md#sizes) | [4 MiB; one byte more is 413](zot.md#blob-reads-ranges-sizes) | [4 MiB; one byte more is 400 `MANIFEST_INVALID`](gitlab.md#lajstrom-registry-probe) | [4 MiB; one byte more is 400 `MANIFEST_INVALID`](gitlab.md#lajstrom-registry-probe) | *not measured* | [4 MiB; one byte more is 413](harbor.md#blob-redirect-ranges-sizes-headers) | [10 MiB taken; 32 MiB 400, its JSON parser's limit on a string](nexus.md#blob-reads-ranges-sizes) | [10 MiB; above, 413 `MANIFEST_INVALID`](nexus.md#blob-reads-ranges-sizes) | [10 MiB taken; 16 MiB 413 `MANIFEST_INVALID`](gitea.md#blob-reads-ranges-sizes) |
| Lajstrom's manifest profile (`artifactType`, empty config, own media types) | [accepted](distribution.md#media-types) | [accepted](zot.md#media-types) | [accepted](gitlab.md#media-types) | [**refused**: unknown `artifactType`, unless a flag](gitlab.md#media-types) | [accepted](ghcr.md#create-on-push) | [accepted](harbor.md#media-types) | [accepted](nexus.md#media-types) | [accepted](nexus.md#media-types) | [accepted](gitea.md#media-types) |
| Auth | [none; Basic (htpasswd)](distribution.md#authentication) | [none; Basic (htpasswd)](zot.md#our-tests-against-it) | [none](gitlab.md#version-tested) | [none; Bearer from a test token server](gitlab.md#bearer-tokens) | [Bearer from `/token`: the PAT in base64](ghcr.md#token) | [Bearer from Harbor's token service](harbor.md#projects-robots-and-create-on-push) | [Basic (`forceBasicAuth`); `/v2/token` too](nexus.md#connectors-and-create-on-push) | [Basic (`forceBasicAuth`); `/v2/token` too](nexus.md#connectors-and-create-on-push) | [Bearer from `/v2/token` (`scope="*"`), 24 h; Basic on `/v2/` too](gitea.md#owners-tokens-and-create-on-push) |
| Rate limits seen | [none](distribution.md#our-tests-against-it) | [none](zot.md#blob-reads-ranges-sizes) | *not measured* | *not measured* | [none on `/v2`; REST API 5000 an hour](ghcr.md#rate-limits) | [none](harbor.md#blob-redirect-ranges-sizes-headers) | [**429 for 30 s after three wrong passwords, the right one too**](nexus.md#what-the-provider-must-do-differently) | [the same](nexus.md#what-the-provider-must-do-differently) | *not measured* |
| Upload: chunk out of order | [416 `RANGE_INVALID`](distribution.md#uploads) | [416 `BLOB_UPLOAD_INVALID`](zot.md#uploads) | [416 `CONTENT_RANGE_INVALID`](gitlab.md#lajstrom-registry-probe) | [416 `CONTENT_RANGE_INVALID`](gitlab.md#lajstrom-registry-probe) | [416](ghcr.md#upload-sessions) | [**appended**, no 416](harbor.md#uploads) | [416 `RANGE_NOT_SATISFIABLE`](nexus.md#uploads) | [416 `RANGE_NOT_SATISFIABLE`](nexus.md#uploads) | [416 `BLOB_UPLOAD_INVALID`](gitea.md#uploads) |
| Upload: chunk size limit | [none met: 1 GiB in one `PATCH`](distribution.md#sizes) | [none met: 512 MiB a `PATCH`](zot.md#uploads) | [none met: 2 GiB in one `PATCH`](gitlab.md#lajstrom-registry-probe) | [none met: 8 MiB in one `PATCH`](gitlab.md#lajstrom-registry-probe) | [4 MiB a `PATCH`, then 416](ghcr.md#upload-sessions) | *not measured* | *not measured* | *not measured* | [none met: 512 MiB in one `PATCH`; **a streamed `PATCH` after the first write is 400**](gitea.md#uploads) |
| Upload: session status (`GET`) | [works, 204 with `Range`; 500 on S3 after a refused digest](distribution.md#uploads) | [works, 204 with `Range`](zot.md#uploads) | [works, 204 with `Range`](gitlab.md#lajstrom-registry-probe) | [works, 204 with `Range`](gitlab.md#lajstrom-registry-probe) | [**303 to a web page**](ghcr.md#upload-sessions) | [works (the test)](harbor.md#uploads) | [works, 204 with `Range`](nexus.md#uploads) | [works, 204 with `Range`](nexus.md#uploads) | [works, 204 with `Range`; **Forgejo one byte too many**](gitea.md#forgejo) |
| Upload: cancel (`DELETE`) | [works; 500 on S3 after a refused digest](distribution.md#uploads) | [works; 503 on S3 after a refused digest](zot.md#uploads) | [works](gitlab.md#lajstrom-registry-probe) | [works](gitlab.md#lajstrom-registry-probe) | [405 `UNSUPPORTED`](ghcr.md#upload-sessions) | [works (the test)](harbor.md#uploads) | [204; a `PATCH` after it **500**](nexus.md#uploads) | [works](nexus.md#uploads) | [works](gitea.md#uploads) |
| Upload: mount (`POST ?mount=&from=`) | [201](distribution.md#uploads) | [201](zot.md#uploads) | [201](gitlab.md#lajstrom-registry-probe) | [201](gitlab.md#lajstrom-registry-probe) | [201; without `from` 202, a session](ghcr.md#upload-sessions) | [201 inside the project](harbor.md#uploads) | [**202: a session instead**](nexus.md#blob-reads-ranges-sizes) | [201](nexus.md#blob-reads-ranges-sizes) | [201 inside the owner](gitea.md#blob-reads-ranges-sizes) |
| Upload: wrong digest, empty blob | [a digest mismatch; stored (the test)](distribution.md#our-tests-against-it) | [a digest mismatch; stored (the test)](zot.md#our-tests-against-it) | [400 `DIGEST_INVALID`, the session stays; stored](gitlab.md#lajstrom-registry-probe) | [400 `DIGEST_INVALID`, the session stays; stored](gitlab.md#lajstrom-registry-probe) | [404 `BLOB_UPLOAD_INVALID`; empty blob **refused**](ghcr.md#upload-sessions) | [a digest mismatch; stored (the test)](harbor.md#uploads) | [400 `DIGEST_INVALID`, the session stays; stored](nexus.md#uploads) | [400 `DIGEST_INVALID`, the session ends; stored](nexus.md#uploads) | [400 `DIGEST_INVALID`, the session stays; stored](gitea.md#uploads) |
| Upload continued by another replica | [works (the test)](distribution.md#our-tests-against-it) | [works (the test)](zot.md#our-tests-against-it) | [works (the test)](gitlab.md#our-tests-against-it) | [works; behind a token server fails: ours](gitlab.md#bearer-tokens) | [fails: ours, priming](ghcr.md#our-tests) | [works, with tokens](harbor.md#uploads) | [works (the test)](nexus.md#our-tests-against-it) | [works (the test)](nexus.md#our-tests-against-it) | [works since the provider fix of #120](gitea.md#what-the-provider-must-do-differently) |
| Garbage collector and an untagged referrer (#37) | [kept; with `--delete-untagged`, while a tag-schema index lists it](distribution.md#garbage-collection) | [kept, with or without its subject](zot.md#garbage-collection) | [kept; `--delete-untagged` deletes it](gitlab.md#garbage-collection) | [**deleted 24 h after its push**](gitlab.md#garbage-collection) | *not measured* | [kept; it goes with its subject](harbor.md#garbage-collection) | [**deleted by the task "Delete unused manifests"**; listed by a tag-schema index, kept](nexus.md#garbage-collection) | [kept: no collector task](nexus.md#garbage-collection) | [kept by `cleanup_packages`; an unused blob goes](gitea.md#cleanup) |
| What the probe finds | [`referrers=false catalog=true`](distribution.md#the-probe) | [`referrers=true catalog=true`](zot.md#the-row) | [`referrers=false catalog=true`](gitlab.md#the-row) | [`referrers=false catalog=true`](gitlab.md#the-row) | [`catalog=true`, wrong; vendor table: `false`](ghcr.md#the-probe) | [`referrers=true catalog=false`](harbor.md#the-row) | [`referrers=false catalog=true`](nexus.md#the-row) | [`referrers=true catalog=true`](nexus.md#the-row) | [`referrers=false catalog=true`](gitea.md#the-row) |
| Overrides needed | [none; `deleteManifest: false` with deletes disabled](distribution.md#the-probe) | [none; `immutableTags: true` behind a policy without `update`](zot.md#configuration) | [none; behind GitLab with rules, `deleteManifest: false`](gitlab.md#the-row) | [the flag on the registry; behind GitLab, as A](gitlab.md#media-types) | [none since the vendor table](ghcr.md#the-probe) | [none; `prefix: <project>/`](harbor.md#the-row) | [none since the vendor table (#352): `deleteManifest: false`](nexus.md#what-the-provider-must-do-differently) | [none since the vendor table (#352): blobs read whole](nexus.md#what-the-provider-must-do-differently) | [none; `prefix: <owner>/`](gitea.md#the-row) |

### JFrog Container Registry

JFrog Container Registry 7.161.26, the free Artifactory edition for containers
([jfrog-jcr.md](jfrog-jcr.md)), measured on 2026-11-10 with a local docker repository as
Lajstrom's prefix (`prefix: <repo-key>/`, the repository path method). Its cells are here, in
the rows of the table above, rather than a column of it, so that the spikes of Nexus (#119) and
Gitea (#120), which add theirs, merge apart; they become a column in one edit afterwards. **Not
a backend for data yet**: JCR keeps a blob under whatever digest it is told and drops the body
of a closing `PUT` after a `PATCH`, and its `HEAD` of a blob says 200 where `GET` is 404. Its
deviations are on its page, [Deviations](jfrog-jcr.md#deviations-from-the-specification); its
`registries[]` entry is [Configuration](jfrog-jcr.md#configuration); whoever changes the pin
in its compose file re-runs [Commands](jfrog-jcr.md#commands).

| | JCR 7.161.26 |
| --- | --- |
| Create on push | [yes, inside an existing repository key; a key never (404)](jfrog-jcr.md#create-on-push) |
| Where a repository may be created | [below `<repo-key>/`, at any depth](jfrog-jcr.md#create-on-push) |
| Referrers API | [yes, with the `artifactType` filter](jfrog-jcr.md#referrers) |
| `OCI-Subject` on a push with `subject` | [yes](jfrog-jcr.md#referrers) |
| Catalog (`_catalog`) and its limits | [**200 with an empty body**; per key at `/v2/<repo-key>/_catalog`, `n`/`last`/`Link`, lexical](jfrog-jcr.md#catalog-and-tags) |
| A listing API of its own | [Artifactory's Docker and storage APIs](jfrog-jcr.md#catalog-and-tags) |
| Tag list order and pagination | [lexical; `n`/`last`/`Link`](jfrog-jcr.md#catalog-and-tags) |
| Read-after-write lag | *not measured* |
| Delete a manifest by digest | [yes, 202, with its tags; its referrers stay](jfrog-jcr.md#delete) |
| Delete by tag | [yes: the manifest stays](jfrog-jcr.md#delete) |
| Delete a repository | [Artifactory's REST API](jfrog-jcr.md#delete) |
| Immutable tags, where the rule lives | [none: a tag re-points with 201; Artifactory's overwrite permission, not measurable on JCR](jfrog-jcr.md#immutable-tags) |
| Blob redirect | [none on the filesystem store](jfrog-jcr.md#blob-reads-head-and-get) |
| `Range` and `Accept-Ranges` | [206, 416 past the end; `Accept-Ranges: bytes` on `GET`](jfrog-jcr.md#blob-reads-head-and-get) |
| Largest blob measured | [1 GiB in one `PUT`](jfrog-jcr.md#blob-reads-head-and-get) |
| Manifest size limit | [none met: 8 MiB + 1 byte is 201](jfrog-jcr.md#blob-reads-head-and-get) |
| Lajstrom's manifest profile (`artifactType`, empty config, own media types) | [accepted](jfrog-jcr.md#media-types) |
| Auth | [Bearer from `/v2/token`, Basic too; the admin's access token (no other user through REST)](jfrog-jcr.md#run-it) |
| Rate limits seen | [none](jfrog-jcr.md#our-tests-against-it) |
| Upload: chunk out of order | [416](jfrog-jcr.md#uploads-and-digests) |
| Upload: chunk size limit | *not measured* |
| Upload: session status (`GET`) | [works, 204 with `Range`](jfrog-jcr.md#uploads-and-digests) |
| Upload: cancel (`DELETE`) | [405](jfrog-jcr.md#uploads-and-digests) |
| Upload: mount (`POST ?mount=&from=`) | [**202: a session instead**](jfrog-jcr.md#uploads-and-digests) |
| Upload: wrong digest, empty blob | [**a wrong digest is 201 and stored**; the empty blob stored](jfrog-jcr.md#uploads-and-digests) |
| Upload continued by another replica | [fails: ours (#342), then **the closing `PUT`'s body dropped**](jfrog-jcr.md#uploads-and-digests) |
| Garbage collector and an untagged referrer (#37) | *not measured* |
| What the probe finds | [`referrers=true catalog=true`, wrong; with the probe fix of #338 `catalog=false`](jfrog-jcr.md#what-the-provider-must-do-differently) |
| Overrides needed | [none; `prefix: <repo-key>/`](jfrog-jcr.md#configuration) |

## Object stores behind distribution

distribution 3.1.2 on its `s3` storage driver, the profiles `s3` and `s3-minio` of the
[dev stack](../dev-stack.md#the-s3-profile), side by side (#27, #118; measured again for #17,
[distribution.md](distribution.md#blob-redirect)). The third column is for the next store.

| | SeaweedFS 4.52 | MinIO 2025-07-23 | next store |
| --- | --- | --- | --- |
| Profile and ports | `s3`: the S3 gateway on `localhost:9000`, distribution on `localhost:5002` | `s3-minio`: MinIO on `localhost:9001`, distribution on `localhost:5006` | *not measured* |
| Image and platforms | 270 MB to pull, 1 GB on disk; linux/amd64 and arm64 | the Bitnami build (`minio/minio` left Docker Hub); 412 MB on disk; linux/amd64 and arm64 | *not measured* |
| Bucket creation | none by the image: the one-shot `seaweedfs-bucket` runs `weed shell` | the image, from `MINIO_DEFAULT_BUCKETS` | *not measured* |
| Start | about 15 s from an empty volume, 8 s after | about 4 s with its volume; from an empty one *not measured* | *not measured* |
| Blob `GET` and `HEAD` | 307 to a presigned URL | 307 to a presigned URL | *not measured* |
| The presigned URL's host | the endpoint distribution talks to: `localhost:9000` | the endpoint distribution talks to: `localhost:9001` | *not measured* |
| Its lifetime | 20 minutes, `X-Amz-Expires=1200` (fixed in the s3 driver) | 20 minutes, `X-Amz-Expires=1200` | *not measured* |
| Expiry enforced | yes: past it 403 "Request has expired"; a tampered signature `SignatureDoesNotMatch` | yes: past it 403 "Request has expired"; a tampered signature `SignatureDoesNotMatch` | *not measured* |
| `Range` through the redirect | 206; past the end 416 `InvalidRange`; two ranges 200, the whole blob | the same | *not measured* |
| `storage.redirect.disable: true` | no redirect: 200 with the bytes, a range 206 | the same | *not measured* |
| The driver's `redirectendpoint` | redirects signed for `localhost:9000` while distribution talks to `seaweedfs:9000`: followed, 200 | the same, `localhost:9001` and `minio:9001` | *not measured* |
| Largest blob measured | 1 GiB in one `PUT` | 1 GiB in one `PUT` | *not measured* |
| Read-after-write lag | [none seen in 3 writes](#consistency); a blob `HEAD` is the 307 | the same | *not measured* |
| `HEAD` | 307; the provider follows it, 200 (`StatBlob`) | 307; the provider follows it, 200 | *not measured* |
| Cancel of an upload session after a refused digest | 500 `s3aws: append to zero-size path … unsupported`; its status `GET` 500 too | the same 500s: distribution's s3 driver, not the store | *not measured* |
| Tests | `TestIntegrationS3Redirect`, `TestIntegration` with and without the prefix, `TestAptGetInstall/s3`: pass; `TestIntegrationUploads` but for the cancel | the same | *not measured* |

A presigned URL names the endpoint distribution signs for, and the signature covers the host:
the dev stack runs distribution in the store's network namespace so that `localhost:<port>`
works inside the registry's container and on the machine alike
([dev-stack.md](../dev-stack.md#the-s3-profile)). The s3 driver's `redirectendpoint` would do
without it: distribution talks to the store on one address and signs for another
([distribution.md](distribution.md#blob-redirect)); the dev stack does not use it.
`TestIntegrationS3Redirect` (`internal/facade`, `LAJSTROM_TEST_REGISTRY_S3`) checks the 307 of
the facade and of `/raw/`, that the bytes behind the `Location` have the blob's digest, a range
after the redirect, `HEAD`, and that `policy.blobRedirect: never` streams. CI does not run these
profiles.

## Deviations from the specification

Where a registry's protocol does not do what distribution-spec v1.1.1 or image-spec v1.1.1 says,
measured: the list from which we report upstream. Each row was checked against the spec's text;
what the spec allows is in the second table, not here. "To file" means nobody from Lajstrom has
reported it yet.

| Registry, version | What it does (measured) | What the spec says | What it costs Lajstrom | Our handling today | Upstream |
| --- | --- | --- | --- | --- | --- |
| GitLab container registry v4.43.0, B (metadata database) | Refuses a manifest whose `artifactType` or `config.mediaType` is not in its table of known types: 400 `MANIFEST_INVALID` "unknown media type", every Lajstrom manifest among them ([gitlab.md](gitlab.md#media-types)) | image-spec 1.1.1, manifest, `artifactType`: "Implementations storing or copying image manifests MUST NOT error on encountering an `artifactType` that is unknown to the implementation"; the same for `config.mediaType` | Nothing Lajstrom writes can be stored; GitLab.com runs this variant | `REGISTRY_FF_DYNAMIC_MEDIA_TYPES=true` on the registry; since #35 `serve` warns at startup when `GET /v2/` says `Gitlab-Container-Registry-Database-Enabled: true`; the profile is not changed (a contract question) | [gitlab-org/container-registry](https://gitlab.com/gitlab-org/container-registry/-/issues): to file (accept unknown types, or seed `application/vnd.lajstrom.*`) |
| GitLab container registry v4.43.0, B, with GitLab's rules in the token (a test token server played GitLab) | A push to an immutable tag is 401 `IMMUTABLE_TAG`; every digest delete in a repository a pattern applies to is 401 `PROTECTED_MANIFEST` ([gitlab.md](gitlab.md#immutable-tags)) | Error codes: "The `code` field MUST be one of the following", and neither code is in the list; Deleting Manifests: "If manifest deletion is disabled, the registry MUST respond with either a `400 Bad Request` or a `405 Method Not Allowed`" | A policy refusal reads as bad credentials: `ErrUnauthorized`, 502 at `/raw/`; `deleteManifest` stays true | `IMMUTABLE_TAG` becomes `ErrConflict` by its message; `capabilities: {deleteManifest: false}` behind a GitLab with rules; reading `PROTECTED_MANIFEST` as `ErrUnsupported` is item 1 of gitlab.md's list, no issue yet | [gitlab-org/container-registry](https://gitlab.com/gitlab-org/container-registry/-/issues): to file |
| GitLab container registry v4.43.0, B (metadata database) | The digest delete of a manifest that a referrer names is 500 `UNKNOWN` (its log: a foreign key of `gc_manifest_review_queue` violated); the delete of a manifest an image index lists is 409 `MANIFEST_REFERENCED` (`lajstrom registry probe`, [gitlab.md](gitlab.md#lajstrom-registry-probe)) | Deleting Manifests: "Upon success, the registry MUST respond with a `202 Accepted` code"; a 500 is a bug. Error codes: "The `code` field MUST be one of the following", and `MANIFEST_REFERENCED` is not in the list | None for Lajstrom's own deletes, which delete what refers to a version before the version; a client that deletes a subject through the facade's `/v2` gets the 500. The 409 is `ErrConflict`, as Lajstrom wants | ingest deletes referrers first | [gitlab-org/container-registry](https://gitlab.com/gitlab-org/container-registry/-/issues): to file |
| GitLab container registry v4.43.0, A (metadata on the filesystem) | `tags/list` ignores `n` and `last`: `?n=2` answers every tag and no `Link`, and `?n=1&last=a` answers `a` too (`lajstrom registry probe` and curl, [gitlab.md](gitlab.md#lajstrom-registry-probe)); B pages | Listing Tags: with `n`, "the response MUST include `<int>` results" unless fewer tags exist; with `last`, "A request of this sort will return up to `<int>` tags, beginning non-inclusively with `<tagname>`" | None: the provider follows `Link` and takes a whole list in one page | nothing needed | [gitlab-org/container-registry](https://gitlab.com/gitlab-org/container-registry/-/issues): to file |
| GitLab container registry v4.43.0, A and B | A repository name over 255 characters is 500 `UNKNOWN` with the name in its detail, where distribution answers 400 `NAME_INVALID` ([gitlab.md](gitlab.md#lajstrom-registry-probe)) | Error codes: `NAME_INVALID`, "invalid repository name"; a 500 is a server error | None: Lajstrom's names are at most 255 characters | nothing needed | [gitlab-org/container-registry](https://gitlab.com/gitlab-org/container-registry/-/issues): to file |
| GHCR, 2026-11-07 | In a repository without a manifest, a blob answers `HEAD` 200 and `GET` 404 `BLOB_UNKNOWN`, seconds later too ([ghcr.md](ghcr.md#create-on-push)) | Checking if content exists: "A HEAD request to an existing blob or manifest URL MUST return `200 OK`"; Pulling blobs: "A GET request to an existing blob URL MUST provide the expected blob, with a response code that MUST be `200 OK`" | Ingest reads the metadata layer back before it pushes the manifest, so the first version of every package fails; raw `PUT` answers 400 | none; ingest should check the layer from its spool (ghcr.md), no issue yet | GitHub has no public tracker: [Community, Packages](https://github.com/orgs/community/discussions/categories/packages): to file |
| GHCR, 2026-11-07 | The empty blob `sha256:e3b0c442…` cannot be pushed (404 `BLOB_UNKNOWN` to `PUT` and `POST ?digest=`), yet `HEAD` and `GET` answer 200 with 0 bytes, in a repository that never existed too ([ghcr.md](ghcr.md#upload-sessions)) | Checking if content exists: "If the blob or manifest is not found in the repository, the response code MUST be `404 Not Found`"; Pushing Manifests: "An attempt to pull a nonexistent repository MUST return response code `404 Not Found`" | A client's push of the empty blob through the facade fails, though a manifest with it as a layer is taken | none; never upload the 0-byte blob to GHCR (ghcr.md), no issue yet | [Community, Packages](https://github.com/orgs/community/discussions/categories/packages): to file |
| GHCR, 2026-11-07 | `GET` of an upload session is `303 See Other` to a github.com web page that answers 404, with no challenge; `HEAD` is 405 ([ghcr.md](ghcr.md#upload-sessions)) | `GET` of the session (end-13): "The response to an active upload `<location>` MUST be a `204 No Content` response code", with `Location` and `Range` | No offset from the registry; a replica that primes its token with this `GET` gets none, and its streamed `PATCH` fails ("request body is not rewindable") | none; prime with a request GHCR challenges and keep the offset in Lajstrom's session (ghcr.md), no issue yet | [Community, Packages](https://github.com/orgs/community/discussions/categories/packages): to file |
| GHCR, 2026-11-07 | `tags/list` is in creation order; `last` is a position, and a `last` that is no tag gives `"tags":null` ([ghcr.md](ghcr.md#tags)) | Listing Tags: "the tags MUST be in lexical order"; with `last`, "up to `<int>` tags *after* `<tagname>`"; the body's `tags` is a list | None today: the provider follows `Link` and never makes up a `last` | oras-go follows `Link` | [Community, Packages](https://github.com/orgs/community/discussions/categories/packages): to file |
| Harbor v2.15.4 | Appends a chunk at the end of the session whatever its `Content-Range` says: 202 and a longer `Range` ([harbor.md](harbor.md#uploads)) | Pushing a blob in chunks: "If a chunk is uploaded out of order, the registry MUST respond with a `416 Requested Range Not Satisfiable` code" | A client resuming at a wrong offset is caught only by the digest of the closing `PUT`; `TestIntegrationUploads/chunk out of order` fails on Harbor | nothing needed: the closing digest refuses the blob | [goharbor/harbor](https://github.com/goharbor/harbor/issues): to file. Harbor's registry is distribution v2.8.3, whose `PATCH` ignores `Content-Range` (a TODO in `registry/handlers/blobupload.go`); distribution 3 answers 416 `RANGE_INVALID` (measured on 3.1.2), so Harbor on distribution 3 (goharbor/harbor#20815, open) fixes it |
| Harbor v2.15.4, distribution 3.1.2 | A manifest `GET` or `HEAD` without an OCI type in `Accept` (none, `*/*`, Docker's v2 type alone) is 404 `MANIFEST_UNKNOWN` "OCI manifest found, but Accept header does not support OCI manifests" (distribution: "accept header") ([harbor.md](harbor.md#blob-redirect-ranges-sizes-headers), [distribution.md](distribution.md#create-on-push)) | Pulling manifests: "The client SHOULD include an `Accept` header"; "A GET request to an existing manifest URL MUST provide the expected manifest, with a response code that MUST be `200 OK`"; 404 is for a manifest "not found in the repository" | None through Lajstrom: oras-go and the provider send OCI types; a client of the facade without one gets the 404 | nothing; the facade passes the client's `Accept` | [distribution/distribution](https://github.com/distribution/distribution/issues): to file (the code is distribution's; #4385 asked about it and was closed as a Docker Hub question); goharbor/harbor through it |
| Harbor v2.15.4 | Error codes outside the spec's list on `/v2`: `BAD_REQUEST` (a repository outside a project, a malformed digest), `NOT_FOUND` (tags of a missing repository, a deleted manifest), `PRECONDITION` (412, an immutable tag), `REQUEST_ENTITY_TOO_LARGE` (413) ([harbor.md](harbor.md)) | Error codes: "The `code` field MUST be one of the following" (`NAME_INVALID`, `NAME_UNKNOWN`, `MANIFEST_UNKNOWN`, `DENIED`, …) | The provider reads the status, so 404 is `ErrNotFound`; a 412 is `ErrConflict` only because Harbor's message says "configured as immutable"; the referrers probe's 400 for a name outside a project says nothing about the cause | `classifyResponse` by status, and by the message for 412 (`mentionsImmutable`); harbor.md items 3 and 4; the Harbor adapter, #73 | [goharbor/harbor](https://github.com/goharbor/harbor/issues): to file |
| distribution 3.1.2, `s3` storage driver (on SeaweedFS 4.52 and MinIO 2025-07-23); Zot v2.1.23 on S3 (MinIO), which uses the same driver | After the closing `PUT` of a session is refused for its digest, the status `GET` and the cancel of that session answer 500 `s3aws: append to zero-size path … unsupported` on distribution ([distribution.md](distribution.md#uploads)); Zot's cancel answers 503 with no body and logs the same error ([zot.md](zot.md#uploads)) | `GET` of the session (end-13): 204 for an active session, 404 otherwise; the cancel is not in 1.1.1 (Docker's API: 204, or 404 for an unknown session). A 500 or a 503 is neither: a bug | The session stays in the bucket until the registry purges old uploads; a log line | the facade cancels the backend session itself after the refused digest, logs the error (`facade: cancel backend upload session`), and answers the client's `DELETE` 404 `BLOB_UPLOAD_UNKNOWN` (#24) | [distribution/distribution](https://github.com/distribution/distribution/issues): to file (the driver is distribution's) |
| Nexus 3.98.1, `docker` format | `DELETE /v2/<name>/manifests/<digest>` answers 202, and every tag that named the manifest still answers 200 with it and is still listed; the digest alone is 404 ([nexus.md](nexus.md#delete)) | Deleting Manifests: "Once deleted, a `GET` to `/v2/<name>/manifests/<digest>` and any tag pointing to that digest will return a 404" | A version deleted through Lajstrom stays in the registry under its tag, and reindex finds it again; its referrers too | none; `capabilities: {deleteManifest: false}` refuses deletes instead (nexus.md, item 2) | [Sonatype's community forum](https://community.sonatype.com/): to file |
| Nexus 3.98.1, `oci` format | Every blob `GET` with a `Range` header is 500 `UNKNOWN` (`IllegalArgumentException: Paths must start with a slash` in its log), though `HEAD` says `Accept-Ranges: bytes` ([nexus.md](nexus.md#blob-reads-ranges-sizes)) | Pulling blobs: "A GET request to an existing blob URL MUST provide the expected blob"; "A registry SHOULD support the `Range` request header" (RFC 9110 lets a server ignore it, with a 200, not fail) | Ingest's range reads of a wheel fail: no pypi upload works; a client's range through the facade and `/raw/` is 500 | none (nexus.md, item 3) | [Sonatype's community forum](https://community.sonatype.com/): to file |
| Nexus 3.98.1, both formats | The `Location` of a manifest `PUT` is two paths glued together, `http://localhost:5008/v2/one/manifests/v1/repository/lajstrom/v2/one/manifests/v1`; and the 201 carries the manifest as its body ([nexus.md](nexus.md#connectors-and-create-on-push)) | Pushing Manifests: the registry "MUST have the following header: `Location: <location>`", and "The `<location>` is a pullable manifest URL" | None: oras-go reads neither | nothing needed | [Sonatype's community forum](https://community.sonatype.com/): to file |
| Nexus 3.98.1, `docker` format | A `PATCH` to an upload session after its cancel is 500 ([nexus.md](nexus.md#uploads)); the `oci` format answers 404 | The error table has `BLOB_UPLOAD_UNKNOWN`, "blob upload unknown to registry", for this; a 500 is a bug | `TestIntegrationUploads/cancel` fails; a client gets 500 for a session that is gone | none | [Sonatype's community forum](https://community.sonatype.com/): to file |
| Nexus 3.98.1, both formats | Error codes outside the spec's list on `/v2`: `REDEPLOY_DISABLED` (409, a tag under `writePolicy: allow_once`), `RANGE_NOT_SATISFIABLE` (416, a chunk out of order) ([nexus.md](nexus.md#immutable-tags)) | Error codes: "The `code` field MUST be one of the following" | None: the provider reads the status, 409 is `ErrConflict` | nothing needed | [Sonatype's community forum](https://community.sonatype.com/): to file |
| Gitea 1.27.3, Forgejo 17.0.1 | Tags are lower-cased when pushed and resolved without case: `5.4.Final` is listed as `5.4.final`, and a push to `5.4.final` re-points it ([gitea.md](gitea.md#tags)) | Pulling manifests: a tag "MUST match the following regular expression: `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`", case included; Listing Tags returns the tags of the repository, not others | Two versions that differ only in case are one tag (the second is a 409 through Lajstrom); `tags/list` gives other tags than Lajstrom wrote | none (gitea.md, item 2) | [go-gitea/gitea](https://github.com/go-gitea/gitea/issues), [Forgejo](https://codeberg.org/forgejo/forgejo/issues): to file |
| Forgejo 17.0.1 | `GET` of an upload session answers `Range: 0-5` after 5 bytes (`0-4` in the `PATCH`'s answer) ([gitea.md](gitea.md#forgejo)) | Pushing a blob in chunks: "The `<end-of-range>` value is the position of the last uploaded byte" | A client that resumes from the status sends its next chunk one byte late, 416; `TestIntegrationUploadAcrossReplicas` fails | none: the facade passes the backend's `Range` | [Forgejo](https://codeberg.org/forgejo/forgejo/issues): to file |
| distribution 3.1.2 | A repository name over 255 characters is 400 `NAME_INVALID` with the error object written twice: two JSON documents in one `application/json` body of 845 bytes ([distribution.md](distribution.md#create-on-push)) | Error Codes: "If the response body is in JSON format, it MUST have the following format", one object with `errors` | None: the provider decodes the first object, and Lajstrom's names are at most 255 characters | nothing needed | [distribution/distribution](https://github.com/distribution/distribution/issues): to file |

**Not deviations**: measured, and allowed by the spec or outside it. Some are still worth a
word upstream, and the row says so; where one costs Lajstrom something, the row says that too.

| Registry | What it does | Why it is not a deviation |
| --- | --- | --- |
| distribution 3.1.2, GitLab v4.43.0, GHCR | No referrers API (a 404); none of them sends `OCI-Subject` | Optional: "If the referrers API returns a 404, the client MUST fallback to pulling the referrers tag schema" (Listing Referrers, Backwards Compatibility). `OCI-Subject` is for "a registry implementation that supports the referrers API" |
| GHCR, Harbor, GitLab, distribution | GHCR's `_catalog` is 200 with every repository of GHCR as the token sees it, other users' among them; Harbor's is for system administrators with Basic auth; GitLab's and distribution's refuse `n` above 1000 (distribution: 400 `PAGINATION_NUMBER_INVALID`); distribution's order is not byte order and differs between its drivers | `_catalog` is not in distribution-spec 1.1.1 at all (it is Docker's registry API). A catalog of what the token may push is still worth asking GitHub for |
| GHCR; distribution with deletes disabled; Harbor | Every delete through `/v2` is 405 `UNSUPPORTED` (GHCR, and distribution: a manifest or a blob); a tag delete 400 (Harbor) | Content Management: "Registries MAY implement deletion or they MAY disable it"; disabled is "either a `400 Bad Request` or a `405 Method Not Allowed`" |
| GHCR | The cancel of an upload session is 405 | The cancel is not in distribution-spec 1.1.1 |
| GHCR | A `PATCH` above 4 MiB is 416 "the request body is too large" | The spec sets no largest chunk (only `OCI-Chunk-Min-Length`, a smallest); 416 is the status it gives an out-of-order chunk, and no header announces the limit: worth a word upstream |
| GHCR | A `PATCH` without `Content-Range` starts the session over | 1.1.1 defines a chunk `PATCH` with `Content-Range` only |
| GHCR | A wrong digest on the closing `PUT` is 404 `BLOB_UPLOAD_INVALID` | The error table has `DIGEST_INVALID`, "provided digest did not match uploaded content", for exactly this, but the spec binds no code and no status to the condition; worth asking. The provider reads it as `ErrNotFound`, not `content.ErrMismatchedDigest` |
| GHCR | The bearer token is the PAT in base64: no expiry, not narrowed by the scope | Authentication is "out of the purview of this specification" (Determining Support). It means the token cache and any log line with a bearer carry the PAT |
| Zot v2.1.23 | The delete of a manifest that an image index lists, and of a blob a manifest uses, is 405 `DENIED`; every other delete works ([zot.md](zot.md#delete)) | Content Management lets a registry disable deletion ("either a `400 Bad Request` or a `405 Method Not Allowed`") and says nothing about refusing one manifest; a 409 would say it more clearly, worth a word upstream. Until #153 the provider read the 405 as "this registry does not delete" and turned `DeleteManifest` false until a restart; now a 405 `DENIED` is `ErrConflict` for that one delete (409 at `/raw/`) |
| Zot v2.1.23 | No `Accept-Ranges` on a blob `GET` (its `HEAD` has one); a range is 206 ([zot.md](zot.md#blob-reads-ranges-sizes)) | "A registry SHOULD support the `Range` request header in accordance with RFC 9110", which Zot does; in RFC 9110 (§14.3) a server that supports ranges "can send" `Accept-Ranges`, and "a client MAY generate range requests regardless" |
| Harbor v2.15.4, distribution 3.1.2 | `POST …/blobs/uploads/?digest=` with the bytes opens a session (202) and ignores them ([distribution.md](distribution.md#create-on-push)) | Single POST: "Registries that do not support single request monolithic uploads SHOULD return a `202 Accepted`" |
| distribution 3.1.2 | A manifest over 4 MiB is 400 `MANIFEST_INVALID` "http: request body too large" ([distribution.md](distribution.md#sizes)) | Pushing Manifests: a registry that enforces a limit "SHOULD respond … with a response code `413 Payload Too Large`": a SHOULD; Harbor and Zot answer 413. Lajstrom's `server.maxManifestSize`, 4 MiB by default, refuses a larger one first |
| distribution 3.1.2, `s3` storage driver | `tags/list` of a repository that does not exist is 200 `{"tags":[]}`; on the filesystem it is 404 `NAME_UNKNOWN` ([distribution.md](distribution.md#tags)) | Listing Tags defines the found case only: "Assuming a repository is found, this request MUST return a `200 OK` response code". Through the facade a path never pushed lists no tags instead of `NAME_UNKNOWN` (read in the facade, not run) |
| Harbor v2.15.4 | Over a project's quota, 403 `DENIED` on the blob and manifest `PUT` | Quotas are outside the spec, and `DENIED` is in its table. The provider reads it as bad credentials (502); a sentinel of its own is a contract question |
| Harbor v2.15.4 | The referrers API ignores `n`, and `X-Total-Count` counts before the filter | The referrers API has no `n` and no count |
| GitLab v4.43.0 | `GET /v2/` advertises `Gitlab-Container-Registry-Features: tag_delete`, but `DELETE /v2/<name>/tags/reference/<tag>` is a plain 404 | A GitLab extension, not the spec; the OCI tag delete on `/manifests/<tag>` works. Worth a word to GitLab: the header and its docs promise a route v4.43.0 does not have |
| GitLab v4.43.0, B | The online garbage collector deletes an untagged manifest, a referrer with `subject` among them, 24 h after its push | The spec says nothing about retention. Lajstrom writes the tag schema on GitLab (no referrers API), which survives (#37) |
| Nexus 3.98.1, `docker` format | With `Accept-Encoding: gzip` a blob that looks like text comes back `Content-Encoding: gzip`, chunked, without `Content-Length` ([nexus.md](nexus.md#blob-reads-ranges-sizes)) | HTTP content coding (RFC 9110 §8.4) is the client's to offer; the bytes after decoding have the digest. It cost Lajstrom the `Content-Length` until the provider stopped offering gzip (#119) |
| Nexus 3.98.1, `oci` format | Writes the referrers tag schema itself (`sha256-<subject>`) besides answering the referrers API; with `writePolicy: allow_once` that index keeps the first referrer only ([nexus.md](nexus.md#referrers)) | "Maintaining the content of this tag is the responsibility of clients", which does not forbid a registry to; a client that reads the tag under `allow_once` misses referrers. Lajstrom reads the API there |
| Nexus 3.98.1, `oci` format | `_catalog` pages with an opaque `cursor` in `Link` and ignores `last` (an empty page) ([nexus.md](nexus.md#catalog)) | `_catalog` is not in the spec. The provider follows `Link` |
| Nexus 3.98.1 | Three wrong passwords for a user make Nexus answer that user 429 with `Retry-After`, the right password included, for about 30 s ([nexus.md](nexus.md#what-the-provider-must-do-differently)) | Authentication is out of the spec's purview. Anyone who knows the service account's name can keep Lajstrom out; the provider waits out the `Retry-After` |
| Nexus 3.98.1, `docker` format | The task "Docker - Delete unused manifests and images" deletes an untagged referrer pushed by digest ([nexus.md](nexus.md#garbage-collection)) | Retention is not in the spec; Lajstrom writes the tag schema on this format, whose referrers the task keeps (#37) |
| Gitea 1.27.3 | No `Range` support: a range is 200 with the whole blob, no `Accept-Ranges` ([gitea.md](gitea.md#blob-reads-ranges-sizes)); Forgejo answers 206 | "A registry SHOULD support the `Range` request header"; RFC 9110 lets a server ignore it. A resumed download starts over; worth a word upstream |
| Gitea 1.27.3, Forgejo 17.0.1 | A `PATCH` without `Content-Range` after the first write is 400 `BLOB_UPLOAD_INVALID` "Stream uploads after first write are not allowed" ([gitea.md](gitea.md#uploads)) | 1.1.1 defines a chunk `PATCH` with `Content-Range` only; the streamed `PATCH` is Docker's, and one works. The provider streams a chunk of unknown size into a session that holds bytes, and spools it first with `registries[].uploads.spoolUnknownLength` (gitea.md, item 4; #425) |
| Gitea 1.27.3, Forgejo 17.0.1 | A re-pointed tag, and the delete of a tag, delete the manifest it named when nothing else holds it, with the layers only it used ([gitea.md](gitea.md#immutable-tags)) | Retention is not in the spec. Lajstrom re-points only the tag schema's index, whose old version is garbage |
| Gitea 1.27.3, Forgejo 17.0.1 | Every challenge says `scope="*"`, whatever the request; the token is the user's for 24 h ([gitea.md](gitea.md#owners-tokens-and-create-on-push)) | Token auth is outside the spec. It cost every streamed upload until the provider kept its primed token under the request's own scopes (#120) |

## Configuration

One `registries[]` entry per registry, from the pages; the keys are in
[configuration.md](../configuration.md#registries). `prefix` is optional unless the registry
needs one.

distribution and Zot, on the filesystem or on S3: the probe finds everything their default
configurations do. Two settings of theirs it cannot see: distribution with
`storage.delete.enabled: false` is found out at the first refused delete, which
`deleteManifest: false` saves ([distribution.md](distribution.md#the-probe)); Zot behind an
access-control policy without `update` refuses to re-point a tag, which it needs
`immutableTags: true` for ([zot.md](zot.md#configuration)). Zot redirects blob downloads only on
S3 with `storage.redirectBlobURL: true`; distribution on S3 always does, unless
`storage.redirect.disable`.

```yaml
registries:
  - name: local
    type: generic
    url: http://localhost:5000          # Zot of the dev stack; distribution is :5001, distribution on S3 :5002
    auth:                               # for htpasswd; leave auth out for an anonymous registry
      username: lajstrom
      passwordEnv: LAJSTROM_REGISTRY_PASSWORD
    # capabilities: {blobRedirect: false}   # on S3, when clients cannot reach the object store: Lajstrom streams
    # capabilities: {deleteManifest: false} # distribution with storage.delete.enabled: false
    # capabilities: {immutableTags: true}   # Zot behind a policy without update
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

Nexus Repository ([nexus.md](nexus.md#run-it)), a hosted repository's HTTP connector:

```yaml
registries:
  - name: nexus
    type: generic
    url: http://localhost:5015          # the oci format's connector; the docker format's is :5008
    auth:                               # forceBasicAuth on the repository: Basic on /v2/
      username: lajstrom                # a user with nx-repository-view-<format>-<repository>-*
      passwordEnv: LAJSTROM_NEXUS_PASSWORD
    # The docker format leaves the tags of a manifest deleted by digest (nexus.md, item 2):
    # capabilities: {deleteManifest: false}
    # The oci format answers every Range with 500: pypi uploads fail (nexus.md, item 3).
```

Gitea's or Forgejo's container registry ([gitea.md](gitea.md#run-it)):

```yaml
registries:
  - name: gitea
    type: generic
    url: http://localhost:5009          # Gitea's own address, its ROOT_URL; Forgejo of gitea.sh is :5014
    prefix: <owner>/                    # required: a user or an organisation the user may write packages of
    auth:
      username: <user>
      passwordEnv: LAJSTROM_GITEA_TOKEN # an access token with the scope write:package
```

## Consistency

A registry is **strongly consistent** when every read after the `201` of a write sees it: a
`HEAD` or `GET` of the manifest by digest and by tag, of each blob, the tag list and the
catalog. It is **eventually consistent** when a read can miss the write for a while, its
**lag**; the longest lag known is its **bound**. `provider.Capabilities.Consistency` and
`ConsistencyBound` say which ([configuration.md](../configuration.md#registry-capabilities):
`capabilities.consistency`, `capabilities.consistencyBound`).

| Registry | Model | What is known |
| --- | --- | --- |
| distribution 3.1.2 on the filesystem, on SeaweedFS and on MinIO; Zot v2.1.23 | strong (the vendor table) | the probe, 2026-11-11: every read saw each write at once (below) |
| GitLab A and B | strong (the vendor table) | the probe, 2026-11-11 |
| Amazon ECR | eventual, no bound | 100 ms to several seconds, from reports only ([ecr.md](ecr.md#capabilities)); #19 measures it |
| GHCR | eventual (the vendor table), no bound | users saw a tag `404` 0.2 s after its push, once in 30 runs; reads from another runner fail more often |
| Docker Hub | none seen for manifests and blobs | its referrers list lagged up to about a minute behind a signature (cosign users' reports) |
| Quay | immediate by its code for manifests and blobs | the tag list is cached up to 120 s, the catalog up to 60 s |
| Azure Container Registry | per replica | geo-replicas are eventual, without a published bound; a push and its reads must use one regional endpoint |
| Google Artifact Registry | none seen | a remote repository caches its tags for an hour |

"Strong" for distribution, Zot and GitLab is the vendor table's reading of a measurement of a
few writes, not a guarantee of theirs: the probe saw no lag, so a blob a manifest names and the
registry does not serve is refused at once rather than after `ingest.visibilityTimeout`. A
configuration that adds a lag (a CDN, a cache, a replica) sets `capabilities.consistency:
eventual`. "None seen" elsewhere is reports, not a measurement. The reports behind the rows
without a probe are in #446.

The probe's row "Read-after-write lag" is three writes in the repository `lag` of the scratch
prefix: a new blob, read with `HEAD` until it answers 200; then a manifest that has it as its
layer, under a new tag, read until `HEAD` by digest answers 200, `HEAD` by tag answers its
digest, the tag list names the tag and, when the registry has a catalog Lajstrom uses, the
catalog names the repository. A read that misses is asked again after 25 ms, then twice as long
each time up to 2 s, for at most 2 minutes, past the 120 s of Quay's tag-list cache. The cell
is `none: …` when every read saw every write at the first ask, otherwise each read's worst
time; "Overrides needed" then names `consistency: eventual` and a `consistencyBound` in whole
seconds above the worst.

## Measuring a registry: `lajstrom registry probe`

`lajstrom registry probe` (#339) is the method of the spikes as one run: it measures a registry's
column of [Capabilities](#capabilities), in this page's words, and deletes what it pushed. It
works below a scratch prefix it makes up, `lajstrom-probe-<8 hex digits>/` below the instance
prefix, and refuses one that holds anything (a repository in the catalog, a tag in one of the
repositories it pushes to). It asks the generic provider's own probe first, as `serve` does: the
row "What the probe finds" is that answer, and "Overrides needed" compares it with what the run
measured.

```sh
make registry-matrix                                    # the dev stack's Zot and distribution, one column each
make registry-matrix MATRIX_URLS='http://localhost:5003 http://localhost:5004' MATRIX_ARGS=-v   # GitLab A and B (PROFILES=gitlab)
lajstrom registry probe --config lajstrom.yaml --registry harbor        # a registry of the configuration, its credentials
lajstrom registry probe --url http://localhost:5009 --registry-prefix lajstrom-it/ \
  --username lajstrom --password-file deploy/compose/gitea/.work/gitea.token   # by URL, without a configuration
lajstrom registry probe --url http://localhost:5000 --max-blob-size 1GiB --max-manifest-size 32MiB -o json -v
```

The output is a Markdown table with a column per registry, rows in this table's order (and
"Upload: mount", which the probe added to it), cells in its vocabulary; `-o json` is the same for
tooling, and `-v` adds the requests and the trimmed answers of every cell, as the pages show
them. Credentials, tokens, a session's `_state` and a presigned URL's signature are never
written. How each row is measured, in requests:

| Row | What the probe does |
| --- | --- |
| Create on push, where | in a new repository: an upload session (cancelled), the empty blob, a manifest; after each, whether the repository exists (its tag list, and the catalog for an empty list). A manifest eight levels deep inside it; upload sessions in names of 255 and 256 characters and with a component of 256 bytes. Outside the instance prefix nothing is tried |
| Referrers API, `OCI-Subject` | a subject tagged `v1`, two referrers by digest with artifact types of their own (one of the empty type where the registry refuses unknown ones); `referrers/<subject>`, with `?artifactType=`; whether a `sha256-<subject>` tag appeared |
| Catalog | `_catalog?n=1`; five repositories `pre`, `pre/sub`, `pre-x`, `pre.y`, `prea` and their order; a page of two after the first by `last` and its `Link`; `n=1000`, `n=1001`, `n=100000`; the page without `n` |
| Tag list | one manifest under seven tags pushed out of order (`b`, `A`, `1.10`, `_x`, `1.2`, `Z`, `a`); the list, a page of two by `last`, the list of a repository never pushed |
| Read-after-write lag | three blobs and three manifests, each read back until every read sees it ([Consistency](#consistency)) |
| Deletes | a manifest under two tags deleted by digest, then its tags and the blob only it used; a subject with a referrer; a manifest an image index lists; a tag of a manifest pushed by tag only, and of one pushed by digest first. "Delete a repository": what stays after the cleanup |
| Immutable tags | `A` under `v1`, `A` again, `B` under `v1`, then `A` by digest |
| Blob redirect, `Range` | a 64 KiB blob by `GET` and `HEAD`, redirects not followed; then whole, `bytes=0-9` and a range past its end, after the redirect when there is one |
| Largest blob, chunk size | a blob of `--max-blob-size` (8 MiB) in one `PUT`, and one in one `PATCH` and a closing `PUT`; a refused size is halved down to 1 MiB |
| Manifest size | manifests of 4 MiB, 4 MiB + 1 byte, then doubling from 8 MiB up to `--max-manifest-size` (4 MiB + 1 byte), until one is refused |
| Profile | a raw manifest of [the profile](../spec/manifest-profile.md), a file layer and a metadata layer, by tag; read back |
| Uploads | `PATCH` `0-2`, the session's `GET`, a chunk at `7-7` out of order; a cancel, then the session's `GET` and a `PATCH`; a mount into another repository; a monolithic `PUT` with the digest of other bytes, then the session and that digest; the empty blob; a session started by one generic provider and continued by another (a chunk that cannot be rewound) |

**Not measured by the probe**, printed so: "A listing API of its own" (each vendor's REST API),
"Auth" (the flavours of a registry are set up, not found), "Rate limits seen" (unless an answer
was 429) and the garbage collector (#37: it takes hours or a task run by an administrator). Nor
can a run see another configuration of the same registry: S3 storage, deletes disabled, an
access-control policy, a quota, GitLab's rules in its tokens are a run each against that
configuration. A cell the run could not measure says why (`*not measured*: …`).

**What it leaves.** It deletes its manifests, referrers first, then its blobs; what the
registry refuses to delete is listed after the table ("Left behind"). The distribution API has
no repository delete: distribution, GitLab A and Zot (until its collector runs) keep the emptied
repositories in `_catalog`, which is why every run takes a new scratch prefix. A registry on a
filesystem keeps a deleted blob's bytes until its garbage collector runs: a large
`--max-blob-size` costs twice that on its disk (the `PUT` and the `PATCH`), and the dev stack's
registries share the machine's disk (a run with 2 GiB against GitLab A and B filled it once).

**What it cannot probe.** A registry whose bearer challenge names a token realm on another host
than the registry's (Docker Hub, GitLab.com, `registry.gitlab.com`): the generic provider refuses
to send its credentials there since #354, until `registries[].tokenRealmHosts` exists, and the
probe stops with that error. A registry of type `ecr` (its credentials come from the AWS SDK; the
probe is for type `generic`).

### Adding a registry

1. Start it, with a user or token that may push and delete below a prefix of its own, and run
   the probe with `-v` (the instance prefix with `--registry-prefix` or `prefix:`); keep the
   output.
2. Write its page, as [gitlab.md](gitlab.md): the version, how to run it, the probe's output,
   and the cells the probe does not measure, by hand.
3. Add a row to [Versions tested](#versions-tested), the probe's cells as a column of
   [Capabilities](#capabilities) (each linked to the page), its
   [deviations](#deviations-from-the-specification), and its `registries[]` entry to
   [Configuration](#configuration).
4. A cell that disagrees with what the integration tests found against it is a probe bug or a
   finding: say which on the page.

## Keeping the matrix current

The versions are pinned in four places: `deploy/compose/compose.yaml` (distribution, Zot, the
GitLab container registry and its Postgres, SeaweedFS, MinIO), `deploy/compose/harbor/harbor.sh`
(Harbor's version and the installer's sha256), `deploy/compose/nexus/compose.yaml` (Nexus's
image by digest), `deploy/compose/gitea/compose.yaml` (Gitea's and Forgejo's images by digest)
and `.github/workflows/ci.yml` (distribution and
Zot of the integration job and of the e2e job, the same as in `compose.yaml`). **Whoever changes a pin re-runs the
commands of that registry's page, updates the cells that changed and the "last verified" date,
and says so in the PR.** GHCR has no pin: it is re-measured when something on its page stops
being true, or quarterly. A registry that joins gets a page like [gitlab.md](gitlab.md), a row in
[Versions tested](#versions-tested), a column in [Capabilities](#capabilities), and its
deviations: [Adding a registry](#adding-a-registry). The first step of every re-measurement is
`lajstrom registry probe` against the registry (`make registry-matrix` for the dev stack's),
compared cell by cell with its column; the commands of the page are for what the probe does
not measure and for the configurations it is not run against.

The commands per registry:

- [ ] **distribution and Zot** (`compose.yaml` and `ci.yml`, together): the integration job runs
      on the pull request; by hand, [distribution.md, Commands](distribution.md#commands) and
      [zot.md, Commands](zot.md#commands): the curl probes, the suite with and without the
      prefix, distribution with deletes disabled and with htpasswd, Zot with htpasswd and a
      policy without `update`, Zot on S3.
- [ ] **distribution on S3, SeaweedFS, MinIO** (`compose.yaml`, profiles `s3` and `s3-minio`):
      `make dev-up PROFILES=s3,s3-minio`, then the commands of
      [dev-stack.md](../dev-stack.md#the-s3-profile) against both stores:
      `TestIntegrationS3Redirect`, `TestIntegration`, `TestIntegrationUploads`, `TestAptGetInstall`;
      and the redirects of [distribution.md](distribution.md#blob-redirect).
- [ ] **GitLab container registry** (`compose.yaml`, profile `gitlab`; a new CNG build of the same
      tag is a new digest): [gitlab.md, Commands](gitlab.md#commands): the curl probes, the tests
      on A and B with and without dynamic media types and the prefix, the media-type table, the
      garbage collection.
- [ ] **Harbor** (`harbor.sh`): [harbor.md, Commands](harbor.md#commands): the project and the
      robot, the curl probes, the tests with the prefix, without it and with the immutability
      rule, the quota, the garbage collection, the conformance suite.
- [ ] **Nexus Repository** (`nexus/compose.yaml`; a new image is a new digest):
      [nexus.md, Commands](nexus.md#commands): `nexus.sh up`, the curl probes on both
      connectors, the tests and the e2e scenarios against each format, `allow_once`, the
      garbage collection tasks; then `nexus.sh down`.
- [ ] **Gitea and Forgejo** (`gitea/compose.yaml`; a new image is a new digest):
      [gitea.md, Commands](gitea.md#commands): `gitea.sh up` and `gitea.sh up forgejo`, the curl
      probes, the tests and the e2e scenarios with the prefix, the cleanup; then
      `gitea.sh down`.
- [ ] **GHCR** (no pin; quarterly, or when the page stops being true):
      [ghcr.md, Measurements](ghcr.md#measurements) and [Our tests](ghcr.md#our-tests) in
      `lajstrom-extras`, then the deletion of the packages it leaves.
- [ ] **Every registry**: `lajstrom registry probe` against it, cell by cell against its
      column (`make registry-matrix` for distribution and Zot); then the `registry capabilities`
      line of the test log or of `serve` (`vendor=… createOnPush=… referrers=… catalog=…`)
      against the rows "What the probe finds" and "Overrides needed", and each row of
      [Deviations](#deviations-from-the-specification) against the new version.

## Notes

What holds for every registry, and is on no page alone.

### The integration test

`TestIntegration` (`internal/provider/generic/integration_test.go`) pushes a blob, pushes a
manifest with a tag in one `PUT` (`artifactType` `application/vnd.lajstrom.test.v1`, the empty
config, a layer), resolves, fetches, checks existence, lists tags, attaches a referrer (a
manifest with `subject`) and lists it, `OpenBlob` (the first answer of one `GET`: a redirect to
follow, or the bytes; with redirect off, the bytes; a range, 206; a range past the end, 416),
`StatBlob`, lists repositories under a random prefix, and deletes the referrer and the manifest.
Each run works in `raw/it-<random>/…`, below the instance prefix when
`LAJSTROM_TEST_REGISTRY_PREFIX` sets one. The CI job runs it with the rest of the integration
suite (ingest, the raw and apt frontends, the facade, `TestIntegrationUploads`) on Zot and on
distribution, so Lajstrom's manifest profile is pushed to both on every pull request.
`LAJSTROM_TEST_REGISTRY_TLS=1` switches to HTTPS; the test does not clean up what the backend
cannot delete. The runs by hand are on the pages: [distribution.md](distribution.md#our-tests-against-it),
[zot.md](zot.md#our-tests-against-it), [gitlab.md](gitlab.md#our-tests-against-it),
[ghcr.md](ghcr.md#our-tests), [harbor.md](harbor.md#our-tests-against-it),
[nexus.md](nexus.md#our-tests-against-it).
[gitea.md](gitea.md#our-tests-against-it).

### Upload sessions

`TestIntegrationUploads` (#24): a session continued by a second provider and its status, a chunk
of unknown size, two more spooled (`uploads.spoolUnknownLength`, #425), the monolithic `PUT`, the empty blob, a wrong digest refused as a digest
mismatch, a chunk out of order refused, the cancel and the 404 of the session after it, and the
mount. It passes on distribution and Zot on the filesystem, in CI on every pull request. On S3
both fail at one step, the cancel of a session after a refused digest: distribution answers 500,
on SeaweedFS and MinIO alike ([distribution.md](distribution.md#uploads)), and Zot on MinIO 503
([zot.md](zot.md#uploads)). The cause is the `s3` driver of distribution, which Zot uses too, not
the store ([Deviations](#deviations-from-the-specification)). Through Lajstrom the facade cancels
the backend session itself after the refused digest, logs the error
(`facade: cancel backend upload session`), and the client's `DELETE` is 404
`BLOB_UPLOAD_UNKNOWN`.

### Authentication

A registry with htpasswd answers the probe without credentials with 401 and a Basic challenge;
`Capabilities` then fails with `provider.ErrUnauthorized` (no credentials configured), which
`serve` logs at startup (distribution and Zot: [distribution.md](distribution.md#authentication),
[zot.md](zot.md#our-tests-against-it)). Bearer tokens are covered by unit tests against a fake
token endpoint, and against real token services by GitLab B with a test token server
([gitlab.md](gitlab.md#bearer-tokens)) and by Harbor ([harbor.md](harbor.md#projects-robots-and-create-on-push)).

### The other registries

Harbor runs from its installer as a compose project of its own on `localhost:5005`, beside the
dev stack and not part of it (`deploy/compose/harbor/harbor.sh up` and `down`); the GitLab
container registry is the `gitlab` profile of the dev stack on `localhost:5003` (A) and
`localhost:5004` (B). Nexus is a compose project of its own, `lajstrom-nexus`, on
`localhost:8081` (UI and REST API), `localhost:5008` and `localhost:5015` (the connectors),
set up by `deploy/compose/nexus/nexus.sh up`. Gitea and Forgejo are a compose project of their
own, `lajstrom-gitea`, on `localhost:5009` and `localhost:5014`, set up by
`deploy/compose/gitea/gitea.sh up` (`up forgejo`). The packages the GHCR tests left in `lajstrom-extras`, and their deletion,
are in [ghcr.md](ghcr.md#packages-left-in-lajstrom-extras). The ECR provider's stand-in, moto
for the API and a distribution for `/v2`, is the `ecr` profile on `localhost:5010` and
`localhost:5011` ([ecr.md](ecr.md#tested-against-moto)).
