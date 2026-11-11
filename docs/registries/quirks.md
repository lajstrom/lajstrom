# How OCI registries differ

Lajstrom keeps every package in an OCI registry, and the distribution specification makes the
protocol the same everywhere. It does not make the rest the same: what a repository is, which
names a registry takes, which optional parts of the protocol it serves, and what it does to tags
afterwards. This page collects those differences for thirteen registries, with a source for each
claim, and ends with what they mean for Lajstrom. The [matrix](README.md) holds the registries
Lajstrom was run against, cell by cell; this page leans on it and on the vendors' documentation.

How to read it. **Measured**: run by Lajstrom, with the page or the date. A link to a vendor's
documentation or source code: documented. A link to an issue or to a third party: reported. A
trailing **?**: nobody here verified it. The research is of 2026-11-11, and so are the
measurements of [section 2](#2-is-abc-one-repository-or-two), on Zot v2.1.23, distribution
3.1.2 and Harbor v2.15.4. Harbor's other cells come from [harbor.md](harbor.md) (v2.15.4,
2026-11-07) and from its sources.

## Summary

### Names and repositories

| Registry | Repository model | Must exist before the first push | Name limits | `a/b` beside `a/b/c`; API words in names |
| --- | --- | --- | --- | --- |
| Amazon ECR | names with `/`; each repository a resource of its own (policy, lifecycle, scanning, quotas) | the repository: `CreateRepository`, or a creation template with `CREATE_ON_PUSH` for its prefix (since 2025-12) | 2–256 characters; 100,000 repositories a region, 100,000 images a repository, 1,000 tags an image | not tried |
| Azure ACR | names with `/`, "managed independently, not as a hierarchy" | nothing; the last manifest's delete removes the repository | no count limit, a storage limit per SKU | not tried |
| Google Artifact Registry | `PROJECT/REPOSITORY/IMAGE`: one GAR repository holds many images | the GAR repository (30 creations a minute a region) | not documented ? | not tried |
| GitLab | `<namespace>/<project>` and **at most two levels more**; the project path is an image too | the project | 255 characters | yes, below a project; deeper, the token has no scope |
| Harbor | `<project>/` and any depth | the project, never created on push | project 1–255 characters; the whole name 255 | any depth, all served, listed and collected, measured; **below a component `manifests`, `referrers` or `tags/list`, a blob `HEAD` is 404 or 400 at random** (GET 200), measured |
| GHCR | `<owner>/` and a name, which may hold `/` | the owner | lowercase | not tried |
| Docker Hub | `namespace/repository`: **exactly two levels** | the namespace | repository 2–255 characters, no `.` | no third level at all |
| Quay | `namespace/repository`; the repository may hold `/` (quay.io too) | the namespace | namespace 2–255, repository 255 characters | new names ending in `build` or `build/<x>` refused (master, 2026-11) |
| Zot | any name, a directory each | nothing: the first upload session creates it | a component over 255 bytes is 500 | **names ending in `blobs`, `.uploads`, `.sync` are served but neither listed nor collected**, measured |
| distribution | any name | nothing: the first manifest | 255 characters, 400 `NAME_INVALID` | all served and listed, measured |
| Gitea, Forgejo | `<owner>/` and any depth | the owner | **the 1.0 grammar: no `__`, no `--`** | `a/b/c` and deeper, measured |
| Nexus | any name inside one hosted repository, on its connector or path | the Nexus repository | | any depth, measured |
| JFrog Artifactory, JCR | `<repo-key>/` and any depth, stored as folders | the repository key | key at most 63 characters | image `a/b` with tag `c` and image `a/b/c` are one folder ? |

### Protocol and lifecycle

| Registry | Uploads | Deletes through `/v2` | Immutable tags | Referrers API | `_catalog` | Largest blob | Auth | Rules that read tags |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Amazon ECR | `OCI-Chunk-Min-Length` 10 MiB; a part's minimum on `/v2` unverified | by digest | per repository, with exclusions | yes; oras's referrer push 405 | yes, paged by a token | 52,000 MiB; 200 GB by Docker push since 2026-08 | Basic, a 12-hour password | lifecycle policies |
| Azure ACR | chunked and monolithic | by digest only | locks per repository, tag, digest | yes, not with customer-managed keys | not for scope-map tokens | 195 GiB | OAuth exchange, about 3 h | `acr purge`, retention |
| Google Artifact Registry | **monolithic only** | ? | per repository; tagged images undeletable | yes | ? | 5 TB an artifact | OAuth token, 60 min | cleanup policies |
| GitLab | any | yes; **none under any protection rule** | GitLab's rules | no | GitLab admins | none documented | Bearer from GitLab, 5 min | cleanup policies; untagged gone in 24 h |
| Harbor | out-of-order chunks appended | by digest; no tag delete | project rules | yes | system admins | 128 GiB a request | Bearer, 30 min | retention rules |
| GHCR | 4 MiB a `PATCH` | **none** | none | no | all of GHCR | 10 GB, 10 min an upload | the PAT in base64 | none |
| Docker Hub | ? | by digest only | per repository, RE2 patterns | undocumented; 200 with an empty index | none | none documented | Bearer from `auth.docker.io`, 300 s | none documented |
| Quay | ? | yes | 3.17, organisations | yes | the caller's view, cached 60 s | 20 GB by default | Bearer, 1 h, no `delete` | auto-prune, expiry labels |
| Zot | any | yes; 405 if an index lists it | none; a policy without `update` | yes | yes, but see above | none met at 1 GiB | none, htpasswd, more | retention policies |
| distribution | any | yes, if enabled | none | no | yes | the storage driver's | none, htpasswd, token | none (offline collector) |
| Gitea, Forgejo | **no chunk of unknown length after the first** | yes; a re-pointed tag deletes its manifest | none | no | yes | `LIMIT_SIZE_CONTAINER` | Bearer, `scope="*"`, 24 h | cleanup rules |
| Nexus | any | `docker` format: **its tags stay** | `writePolicy: allow_once` | `oci` format only | yes | none set | Basic | cleanup policies |
| JFrog | **no chunk of unknown length after the first** | yes | the overwrite permission | yes | per repository key | upload limit on SaaS | Bearer or Basic | cleanup policies, Max Unique Tags |

Each cell is said again below, or in the [matrix](README.md#capabilities), with its source.

## 1. Repositories and names

### What the specification allows

A `<name>` "MUST match" `[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(\/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*`:
lowercase components separated by `/`, each starting and ending with a letter or a digit
([distribution-spec 1.1.1](https://github.com/opencontainers/distribution-spec/blob/v1.1.1/spec.md#pulling-manifests);
1.0.1 had no `__` and no `--`). Its only length rule is an implementers' note: "many clients
impose a limit of 255 characters" on the registry host, `/` and the name together, and
"registries should avoid" longer names ([#530](https://github.com/opencontainers/distribution-spec/issues/530));
distribution's reference parser counts the name without the host
([reference.go](https://github.com/distribution/reference/blob/0965666a6ade2e06035fe352e38344be1e68951a/reference.go#L60-L67)).
A tag is at most 128 characters of `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`, case included. The
specification reserves no word in a name; its own namespaces, `_catalog`, `_oci` and `_<ext>`,
start with `_`, which no component can ([extensions](https://github.com/opencontainers/distribution-spec/blob/main/extensions/README.md#reserved-namespaces)).
It says nothing about depth, about who may create a repository, or about what one costs.

### Three repository models

- **The name is a key.** distribution, Zot, ACR, and Nexus inside one hosted repository: any
  name of the grammar, at any depth, created by the first push. ACR says it outright: "the
  registry manages all repositories independently, not as a hierarchy"
  ([concepts](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-concepts)).
- **An owner first, then a path.** The first components name something of the registry's own
  model, which must exist and carries the permissions, quotas and policies: a GitLab project, a
  Harbor project, a GHCR or Gitea owner, a JFrog repository key, a GAR repository, a Quay
  namespace. Below it, any depth, but on GitLab.
- **A fixed depth.** Docker Hub "does not support multi-level repository names"
  ([hub-feedback#1404](https://github.com/docker/hub-feedback/issues/1404),
  [jib#2650](https://github.com/GoogleContainerTools/jib/issues/2650#issuecomment-667343375)).
  GitLab allows the project path and two levels more.

ECR is the first model on the wire and an expensive object behind it: every repository has its
ARN, policy, lifecycle policy, scanning and encryption, and a share of the quotas, and until
December 2025 nothing created one on push.

### Per registry

- **ECR.** 2–256 characters of the grammar ([CreateRepository](https://docs.aws.amazon.com/AmazonECR/latest/APIReference/API_CreateRepository.html)).
  A push to a missing repository fails unless a [creation template](https://docs.aws.amazon.com/AmazonECR/latest/userguide/repository-creation-templates.html)
  with `CREATE_ON_PUSH` matches: the longest prefix wins, a prefix ends with an implied `/`,
  `ROOT` matches the rest ([since 2025-12](https://aws.amazon.com/about-aws/whats-new/2025/12/amazon-ecr-creating-repositories-on-push)).
  [Quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html): 100,000
  repositories a region and 100,000 images a repository (adjustable), 1,000 tags an image
  (fixed); a referrer "counts as an image consumed for your repository"
  ([images](https://docs.aws.amazon.com/AmazonECR/latest/userguide/images.html)).
- **ACR.** Lowercase letters, digits, `.`, `-`, `_` and `/`; no length documented ?. "As many
  repositories, images, layers, or tags as you need", though "high numbers of repositories and
  tags can affect the performance" ([storage](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-storage)).
  A repository goes with its last manifest ([retention](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-retention-policy)).
- **Google Artifact Registry.** `LOCATION-docker.pkg.dev/PROJECT/REPOSITORY/IMAGE`; pushing
  "requires creating the repository as a separate step"
  ([troubleshooting](https://docs.cloud.google.com/artifact-registry/docs/docker/troubleshoot)),
  at most 30 creations or deletions a minute in a region
  ([quotas](https://docs.cloud.google.com/artifact-registry/quotas)); images below it, nested
  ones too, come with their push ?. Only the `gcr.io` domain repositories create on push
  ([gcr.io repositories](https://docs.cloud.google.com/artifact-registry/docs/transition/manual-gcr-repositories)).
- **GitLab.** `<registry>/<namespace>/<project>[/<optional path>]`, two levels at most below the
  project, whose own path is an image too ([naming convention](https://docs.gitlab.com/user/packages/container_registry/#naming-convention-for-your-container-images)).
  Rails looks for the project among the three longest prefixes of the name
  ([`path.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/master/lib/container_registry/path.rb),
  `LEVELS_SUPPORTED = 3`); a deeper name gets a token without its scope, and the client reads
  `insufficient_scope` ([auth service](https://gitlab.com/gitlab-org/gitlab/-/blob/master/app/services/auth/container_registry_authentication_service.rb);
  more levels: [gitlab#438563](https://gitlab.com/gitlab-org/gitlab/-/issues/438563), in the
  backlog since 2024). The registry alone takes any depth (measured, [gitlab.md](gitlab.md#lajstrom-registry-probe)).
  Image paths follow the project's: a project with images can be renamed on GitLab.com since
  16.7, with at most 1,000 of them, and on a self-managed GitLab only once they are deleted
  ([moving and renaming](https://docs.gitlab.com/user/packages/container_registry/#move-or-rename-container-registry-repositories)).
- **Harbor.** A project of 1–255 characters of `[a-z0-9]+(?:[._-][a-z0-9]+)*`
  ([manager.go](https://github.com/goharbor/harbor/blob/main/src/pkg/project/manager.go)),
  never created on push ([projects](https://goharbor.io/docs/main/working-with-projects/create-projects/));
  a push into a missing one is 401 "project … not found" (measured, [harbor.md](harbor.md#projects-robots-and-create-on-push);
  [harbor#17097](https://github.com/goharbor/harbor/issues/17097)). Any depth below it
  (measured: twelve levels); the whole name is a column of 255 characters
  ([schema](https://github.com/goharbor/harbor/blob/main/make/migrations/postgresql/0001_initial_schema.up.sql)).
- **GHCR.** `ghcr.io/<owner>/<name>`, the name nested in GitHub's own example
  ([docs](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry));
  the package appears, private, with its first manifest, and a repository of blobs only does
  not become one (measured, [ghcr.md](ghcr.md#create-on-push)).
- **Docker Hub.** A repository is 2–255 characters, "only lowercase letters, numbers, hyphens,
  and underscores", and cannot be renamed ([create](https://docs.docker.com/docker-hub/repos/create/));
  the namespace is a user of 4–30 characters or an organisation; a name without one is
  `library/` ([docker image tag](https://docs.docker.com/reference/cli/docker/image/tag/)).
- **Quay.** A namespace, a user or an organisation, of 2–255 characters
  ([validation.py](https://github.com/quay/quay/blob/master/util/validation.py)), and a
  repository that may hold `/` with `FEATURE_EXTENDED_REPOSITORY_NAMES`, on by default and on
  quay.io ([names.py](https://github.com/quay/quay/blob/master/util/names.py),
  [quay.io/config](https://quay.io/config)), at most 255 characters without the namespace. A
  private repository is created on push, a namespace is not
  ([config.py](https://github.com/quay/quay/blob/master/config.py)).
- **Zot, distribution.** Any name of the grammar, created by the first upload session (Zot) or
  the first manifest (distribution); a name over 255 characters is 400 `NAME_INVALID` on
  distribution, a component over 255 bytes 500 on Zot (measured, [distribution.md](distribution.md#create-on-push),
  [zot.md](zot.md#create-on-push)).
- **Gitea, Forgejo.** `<owner>/<image>`, the image at any depth; another owner is 401, a missing
  one 404 (measured, [gitea.md](gitea.md#owners-tokens-and-create-on-push)). The name pattern
  is the specification's 1.0 grammar, `[._-]` between alphanumerics, so `a__b` and `a--b` are
  refused ([Gitea](https://github.com/go-gitea/gitea/blob/a61ef7aa1426a6207b970d88bff13ed1b0302360/routers/api/packages/container/container.go#L48),
  [Forgejo](https://codeberg.org/forgejo/forgejo/src/commit/d72fc0095164974d35318ca477927057e6c549d1/routers/api/packages/container/container.go#L32)).
- **Nexus.** A `docker` or `oci` hosted repository answers `/v2/` on a connector port, on a
  subdomain (Pro), or since 3.83.0 on a path, `host/<repository>/<image>`
  ([routing](https://help.sonatype.com/en/ssl-and-repository-connector-configuration.html));
  inside it, any name at any depth (measured, [nexus.md](nexus.md#connectors-and-create-on-push)).
- **JFrog.** The first component is the repository key, which must exist (404 "Repository 'raw'
  not found"); below it, any depth; an image is a folder, each tag a folder in it with a
  `manifest.json` (measured, [jfrog-jcr.md](jfrog-jcr.md#the-repository-model-and-lajstroms-prefix);
  [docs](https://docs.jfrog.com/artifactory/docs/additional-docker-information)). A key is at
  most 63 characters, without `_` ([Docker repositories](https://docs.jfrog.com/artifactory/docs/docker-repositories)).

### Case and length

Repository names are lowercase everywhere. An uppercase component is a plain-text 404 on Zot and
on distribution, whose router patterns do not match, rather than `NAME_INVALID` (measured below);
distribution takes an uppercase *first* component, which its pattern reads as a host
([distribution#2771](https://github.com/distribution/distribution/issues/2771)). Tags keep their
case everywhere but on **Gitea and Forgejo, which lower-case a tag on push and resolve it
without case**: `5.4.Final` is listed as `5.4.final`, and a push to `5.4.final` re-points it
(measured, [gitea.md](gitea.md#tags); #426). A tag of 129 characters is 400 `MANIFEST_INVALID` on
Zot and a plain-text 404 on distribution (measured below); ECR's API takes tags of up to 300
characters, its exclusion filters 128 ([ImageIdentifier](https://docs.aws.amazon.com/AmazonECR/latest/APIReference/API_ImageIdentifier.html)).
Over 255 characters GitLab's registry answers 500 `UNKNOWN` (measured, [gitlab.md](gitlab.md#lajstrom-registry-probe)).

## 2. Is `a/b/c` one repository or two?

Two questions: can `a/b` and `a/b/c` both be repositories, and can a name contain a word the
API uses in its paths (`manifests`, `blobs`, `tags`, `uploads`, `referrers`)?

**The protocol is unambiguous.** Every endpoint is `/v2/<name>/` and a fixed suffix:
`manifests/<reference>`, `blobs/<digest>`, `blobs/uploads/`, `blobs/uploads/<id>`, `tags/list`,
`referrers/<digest>` ([endpoints](https://github.com/opencontainers/distribution-spec/blob/v1.1.1/spec.md#endpoints)).
A tag, a digest and an upload id hold no `/`, so a router that matches the suffix at the end of
the path and takes the rest as the name finds one reading: `/v2/x/manifests/y/manifests/v1` is
the tag `v1` of `x/manifests/y`, and `/v2/x/tags/list/tags/list` the tag list of `x/tags/list`.
distribution ([descriptors.go](https://github.com/distribution/distribution/blob/7ef76eb0e8331961ee1eaa7c70e809fe406a63ca/registry/api/v2/descriptors.go#L434-L1541)),
Zot ([routes.go](https://github.com/project-zot/zot/blob/6a2e68c975a126ca90960cd0663628b69ebf7e59/pkg/api/routes.go#L152-L201)),
Gitea and Forgejo route so (read), and so does Lajstrom's facade, which takes the fixed segments from
the end ([route.go](../../internal/facade/route.go); `TestParseRoute` has
`oci/images/blobs/blobs/uploads/0f`). What goes wrong is elsewhere: a storage layout that keeps a
repository's files in the directory of its children, a second reading of the path behind the
router that matches the first pattern it tries (Harbor, below), a rule that picks repositories by
a prefix, and a product whose own URLs share the name space.

### Measured on Zot and distribution

On 2026-11-11, the dev stack's Zot v2.1.23 (`localhost:5000`) and distribution 3.1.2
(`localhost:5001`), both on the filesystem and anonymous. For each name below `quirks/`: the
empty config and a layer whose bytes name the repository, a manifest whose layer title is the
name under the tag `v1`, then the manifest, the tag list and the layer read back:

```sh
R=http://localhost:5000; n=quirks/x/manifests/y
loc=$(curl -si -X POST "$R/v2/$n/blobs/uploads/" | tr -d '\r' | awk -F': ' 'tolower($1)=="location"{print $2}')
curl -X PUT --data-binary '{}' "$R$loc?digest=sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"  # & after distribution's ?_state=
# the same for the layer, then a manifest: artifactType, the empty config, the layer with its title
curl -X PUT -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' --data-binary @m.json "$R/v2/$n/manifests/v1"
curl -H 'Accept: application/vnd.oci.image.manifest.v1+json' "$R/v2/$n/manifests/v1" | jq '.layers[0].annotations'
curl "$R/v2/$n/tags/list"; curl -o /dev/null -w '%{http_code}\n' "$R/v2/$n/blobs/<layer digest>"
curl "$R/v2/_catalog?n=100000" | jq '[.repositories[] | select(startswith("quirks/"))]'
```

| Name below `quirks/` | Zot: pushed, read back | Zot: in `_catalog` | distribution: pushed, read back | distribution: in `_catalog` |
| --- | --- | --- | --- | --- |
| `a/b/c`, then `a/b`, then `a` | 201 each; three manifests, each its own layer | yes | the same | yes |
| `x/manifests/y`, `x/tags/list`, `x/referrers/y`, `x/manifests`, `x/tags`, `x/uploads`, `v2/x` | 201; `GET /v2/quirks/x/tags/list/tags/list` 200 `{"name":"quirks/x/tags/list","tags":["v1"]}` | yes | the same | yes |
| `x/blobs`, `x/blobs/y`, `x/blobs/uploads` | 201, read back | **no** | 201, read back | yes |
| `myblobs`, `myblobs/child`, `my.uploads`, `my.sync` | 201, read back | **no** | not tried | |
| `x`, after `x/blobs` | 201, read back | yes | not tried | |
| `x/_catalog`, `Upper` | the `POST` 404 `404 page not found`, plain text | | the same | |

Tags the same day, on `quirks/a`: 128 characters 201 on both, 129 characters 400
`MANIFEST_INVALID` on Zot and the plain-text 404 on distribution; `.hidden` and `-dash` the same;
`Mixed.Case` and `mixed.case` are two tags on both; `_j00000007` and `sha256-<64 hex>.sig` 201.
Afterwards every manifest and blob was deleted (202). distribution keeps the emptied repositories
in `_catalog`, having no repository delete; Zot drops them at a collection, but for those its
collector never visits (below), whose empty directories stay.

`lajstrom registry probe` asks every registry what Harbor gets wrong below (its row "Routing
words in a name", [README](README.md#routing-words)). On 2026-11-11, on both, an image in
`route/x`, `route/x/manifests/y`, `route/x/manifests`, `route/x/referrers/y` and
`route/x/tags/list`, each layer 20 times by `HEAD` and once by `GET`: 200 every time, `ok`.

### Zot's catalog and collector skip names that end in `blobs`, `.uploads` or `.sync`

Zot keeps each repository as an OCI image layout in a directory of its name (`index.json`,
`oci-layout`, `blobs/sha256/…`, `.uploads/`), and a nested repository as a subdirectory. Its
catalog walks that tree and skips, with everything below it, every directory whose **path ends
in** `blobs` (the layout's blob directory), `.uploads` (upload sessions) or `.sync` (the sync
extension): `strings.HasSuffix` on the path, not a comparison of the last component
([`GetRepositories`](https://github.com/project-zot/zot/blob/v2.1.23/pkg/storage/imagestore/imagestore.go#L547-L552)).
`GetNextRepository`, which hands the collector its repositories, has the same test
([L672-L676](https://github.com/project-zot/zot/blob/v2.1.23/pkg/storage/imagestore/imagestore.go#L672-L676)):
on the private Zot of the next section, every collector task named `quirks/x`, none
`quirks/x/blobs`.
Such a repository is served but not listed and never collected: npm's `@netlify/blobs`
(`npm/<repo>/netlify/blobs` in Lajstrom), the pypi package `blobs`, a Maven group ending in
`.sync`, every raw file below a directory `blobs/`. Reported as
[zot#4694](https://github.com/project-zot/zot/issues/4694).

### Zot: `<x>/blobs` lives in `<x>`'s blob directory

`quirks/x/blobs` keeps its `index.json` and `oci-layout` in `quirks/x/blobs/`, beside
`quirks/x/blobs/sha256/`, where `quirks/x` keeps its blobs. Both serve. One level down, a
repository `quirks/x/blobs/sha256` puts its `index.json`, `oci-layout`, `blobs/` and `.uploads/`
among `quirks/x`'s blobs ([zot#4695](https://github.com/project-zot/zot/issues/4695)). On a
private Zot of the same image (`gcDelay: 30s`, `gcInterval: 1m`;
not the shared one), with the three pushed, every collection of `quirks/x` failed:

```text
{"level":"error","message":"failed to parse digest","error":"invalid checksum digest length","repository":"quirks/x","digest":"sha256:.uploads"}
{"level":"info","message":"gc unsuccessfully completed for /var/lib/registry/quirks/x"}
```

Nothing was deleted, and nothing in `quirks/x` is collected while the other exists: the collector
stops at the first entry of `blobs/sha256/` that is not a digest
([gc.go](https://github.com/project-zot/zot/blob/v2.1.23/pkg/storage/gc/gc.go#L1287-L1293)).
The layout's files clash the same way (measured the same day, on the dev stack's Zot, below
`names/probe/`): with `x` pushed, a blob upload to `x/index.json` or `x/oci-layout` is 404 and
its manifest 500; with `y/index.json` pushed first, the manifest of `y` is 500, and `y` stays in
the catalog without one.
distribution keeps a repository below `docker/registry/v2/repositories/<name>/` and its own data
in `_manifests`, `_layers` and `_uploads`, names no component can have
([paths.go](https://github.com/distribution/distribution/blob/7ef76eb0e8331961ee1eaa7c70e809fe406a63ca/registry/storage/paths.go#L20-L60));
its catalog skips those and descends into the rest, and listed every name above.

### Measured on Harbor

On 2026-11-11, Harbor v2.15.4 (`v2.15.4-d8841742`, its registry distribution
`v2.8.3-55-gac7c4ced`) from [harbor.sh](../../deploy/compose/harbor/harbor.sh), as a compose
project of its own, on the filesystem, with core's `GC_TIME_WINDOW_HOURS` at 0
(`LAJSTROM_HARBOR_GC_TIME_WINDOW_HOURS=0`). A name is
`<project>/<repository>`: the names of the Zot reports below the project `demo`, those of the
table above below `quirks`. The same pushes under the tag `1`, as the admin with Basic auth (`/v2/`
challenges with Bearer, but core takes Basic, and `_catalog` only Basic: an admin's token with
`registry:catalog:*` is 401); in each repository one more blob that no manifest names; then a
manual collection, and each blob read again:

```sh
A=admin:Harbor12345; R=http://localhost:5005
curl -u $A -H 'Content-Type: application/json' -d '{"project_name":"demo"}' "$R/api/v2.0/projects"
curl -u $A "$R/v2/_catalog?n=100000"; curl -u $A "$R/api/v2.0/projects/demo/repositories?page_size=100"
curl -u $A "$R/api/v2.0/projects/demo/repositories/x%252Fblobs%252Fsha256/artifacts?with_tag=true"
curl -u $A -H 'Content-Type: application/json' "$R/api/v2.0/system/gc/schedule" \
  -d '{"schedule":{"type":"Manual"},"parameters":{"delete_untagged":true,"dry_run":false,"workers":1}}'
curl -u $A "$R/api/v2.0/system/gc?page_size=1&sort=-creation_time"; curl -u $A "$R/api/v2.0/system/gc/<id>/log"
```

| Name | Pushed, read back | `_catalog`, REST API | Its layer: `HEAD`, `GET` | The orphan after the collection |
| --- | --- | --- | --- | --- |
| `demo/`: `plain`, `netlify/blobs`, `myblobs`, `my.uploads`, `my.sync`, `x`, `x/blobs`, `x/blobs/sha256` | 201, 200 | listed by both; the REST API shows the tag | 200, 200 | 404, collected; the image kept |
| `quirks/`: `a/b/c`, `a/b`, `a`, `x/tags`, `x/uploads`, `v2/x`, `x/blobs`, `x/blobs/y`, `x/blobs/uploads`, `myblobs`, `myblobs/child`, `my.uploads`, `my.sync`, `x` | the same | the same | 200, 200 | the same |
| `quirks/x/manifests/y`, `quirks/x/manifests`, `quirks/x/referrers/y` | the same | the same | **200 or 404 at random**: 404 in 13, 14 and 3 of 20; `GET` 200 | the same |
| `quirks/x/tags/list` | the same; `GET /v2/quirks/x/tags/list/tags/list` 200 `{"name":"quirks/x/tags/list","tags":["1"]}` | the same | **200 or 400 at random**: 400 in 17 of 20; `GET` 200 | the same |

Neither Zot report holds for Harbor. Its catalog is a query of its database, the repositories
that hold an artifact ([catalog.go](https://github.com/goharbor/harbor/blob/v2.15.4/src/server/registry/catalog.go#L56-L69)).
Its collector also takes its candidates from the database, the blobs no artifact refers to,
and deletes them by digest through registryctl
([garbage_collection.go](https://github.com/goharbor/harbor/blob/v2.15.4/src/jobservice/job/impl/gc/garbage_collection.go#L212-L240),
[L429-L436](https://github.com/goharbor/harbor/blob/v2.15.4/src/jobservice/job/impl/gc/garbage_collection.go#L429-L436)).
Neither walks the storage, so no name hides a repository from them. The job logged each orphan by
digest, without a repository, "8 blobs and 0 manifests are actually deleted", and no error. The
storage is distribution's: `repositories/demo/x/` holds `_layers`, `_manifests`, `_uploads` and
`blobs`, the child. The REST API, with each `/` of a name encoded twice, listed every
repository's artifact; `a/b` beside `a/b/c` is also in
[harbor#21049](https://github.com/goharbor/harbor/issues/21049), and the UI of 2.15 answers 404
for nested names ([harbor#24209](https://github.com/goharbor/harbor/issues/24209); not tried).

An orphan needs time or a setting: the collector keeps every blob younger than core's
`GC_TIME_WINDOW_HOURS`, 2 h by default, an environment variable "for testing/debugging"
([systemconfig.go](https://github.com/goharbor/harbor/blob/v2.15.4/src/lib/config/systemconfig.go#L114-L124)).
The request's `time_window` is ignored, and the collector runs only when started or on its
schedule ([harbor.md](harbor.md#garbage-collection), 2026-11-07: at 2 h an orphan was kept).

The route words break one request. Routing worked for every request: manifests, tag lists and
`GET` of blobs answered for the right repository each time. Then core's artifactinfo middleware
matches the path against five regular expressions kept in a Go map and takes the first that
matches, in the map's random order
([artifact_info.go](https://github.com/goharbor/harbor/blob/v2.15.4/src/server/middleware/artifactinfo/artifact_info.go#L42-L48),
[L110-L140](https://github.com/goharbor/harbor/blob/v2.15.4/src/server/middleware/artifactinfo/artifact_info.go#L110-L140)).
The manifest and referrers patterns end in `(?P<reference>.*)$`, and the tag list pattern is not
anchored at its end ([patterns.go](https://github.com/goharbor/harbor/blob/v2.15.4/src/lib/patterns.go#L36-L46)).
So `/v2/quirks/x/manifests/y/blobs/<digest>` also reads as the manifest `y/blobs/<digest>` of
`quirks/x`, an invalid reference and 404, and `/v2/quirks/x/tags/list/blobs/<digest>` as the tag
list of `quirks/x`, which leaves the blob `HEAD` handler without a digest: 400 (read). A client
that asks `HEAD` before an upload sends the blob again; one that checks a blob after a push may
find it missing. Reported as [harbor#24363](https://github.com/goharbor/harbor/issues/24363).

`lajstrom registry probe` (#623) measured the same on a fresh Harbor v2.15.4 the same day, under the
prefix `hnprobe/`, its row "Routing words in a name":

```text
misroutes: blob HEAD in x/manifests/y 13×404, x/manifests 12×404, x/referrers/y 1×404,
x/tags/list 15×400 of 20; GET 200
x HEAD 20×200, GET 200 (the control name)
```

### Elsewhere (read, not measured)

- **GitLab.** A group and a project cannot share a path under one parent
  ([gitlab-foss#35100](https://gitlab.com/gitlab-org/gitlab-foss/-/issues/35100)), so among the
  three prefixes `path.rb` tries at most one is a project: no ambiguity, only the depth limit.
- **Quay.** Since 2026-11-05 on master, not released, a new repository whose name ends in a
  `build` segment or has one segment after it (`foo/build`, `foo/build/bar`) is refused, because
  `org/foo/build/bar` reads as build `bar` of `org/foo` in Quay's web URLs
  ([commit 77b369a](https://github.com/quay/quay/commit/77b369a411)).
- **JFrog.** A tag is a folder in the image's folder, so image `a/b` with tag `c` and image
  `a/b/c` are both `a/b/c/`; no JFrog document or report says what happens then ?. Pushes by
  digest go to `<image>/sha256:<hex>/` ([reported](https://github.com/weaveplatform/artifactweave/issues/387)).
- **Rules that pick repositories by prefix** meet `a/b` and `a/b/c` too. ECR's template prefixes
  end with an implied `/`; an ACR ABAC condition needs the `/` written,
  `StringStartsWithIgnoreCase 'prefix/'` ([ABAC](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-rbac-abac-repository-permissions));
  Nexus's content selectors (`path =^ "/v2/team/"`) and its cleanup's asset regex, a partial match
  on PostgreSQL, catch `a/b/c` with `a/b` ([cleanup](https://help.sonatype.com/en/cleanup-policies.html));
  Harbor's and Zot's doublestar patterns take one level with `*` and every level with `**`.

## 3. Everything else that differs

### Uploads

A blob goes up as a `POST` and one `PUT` with the bytes (monolithic), as `PATCH`es with
`Content-Range` and a closing `PUT` (chunked), or Docker's way, one `PATCH` that streams the
layer without a length. distribution-spec 1.1.1 defines the first two; Docker pushes the third
([distribution-spec#485](https://github.com/opencontainers/distribution-spec/issues/485)).

- **Google Artifact Registry takes monolithic uploads only**: it "doesn't support Docker chunked
  uploads" ([Docker API](https://docs.cloud.google.com/artifact-registry/docs/reference/docker-api)),
  a second `PATCH` is 405 ([reported](https://github.com/greenticai/greentic-deployer/pull/616)),
  and its upload `Location` is outside `/v2/` ([ocync, GAR](https://clowdhaus.github.io/ocync/registries/gar)).
- **ECR** sends `OCI-Chunk-Min-Length: 10485760`, answers
  a `PATCH` with 201 instead of 202, and its `Range` stopped advancing after the first chunk until
  a fix of 2026-07 ([aws/containers-roadmap#2831](https://github.com/aws/containers-roadmap/issues/2831));
  AWS's 5 MiB minimum part is documented for its API only, so a part's minimum on `/v2` is
  unverified ([ecr.md](ecr.md#what-the-provider-does));
  one `PUT` was cut off above about 300 MiB ([reported](https://github.com/alienplatform/alien/pull/1418)).
- **GHCR** takes 4 MiB a `PATCH` and answers 416 above, with no header to say so; the `GET` of a
  session is a 303 to a web page; the empty blob cannot be pushed (measured, [ghcr.md](ghcr.md#upload-sessions)).
- **Gitea, Forgejo and JFrog refuse a chunk of unknown length into a session that holds bytes**,
  400 "Stream uploads after first write are not allowed" and 416; `registries[].uploads.spoolUnknownLength`
  spools it (measured, [gitea.md](gitea.md#uploads), [jfrog-jcr.md](jfrog-jcr.md#uploads-and-digests)).
- **Harbor appends a chunk at the end of the session whatever its `Content-Range` says**, and
  JFrog stores a blob under whatever digest it is told (measured, [harbor.md](harbor.md#uploads),
  [jfrog-jcr.md](jfrog-jcr.md#uploads-and-digests)). distribution, Zot and ACR take all three
  ways (measured; ACR [reported](https://clowdhaus.github.io/ocync/registries/acr)).
- **A cross-repository mount** (`POST ?mount=&from=`) is 201 on most; Nexus's `docker` format and
  JFrog open a session instead (202), Harbor and Gitea mount inside one project or owner
  (measured, [matrix](README.md#capabilities)). ECR mounts only with the account setting
  `BLOB_MOUNTING`, since 2026-01, between repositories of one encryption
  ([blob mounting](https://docs.aws.amazon.com/AmazonECR/latest/userguide/blob-mounting.html)).

### Blob downloads

distribution on S3 and GHCR answer a blob `GET` with a 307 to object storage or a CDN, valid 20
and 5–10 minutes (measured, [distribution.md](distribution.md#blob-redirect), [ghcr.md](ghcr.md#blob-redirect));
Zot does on S3 with `redirectBlobURL`. ECR serves layers from presigned S3 URLs, status and
lifetime not measured ?; ACR redirects to `*.blob.core.windows.net` or its data endpoint
([geo-replication](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-geo-replication));
GAR's `Location` is relative, `/artifacts-downloads/…` ([nexus-public#1061](https://github.com/sonatype/nexus-public/issues/1061)).
A `Range` is 500 on Nexus's `oci` format and ignored by Gitea, which sends the whole blob
(measured, [matrix](README.md#capabilities)).

### Deletes

- **GHCR deletes nothing through `/v2`** (405); its REST API deletes package versions, and not a
  public one with more than 5,000 downloads (measured, [ghcr.md](ghcr.md#delete);
  [REST](https://docs.github.com/en/rest/packages/packages)).
- **Harbor deletes no tag through `/v2`** (400); a digest delete takes the artifact with every
  tag (measured; [harbor#24213](https://github.com/goharbor/harbor/issues/24213)).
- **Nexus's `docker` format answers 202 to a digest delete and keeps serving the manifest under
  its tags** (measured, [nexus.md](nexus.md#delete)).
- **On GitLab, any protection or immutability rule in a project, matching or not, refuses every
  digest delete through `/v2`**, 401 `PROTECTED_MANIFEST` (measured with a token server playing
  GitLab, [gitlab.md](gitlab.md#immutable-tags); [immutable tags](https://docs.gitlab.com/user/packages/container_registry/immutable_container_tags/)).
- **Gitea deletes the manifest a tag named when the tag is deleted or re-pointed** and nothing
  else holds it (measured, [gitea.md](gitea.md#immutable-tags)).
- distribution deletes only with `storage.delete.enabled`, false by default
  ([configuration](https://github.com/distribution/distribution/blob/v3.1.2/docs/content/about/configuration.md#delete));
  the `registry:3` image enables it (measured). Zot refuses (405) a manifest an image index lists,
  GitLab with its database a manifest a referrer names (500) (measured). Docker Hub deletes by
  digest only, 403 while a tag or an image refers to it ([API](https://docs.docker.com/reference/api/registry/latest/operations/DeleteImageManifest/)).
  ACR deletes a manifest by digest only, a tag through `/acr/v1/<name>/_tags/<tag>`, and a subject
  with its referrers ([REST spec](https://github.com/Azure/azure-rest-api-specs/blob/main/specification/containerregistry/data-plane/Registry/stable/2021-07-01/containerregistry.json),
  [artifacts](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-manage-artifact));
  with its soft delete on, a deleted digest cannot be pushed again
  ([soft delete](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-soft-delete-policy)).
- No registry deletes a repository through `/v2`. distribution and GitLab keep an emptied one in
  `_catalog`, Zot drops it at a collection, ACR with its last manifest; ECR, Harbor and GHCR have
  a REST call.

### Tag mutability

distribution, Zot, GHCR and Gitea re-point a tag when asked; JFrog does for a user with the
overwrite permission ([docs](https://docs.jfrog.com/artifactory/docs/additional-docker-information)),
and its "Max Unique Tags" deletes the oldest tags without a word. The others keep the rule in
different places and refuse with different answers:

| Registry | Where the rule lives | A push to an immutable tag |
| --- | --- | --- |
| ECR | per repository: `IMMUTABLE`, or `IMMUTABLE_WITH_EXCLUSION` with up to five wildcard filters (2025) ([API](https://docs.aws.amazon.com/AmazonECR/latest/APIReference/API_ImageTagMutabilityExclusionFilter.html)) | `ImageTagAlreadyExistsException`; `TAG_INVALID` on `/v2`, status ? ([kaniko#3481](https://github.com/GoogleContainerTools/kaniko/issues/3481)) |
| GAR | per repository, `--immutable-tags`: no tag moved or deleted, no tagged image deleted ([REST](https://docs.cloud.google.com/artifact-registry/docs/reference/rest/v1/projects.locations.repositories)) | "The repository has enabled tag immutability"; code ? |
| ACR | a lock per repository, tag or digest, `--write-enabled false` ([image lock](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-image-lock)) | "The operation is disallowed"; code ? |
| Docker Hub | per repository: all tags, or those matching RE2 patterns ([immutable tags](https://docs.docker.com/docker-hub/repos/manage/hub-images/immutable-tags/)) | ? |
| Harbor | up to 15 rules a project, doublestar on repository and tag ([rules](https://goharbor.io/docs/main/working-with-projects/working-with-images/create-tag-immutability-rules/)) | 412 `PRECONDITION`, measured |
| GitLab | protected tags (17.11) and immutable tags (18.10, Ultimate), in GitLab, carried in its tokens | 401 `IMMUTABLE_TAG`, measured with a stand-in token server |
| Nexus | the repository's `writePolicy: allow_once` | 409 `REDEPLOY_DISABLED`, measured |
| Zot | an access-control policy without `update` | 403 `DENIED`, measured |
| Quay | 3.17, organisations: per tag, a regex policy or the `quay.immutable` label ([docs](https://github.com/quay/quay-docs/blob/master/modules/immutable-tag-overview.adoc)) | ? |

### Referrers and OCI 1.1 artifacts

The referrers API is served by Zot, Harbor (since 2.8), Nexus's `oci` format, JFrog, ACR (not on
a registry with customer-managed keys), ECR (since 2024-06), GAR (GA 2024-10) and Quay (since
2024-07); not by distribution, GitLab ([container-registry#1205](https://gitlab.com/gitlab-org/container-registry/-/issues/1205)),
GHCR, Gitea or Nexus's `docker` format, where Lajstrom writes the `sha256-<digest>` tag schema
(measured, [matrix](README.md#capabilities); [ECR](https://aws.amazon.com/about-aws/whats-new/2024/06/amazon-ecr-oci-image-distribution-version-1-1),
[GAR](https://docs.cloud.google.com/artifact-registry/docs/release-notes),
[Quay](https://github.com/quay/quay/commit/edf159007e),
[ACR](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-manage-artifact)).
Docker Hub does not document one, and answered an anonymous
`GET /v2/library/alpine/referrers/<digest>` with 200 and an empty index (curl, 2026-11-11). On
ECR, `oras copy -r` of a referrer is 405 "Invalid JSON syntax"
([aws/containers-roadmap#2783](https://github.com/aws/containers-roadmap/issues/2783), open).

What a manifest may contain differs more than the API:

- **GitLab with its metadata database, as on GitLab.com, refuses any `artifactType` or config
  type outside its table**: 400 "unknown media type" (measured, [gitlab.md](gitlab.md#media-types)),
  unless `REGISTRY_FF_DYNAMIC_MEDIA_TYPES`, which GitLab.com does not set
  ([supported types](https://gitlab.com/gitlab-org/container-registry/-/blob/master/docs/supported-media-types.md),
  [production values](https://gitlab.com/gitlab-com/gl-infra/k8s-workloads/gitlab-com/-/blob/master/releases/gitlab/values/gprd.yaml.gotmpl)).
- **ECR refuses media types with parameters**, `application/vnd.dev.sigstore.bundle+json;version=0.2`
  ([aws/containers-roadmap#2306](https://github.com/aws/containers-roadmap/issues/2306), open since
  2024), and converts between OCI and Docker manifests on a pull by tag, after the `Accept` header
  ([manifest formats](https://docs.aws.amazon.com/AmazonECR/latest/userguide/image-manifest-formats.html)).
- **Zot refuses Docker schema 2 manifests** unless `http.compat: ["docker2s2"]`
  ([configuration](https://zotregistry.dev/v2.1.23/admin-guide/admin-configuration/#compatibility-with-other-image-schema-types)).
  Quay refused unknown types unless `IGNORE_UNKNOWN_MEDIATYPES` until mid-2024
  ([commit 4546163](https://github.com/quay/quay/commit/4546163e83)). Harbor takes any type, and
  parses a config whose type looks like `application/vnd.X.Y.config.Z+json` as JSON
  ([default.go](https://github.com/goharbor/harbor/blob/main/src/controller/artifact/processor/default.go)).

### Catalog

`_catalog` is Docker's API, not distribution-spec 1.1.1's, and registries treat it so. GHCR
answers with every repository of GHCR the token can see; Harbor only to system administrators;
GitLab behind GitLab only to admins ([API](https://docs.gitlab.com/api/container_registry/#listing-all-container-repositories));
Docker Hub not at all (401 `insufficient_scope`); JFrog with an empty body at the root and a
catalog per key; Nexus's `oci` format pages with an opaque cursor and ignores `last` (measured,
[matrix](README.md#capabilities)). **ECR has one**, paged by an opaque token in `Link`, and a
repository name as `last` is 405 `UNSUPPORTED` ([zot#4528](https://github.com/project-zot/zot/pull/4528));
Lajstrom's adapter lists with `DescribeRepositories` ([ecr.md](ecr.md)).
ACR's needs `registry:catalog:*`, which a scope-map token cannot have
([tokens](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-token-based-repository-permissions));
GAR's Docker listing is incomplete above 10,000 images or tags in a repository
([quotas](https://docs.cloud.google.com/artifact-registry/quotas)); Quay shows an anonymous
caller nothing and caches a page for 60 s ([catalog.py](https://github.com/quay/quay/blob/master/endpoints/v2/catalog.py)).
Zot's leaves names out ([section 2](#zots-catalog-and-collector-skip-names-that-end-in-blobs-uploads-or-sync)).

### Sizes

Largest layer: GHCR 10 GB and 10 minutes an upload
([GHCR](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry));
ECR 52,000 MiB ([quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html)),
200 GB by Docker push since 2026-08-03 ([announcement](https://aws.amazon.com/about-aws/whats-new/2026/08/amazon-ecr-image-layers/));
ACR 195 GiB ([SKUs](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-skus));
GAR 5 TB an artifact ([pushing](https://docs.cloud.google.com/artifact-registry/docs/docker/pushing-and-pulling));
Quay 20 GB by default, `MAXIMUM_LAYER_SIZE` ([config.py](https://github.com/quay/quay/blob/master/config.py));
Harbor 128 GiB a request ([harbor#19578](https://github.com/goharbor/harbor/pull/19578));
distribution on S3 about 97.7 GiB, 10,000 parts of the default 10 MiB `chunksize`
([s3 driver](https://distribution.github.io/distribution/storage-drivers/s3/)); Docker Hub,
GitLab, Zot and Nexus document none. Largest manifest: 4 MiB nearly everywhere (distribution 400,
Zot and Harbor 413, measured), 10 MiB on Nexus and Gitea (measured), and **256,000 bytes and 200
references on GitLab.com** (`validation.manifests.payloadsizelimit`, [production values](https://gitlab.com/gitlab-com/gl-infra/k8s-workloads/gitlab-com/-/blob/master/releases/gitlab/values/gprd.yaml.gotmpl)).

### Auth

The bearer flow is one; what the token is differs. ECR's is a Basic password that lives 12 hours
([registry auth](https://docs.aws.amazon.com/AmazonECR/latest/userguide/registry_auth.html)). ACR
exchanges an Entra token for a refresh token of about 3 hours and that for access tokens of about
75 minutes ([AAD-OAuth](https://github.com/Azure/acr/blob/main/docs/AAD-OAuth.md),
[ocync, ACR](https://clowdhaus.github.io/ocync/registries/acr)); GAR takes `oauth2accesstoken`
and an OAuth token of 60 minutes ([auth](https://docs.cloud.google.com/artifact-registry/docs/docker/authentication)).
Docker Hub's realm is `auth.docker.io`, its anonymous token 300 s (curl, 2026-11-11);
GitLab.com's is `gitlab.com/jwt/auth`, its token 15 minutes, 5 by default elsewhere
([GitLab.com](https://docs.gitlab.com/user/gitlab_com/#container-registry));
Harbor's lives 30 minutes, Quay's an hour, Gitea's a day with `scope="*"` whatever was asked, and
GHCR's is the personal access token in base64 (measured, [matrix](README.md#capabilities)). Quay's
token endpoint knows no `delete` action ([v2auth.py](https://github.com/quay/quay/blob/master/endpoints/v2/v2auth.py)).
Nexus answers a user 429 for about 30 s after three wrong passwords, the right one too
(measured, [nexus.md](nexus.md#what-the-provider-must-do-differently)).

### Rate limits and read-after-write

ECR's default rates a region: `PutImage` 10 a second, `InitiateLayerUpload` and
`CompleteLayerUpload` 100, `UploadLayerPart` 500, `BatchGetImage` 2,000, `GetAuthorizationToken`
500, all adjustable ([quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html)).
ACR counts a minute, per registry and per identity: on Basic and Standard 10,000 data-plane reads,
2,000 writes and 500 referrer listings, half of each for one identity
([SKUs](https://learn.microsoft.com/en-us/azure/container-registry/container-registry-skus)). GAR
counts every HTTP request against 60,000 a minute for a project and region
([quotas](https://docs.cloud.google.com/artifact-registry/quotas)). Docker Hub allows 100 pulls in
6 hours to an anonymous address and 200 to a personal account; a manifest `GET` counts, a `HEAD`
does not ([pulls](https://docs.docker.com/docker-hub/usage/pulls/)). quay.io allows "a few requests
per second per IP" ([KB 6218921](https://access.redhat.com/solutions/6218921)); GHCR set no limit
on `/v2` (measured). A read after a write lags on ECR by up to seconds (reported), Quay caches the
tag list up to 120 s, ACR's geo-replicas are eventual ([Consistency](README.md#consistency)).

### Garbage collection and rules that read tags

Two things delete what Lajstrom wrote without asking it: a collector of untagged manifests, and a
retention rule written as patterns on tags. Lajstrom's tags are versions
(`Escape(version)[__Escape(variant)]`, `_h…` when hashed), npm's journal `_j00000001` and up, and,
where there is no referrers API, `sha256-<digest>`; cosign adds `sha256-<digest>.sig` for a client
that signs through the facade. On a registry with the API, Lajstrom's referrers are untagged.

| Registry | Untagged manifests | Rules on tags |
| --- | --- | --- |
| ECR | a lifecycle rule with `tagStatus: untagged`; a referrer is safe while its subject exists ([blog](https://aws.amazon.com/blogs/opensource/diving-into-oci-image-and-distribution-1-1-support-in-amazon-ecr/)) | lifecycle policies: `tagPrefixList`, `tagPatternList` with up to four `*`, `imageCountMoreThan`, `sinceImagePushed`, applied "within 24 hours" ([lifecycle](https://docs.aws.amazon.com/AmazonECR/latest/userguide/LifecyclePolicies.html)) |
| GAR | a cleanup policy on `UNTAGGED`; attachments are exempt ([attachments](https://docs.cloud.google.com/artifact-registry/docs/manage-metadata-with-attachments)) | cleanup policies by tag, version and package prefixes, age, and `keepCount` per package ([cleanup](https://docs.cloud.google.com/artifact-registry/docs/repositories/cleanup-policy)) |
| ACR | a retention policy (Premium, preview), which skips OCI manifests | `acr purge --filter <repo>:<tag regex>`, the tag regex not anchored ([acr-cli](https://github.com/Azure/acr-cli)) |
| GitLab | **the online collector deletes an untagged manifest, a referrer too, 24 h after its push** (measured, [gitlab.md](gitlab.md#garbage-collection)) | a cleanup policy: `name_regex`, `name_regex_keep`, `keep_n`, `older_than`, `latest` always kept ([cleanup](https://docs.gitlab.com/user/packages/container_registry/reduce_container_registry_storage/#how-the-cleanup-policy-works)) |
| Harbor | the collector deletes untagged artifacts if asked; a referrer is an accessory and follows its subject ([accessories](https://goharbor.io/docs/main/working-with-projects/working-with-images/sign-images/)) | retention rules, doublestar on repository and tag; nested repositories misbehave ([harbor#21049](https://github.com/goharbor/harbor/issues/21049)) |
| Quay | referrers kept by hidden tags ([commit 1452d1a](https://github.com/quay/quay/commit/1452d1aed0)) | auto-prune by count or age with a tag regex, off on quay.io; a `quay.expires-after` label expires a tag ([docs](https://github.com/quay/quay-docs/blob/master/modules/setting-tag-expiration-annotation.adoc)) |
| Zot | retention's `deleteUntagged`, on by default once a policy exists; referrers kept while their subject is ([retention](https://zotregistry.dev/v2.1.23/articles/retention/)) | `keepTags` patterns, unanchored regexes: `v1` keeps `v10`; a tag no pattern keeps goes ([matcher.go](https://github.com/project-zot/zot/blob/6a2e68c975a126ca90960cd0663628b69ebf7e59/pkg/retention/matcher.go#L33-L40)) |
| Gitea | kept; an unused blob goes after 24 h (measured, [gitea.md](gitea.md#cleanup)) | cleanup rules: keep N, keep and remove regexes, anchored and case-insensitive ([storage](https://docs.gitea.com/usage/packages/storage)) |
| Nexus `docker` | "Delete unused manifests and images" deletes an untagged referrer (measured, [nexus.md](nexus.md#garbage-collection)) | cleanup policies by age, last download and an asset regex; "only evaluates tagged manifests for Docker" ([cleanup](https://help.sonatype.com/en/cleanup-policies.html)) |
| JFrog | not measured | cleanup policies (Enterprise+), wildcards at the ends only; a package goes when any file in it qualifies ([cleanup](https://docs.jfrog.com/administration/docs/cleanup-policies)); a repository's "Max Unique Tags" deletes the oldest tags |
| distribution | `garbage-collect --delete-untagged`, offline, run by hand | none |

What those rules would do to Lajstrom: a count rule (ECR's `imageCountMoreThan`, GAR's
`keepCount`, GitLab's `keep_n`) counts a package's versions, its journal entries and its tag-schema
indexes together; a pattern written for versions catches or misses the others: GitLab's `.*`
removes journal and `sha256-` tags with the versions, a Zot `keepTags` that lists versions only
removes the rest; an untagged-manifest rule takes Lajstrom's referrers on GitLab and Nexus, which
is why Lajstrom writes the tag schema there (#37).

## 4. What this means for Lajstrom

Lajstrom names an OCI repository `<prefix><format>/<repo>/<package path>`
([manifest-profile.md](../spec/manifest-profile.md#41-repository-path), ADR 0004): a repository
per package, three components or more below the prefix (maven four, a raw file one per directory
of its path), lowercase, at most 255 bytes, in the 1.0 grammar and without the words of Zot's
layout and Harbor's routes since [ADR 0017](../adr/0017-the-grammar-of-oci-names.md) and its
amendment: a component that fails the grammar is replaced by `h0_h` and 32 hex digits, a run of
`-` and the last byte of a reserved word are escaped, and so is the last byte of `tags` before a
`list…`. A package's versions are tags in its repository.

### Consequences, registry by registry

- **ECR: a repository per package.** Each new package is a `CreateRepository` or a creation
  template; the region's 100,000 repositories are the instance's package limit, shared with every
  other user of the account, and a proxy repo that caches npm or Maven Central spends them fast.
  Lifecycle, scanning, replication and policies are per repository, so they come from a template
  or not at all. `PutImage` at 10 a second is the publish rate of the account in a region,
  referrers included, which are images too. The other answer, one repository per Lajstrom repo and the
  package in the tag, is what ADR 0004 rules out today.
- **Docker Hub cannot hold Lajstrom's names**: three levels against its two, and no `.` in
  `com.example`. A **GitLab** project cannot hold them below itself: a project per Lajstrom repo,
  `<group>/<format>/<repo>` with `prefix: <group>/`, leaves two levels, enough for pypi, deb, npm
  and maven, not for a raw path deeper than two ([gitlab.md](gitlab.md#behind-gitlab-read-not-measured)).
  Since #599 the vendor table knows both depths (`capabilities.maxNameDepth`): a publish into a
  deeper name is a `400` that names the limit, not the registry's `insufficient_scope`, and a
  registry that takes no name of Lajstrom's, Docker Hub, is an error at startup.
- **Zot** leaves out of its catalog, and so out of `reindex` from an empty database, which lists
  the catalog ([scan.go](../../internal/index/reindex/scan.go)), every package whose OCI name has
  a component ending in `blobs`, `.uploads` or `.sync`; a later run keeps their rows, since the
  sweep looks a version up by its tag before it removes it. A raw path `x/blobs/sha256/…` stops
  the collection of `x`. Since ADR 0017 Lajstrom writes no such component (`blobs` is
  `blob_73`); a name written before keeps it (#597).
- **Gitea and Forgejo** refuse `__` and `--` in a name: every hashed component (`h0__…`: a raw
  file `_config.yml` or `.gitignore`, a component over 64 bytes) and every `--` in a raw path
  could not be stored there (from the source, not run). Since ADR 0017 a hashed component is
  `h0_h…` and `--` is `_2d_2d` (#598).
- **Quay**, once its 2026-11 change ships, refuses new names ending in `build` or `build/<x>`:
  a raw file `ci/build`.
- **GitLab.com** refuses Lajstrom's manifests: its registry runs with the database and without
  the dynamic media types (measured on that variant, read in GitLab.com's values); and its
  256,000-byte manifest limit is below Lajstrom's `server.maxManifestSize` of 4 MiB.
- **Harbor** answers a blob `HEAD` with 404 or 400 at random in a name with a component
  `manifests` or `referrers`, or `tags/list` (measured, section 2): a raw file below a directory
  `manifests/`, such as `k8s/manifests/app.yaml`. A check by `HEAD` misses the blob there: an
  upload sends it again, a check after a push fails now and then (inference). Since the amendment
  of ADR 0017 these are `manifest_73`, `referrer_73` and `tag_73/list…` on every registry, until
  Harbor fixes [harbor#24363](https://github.com/goharbor/harbor/issues/24363); a name written
  before keeps its form.
- **GHCR** cannot delete: no unpublish, no retention. **Harbor** cannot delete one tag of
  several. **Nexus**'s `docker` format keeps a deleted manifest's tags.
- **Tokens scale with packages.** A bearer token is scoped to repositories, so a run that touches
  every package (reindex, a prefetch) asks the token service once a package: GitLab's Rails,
  Harbor's core, `auth.docker.io` (inference; ECR's Basic password and Gitea's `scope="*"` are
  exceptions).
- **The repository list is weakest where it matters most.** Reindex from nothing needs a
  complete listing: GHCR's catalog is everyone's, Harbor's needs an administrator, GAR's stops at
  10,000, Zot's hides names, and without a catalog Lajstrom's `oci_repos` is the list, which a
  lost database loses.

### Tags, immutability and retention

Lajstrom's version and journal tags are never re-pointed, so `IMMUTABLE` fits them. The tag
schema's `sha256-<digest>` and cosign's tags are re-pointed with each new referrer: they need
ECR's exclusion `sha256-*`, and no immutability rule for that pattern on Harbor, GitLab or Docker
Hub. GAR's `--immutable-tags` also forbids deleting a tagged image, so Lajstrom's deletes fail
there. Retention belongs to Lajstrom (#72) or to the registry, not both: a rule on the backend
that deletes a version leaves an index row that reindex removes later, and the package
disappears from its clients without an audit record.

### Recommended settings

| Registry | `prefix` | On the registry | In `registries[]` |
| --- | --- | --- | --- |
| distribution | optional | `storage.delete.enabled: true` for deletes | `capabilities.deleteManifest: false` without it |
| Zot | optional | no `retention` policy for the prefix, or one whose `keepTags` keeps `.*` | none |
| ECR | `lajstrom/` | no lifecycle rule with an untagged expiry or a count on the prefix's repositories; a raised repository quota; `BLOB_MOUNTING` for mounts; a creation template with `CREATE_ON_PUSH` only when Lajstrom should not create repositories | `type: ecr`, `ecr.imageTagMutability: IMMUTABLE_WITH_EXCLUSION`, `imageTagMutabilityExclusions: ["sha256-*"]`; with a template, `capabilities.createOnPush: true` |
| GitLab | `<group>/` | a project per Lajstrom repo; `REGISTRY_FF_DYNAMIC_MEDIA_TYPES=true` with the database, which GitLab.com does not offer; no cleanup policy | `capabilities.deleteManifest: false` under protection rules |
| Harbor | `<project>/` | a project and a robot with push, pull and delete; a quota; no retention rule | none |
| GHCR | `<org>/` | a classic PAT with `write:packages` | none: the vendor table sets `catalog`, `deleteManifest`, `maxBlobSize` |
| Gitea, Forgejo | `<owner>/` | a token with `write:package`; no cleanup rule | `uploads.spoolUnknownLength: true` |
| Nexus | `<repository>/` with path routing, else none | `writePolicy: allow_once`; the `docker` format until `oci` serves ranges | none: the vendor table sets `deleteManifest: false` on `docker` |
| ACR, GAR, Quay | | not measured ([matrix](README.md#versions-tested)); GAR needs monolithic uploads | |
| Docker Hub, JFrog | | not a backend: two levels; not for data yet ([jfrog-jcr.md](jfrog-jcr.md)) | |

### Design risks

1. **The repository is the unit of cost on some registries** (ECR's quota and per-repository
   settings, ACR's performance note), and Lajstrom makes one per package.
2. **Depth and name grammar vary**: two levels on Docker Hub, project and two on GitLab, the 1.0
   grammar on Gitea, route words on Quay. Lajstrom finds out at the first push, with an error
   that names a token or a 404.
3. **Storage layouts and routers reserve words the grammar allows**: `blobs` on Zot, `manifests`,
   `referrers` and `tags/list` on Harbor, tag folders on JFrog. ADR 0017 escapes the words of Zot
   and Harbor; a word found on another registry is another amendment.
4. **Backend rules read Lajstrom's tags and untagged manifests**, and their patterns cannot tell
   a version from a journal entry or a referrer index.
5. **Reindex trusts the catalog**, which is partial on Zot, everyone's on GHCR, an
   administrator's on Harbor and empty on JFrog.
