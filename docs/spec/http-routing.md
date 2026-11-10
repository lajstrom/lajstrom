# Spec: HTTP routing and protocol endpoints

- Status: normative for the MVP (2026-11-06). New endpoints are added by the format that owns them;
  changing a documented endpoint needs an issue labelled `contract`.
- Related: [ADR 0004](../adr/0004-path-first-addressing.md), [rbac.md](rbac.md).

## 1. One host, one tree

```
https://<host>/<format>/<repo>/...      protocol frontends (plugin routes)
https://<host>/v2/...                   OCI facade (distribution API)
https://<host>/v2/token                 token endpoint (docker bearer flow)
https://<host>/api/v1/...               management REST
https://<host>/-/healthz  /-/readyz  /-/version
http://<metricsListen>/metrics        Prometheus, a separate listener, never on the public host
https://<host>/.well-known/terraform.json   (after the MVP)
https://<host>/ui/...                   (after the MVP)
```

Reserved first segments: `v2`, `api`, `ui`, `-`, `.well-known`, `oci`, and every registered format
name. `oci` is the format of the pass-through repos: a repo `oci/<repo>` holds arbitrary OCI
artifacts that pass through `/v2` unchanged, indexes nothing and has no frontend, so no plugin
may take the name and the management API creates such a repo without one.
A format name is lowercase ASCII letters and digits with single hyphens between them
(`^[a-z0-9]+(-[a-z0-9]+)*$`) and never one of the other reserved segments.
A repo name is unique within its format; the pair `<format>/<repo>` is the resource for RBAC.
The optional FQDN mode (`<repo>.<host>`) is after the MVP and rewrites to the path form at the
edge; nothing else changes.

Lajstrom owns the whole host: nothing lives under a path prefix. OCI clients address a registry
as `host[:port]/<name>` and always request `https://<host>/v2/…`, and Terraform discovery reads
`/.well-known/terraform.json` at the host root, so a prefix cannot work for them, and the server
has one tree. `server.externalURL` is `https://<host>[:port]` without a path, and a reverse proxy
in front of Lajstrom must not rewrite paths.

Clients see Lajstrom over TLS only; there is no scheme to choose. docker refuses a plain-HTTP
registry, pip and Maven refuse a plain-HTTP index by default, and bearer tokens must not travel
in clear. TLS terminates in one of two places: at the edge (an ingress or a reverse proxy, the
default in Kubernetes; Lajstrom then listens on plain HTTP inside the network and trusts
`X-Forwarded-*` only from `server.trustedProxies`), or in Lajstrom itself for a single host
(`server.tls.certFile` and `server.tls.keyFile`; ACME after the MVP). Plain HTTP to clients is
for the dev stack on loopback only, behind an explicit `server.insecureHTTP: true`.
`server.trustedProxies`, a list of CIDR prefixes (ADR 0011), came with #130; the TLS keys and
`server.insecureHTTP` are part of the hardening issue (#50).

`server.externalURL` is the base for every absolute URL Lajstrom renders (simple index links,
`WWW-Authenticate` realm, apt `Release` is path-relative and needs none).

## 2. OCI facade `/v2`

Implements the OCI distribution spec against the backend through the provider, with the OCI
repository name `<format>/<repo>/<package path>` (the instance prefix is added by the provider and
never visible to clients).

| Method and path | Behaviour |
| --- | --- |
| `GET /v2/` | `200` if authenticated, else `401` with `WWW-Authenticate: Bearer realm="<externalURL>/v2/token",service="<host>"`. Every `/v2` answer has `Docker-Distribution-API-Version: registry/2.0`. |
| `GET /v2/token?service=&scope=repository:<format>/<repo>/<pkg>:pull,push` | Basic auth (user:password, user:PAT, or deploy token) → JWT bearer with the scopes the caller is allowed (rbac.md §4). Anonymous: scopes limited to `pull` on repos with anonymous read. |
| `HEAD/GET /v2/<name>/manifests/<ref>` | proxied to the backend, which is asked with the manifest media types of oras-go (OCI and Docker manifests and indexes); the answer has the manifest's own `Content-Type`, `Docker-Content-Digest` and `Content-Length`. The client's `Accept` converts nothing: a manifest of a type it did not list is returned all the same. `404 MANIFEST_UNKNOWN`. |
| `PUT /v2/<name>/manifests/<ref>` | **hook**: body is parsed (`Content-Length` required, at most `server.maxManifestSize`, default 4 MiB, else `413`); for a non-`oci` repo it must be an OCI image manifest that satisfies the manifest profile (an image index or a Docker manifest is `400 MANIFEST_INVALID` naming the media type the profile wants), every layer must be in the backend (`400 MANIFEST_BLOB_UNKNOWN` naming the digest), and it is passed to `ingest.Ingest` with the `<path>` below the repo and the tag; on success the manifest is stored and indexed, `201` with `Location`, `Docker-Content-Digest` (also for the idempotent repeat of the same manifest) and, when the manifest has a `subject`, `OCI-Subject`. A push by digest is accepted when the annotations yield a tag, `Tag(lajstrom.version, lajstrom.variant)` (manifest-profile.md §4.2), which ingest sets in the backend; the `Location` names that tag; without `lajstrom.version` it is `400 MANIFEST_INVALID`. Errors: `413 SIZE_INVALID` for a file layer above the repo's `maxFileSize` (policy, #60), naming the limit; `400 MANIFEST_INVALID` with the rule that failed in `detail`, and, when the `artifactType` is oras's default `application/vnd.unknown.artifact.v1`, the hint to pass `--artifact-type application/vnd.lajstrom.<type>.v1` in the message; `409 TAG_INVALID` (tag exists with another digest, named in the message and in `detail.existing`); `404 NAME_UNKNOWN` (repo not configured). On an `oci` repo the manifest passes through unchanged (an OCI or Docker manifest or index media type that the body's `mediaType` does not contradict), by tag or by digest, and `oci_repos` records the repository; a push by tag holds the tag lock of the manifest (index-schema.md, Coordination). **Referrers** (manifest-profile.md §6): on a managed repo, a manifest with a `subject` and an `artifactType` that is not `application/vnd.lajstrom.*`, pushed by digest, or a manifest under cosign's legacy tag `sha256-<hex>.sig`, `.att` or `.sbom`; on an `oci` repo, any manifest with a `subject`. The facade checks `write` and the blobs (`400 MANIFEST_BLOB_UNKNOWN`) and hands it to `ingest.PutReferrer`, which indexes nothing: on a managed repo the subject must be a manifest of the repository and any other tag is refused (`400 MANIFEST_INVALID`), and without the backend's referrers API the push runs under the referrers lock of the subject (index-schema.md, Coordination). The answer is `201` with `Location`, `Docker-Content-Digest` and `OCI-Subject` for a manifest with a subject, whatever the backend sent. |
| `DELETE /v2/<name>/manifests/<ref>` | requires `delete`, checked before the reference is looked up; `ingest.Delete` (#61). On a managed repo a reference that names a version, its tag or its digest, deletes the version: its referrers first, each with its own before it (cosign's legacy manifests under `sha256-<hex>.sig`, `.att` and `.sbom`, the tag-schema index `sha256-<hex>`, the referrers the backend lists), then the manifest, then its rows in the index, its package's when it was the last version, and the repo's rendered indexes (`index_cache`), with one audit row `delete` that counts the referrers. Any other manifest of the repository (a referrer, a foreign manifest, any manifest of an `oci` repo) is deleted with its referrers and indexes nothing; the audit row is written all the same. Without the backend's referrers API, the `sha256-<hex>` index of a referrer's subject is written again without it, under the referrers lock of the subject (index-schema.md, Coordination). A tag names the manifest it holds. On an `oci` repo a delete by tag of a manifest that other tags also name is `409 DENIED` naming them, and nothing changes (#284); by digest the manifest goes with every tag that names it: the distribution API has no untag. On a managed repo a manifest has one tag (manifest-profile.md §4.2). The delete counts the tags and deletes under the tag lock of the manifest (index-schema.md, Coordination), so that a tag pushed meanwhile is either counted or pushed after the delete. `202 Accepted`. A backend that cannot delete manifests (`Capabilities.DeleteManifest` false, or one that refuses the first delete: the first request that changes anything is a plain delete) is `405 UNSUPPORTED` naming the backend, and nothing changes. `404 MANIFEST_UNKNOWN` for a reference that names nothing, `400 DIGEST_INVALID` or `TAG_INVALID` for one that is neither; a delete the backend refuses for this manifest alone (Zot keeps a manifest that an image index lists) is `409 DENIED`. `DELETE …/blobs/<digest>` stays `405 UNSUPPORTED`: unused blobs are the backend's garbage collection's. After the MVP: retention (#72) deletes through the same path. |
| `POST /v2/<name>/blobs/uploads/`, `PATCH`, `PUT …?digest=`, `GET`, `DELETE` | proxied upload session (monolithic and chunked, a single `POST ?digest=` too): Lajstrom drives the backend's own session and streams; it never buffers a blob on disk or in memory. The upload id in the `Location` Lajstrom answers is opaque and does not depend on the replica: it is the key of a `sessions` row (kind `blob-upload`, one hour after the last request) that holds the backend's session, so any replica continues it; the backend's location never reaches the client. `Range` is what the backend holds. `?mount=&from=` is passed to the backend when `from` is in the same repo, or in another repo of the same registry that the principal may read; otherwise, and without `from`, the answer is a plain session. Errors: `404 BLOB_UPLOAD_UNKNOWN` (unknown, expired, another repo's or principal's id), `416 BLOB_UPLOAD_INVALID` (a `Content-Range` that does not start at the offset), `413 SIZE_INVALID` (a blob above the repo's `maxFileSize`: a declared length over it before anything streams, or a body cut at it; the session ends), `400 DIGEST_INVALID` (the backend refuses the digest: the session ends). |
| `HEAD /v2/<name>/blobs/<digest>` | the backend's `HEAD` (`Provider.StatBlob`, redirects followed): `200` with `Content-Length`, `Docker-Content-Digest`, `ETag` (the digest, quoted), `Accept-Ranges` when the backend says so; `404 BLOB_UNKNOWN`. `If-None-Match` with the `ETag` is `304` without a request to the backend. |
| `GET /v2/<name>/blobs/<digest>` | one request to the backend (`format.ServeBlob`, `Provider.OpenBlob`), with the client's `Range` passed through when it is one byte range: `307` with `Location` and `Docker-Content-Digest` when the backend redirects to another origin (a presigned URL of its object storage) and the repo's `policy.blobRedirect` is not `never` and the registry's `capabilities.blobRedirect` not `false`; otherwise the backend's answer streamed, `200`, `206` with `Content-Range` or `416`, with `Content-Length` and `Accept-Ranges` as the backend sent them, `Docker-Content-Digest` and `ETag` (the digest, quoted). A redirect into the backend's own origin is followed by the provider, never sent to the client. Several ranges, or a `Range` that is not one, are not sent: the answer is the whole blob with `200` (backends answer several ranges with `multipart/byteranges`). `If-None-Match` is `304` before any request. `404 BLOB_UNKNOWN`. |
| `GET /v2/<name>/tags/list` | from the index for Lajstrom-managed repos (fast, consistent): the versions of the package whose `name_oci` is the path, an empty list for a repository only `oci_repos` knows; from the backend for `oci` repos. `n`, `last` and a `Link` header (`rel="next"`) paginate both. |
| `GET /v2/<name>/referrers/<digest>` | `read` on the repo, checked before the digest. The manifests whose `subject` is `<digest>`, as an image index (`Content-Type: application/vnd.oci.image.index.v1+json`), from `Repository.Referrers` of the provider: the backend's referrers API, or the `sha256-<hex>` tag schema where it has none (`Capabilities.Referrers`). `?artifactType=` keeps the referrers of that type and the answer carries `OCI-Filters-Applied: artifactType`. A subject without referrers, in a repository the backend does not have too, is `200` with an empty index, never `404`. A malformed digest is `400 DIGEST_INVALID`. |
| `GET /v2/_catalog` | from the control plane, filtered by RBAC: the `oci_repos` of every repo the principal may read, as `<format>/<repo>/<path>`, with `n`, `last` and `Link` |

Mount of a package under a repo: the name after `/v2/` is split as `<format>/<repo>/<rest>`; the
repo must exist in the control plane, else `404 NAME_UNKNOWN`. Auto-creation of repos on push is
not supported (an explicit `POST /api/v1/repos` is one call).

An `oci` repo takes any manifest and blob as they come and indexes nothing beyond its
repository (the OCI distribution-spec conformance suite runs against one, `test/conformance/`).
Docker image passthrough with what it needs beyond that (recognising images for the UI,
multi-arch stitching) is after the MVP; in the MVP only the paths above are exercised, with
`oras`, `cosign` and `helm` as the OCI-native clients tested.

## 3. Protocol frontends (MVP formats)

Authentication on all frontend endpoints: HTTP Basic with a user password, a PAT or a deploy
token; the same credentials as `/v2/token`. Anonymous `GET` on repos with `anonymousRead: true`.
Writes require `write` on `<format>/<repo>`.

### raw

| Method and path | Behaviour |
| --- | --- |
| `GET /raw/<repo>/<path…>` | newest version of the file at `<path>` (the most recently created), read as the blob `GET` of the facade (§2, `format.ServeBlob`): one request to the backend with the client's `Range`, `307` to the backend's storage when it redirects and `policy.blobRedirect` allows it, else the backend's `200`, `206` or `416` streamed. `ETag` is the blob digest (`If-None-Match` is `304` without a request), `Last-Modified` the version's `created`, `Docker-Content-Digest` the manifest digest. `?digest=sha256:…` pins a version. `Content-Type` from the metadata layer or sniffed. A `PUT` of bytes an older version already holds is the idempotent repeat of that version and does not make it the newest: to roll back, `DELETE` the newer version or pin `?digest=`. |
| `HEAD` | same headers, from the backend's `HEAD`; no body, never a `307` |
| `PUT /raw/<repo>/<path…>` | upload one file (body). Idempotent for identical bytes. Creates a new version (tag) for different bytes; the path now resolves to it. `201` with `Location` and `Docker-Content-Digest`. `409` when the path exists in another spelling (the message names it). |
| `DELETE /raw/<repo>/<path…>` | requires `delete`; removes all versions of the path, each as the `DELETE` of its manifest on `/v2` deletes it, its referrers with it (after the MVP: one version). `204`; `501` when the backend cannot delete manifests |
| `GET /raw/<repo>/` and any directory prefix with trailing `/` | JSON listing (`{"entries":[{"path","size","digest","created"}]}`), recursive: one entry per version of every path below the prefix, at any depth, sorted by path and then `created`; `digest` is the manifest digest, the value `?digest=` takes. HTML after the MVP |

Model: package = path, version = first 16 hex of the content sha256, variant none. `oras push`
to `/v2/raw/<repo>/<escaped path>:<version>` works with the same annotations.

### pypi

Implements PEP 503 (simple index HTML), PEP 691 (JSON simple API, content negotiation), PEP 658
metadata files, and the legacy upload API that `twine` uses.

| Method and path | Behaviour |
| --- | --- |
| `GET /pypi/<repo>/simple/` | project list |
| `GET /pypi/<repo>/simple/<project>/` | file list of the project (normalised name; non-normalised redirects `301`). Links point to `/pypi/<repo>/files/…` with `#sha256=`; `data-requires-python`, `data-dist-info-metadata`/`data-core-metadata` set. |
| `GET /pypi/<repo>/files/<project>/<version>/<filename>` | the distribution file (`307` or stream) |
| `GET /pypi/<repo>/files/<project>/<version>/<filename>.metadata` | the core metadata (PEP 658), from the metadata layer |
| `POST /pypi/<repo>/` | legacy upload: `multipart/form-data` with `:action=file_upload`, `protocol_version=1`, `content`, metadata fields. One file = one manifest (variant = wheel tag or `sdist`). `200` on success; `400` on validation errors; `409` if the file name exists with a different digest. |

Client config: `pip install --index-url https://host/pypi/<repo>/simple/ …`,
`twine upload --repository-url https://host/pypi/<repo>/ dist/*`.

### apt

Implements the Debian repository format (`Release`, `InRelease`, `Packages`) for binary packages.
Suites and components are upload metadata (annotations), not separate repos.

| Method and path | Behaviour |
| --- | --- |
| `GET /apt/<repo>/dists/<suite>/InRelease` | clearsigned Release (per-repo GPG key) of the bytes of the `Release` of the same render; from `index_cache`, made on the first `GET` after the render and kept beside the `Release`. Signed by the repo's active key and, for `keys.retireGrace` after a rotation, by the retired key too. `404` when the server has no master key (`keys.masterKeyEnv`): the `Release` is unsigned then, and clients mark the source `[trusted=yes]` |
| `GET /apt/<repo>/dists/<suite>/Release`, `Release.gpg` | `Release`: rendered with every `Packages` of the suite in one render on the first `GET` after a publish or a delete (both invalidate the repo's documents) and kept with them in `index_cache`, so its hashes are those of the files served. `Origin: Lajstrom`, `Label` the repo, `Suite` and `Codename` the suite, `Date`, `Architectures`, `Components`, `Acquire-By-Hash: yes`, and `MD5Sum`, `SHA1` and `SHA256` of every `Packages`, `.gz` and `.xz`. `Release.gpg`: the armored detached signatures of the same `Release` bytes, by the keys that sign `InRelease`; `404` without a master key. Suites, components and architectures come from the versions (`lajstrom.deb.suite`, `.component`, `.arch`), with no configuration; a suite without versions is `404` |
| `GET /apt/<repo>/dists/<suite>/<component>/binary-<arch>/Packages`, `Packages.gz`, `Packages.xz` | package index: the versions of the suite and component whose architecture is `<arch>` or `all`, sorted by name and version. The architectures of a suite are the concrete architectures of its versions, and the `all` packages are in the index of each of them; a suite with `all` packages only has `binary-all` alone. Every component has an index for every architecture of the suite, empty when it has no package for it. `by-hash/SHA256/<hash>` serves the `Packages`, `.gz` or `.xz` of that directory whose SHA256 the current `Release` lists (apt uses it when `Acquire-By-Hash: yes`); a hash of an older render is `404`, and apt falls back to the file by name. A path the `Release` does not list is `404` |
| `GET /apt/<repo>/pool/<component>/<p>/<source>/<filename>` | the `.deb`, the `Filename` of its `Packages` paragraph. The directory is the source package's, as the Debian archive lays out a pool: `<source>` is the name of the control file's `Source` field, or the package name without one, and `<p>` its first letter, or its first four when it starts with `lib` (`libf/libfoo/libfoo2_2.1-3_amd64.deb`); `<filename>` is `<package>_<version without the epoch>_<arch>.deb`. Resolved by file name: the versions of the component that hold the file, one per suite, whose `<p>/<source>` is the path (else `404`; a version without metadata in the index is taken on the file name), read as the blob `GET` of the facade (§2, `format.ServeBlob`): `307` or stream, `Range`, `ETag` the blob digest, `Last-Modified` and `Docker-Content-Digest` those of the newest such version, `Content-Type: application/vnd.debian.binary-package`; `HEAD` from the backend's `HEAD`. The same file in two suites of a component is one URL, in two components two URLs for one blob. Versions that hold one file name with two blobs are `500`, logged with both digests for the operator: the upload refuses another content under a file name, an `oras push` cannot be checked |
| `GET /apt/<repo>/key.asc` | ASCII-armored public key of the repo (`signed-by=`): one public key block with the active key, made on the first request when the repo has none, and the keys retired within `keys.retireGrace`; `application/pgp-keys`, not cached. `404` without a master key |
| `PUT /apt/<repo>/upload?suite=<suite>&component=<component>[&arch=<arch>]` | convenience upload of one `.deb` (body). Equivalent to `oras push` with the deb profile: the deb plugin builds the manifest from the control file and the query. `suite` and `component` are required, lowercase letters, digits, `.`, `+` and `-`, the first a letter or a digit (`400` naming the one missing or malformed); `arch`, when given, must be the control file's `Architecture`. A body above the repo's `maxFileSize` is `413`, one that is not a `.deb` `400`. `201` with `Location` (the file in the pool), `Docker-Content-Digest` (the manifest digest) and a JSON body `{"package", "version", "architecture", "suite", "component", "filename", "digest", "tag"}`. The same bytes again are the idempotent repeat, `201` with the same digest; the same bytes in another suite or component are a new version over the same blob. `409` for other bytes under the same name, version and architecture in the same suite and component (the tag holds another manifest), and for other bytes under a pool file name that a version of another suite, component or epoch holds, naming that version: a Debian pool holds one content per file name |

Client config: `deb [signed-by=/etc/apt/keyrings/<repo>.asc] https://host/apt/<repo> <suite> <component>`.
Source packages (`dsc`, `Sources`) are after the MVP.

### maven

Implements the Maven 2 repository layout for release versions. SNAPSHOT versions are after the
MVP (they will be a repo option, not a separate format).

| Method and path | Behaviour |
| --- | --- |
| `GET /maven/<repo>/<groupPath>/<artifactId>/<version>/<file>` | artifact, POM, or checksum (`.sha1`, `.md5`, `.sha256`, `.sha512` computed at ingest and stored in the metadata layer) |
| `GET /maven/<repo>/<groupPath>/<artifactId>/maven-metadata.xml` (+ checksums) | rendered from the index: `latest`, `release`, `versions`, `lastUpdated` |
| `PUT /maven/<repo>/<groupPath>/<artifactId>/<version>/<file>` | adds the file to the publication session `(repo, g, a, v)` of the principal, opened by the first file; another principal's open session of the version is `409`. A checksum file (`.md5`, `.sha1`, `.sha256`, `.sha512`) is verified against the file it names, as the session computed it when the file arrived, and is never stored: `400` when it differs, or when its file was not uploaded first. `201`. |
| `PUT /maven/<repo>/<groupPath>/<artifactId>/maven-metadata.xml` (+ checksum PUTs) | **commit** of the principal's session of the version the document is for, its `release` (else `latest`), which mvn and Gradle set to the version they deploy; never another version it lists, whose session would be an unfinished deploy. Builds the manifest with all files, ingests, closes the session: `201`. Idempotent re-PUT of an identical version: `200`, also without a session when the version is in the index. Different bytes for an existing version: `409`, and the session is aborted. No session and no such version: `400`. The checksums of `maven-metadata.xml` are accepted and not checked, and the metadata of a group (plugin prefixes) is accepted and not kept: Lajstrom renders its own. |

A session expires 30 minutes after its last request, or after the repo's `maven.sessionTimeout`
(policy, #60, `1m` to `24h`), and its files are dropped: nothing is published but by the commit. There is no `seal`,
which would publish what had arrived, a version missing files (decided on #53). Client config:
`<repository>` with `<url>https://host/maven/<repo></url>` and a `<server>` entry with
user/token in `settings.xml`.

## 4. Management API `/api/v1`

JSON, bearer (PAT) or Basic. Shapes are fixed in the API issue (M2-09); listed here so frontends
do not invent their own. What each call needs, in the actions of rbac.md §2 (decided on #136,
2026-11-09):

| Calls | Needs |
| --- | --- |
| reading a repo's content: `GET /api/v1/repos` (lists only the repos the caller may read), `GET …/repos/<format>/<repo>`, and the read endpoints of packages, versions and search that #136 adds | `read` on the repo; a repo the caller may not read is `404`, as on the frontends (rbac.md §3) |
| changing a repo: `PATCH`, `DELETE`, `POST …/reindex`, `GET …/reindex`, the deploy tokens of the repo | `manage` on the repo |
| deleting a version: `DELETE …/repos/<format>/<repo>/packages/<name>/versions/<version>` (#61) | `delete` on the repo; a repo the caller may not read is `404` |
| creating a repo, and the system resources: users, bindings, audit, registries, every token | the admin role (rbac.md §2; a `maintainer` on `*/*` is not enough) |
| the personal access tokens of a user | the user themself, within what the credential allows, or the admin role |
| `GET /api/v1/whoami` | nothing |

| Resource | Endpoints |
| --- | --- |
| repos | `GET/POST /api/v1/repos`, `GET/PATCH/DELETE /api/v1/repos/<format>/<repo>`, `POST …/reindex`, `GET …/reindex` |
| versions | `DELETE /api/v1/repos/<format>/<repo>/packages/<name>/versions/<version>[?variant=]`; the reads come with #136 |
| users | `GET/POST /api/v1/users`, `GET/PATCH/DELETE /api/v1/users/<name>`, `POST …/password` |
| tokens | `GET/POST /api/v1/users/<name>/tokens` (PAT), `DELETE …/tokens/<id>`; `GET/POST /api/v1/repos/<format>/<repo>/deploy-tokens`, `DELETE …/<id>` |
| bindings | `GET/POST /api/v1/bindings`, `DELETE /api/v1/bindings/<id>` |
| audit | `GET /api/v1/audit?repo=&principal=&since=` |
| whoami | `GET /api/v1/whoami` |

Errors: `{"error": {"code": "NOT_FOUND|CONFLICT|FORBIDDEN|INVALID|UNAVAILABLE|INTERNAL", "message": "…"}}`.
A `429` (a throttle of failed logins, with `Retry-After`; #130) is `UNAVAILABLE`: try again
later. There is no seventh code.

Deleting a version (`delete`): `<name>` is the package name in the format's spelling, one path
segment (a raw path escapes its `/` as `%2F`). Without `variant` every variant of the version
goes, with it the one variant (`""`, the empty value, is the variant of a format that has none);
each manifest is deleted as the `DELETE` of §2 deletes it, its referrers first, with an audit row
of its own. `204`; `404 NOT_FOUND` when the repo has no such package, version or variant; a
backend that cannot delete manifests is `501 UNAVAILABLE` naming the backend, and nothing
changes. Versions deleted before a failure stay deleted.

Reindex of one repo (`manage`): `POST …/reindex` with an optional body `{"prune": true,
"verify": false}` starts the run on the replica that received it and answers `202` with the run
(`id`, `state`, `source`, `principal`, `replica`, `startedAt`), before the run reads the
registry; a running run that overlaps it, on any replica or in the CLI, is `409 CONFLICT` naming
where it runs and since when (index-schema.md, Coordination). `GET …/reindex` answers the latest
run that covered the repo, its own or a run of every repo, with `state`, the times, `error` and
the repo's part of the report (layout.md, `reindex.Report`); `404` when no run has covered it.
The run writes the audit event `repo.reindex` when it starts.

## 5. System endpoints

`/-/healthz` (process up), `/-/readyz` (DB reachable, every configured registry answered `/v2/`
within the last minute), `/-/version` (JSON: version, commit, date).

Prometheus metrics are not on the public host: `server.metricsListen` (default
`127.0.0.1:9464`, the OpenTelemetry Prometheus exporter convention) serves `GET /metrics`, and
`/debug/pprof/` when `server.pprof` is on. The edge never routes to it; Prometheus scrapes it
inside the network. Issue #23.
