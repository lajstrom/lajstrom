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
https://<host>/-/oidc/...               OIDC login: browser and device flows (§7, M6)
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
`X-Forwarded-For` only from `server.trustedProxies`, and reads no other forwarded header), or in
Lajstrom itself for a single host (`server.tls.certFile` and `server.tls.keyFile`; ACME after the
MVP). Plain HTTP to clients is for the dev stack on loopback only, behind an explicit
`server.insecureHTTP: true`.
`server.trustedProxies`, a list of CIDR prefixes (ADR 0011), came with #130; the TLS keys and
`server.insecureHTTP` came with the hardening issue (#50).

`server.externalURL` is the base for every absolute URL Lajstrom renders: the `WWW-Authenticate`
realm and npm's `dist.tarball` (§3, npm), so a new value needs npm's documents rendered again
(`lajstrom reindex`). Redirects and the `Location` of an upload are paths, the file links of the
PyPI simple index are relative, and apt `Release` is path-relative and needs none. Its host is the
`service` of the token flow, the `iss` and `aud` of every bearer token. It is required: only with
`server.insecureHTTP: true`, the dev stack, may it be empty (#248). Then the realm and the service
come from the request's `Host`, and the realm is `https://` when the request came over TLS
(`server.tls`), `http://` otherwise.

## 2. OCI facade `/v2`

Implements the OCI distribution spec against the backend through the provider, with the OCI
repository name `<format>/<repo>/<package path>` (the instance prefix is added by the provider and
never visible to clients).

A name whose `<package path>` has a component that is a file of a registry's image layout,
`index.json` or `oci-layout` (`profile.LayoutFileComponent`, [ADR 0017](../adr/0017-the-grammar-of-oci-names.md)),
is `400 NAME_INVALID` before the repo is looked up, like a name that is not the grammar: such a
name collides with another repository's layout and the backend answers every request of it a
permanent `5xx` (zot#4695), which would otherwise count against the registry's circuit breaker and
fail every other repo on it. `EscapeComponent` never writes these components, so no managed
package's name carries one.

| Method and path | Behaviour |
| --- | --- |
| `GET /v2/` | `200` if authenticated, else `401` with `WWW-Authenticate: Bearer realm="<externalURL>/v2/token",service="<host>"`. Every `/v2` answer has `Docker-Distribution-API-Version: registry/2.0`. |
| `GET /v2/token?service=&scope=repository:<format>/<repo>/<pkg>:pull,push` | Basic auth (user:password, user:PAT, or deploy token) → JWT bearer with the scopes the caller is allowed (rbac.md §4). Anonymous: scopes limited to `pull` on repos with anonymous read. |
| `HEAD/GET /v2/<name>/manifests/<ref>` | proxied to the backend, which is asked with the manifest media types of oras-go (OCI and Docker manifests and indexes); the answer has the manifest's own `Content-Type`, `Docker-Content-Digest` and `Content-Length`. The client's `Accept` converts nothing: a manifest of a type it did not list is returned all the same. `404 MANIFEST_UNKNOWN`. |
| `PUT /v2/<name>/manifests/<ref>` | **hook**: body is parsed, on every repo, an `oci` one included (`Content-Length` required, else `411 MANIFEST_INVALID`; at most `server.maxManifestSize`, default 4 MiB, else `413 MANIFEST_INVALID`; at most 1000 layers, else `400 MANIFEST_INVALID`); for a non-`oci` repo it must be an OCI image manifest that satisfies the manifest profile (an image index or a Docker manifest is `400 MANIFEST_INVALID` naming the media type the profile wants), its `org.opencontainers.image.created` must be at most `server.maxCreatedAhead` (5 minutes) after the server's clock and `server.maxCreatedBehind` (an hour) before it, unless the repo has the manifest already, else `400 MANIFEST_INVALID` naming the time and the clock (the time is refused, never replaced); every layer must be in the backend (`400 MANIFEST_BLOB_UNKNOWN` naming the digest; on a registry not classified `strong` a layer it does not serve is read again for `ingest.visibilityTimeout` first), and it is passed to `ingest.Ingest` with the `<path>` below the repo and the tag; on success the manifest is stored and indexed, `201` with `Location`, `Docker-Content-Digest` (also for the idempotent repeat of the same manifest) and, when the manifest has a `subject`, `OCI-Subject`. A push by digest is accepted when the annotations yield a tag, `Tag(lajstrom.version, lajstrom.variant)` (manifest-profile.md §4.2), which ingest sets in the backend; the `Location` names that tag; without `lajstrom.version` it is `400 MANIFEST_INVALID`. Errors: `413 SIZE_INVALID` for a file layer above the repo's `maxFileSize` (policy, #60), naming the limit; `400 MANIFEST_INVALID` with the rule that failed in `detail`, and, when the `artifactType` is oras's default `application/vnd.unknown.artifact.v1`, the hint to pass `--artifact-type application/vnd.lajstrom.<type>.v1` in the message; `409 TAG_INVALID` (tag exists with another digest, named in the message and in `detail.existing`; or the package path is another spelling's, named in the message, or is being published under another spelling: manifest-profile.md §4.1); `404 NAME_UNKNOWN` (repo not configured); `503 UNAVAILABLE` with `Retry-After: 30` when the registry took the manifest and did not serve it back within `ingest.visibilityTimeout`: nothing is indexed, the claim on the tag stays, and the same `PUT` again completes it; the message names what the registry does not serve yet, and so does `detail`: `name` (the `<name>` of the request), `digest` (the manifest's) and `missing`, with `manifest: true` when the manifest is not served by its digest, `tag` when the tag does not name it yet, `blobs` (the digests not served, at most the first 20 in digest order), `moreBlobs` (how many more blobs are not served, past those `blobs` names) and, for a referrer, `referrersOf` (the subject whose referrers do not list it), each only when it is missing (#565); the message, too, names at most 20 blobs and counts the rest, `… 981 more blobs` (#693). On an `oci` repo the manifest passes through unchanged (an OCI or Docker manifest or index media type that the body's `mediaType` does not contradict), by tag or by digest, and `oci_repos` records the repository; a push by tag holds the tag lock of the manifest (index-schema.md, Coordination). **Referrers** (manifest-profile.md §6): on a managed repo, a manifest with a `subject` and an `artifactType` that is not `application/vnd.lajstrom.*`, pushed by digest, or a manifest under cosign's legacy tag `sha256-<hex>.sig`, `.att` or `.sbom`; on an `oci` repo, any manifest with a `subject`. The facade checks `write` and the blobs (`400 MANIFEST_BLOB_UNKNOWN`) and hands it to `ingest.PutReferrer`, which indexes nothing: on a managed repo the subject must be a manifest of the repository and any other tag is refused (`400 MANIFEST_INVALID`), and the push runs under the referrers lock of the subject, the tag lock that the delete of a version holds, with the backend's referrers API or without: the subject is resolved again under it, so a referrer pushed while its subject is deleted is either deleted with it or refused after it (`400 MANIFEST_INVALID`, #806); on an `oci` repo the push runs under that lock without the referrers API (index-schema.md, Coordination). The answer is `201` with `Location`, `Docker-Content-Digest` and `OCI-Subject` for a manifest with a subject, whatever the backend sent. |
| `DELETE /v2/<name>/manifests/<ref>` | requires `delete`, checked before the reference is looked up; `ingest.Delete` (#61). On a managed repo a reference that names a version, its tag or its digest, deletes the version: its referrers first, each with its own before it (cosign's legacy manifests under `sha256-<hex>.sig`, `.att` and `.sbom`, the tag-schema index `sha256-<hex>`, the referrers the backend lists), then the manifest, then its rows in the index, its package's when it was the last version, and the repo's rendered indexes (`index_cache`), with one audit row `delete` that counts the referrers. Any other manifest of the repository (a referrer, a foreign manifest, any manifest of an `oci` repo) is deleted with its referrers and indexes nothing; the audit row is written all the same. On an `oci` repo, a delete that leaves the repository without a tag then drops its `oci_repos` row (#748). Without the backend's referrers API, the `sha256-<hex>` index of a referrer's subject is written again without it, under the referrers lock of the subject (index-schema.md, Coordination). A tag names the manifest it holds. On an `oci` repo a delete by tag of a manifest that other tags also name is `409 DENIED` naming them, and nothing changes (#284); by digest the manifest goes with every tag that names it: the distribution API has no untag. On a managed repo a manifest has one tag (manifest-profile.md §4.2). The delete counts the tags and deletes under the tag lock of the manifest (index-schema.md, Coordination), so that a tag pushed meanwhile is either counted or pushed after the delete. `202 Accepted`. A backend that cannot delete manifests (`Capabilities.DeleteManifest` false, or one that refuses the first delete: the first request that changes anything is a plain delete) is `405 UNSUPPORTED` naming the backend, and nothing changes; with `DeleteManifest` false, a reference that names a version whose OCI repository the backend does not know (its manifest and its tag list answer 404 without `MANIFEST_UNKNOWN`, reindex's `repositoryUnknown`) deletes the version's rows and writes its audit row, under the same locks and conditions, and nothing is written to the backend (#692). `404 MANIFEST_UNKNOWN` for a reference that names nothing, `400 DIGEST_INVALID` or `TAG_INVALID` for one that is neither; a delete the backend refuses for this manifest alone (Zot keeps a manifest that an image index lists) is `409 DENIED`. `DELETE …/blobs/<digest>` stays `405 UNSUPPORTED`: unused blobs are the backend's garbage collection's. Retention (`lajstrom admin retention`, #72) deletes through the same path, via `POST …/versions:delete` (§4). |
| `POST /v2/<name>/blobs/uploads/`, `PATCH`, `PUT …?digest=`, `GET`, `DELETE` | proxied upload session (monolithic and chunked, a single `POST ?digest=` too): Lajstrom drives the backend's own session and streams; it never buffers a blob on disk or in memory, unless the operator turns on `registries[].uploads.spoolUnknownLength` for that registry: then a chunk of unknown length (no `Content-Length`: chunked encoding) into a backend session that already holds bytes is spooled to a temporary file for one request, to learn its length and send it with `Content-Range`, bounded by the repo's `maxFileSize`, removed at once, never served from ([configuration.md](../configuration.md#registries)). It hashes the bytes as they pass and holds a blob's last byte back until the hash is known (#368), so that a blob whose bytes do not have the digest of its closing `PUT` or `POST ?digest=` never completes in the backend, whether the backend checks digests or not: a blob in one request is hashed in the algorithm of its digest, one in chunks in sha256, whose state the row keeps between chunks. A mount streams nothing and is not hashed. The upload id in the `Location` Lajstrom answers is opaque and does not depend on the replica: it is the key of a `sessions` row (kind `blob-upload`, one hour after the last request) that holds the backend's session, so any replica continues it; the backend's location never reaches the client. `Range` is what the backend holds. `?mount=&from=` is passed to the backend when `from` is in the same repo, or in another repo of the same registry that the principal may read; otherwise, and without `from`, the answer is a plain session. Errors: `404 BLOB_UPLOAD_UNKNOWN` (unknown, expired, another repo's or principal's id; a chunk after which the backend holds bytes the hash did not see, part of an earlier chunk that failed: the session ends), `416 BLOB_UPLOAD_INVALID` (a `Content-Range` that does not start at the offset), `413 SIZE_INVALID` (a blob above its bound, the repo's `maxFileSize`, else its format's default, `oci` 16 GiB (manifest-profile.md §5), or the registry's `capabilities.maxBlobSize` when that is smaller, the message naming the bound: a declared length, `Content-Length` or `Content-Range`, that would carry the session past it, before anything streams, or a body of unknown length cut where the bytes the session holds pass it, counted across its `PATCH`es and the closing `PUT`; the session ends), `429 TOOMANYREQUESTS` with `Retry-After` (#246: a new session, a `POST` without `?digest=` or a mount that does not mount, while the principal holds `server.uploadSessions.perRepo` live sessions of the repo, 256 by default, or `server.uploadSessions.perPrincipal` live sessions of all repos, 1024 by default, on the replica that answers; the message names the bound and its key, and `0` turns a bound off. A session is live while a request on it runs and for 5 minutes after the last one ended; an idle session takes no slot and stays resumable until it expires, and a request on a session that exists is never refused for the bound. The `DELETE` of a session, its closing `PUT` and an error that ends it free its slot at once. `Retry-After` is the seconds until one of the sessions counted stops counting, at most 5 while a request on one of them runs. Per replica, as the throttles of failed logins: a replica counts the sessions it has answered a request of, and one that another replica ended counts until the bound is reached and the index says it is gone. The anonymous principal opens none: it only reads, rbac.md §1), `400 DIGEST_INVALID` (the bytes do not have the digest, the backend refuses it, a chunked upload is closed with a digest that is not sha256, or the backend holds bytes the hash did not see: the session ends), `503 UNAVAILABLE` with `Retry-After: 5` (#427: a chunk that `uploads.spoolUnknownLength` spools would pass `server.spoolBudget`, the bytes the spooled chunks of the replica may hold at once; nothing of it reached the backend, and the session keeps its offset). A closing `PUT` that fails once its last chunk has begun to stream ends the session too. |
| `HEAD /v2/<name>/blobs/<digest>` | the backend's `HEAD` (`Provider.StatBlob`, redirects followed): `200` with `Content-Length`, `Docker-Content-Digest`, `ETag` (the digest, quoted), `Accept-Ranges` when the backend says so; `404 BLOB_UNKNOWN`. `If-None-Match` with the `ETag` is `304` without a request to the backend. |
| `GET /v2/<name>/blobs/<digest>` | one request to the backend (`format.ServeBlob`, `Provider.OpenBlob`), with the client's `Range` passed through when it is one byte range: `307` with `Location` and `Docker-Content-Digest` when the backend redirects to another origin (a presigned URL of its object storage) and the repo's `policy.blobRedirect` is not `never` and the registry's `capabilities.blobRedirect` not `false`; otherwise the backend's answer streamed, `200`, `206` with `Content-Range` or `416`, with `Content-Length` and `Accept-Ranges` as the backend sent them, `Docker-Content-Digest` and `ETag` (the digest, quoted). A redirect into the backend's own origin is followed by the provider, never sent to the client. Several ranges, or a `Range` that is not one, are not sent: the answer is the whole blob with `200` (backends answer several ranges with `multipart/byteranges`). `If-None-Match` is `304` before any request. `404 BLOB_UNKNOWN`. |
| `GET /v2/<name>/tags/list` | from the index for Lajstrom-managed repos (fast, consistent): the versions whose manifest is at the path (`index.ManifestPath`), those of the package whose `name_oci` is the path or, in a split, those of the package at another form of the path that reindex found there (index-schema.md, Invariants and rules; #642), an empty list for a repository only `oci_repos` knows; from the backend for `oci` repos. `n`, `last` and a `Link` header (`rel="next"`) paginate both. |
| `GET /v2/<name>/referrers/<digest>` | `read` on the repo, checked before the digest. The manifests whose `subject` is `<digest>`, as an image index (`Content-Type: application/vnd.oci.image.index.v1+json`), from `Repository.Referrers` of the provider: the backend's referrers API, or the `sha256-<hex>` tag schema where it has none (`Capabilities.Referrers`). `?artifactType=` keeps the referrers of that type and the answer carries `OCI-Filters-Applied: artifactType`. A subject without referrers, in a repository the backend does not have too, is `200` with an empty index, never `404`. A malformed digest is `400 DIGEST_INVALID`. |
| `GET /v2/_catalog` | from the control plane, filtered by RBAC: the OCI repositories of every repo the principal may read, as `<format>/<repo>/<path>`, with `n`, `last` and `Link`. For a managed repo they are the package paths of its packages (`packages.name_oci`), the repositories whose `tags/list` has a version (the other form of a split is not listed, #642): a repository whose versions are all deleted is not listed, though the registry may keep it and `oci_repos` with it (Zot does), and neither is one that holds foreign manifests only (#728). An `oci` repo indexes no tags: its repositories are its `oci_repos`, those that hold a tag, so one whose manifests are all deleted is not listed either, though Zot keeps it with an empty tag list: the delete that takes its last tag drops its row, and so does reindex (index-schema.md, Invariants and rules; #748). An `oci` group lists, to its readers, the repositories of each member that its routes send to that member (§6) |

A request that the registry refuses with a `4xx` that none of the errors above names keeps
that status for the client: one of a blob upload (any of the upload rows, #449), such as
Gitea's `400 BLOB_UPLOAD_INVALID` for a chunk of unknown length after the first write, JFrog's
`416` for it, a `413` of a proxy in front of the registry; the manifest push of an `oci` repo,
a blob or manifest `GET` or `HEAD`, and `tags/list` of an `oci` repo (#495). The code is the
registry's when it is `BLOB_UPLOAD_INVALID`, `DIGEST_INVALID`, `NAME_INVALID`, `SIZE_INVALID`,
`MANIFEST_INVALID` or `MANIFEST_BLOB_UNKNOWN`; else `SIZE_INVALID` for the `413` of a blob
upload; else the code of the route's own errors: `BLOB_UPLOAD_INVALID` for an upload,
`MANIFEST_INVALID` for a manifest push, `BLOB_UNKNOWN`, `MANIFEST_UNKNOWN` or `NAME_UNKNOWN`
for a read. The message names the registry by its `registries[].name`, with the status and the
code, and nothing else of it: no URL, realm, credential or message of the registry's own; when
the vendor table knows the fix, a hint follows (`registries[].uploads.spoolUnknownLength` for
Gitea, Forgejo and JFrog). A manifest push of an `oci` repo that the registry refuses with
`MANIFEST_BLOB_UNKNOWN`, on a registry not classified `strong` still after the retries
(guarantees.md INV-4), then names the first blob of the manifest that the registry does not
have: in the message, after the code, as the hook names a layer of a managed repo, and in
`detail.digest` (#850). Lajstrom sends one `HEAD` per distinct blob, the config and the layers in
order, or the manifests of an index (its first 1000), but not a layer with `urls`, which the
registry need not hold, and stops at the first the registry does not have. When it has them
all, or a `HEAD` fails otherwise, the message is the registry's name, status and code alone.
The refusal is logged at warn. An upload session stays, as after any
chunk that fails, except when the registry
no longer knows it (`404`, or `BLOB_UPLOAD_UNKNOWN`: `404 BLOB_UPLOAD_UNKNOWN`) or refuses the
digest (`400 DIGEST_INVALID`): then it ends. A `429` or `503` of the registry is
`503 UNAVAILABLE` with the registry's `Retry-After`, `1` when it sent none, as an open circuit
breaker is and as on the proxy routes: the registry throttles Lajstrom's own credential, which
every client shares, not this client, and docker retries a `503`. Lajstrom's own bound of a
principal, its open upload sessions (#246), is `429 TOOMANYREQUESTS`: there the client itself
is over its limit. A registry that refuses Lajstrom's credentials (`401`, `403`,
`UNAUTHORIZED`, `DENIED`) is `502`, and any other `5xx` of the registry stays a `5xx`, with the
status text only.

Mount of a package under a repo: the name after `/v2/` is split as `<format>/<repo>/<rest>`; the
repo must exist in the control plane, else `404 NAME_UNKNOWN`. Auto-creation of repos on push is
not supported (an explicit `POST /api/v1/repos` is one call). A manifest `PUT` and the `POST` that
starts a blob upload are `400 NAME_INVALID` when the registry does not take the OCI repository
name, before anything goes to it (`provider.Capabilities.CheckName`): a name with more components
below the instance prefix than `capabilities.maxNameDepth` (#599), or longer than the registry
takes, 255 bytes with the instance prefix on distribution and most registries, 256 on ECR
(`MaxNameLength`, #661). The message names the limit, not the prefix.

An `oci` repo takes any manifest and blob as they come, within the bounds of the rows above (a
manifest's length and layers, a blob's size), and indexes nothing beyond its repository (the OCI
distribution-spec conformance suite runs against one, `test/conformance/`). Docker image
passthrough with what it needs beyond that (recognising images for the UI, multi-arch stitching) is
after the MVP; in the MVP only the paths above are exercised, with `oras` and `cosign` as the
OCI-native clients tested (`test/e2e/oras.sh`, `cosign.sh`).

## 3. Protocol frontends (MVP formats)

Authentication on all frontend endpoints: HTTP Basic with a user password, a PAT or a deploy
token, the same credentials as `/v2/token`; or `Authorization: Bearer` with a PAT or a deploy
token, which every route but `/v2` takes (npm's `_authToken` sends it). Anonymous `GET` on repos
with `anonymousRead: true`.
Writes require `write` on `<format>/<repo>`.

A request that Lajstrom sends the registry for a frontend and that the registry refuses with a
status no other rule names (#494; `format.StatusOf` is the one table): a `413` stays `413`, the
file is larger than the registry takes; a `429` or `503` is `503` with the registry's
`Retry-After`, `1` when it sent none, as an open circuit breaker is: the registry throttles
Lajstrom's own credential, which every client shares, not this client; any other `4xx`, such as
a `400 MANIFEST_INVALID` for a manifest Lajstrom built, is `502`: the client sent its file, not
the request the registry refused. A `5xx` says its status text only, and no answer carries the
registry's URL, realm or message.

### raw

| Method and path | Behaviour |
| --- | --- |
| `GET /raw/<repo>/<path…>` | newest version of the file at `<path>` (the most recently created), read as the blob `GET` of the facade (§2, `format.ServeBlob`): one request to the backend with the client's `Range`, `307` to the backend's storage when it redirects and `policy.blobRedirect` allows it, else the backend's `200`, `206` or `416` streamed. `ETag` is the blob digest (`If-None-Match` is `304` without a request), `Last-Modified` the version's `created`, `Docker-Content-Digest` the manifest digest. `?digest=sha256:…` pins a version. `Content-Type` from the metadata layer or sniffed. A `PUT` of bytes an older version already holds is the idempotent repeat of that version and does not make it the newest: to roll back, `DELETE` the newer version or pin `?digest=`. |
| `HEAD` | same headers, from the backend's `HEAD`; no body, never a `307` |
| `PUT /raw/<repo>/<path…>` | upload one file (body). Idempotent for identical bytes. Creates a new version (tag) for different bytes; the path now resolves to it. `201` with `Location`, `Docker-Content-Digest` and a JSON body `{"path", "version", "digest", "size", "created"}` (`digest` the manifest's, as `Docker-Content-Digest`). `409` when the path exists in another spelling (the message names it). |
| `DELETE /raw/<repo>/<path…>` | requires `delete`; removes all versions of the path, each as the `DELETE` of its manifest on `/v2` deletes it, its referrers with it (after the MVP: one version). `204`; `501` when the backend cannot delete manifests; a failure after the first version names the versions deleted before it |
| `GET /raw/<repo>/` and any directory prefix with trailing `/` | JSON listing (`{"entries":[{"path","size","digest","created"}]}`), recursive: one entry per version of every path below the prefix, at any depth, sorted by path and then `created`; `digest` is the manifest digest, the value `?digest=` takes. HTML after the MVP |

Model: package = path, version = first 16 hex of the content sha256, variant none. `oras push`
to `/v2/raw/<repo>/<escaped path>:<version>` works with the same annotations.

### pypi

Implements PEP 503 (simple index HTML), PEP 691 (JSON simple API, content negotiation), PEP 658
metadata files, and the legacy upload API that `twine` uses.

| Method and path | Behaviour |
| --- | --- |
| `GET /pypi/<repo>/simple/` | project list |
| `GET /pypi/<repo>/simple/<project>/` | file list of the project (normalised name; non-normalised redirects `301`). Links point to `/pypi/<repo>/files/…` with `#sha256=`; `data-requires-python` set, and `data-dist-info-metadata`/`data-core-metadata` (in JSON `core-metadata`/`dist-info-metadata`) on a wheel only: a build may change an sdist's `PKG-INFO` (PEP 643 `Dynamic`), and PEP 658 is for metadata the processing does not modify, as PyPI does. |
| `GET /pypi/<repo>/files/<project>/<version>/<filename>` | the distribution file (`307` or stream) |
| `GET /pypi/<repo>/files/<project>/<version>/<filename>.metadata` | the core metadata of a wheel (PEP 658), from the metadata layer; `404` for an sdist |
| `POST /pypi/<repo>/` | legacy upload: `multipart/form-data` with `:action=file_upload`, `protocol_version=1`, `content`, metadata fields. One file = one manifest (variant = the wheel's build tag, when it has one, and tag set, or `sdist`: manifest-profile.md §4.2). `200` on success; `400` on validation errors; `409` if the file name exists with a different digest, or another file holds its version and variant (a wheel with a build tag uploaded before #726, in the earlier form of its variant), with `File already exists` and the name of the file there. |

Client config: `pip install --index-url https://host/pypi/<repo>/simple/ …`,
`twine upload --repository-url https://host/pypi/<repo>/ dist/*`.

### apt

Implements the Debian repository format (`Release`, `InRelease`, `Packages`) for binary packages.
Suites and components are upload metadata (annotations), not separate repos.

| Method and path | Behaviour |
| --- | --- |
| `GET /apt/<repo>/dists/<suite>/InRelease` | clearsigned Release (per-repo GPG key) of the bytes of the `Release` of the same render; from `index_cache`, made on the first `GET` after the render and kept beside the `Release`. Signed by the repo's active key and, for `keys.retireGrace` after a rotation, by the retired key too. `404` when the server has no master key (`keys.masterKeyEnv` or `keys.masterKeyFile`): the `Release` is unsigned then, and clients mark the source `[trusted=yes]` |
| `GET /apt/<repo>/dists/<suite>/Release`, `Release.gpg` | `Release`: rendered with every `Packages` of the suite in one render on the first `GET` after a publish or a delete (both invalidate the repo's documents) and kept with them in `index_cache`, so its hashes are those of the files served. `Origin: Lajstrom`, `Label` the repo, `Suite` and `Codename` the suite, `Date`, `Architectures`, `Components`, `Acquire-By-Hash: yes`, and `MD5Sum`, `SHA1` and `SHA256` of every `Packages`, `.gz` and `.xz`. `Release.gpg`: the armored detached signatures of the same `Release` bytes, by the keys that sign `InRelease`; `404` without a master key. Suites, components and architectures come from the versions (`lajstrom.deb.suite`, `.component`, `.arch`), with no configuration; a suite without versions is `404` |
| `GET /apt/<repo>/dists/<suite>/<component>/binary-<arch>/Packages`, `Packages.gz`, `Packages.xz` | package index: the versions of the suite and component whose architecture is `<arch>` or `all`, sorted by name and version. The architectures of a suite are the architectures of its versions, `all` among them when it has `all` packages: `binary-all` holds the `all` packages alone, and they are also in the index of each concrete architecture, which apt merges without a warning. apt on a host of an architecture the suite has no package for reads `binary-all` alone, so the `all` packages install there (#725); the `Release` has no `No-Support-for-Architecture-all`, with which apt skips `binary-all`. A suite with `all` packages only has `binary-all` alone. Every component has an index for every architecture of the suite, empty when it has no package for it. `by-hash/SHA256/<hash>` serves the `Packages`, `.gz` or `.xz` of that directory whose SHA256 the current `Release` lists (apt uses it when `Acquire-By-Hash: yes`); a hash of an older render is `404`, and apt falls back to the file by name. A path the `Release` does not list is `404` |
| `GET /apt/<repo>/pool/<component>/<p>/<source>/<filename>` | the `.deb`, the `Filename` of its `Packages` paragraph. The directory is the source package's, as the Debian archive lays out a pool: `<source>` is the name of the control file's `Source` field, or the package name without one, and `<p>` its first letter, or its first four when it starts with `lib` (`libf/libfoo/libfoo2_2.1-3_amd64.deb`); `<filename>` is `<package>_<version without the epoch>_<arch>.deb`. Resolved by file name: the versions of the component that hold the file, one per suite, whose `<p>/<source>` is the path (else `404`; a version without metadata in the index is taken on the file name), read as the blob `GET` of the facade (§2, `format.ServeBlob`): `307` or stream, `Range`, `ETag` the blob digest, `Last-Modified` and `Docker-Content-Digest` those of the newest such version, `Content-Type: application/vnd.debian.binary-package`; `HEAD` from the backend's `HEAD`. The same file in two suites of a component is one URL, in two components two URLs for one blob. Versions that hold one file name with two blobs are `500`, logged with both digests for the operator: the upload refuses another content under a file name, an `oras push` cannot be checked |
| `GET /apt/<repo>/key.asc` | ASCII-armored public key of the repo (`signed-by=`): one public key block with the active key, made on the first request when the repo has none, and the keys retired within `keys.retireGrace`; `application/pgp-keys`, not cached. `404` without a master key |
| `PUT /apt/<repo>/upload?suite=<suite>&component=<component>[&arch=<arch>]` | convenience upload of one `.deb` (body). Equivalent to `oras push` with the deb profile: the deb plugin builds the manifest from the control file and the query. `suite` and `component` are required, lowercase letters, digits, `.`, `+` and `-`, the first a letter or a digit (`400` naming the one missing or malformed); `arch`, when given, must be the control file's `Architecture`. A body above the repo's `maxFileSize` is `413`, one that is not a `.deb` `400`. The body streams into the registry ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)): its head, up to the end of the control member, is read first for the package name, so a control member over 4 MiB is `400` before the rest is read; a refused upload stores nothing. `201` with `Location` (the file in the pool), `Docker-Content-Digest` (the manifest digest) and a JSON body `{"package", "version", "architecture", "suite", "component", "filename", "digest", "tag"}`. The same bytes again are the idempotent repeat, `201` with the same digest; the same bytes in another suite or component are a new version over the same blob. `409` for other bytes under the same name, version and architecture in the same suite and component (the tag holds another manifest), and for other bytes under a pool file name that a version of another suite, component or epoch holds, naming that version: a Debian pool holds one content per file name |

Client config: `deb [signed-by=/etc/apt/keyrings/<repo>.asc] https://host/apt/<repo> <suite> <component>`.
Source packages (`dsc`, `Sources`) are after the MVP.

### maven

Implements the Maven 2 repository layout for release versions. SNAPSHOT versions are after the
MVP (they will be a repo option, not a separate format).

| Method and path | Behaviour |
| --- | --- |
| `GET /maven/<repo>/<groupPath>/<artifactId>/<version>/<file>` | artifact, POM, or checksum (`.sha1`, `.md5`, `.sha256`, `.sha512` computed at ingest and stored in the metadata layer) |
| `GET /maven/<repo>/<groupPath>/<artifactId>/maven-metadata.xml` (+ checksums) | rendered from the index: `latest`, `release`, `versions`, `lastUpdated` |
| `PUT /maven/<repo>/<groupPath>/<artifactId>/<version>/<file>` | adds the file to the publication session `(repo, g, a, v)` of the principal, opened by the first file; another principal's open session of the version is `409`, and so is a `groupId:artifactId` whose package path another spelling holds (the message names it; manifest-profile.md §4.1), before the file is stored. A session holds at most the repo's `maven.maxSessionFiles` files, 256 by default ([api.md](../api.md#policy)): another file is `400` naming the bound and that key, before it is stored, while a file of the session sent again replaces it (#246). The bound counts the files the session stores, signatures (`.asc`, `.asc.asc`) and `.sigstore.json` included; a checksum file, of any file (a `.asc` or a `.module` too), and `maven-metadata.xml` with its checksums are never stored and never counted. It is a bound of one session, not of the version: a deploy of a version that exists opens a session of its own, which commits as the idempotent repeat when its files are the version's and is `409` otherwise. A checksum file (`.md5`, `.sha1`, `.sha256`, `.sha512`) is verified against the file it names, as the session computed it when the file streamed in, and is never stored: `400` when it differs, or when its file was not uploaded first. One that differs also keeps the session from committing until its file is uploaded again: mvn logs the `400` of a checksum and goes on to `maven-metadata.xml`, which is then `400` naming the file, and nothing is published. The file streams into the registry ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)): nothing of it is written to a local disk, and one refused while it arrives (`maxFileSize`, a body shorter than its `Content-Length`) is not stored. `201`. |
| `PUT /maven/<repo>/<groupPath>/<artifactId>/maven-metadata.xml` (+ checksum PUTs) | **commit** of the principal's session of the version the document is for, its `release` (else `latest`), which mvn and Gradle set to the version they deploy; never another version it lists, whose session would be an unfinished deploy. Builds the manifest with all files, ingests, closes the session: `201`. Idempotent re-PUT of an identical version: `200`, also without a session when the version is in the index. Different bytes for an existing version: `409`, and the session is aborted. No session and no such version: `400`. The checksums of `maven-metadata.xml` are accepted and not checked, and the metadata of a group (plugin prefixes) is accepted and not kept: Lajstrom renders its own. |

A session expires 30 minutes after its last request, or after the repo's `maven.sessionTimeout`
(policy, #60, `1m` to `24h`), and its files are dropped: nothing is published but by the commit. There is no `seal`,
which would publish what had arrived, a version missing files (decided on #53). Client config:
`<repository>` with `<url>https://host/maven/<repo></url>` and a `<server>` entry with
user/token in `settings.xml`.

### npm (M4, ADR 0012)

The npm registry API as npm 10 and 11, pnpm 9 and 10 and yarn 4 use it (the survey of ADR
0012), for a hosted repo; a proxy and a group repo answer the same routes as ADR 0013 says, a
group passing its writes to its hosted member. `<name>` is a package name: `lodash`, or a
scoped name as one segment `@scope%2fname` (`%2F` too) or as two, `@scope/name`; a segment that
begins with `@` is a scope. Every answer is JSON, an error `{"error": "<message>"}`; the denials
of rbac.md §3 hold (`401` with `WWW-Authenticate: Basic realm="lajstrom"` to anonymous, `404`
without access, `403` without the action). Credentials: Basic as on every frontend, and
`Authorization: Bearer` with a personal access token or a deploy token, which `_authToken` in
`.npmrc` sends (rbac.md §1).

| Method and path | Behaviour |
| --- | --- |
| `GET /npm/<repo>/<name>`, `HEAD` | `read`. The packument (below), from `index_cache`: the abbreviated one, `Content-Type: application/vnd.npm.install-v1+json`, when `Accept` names that type with a `q` above 0 and the query has no `write=true`; else the full one, `application/json` (npm 10 and 11 install with it, and yarn 4). `ETag` the document's sha256, `If-None-Match` is `304`, `Vary: Accept`, `Cache-Control: no-cache` (clients revalidate, and see a publish at once). `404` for a package the repo does not have |
| `GET /npm/<repo>/<name>/<version>` | `read`. One version of the full packument; a dist-tag in place of the version resolves through the map. `404` |
| `GET /npm/<repo>/<name>/-/<file>`, `HEAD` | `read`. The tarball of the package's version whose tarball layer is titled `<file>` (`widgets-1.2.0.tgz`), read as the blob `GET` of the facade (§2, `format.ServeBlob`): `307` to the backend's storage when it redirects and `policy.blobRedirect` allows it, else the backend's `200`, `206` or `416` streamed. `ETag` the blob digest, `Last-Modified` the version's `created`, `Docker-Content-Digest` the manifest digest, `Content-Type: application/octet-stream`. `404` for a file no version has |
| `PUT /npm/<repo>/<name>` with `_attachments` | publish, `write` (below). `201 {"ok": true}` |
| `PUT /npm/<repo>/<name>` without `_attachments`, `PUT /npm/<repo>/<name>/-rev/<rev>` | a packument update: npm's `deprecate`, and the first step of its unpublish of a version (below). `201 {"ok": true}` |
| `DELETE /npm/<repo>/<name>/-rev/<rev>` | unpublish of the package, `delete`. `<rev>` must be the current `_rev`, else `409`. Every version is deleted as the `DELETE` of its manifest on `/v2` deletes it (§2, #61: its referrers first, an audit row each), then every journal entry, under the journal lock (manifest-profile.md §8). `200 {"ok": true}`; a backend that cannot delete manifests is `501` and nothing changes; versions deleted before a failure stay deleted, and the error names them |
| `DELETE /npm/<repo>/<name>/-/<file>/-rev/<rev>` | the last step of npm's unpublish of a version, `delete`, at the path of `dist.tarball`: the version whose tarball is `<file>`, when it is still there, is deleted as on `/v2`. `200 {"ok": true}` either way; `<rev>` is not checked (npm read the packument again just before) |
| `GET /npm/<repo>/-/package/<name>/dist-tags` | `read`. The packument's `dist-tags` |
| `PUT /npm/<repo>/-/package/<name>/dist-tags/<tag>` | `write`. The body is a JSON string, a version of the package; a malformed tag (manifest-profile.md §8) or a version the package does not have is `400`. A journal `add`, none when the tag names the version already. `201 {"ok": true}` |
| `DELETE /npm/<repo>/-/package/<name>/dist-tags/<tag>` | `write`. A journal `rm`; `latest` is `400`, a tag the package does not have `404`. `200 {"ok": true}` |
| `GET /npm/<repo>/-/ping` | `read`. `200 {}` |
| `GET /npm/<repo>/-/whoami` | `read`. `200 {"username": "<name>"}`: the user's name, `token:<id>` for a deploy token; `401` to anonymous, on a repo with anonymous read too |
| `PUT /npm/<repo>/-/user/org.couchdb.user:<name>` | the legacy login of `npm login`, `npm adduser` and `yarn npm login` (below) |
| `DELETE /npm/<repo>/-/user/token/<token>` | `npm logout`: when `<token>` is the request's own personal access token, it is revoked, with an audit row; `200 {"ok": true}`. Any other token is `404` |
| `POST /npm/<repo>/-/v1/login` | `404`: no web login in M4. npm then prompts for the user name and password and logs in with the legacy route |
| `POST /npm/<repo>/-/npm/v1/security/advisories/bulk`, `…/audits`, `…/audits/quick` | `read`. `npm audit`, `yarn npm audit` and the audit of `npm install` (bulk), `pnpm audit` (the two others); below |
| `GET /npm/<repo>/-/v1/search?text=&size=&from=` | `read`. `npm search`; below |

Everything else under `/npm/<repo>/-/` (`npm token`, `access`, `owner`, `team`, `star`, `profile`)
is `404`. Every journal entry (manifest-profile.md §8) is an audit row of its own, action
`npm.dist-tag` or `npm.deprecate`, with the entry's digest.

**Packument.** Rendered from the package's versions, their metadata layers
(manifest-profile.md §2) and its `journal_state` (index-schema.md, npm), in one render with
both documents, scope the package name, kinds `packument` and `packument-abbreviated`.

- Full: `_id` and `name`, `_rev`, `dist-tags`, `versions`, `time`, and from the version that
  `latest` names `description`, `keywords`, `license`, `homepage`, `repository`, `bugs`,
  `author` and `readme`. A version is its metadata's `manifest` with `_id` (`<name>@<version>`),
  `dist` (`tarball`, `shasum`, `integrity`), `deprecated` when the state has a message for it,
  `_hasShrinkwrap` and `hasInstallScript`. `time` has `created` (the first version's),
  `modified` (the newest version's or journal entry's) and each version's `created`, which yarn
  4's age gate reads.
- Abbreviated: `name`, `modified`, `dist-tags` and `versions`, each version with the fields of
  the registry API's abbreviated form that it has: `name`, `version`, `deprecated`,
  `dependencies`, `optionalDependencies`, `devDependencies`, `peerDependencies`,
  `peerDependenciesMeta`, `bundleDependencies`, `acceptDependencies`, `bin`, `directories`,
  `engines`, `cpu`, `os`, `libc`, `funding`, `dist`, `_hasShrinkwrap`, `hasInstallScript`.
- `dist.tarball` is absolute, `<server.externalURL>/npm/<repo>/<name>/-/<file>` with the scope's
  `/` as it is (`…/npm/internal/@acme/widgets/-/widgets-1.2.0.tgz`), `<repo>` the repo asked (a
  group's own, ADR 0013). A new `externalURL` needs the documents reset (`reindex`).
- `dist-tags`: the state's tags whose version the package has. Without such a `latest`, `latest`
  is the highest version by SemVer precedence that is not a prerelease, else the highest. A
  version without its metadata layer (a degraded row) is `{name, version, dist}` from its
  annotations.
- `_rev` is `<seq>-<hex>`: the package's highest journal `seq` (`0` without one) and the first 32
  hex digits of the sha256 of its versions' manifest digests, sorted, one per line. Every
  publish, delete and journal entry changes it, and every replica computes the same.

**Publish.** npm's body (ADR 0012): `name`, `dist-tags`, `versions` with one version document,
`_attachments` with its tarball in base64. The tarball is decoded as it streams into the
registry (ADR 0014), never held whole in memory nor written to a disk, and its upload session
is closed only once the rest of the body, whatever the order of its members, is read and
checked: a body refused stores nothing. The rest of the body is at most 4 MiB, and the repo's
`maxFileSize` applies to the tarball (`413` for either). `400`, naming the rule, for:

- a body `name` that is not `<name>`; not exactly one version and one attachment (npm's
  provenance bundle, a second attachment, is refused in M4); a version document whose `name` and
  `version` are not the body's;
- a version document that differs from the tarball's `package.json` in `dependencies`,
  `optionalDependencies`, `peerDependencies`, `bundleDependencies` (or `bundledDependencies`),
  `bin`, or the scripts `preinstall`, `install` and `postinstall` (compared as JSON values, a
  missing key equal to an empty one, after npm's three normalisations of the tarball's file:
  a string `bin`, `directories.bin`, and `binding.gyp` without an install script; ADR 0012),
  naming the key: the packument never says other than what an install of the tarball runs;
- a name npm refuses for a new package: more than 214 bytes, an upper-case letter, a leading `.`
  or `_`, a character that `encodeURIComponent` changes (the `/` of a scope aside),
  `node_modules`, `favicon.ico`;
- a version that is not SemVer 2.0.0, or has build metadata (npm strips it, so no two versions
  differ by it alone);
- an attachment whose `length` is not its decoded size; a `dist.integrity` or `dist.shasum` of
  the document that the tarball does not have;
- a tarball that is not a gzipped tar with one top directory holding a `package.json` of the
  same `name` and `version` (read once, whole, bounded as an sdist is, #243);
- a dist-tag in `dist-tags` that is malformed (manifest-profile.md §8) or names another version.

The version is ingested as one manifest of the profile (claims, push, confirm, an audit row
`push`); the same tarball again is the idempotent repeat, another under a published version
`409`. Then each tag of the body's `dist-tags` (`latest` when it has none) that does not name the
version yet is a journal `add`, so a repeat after a failed append completes it. `access` is
ignored: who reads a package is the repo's RBAC.

**Packument update.** The whole packument, as read with `?write=true` and changed. Its `_rev`,
the URL's `-rev/<rev>` or else the body's, must be the current one: `409` otherwise (the package
changed; read it again), `400` without one. Against the state of that `_rev`:

- a version missing from `versions` is deleted as on `/v2` (`delete`);
- each difference in `dist-tags` is an `add` or an `rm` (`write`); a body without `latest` leaves
  it as it is, and a tag naming a version that is not in the body is `400`;
- each difference in a version's `deprecated` (a message, or `""` or nothing for none) is a
  `deprecate` or an `undeprecate` (`write`);
- a version in the body that the package does not have is `400`: a version comes with its
  tarball. Every other field (`maintainers`, `readme`, `time`, …) is ignored.

The permissions are checked before anything changes; the deletes go first, an audit row
`delete` each, then the journal entries in one append.

**Login.** The body is `{"name", "password", …}` and `<name>` must be its `name` (`400`). The
password is checked as a Basic password is, throttled as failed logins are (`429` with
`Retry-After`); the request's `Authorization` header is ignored, as npm sends the token it had,
revoked or not, and the request is decided as the user of the body, never as anonymous. A wrong
password, an unknown or a disabled user are `401` alike, and nobody is created, whatever
`npm adduser` asks. A user that may not read the repo gets `404` (rbac.md §3). Otherwise the
answer is a new personal access token of the user, not narrowed, with the default expiry, named
`npm login <repo> <date>`, with an audit row:
`201 {"ok": true, "id": "org.couchdb.user:<name>", "token": "<secret>"}`. npm and yarn store it as
their auth token; `GET /api/v1/users/<name>/tokens` lists it.

**Audit.** Three routes, each a `POST` with a JSON body: `-/npm/v1/security/advisories/bulk`,
npm's bulk advisories, `{"<name>": ["<version>", …]}`, which npm 10 and 11 and yarn 4 send (npm
gzipped, for `npm install` too); `-/npm/v1/security/audits`, pnpm 9's, and
`-/npm/v1/security/audits/quick`, pnpm 10's, both with npm 6's dependency tree. The body,
gzipped when `Content-Encoding: gzip` says so, is at most 16 MiB decoded (`413`).

- Hosted: no advisory is known for its packages. Bulk: `{}`; the two others: the empty report,
  `{"actions": [], "advisories": {}, "muted": [], "metadata": {"vulnerabilities": {"info": 0,
  "low": 0, "moderate": 0, "high": 0, "critical": 0}, "dependencies": 0, "devDependencies": 0,
  "optionalDependencies": 0, "totalDependencies": 0}}`.
- Proxy: forwarded through the core (layout.md, `Proxy.Forward`; proxy-and-group.md §9.2): a
  `POST` of the same path below `upstreams[].url`, with the body decoded, `Content-Type` and
  `Accept` `application/json`, and the upstream's credentials, never a header of the
  client's. A `2xx` is answered with its status and its body as the upstream sent it,
  `Content-Type: application/json`, and the upstream's `npm-notice`, which npm prints
  (npmjs.org announces with it that the two older routes retire); no other header of the
  upstream's. Otherwise the statuses of proxy-and-group.md §6, with npm's error body, never
  the upstream's own: the upstream's `404` or `410` is `404`; its
  `400`, `413`, `415` or `422`, the body refused, `400`; its `429` `503` with `Retry-After`; a
  timeout `504`; anything else `502`. An offline proxy (§4.4 there) sends nothing and answers
  `404`. A name that the proxy's `deny` refuses (proxy-and-group.md §3) never leaves the server
  (#514): the bulk request is sent without those names, and is `{}` without a request when no
  name is left; nothing in the answer tells which names were left out. npm 6's tree (the two
  other routes) cannot lose a package without what hangs below it: it is sent whole when no
  name in it, the project's own included, is denied, and is `501` naming the name and the
  pattern when one is. `allow` does not bound an audit.
- Group: the bulk request, without the names its hosted members hold, forwarded as a proxy does
  to its first proxy member, which leaves out the names its `deny` refuses (`{}` when no name is
  left). The two others are `501`: a tree loses what hangs below a private package when the
  package is cut out, and sent whole it would publish the private names.

Nothing is cached, and an audit writes no audit row: it is a read.

**Search.** From the index, never from an upstream: the repo's packages (a group's: those of
its members, a proxy's: those it has cached) whose name contains `text` without regard to case,
sorted by name; `from` and `size` (default 20, at most 250) paginate.
`{"objects": [{"package": {"name", "version", "description", "date"}, "score": {"final": 1,
"detail": {"quality": 1, "popularity": 1, "maintenance": 1}}, "searchScore": 1}], "total", "time"}`,
`version` being `latest` and `description` its.

**Group** (ADR 0013, proxy-and-group.md §8, #311). A group has the routes above:

- The packument of a name merges the packuments of the members that take part (§8.1, §8.2), of
  the kind asked, in member order: `versions` is their union, a version that several members
  have taken from the first; `time` likewise per version, with `created` the earliest and
  `modified` the latest of the members'; each tag of `dist-tags` is the first member's that has
  the tag; every other member of the document (`_id`, `_rev`, `description`, `readme`, …) is
  the first member's. `dist.tarball` names the group. `ETag` is the group's (§8.5).
- `?write=true` answers the hosted member's own full packument, links under the group (§8.4):
  `deprecate` and `unpublish` send it back, and the hosted member takes only its own versions.
- A tarball comes from the member that holds it or, for a proxy member, fetches it (§8.3), and
  is served from that member's registry as a hosted repo's is.
- `GET …/dist-tags` answers the merged packument's `dist-tags`.
- Publish, the packument update, the unpublish routes and the dist-tag `PUT` and `DELETE` are
  the hosted member's, with `read` on the group and the route's own action on the member
  (rbac.md §7); `405` without a hosted member.

**Through `/v2`.** An npm version pushed with `oras push` is ingested as any other
(manifest-profile.md §5) and appends no journal entry: until a dist-tag names a version,
`latest` falls back as above. A journal manifest is never pushed (its artifactType is not the
format's: `400 MANIFEST_INVALID`) nor deleted (`409 DENIED`: an entry goes with its package);
`tags/list` lists the versions only. A version deleted through `/v2` or `/api/v1` drops out of
the packument with the dist-tags that name it.

Client config: `registry=https://host/npm/<repo>/` and `//host/npm/<repo>/:_authToken=<token>`
in `.npmrc`, or `npm login --registry=https://host/npm/<repo>/`; yarn 4: `npmRegistryServer:
"https://host/npm/<repo>"` and `npmAuthToken`, with `npmAlwaysAuth: true` on a repo without
anonymous read and the repo's scopes in `npmPreapprovedPackages`, or yarn holds a new version
back for a day.

### rpm (M6, #64)

Implements the repodata of a yum repository (`repomd.xml` and the `primary`, `filelists` and
`other` documents) that dnf, yum and zypper read, for binary and source packages. One repo is
one repository: its `baseurl` is `https://host/rpm/<repo>/`, every architecture in one repodata
(clients pick theirs), and a distribution release (el8, el9, fc41) is a repo of its own.
Modularity (`modules.yaml`), comps groups, `updateinfo`, deltarpms, zchunk and the sqlite
databases are not served. Lajstrom never signs a package: an rpm's own signature is part of its
bytes, so a client checks it with the key of whoever signed it (`gpgcheck`), and the repodata
with the repo's key (`repo_gpgcheck`).

| Method and path | Behaviour |
| --- | --- |
| `GET /rpm/<repo>/repodata/repomd.xml` | rendered with `primary.xml.gz`, `filelists.xml.gz` and `other.xml.gz` in one render on the first `GET` after a publish or a delete (both invalidate the repo's documents) and kept with them in `index_cache` (scope `repodata`), so its checksums are those of the files served. One `<data>` per document, `type` `primary`, `filelists` and `other`, with `checksum` and `open-checksum` (`sha256` of the compressed and of the uncompressed document), `location href="repodata/<checksum>-<type>.xml.gz"`, `timestamp`, `size` and `open-size`; `<revision>` is the time of the render. A repo without versions has the three documents empty, never `404`: dnf fails on a repo whose `repomd.xml` it cannot read (`skip_if_unavailable=False`). `Content-Type: application/xml` |
| `GET /rpm/<repo>/repodata/repomd.xml.asc` | the armored detached signatures of the bytes of the same render's `repomd.xml`, one per key that signs (the repo's active key and, for `keys.retireGrace` after a rotation, the retired one), made on the first `GET` after the render and kept beside it; signed again when the `Signer` no longer calls them current, as apt's `InRelease`. `application/pgp-signature`. `404` without a master key (`keys.masterKeyEnv`): clients set `repo_gpgcheck=0` then |
| `GET /rpm/<repo>/repodata/repomd.xml.key`, `GET /rpm/<repo>/key.asc` | the ASCII-armored keyring of the repo, as apt's `key.asc`: the active key, made on the first request when the repo has none, and the keys retired within `keys.retireGrace`; `application/pgp-keys`, not cached. zypper fetches `repomd.xml.key` beside a `repomd.xml.asc` by itself; dnf takes either as `gpgkey=`. `404` without a master key |
| `GET /rpm/<repo>/repodata/<checksum>-<type>.xml.gz` | a document the current `repomd.xml` lists, by its `location`; any other name, one of an older render among them, is `404`, without a render. `application/gzip`. `primary.xml` has a `<package type="rpm">` per version, sorted by name, version and arch: `name`, `arch`, `version` (`epoch`, `ver`, `rel`; `epoch="0"` without one), `checksum type="sha256" pkgid="YES"` (the file's sha256), `summary`, `description`, `packager`, `url`, `time file` (the version's publish time, `org.opencontainers.image.created`) and `build`, `size package` (the file), `installed` and `archive`, `location href="Packages/<filename>"`, and `format`: `rpm:license`, `rpm:vendor`, `rpm:group`, `rpm:buildhost`, `rpm:sourcerpm`, `rpm:header-range`, the `rpm:provides`, `rpm:requires`, `rpm:conflicts`, `rpm:obsoletes`, `rpm:recommends`, `rpm:suggests`, `rpm:supplements` and `rpm:enhances` entries (`name`, `flags` `EQ`, `LT`, `LE`, `GT` or `GE`, `epoch`, `ver`, `rel`, and on a requirement of the install scripts `pre="1"`), the `rpmlib(…)` requirements and repeated entries left out, and the files of the package below `/etc/`, in a directory whose name ends in `bin`, and `/usr/lib/sendmail`, which dnf resolves without `filelists` (dnf5 does not fetch it by default). `filelists.xml` lists every file of each version, `type="dir"` for a directory and `type="ghost"` for a `%ghost`; `other.xml` its changelog entries (`author`, `date`, the text), as the package has them |
| `GET /rpm/<repo>/Packages/<filename>` | the package, its `location` in `primary.xml`, `<name>-<version>-<release>.<arch>.rpm` (manifest-profile.md §2). Resolved by file name: the version of the repo that holds the file (else `404`), read as the blob `GET` of the facade (§2, `format.ServeBlob`): `307` or stream, `Range`, `ETag` the blob digest, `Last-Modified` and `Docker-Content-Digest` those of the version, `Content-Type: application/x-rpm`; `HEAD` from the backend's `HEAD`. The file name has no epoch, so two versions that differ only in it share one: the upload refuses that (below), and versions that hold one file name with two blobs, which an `oras push` cannot be checked for, are `500`, logged with both digests for the operator |
| `PUT /rpm/<repo>/upload` | one `.rpm` (body), binary or source. Equivalent to `oras push` with the rpm profile: the rpm plugin builds the manifest from the header. A body above the repo's `maxFileSize` is `413`, one that is not an rpm (a lead, a signature header and a header of rpm v3 or v4, with `NAME`, `VERSION`, `RELEASE` and `ARCH`) `400`. The body streams into the registry ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)): its head, up to the end of the header, is read first for the package name, so a header over 32 MiB is `400` before the rest is read; a refused upload stores nothing. `201` with `Location` (the file below `Packages/`), `Docker-Content-Digest` (the manifest digest) and a JSON body `{"name", "epoch", "version", "release", "arch", "filename", "digest", "tag"}`. The same bytes again are the idempotent repeat, `201` with the same digest. `409` for other bytes under the same name, version and arch (the tag holds another manifest), and for other bytes under a file name that a version with another epoch holds, naming that version |

A version is deleted as any other: `DELETE /api/v1/repos/rpm/<repo>/packages/<name>/versions/<version>`
(`?variant=<arch>` for one architecture) or the `DELETE` of its manifest on `/v2`; the next
`repomd.xml` is rendered without it. `oras push` to `/v2/rpm/<repo>/<package path>:<tag>` with the
rpm profile works too; `lajstrom admin rpm metadata <file.rpm>` prints its metadata layer.

Client config, `/etc/yum.repos.d/<repo>.repo`:

```ini
[<repo>]
name=<repo>
baseurl=https://host/rpm/<repo>/
repo_gpgcheck=1
gpgkey=https://host/rpm/<repo>/repodata/repomd.xml.key
gpgcheck=0
```

with `username=` and `password=` for a repo without anonymous read, and `gpgcheck=1` with the
packager's key in `gpgkey=` for signed packages. zypper: `zypper addrepo https://host/rpm/<repo>/ <repo>`.

## 4. Management API `/api/v1`

JSON, bearer (PAT) or Basic. Shapes are in [api.md](../api.md) and `internal/api/openapi.yaml`;
listed here so frontends do not invent their own. What each call needs, in the actions of rbac.md
§2 (decided on #136, 2026-11-09):

| Calls | Needs |
| --- | --- |
| reading a repo's content: `GET /api/v1/repos` (lists only the repos the caller may read), `GET …/repos/<format>/<repo>`, `GET …/packages` and `GET …/packages/<name>/versions` (#72), `GET …/packages/<name>/versions/<version>` (#136), and the search of #956 after the MVP | `read` on the repo; a repo the caller may not read is `404`, as on the frontends (rbac.md §3) |
| changing a repo: `PATCH`, `DELETE`, `POST …/reindex`, `GET …/reindex`, the deploy tokens of the repo | `manage` on the repo |
| deleting versions: `DELETE …/repos/<format>/<repo>/packages/<name>/versions/<version>` (#61), `POST …/repos/<format>/<repo>/versions:delete` (#72) | `delete` on the repo, checked before the body is read; a repo the caller may not read is `404` |
| creating a repo, and the system resources: users, bindings, audit | the admin role (rbac.md §2; a `maintainer` on `*/*` is not enough) |
| the personal access tokens of a user | the user themself, within what the credential allows, or the admin role |
| `GET /api/v1/whoami`, `GET /api/v1/info` (#136), `GET /api/v1/openapi.yaml` | nothing |
| `POST /api/v1/bootstrap` (#440) | nothing: the one-time bootstrap token in the body is the credential; `404` once an enabled user has an admin binding |

| Resource | Endpoints |
| --- | --- |
| info | `GET /api/v1/info` |
| repos | `GET /api/v1/repos[?format=]`, `POST /api/v1/repos`, `GET/PATCH /api/v1/repos/<format>/<repo>`, `DELETE /api/v1/repos/<format>/<repo>[?force=true]`, `POST …/reindex`, `GET …/reindex`, `DELETE …/upstream-cache[?name=]` (§6) |
| packages, versions | `GET /api/v1/repos/<format>/<repo>/packages[?prefix=&limit=&cursor=]`, `GET …/packages/<name>/versions[?limit=&cursor=]`, `GET …/packages/<name>/versions/<version>[?variant=]`, `DELETE …/packages/<name>/versions/<version>[?variant=&force=]`, `POST /api/v1/repos/<format>/<repo>/versions:delete`; search after the MVP (#956) |
| users | `GET /api/v1/users[?limit=&cursor=]`, `POST /api/v1/users`, `GET/PATCH/DELETE /api/v1/users/<name>`, `POST …/password` |
| tokens | `GET/POST /api/v1/users/<name>/tokens` (PAT), `DELETE …/tokens/<id>`; `GET/POST /api/v1/repos/<format>/<repo>/deploy-tokens`, `DELETE …/<id>` |
| bindings | `GET /api/v1/bindings[?principal=&limit=&cursor=]`, `POST /api/v1/bindings`, `DELETE /api/v1/bindings/<id>`; a binding's principal is `user:<name>`, `user:<uid>` or `group:<name>` (rbac.md §3, §8) |
| audit | `GET /api/v1/audit[?repo=&principal=&action=&state=&since=&until=&limit=&cursor=]` |
| whoami | `GET /api/v1/whoami`; for a user, its current groups too (rbac.md §8) |
| bootstrap | `POST /api/v1/bootstrap` |
| the API's description | `GET /api/v1/openapi.yaml` |

Errors: `{"error": {"code": "NOT_FOUND|CONFLICT|UNAUTHENTICATED|FORBIDDEN|INVALID|UNAVAILABLE|INTERNAL", "message": "…"}}`.
A `401` (no credentials, or credentials refused, with the Basic challenge) is `UNAUTHENTICATED`,
a `403` (the caller lacks the action) `FORBIDDEN` (#579). A `429` (a throttle of failed logins,
with `Retry-After`; #130) is `UNAVAILABLE`: try again later. A `412` (a conditional delete that
found the version changed) is `CONFLICT`, as `409` is. There is no eighth code.

Lists of a repo's content (`read`): `GET …/packages` pages the packages by name, `?prefix=`
keeps those whose name starts with it; `GET …/packages/<name>/versions` pages the versions,
newest first by the publish time of their first variant, each with its variants (`variant`,
`tag`, manifest `digest`, `publishedAt`), `publishedAt` and `lastModified` (of the first and the
newest variant) and its `pointers` (npm dist-tags). The publish time is
`org.opencontainers.image.created`, `versions.created_at`, which reindex rebuilds. Both pages are
`?limit=` and `?cursor=`; the cursor holds the place of the page's last row (a name; a time and a
version), never an offset.

One version (`read`, #136): `GET …/packages/<name>/versions/<version>[?variant=]`, with
`<name>` and `<version>` as the delete below has them, answers the version's row of that list
(`version`, `publishedAt`, `lastModified`, `pointers`, `variants`) and `ociRepository`, the OCI
repository below the instance prefix that holds it (`<format>/<repo>/<name_oci>`, the `<name>`
of §2). Each variant adds what the index has of its manifest: `artifactType`, `attrs`
(`versions.attrs`, by annotation key), `metadata` (the metadata layer; `null` when it is absent
or omitted for size), `degraded`, `indexedAt` and `files`, its file layers in manifest order,
each with `name`, `mediaType`, `digest`, `size` and `attrs` (the layer's annotations).
`?variant=` keeps one variant (`""` is the variant of a format that has none); the version's times
and pointers stay those of all its variants. It is all read from
the derived tables (index-schema.md), so reindex rebuilds it. The manifest itself and its referrers
are live registry reads and not part of it (#951); `/v2/<ociRepository>/…` serves both
(§2). A file's protocol URL is not part of it either: the client builds it from the format, as
for its usage snippets. The answer is `404` when the repo, the package, the version or the variant
does not exist, and for a group, which has no versions.

Server information (nothing, #136): `GET /api/v1/info` answers what a client needs before it
signs in:

- the build: `version`, `commit` and `date`, as `/-/version` has them;
- `externalURL`: `server.externalURL`, or `""` on the dev stack, which may leave it empty;
- `anonymousRead`: `auth.anonymousRead`, the server-wide switch;
- `formats`: the format plugins the server serves, by `name`, each with its `artifactType`.
  `oci` repos need no plugin and are not listed;
- `authModes`: how a person signs in. `password` is a local user's password over Basic; `token`
  is a personal access token. The OIDC login (#69) adds `oidc`;
- `caller`: the request's principal, as `GET /api/v1/whoami` answers it, and `admin`, whether
  the request's credential reaches the admin role (a personal access token with scopes does not).

Unlike whoami, a request without credentials gets `200`, with the caller `anonymous`. Credentials
that are refused get `401`, as on every route.

A browser (#136): the API sets no cookie and reads none. A web UI signs in by creating a
personal access token: `POST /api/v1/users/<name>/tokens` with the user's password over Basic,
since a token creates none. It sends that token as `Authorization: Bearer` on every request, and
signing out revokes it (`DELETE …/tokens/<id>`). A token the person already has is sent the
same way. The OIDC login (#69) mints a personal access token too, and the UI uses it alike. Its
cookie `lajstrom_oidc` has the path `/-/oidc/` and never authenticates `/api/v1`.

- **Where the token is kept** is the UI's choice. Any script of the origin can read any storage
  of it. No answer of Lajstrom on that origin runs a script ([operations.md](../operations.md),
  What Lajstrom answers browsers).
- **The browser's own Basic handling.** The UI sends its requests with `credentials: "omit"`, so
  that the Basic challenge of a `401` opens no browser dialog and no cached Basic credentials go
  along.
- **No CSRF token.** The browser adds a bearer to no request by itself. The one ambient
  credential is cached Basic, and `server.crossOrigin` refuses it on an unsafe request from
  another site.

Same origin only (#136): the UI is served from the host of `server.externalURL`, by its own
container at `/ui/` behind the same edge (§1; lajstrom-ui ADR 0001). No answer of `/api/v1` has
an `Access-Control-*` header, and `OPTIONS` is `404 NOT_FOUND`, as is every method a route does
not have. A page of another origin can neither read an answer nor send a request with a bearer.
A CORS allowlist, for a UI on another origin, is #949.

Deleting a version (`delete`): `<name>` is the package name in the format's spelling, one path
segment (a raw path escapes its `/` as `%2F`). Without `variant` every variant of the version
goes, with it the one variant (`""`, the empty value, is the variant of a format that has none);
each manifest is deleted as the `DELETE` of §2 deletes it, its referrers first, with an audit row
of its own. `204`; `404 NOT_FOUND` when the repo has no such package, version or variant; a
backend that cannot delete manifests is `501 UNAVAILABLE` naming the backend, and nothing
changes. Versions deleted before a failure stay deleted, and the error's message names them
with their digests, a `5xx` too (`format.PartialError`, #476).

The delete is a compare-and-delete (#72): ingest checks its conditions under the locks the
delete holds (index-schema.md, Coordination) and deletes nothing when one fails.
`If-Match: "<digest>", …` lists the digests of the variants the client read; the variants the
delete takes must have exactly them, else `412 CONFLICT` (deleted and published again, a variant
added or removed). `If-Unmodified-Since: <HTTP-date>` is the cut-off; a variant published after
it, in whole seconds, is `412 CONFLICT`. Both are checked, unlike RFC 9110's precedence, and a
value that does not parse is `400`, never ignored. A version a pointer names (an npm dist-tag)
is `409 CONFLICT` naming the pointers, with the headers or without, unless `?force=true`. Without
the headers the client sets no condition of its own: an operator's delete does not wait for an
age.

`POST …/versions:delete` (`delete`, before the body is read) takes
`{"items": [{"package", "version", "digests": […]}], "cutoff", "dryRun", "job"}` and deletes each
item as the conditional `DELETE` above with its digests and the cut-off, never forced, each
under its own locks; it is not all-or-nothing, and a rerun is idempotent. `200` with
`{"job", "dryRun", "results": [{"package", "version", "status", …}]}`, a status per item:
`deleted` (in a dry run: would be), `changed`, `protected`, `young`, `gone`, `error`. Every audit
row of the batch has the job in `details.job`; every `delete` row has the request's token, when it
came with one, in `details.token`.

Reindex of one repo (`manage`): `POST …/reindex` with an optional body `{"prune", "verify"}`,
both `false` by default (as `lajstrom reindex --repo`), starts the run on the replica that
received it and answers `202` with the run
(`id`, `state`, `source`, `principal`, `replica`, `startedAt`), before the run reads the
registry; a running run that overlaps it, on any replica or in the CLI, is `409 CONFLICT` naming
where it runs and since when (index-schema.md, Coordination). `GET …/reindex` answers the latest
run that covered the repo, its own or a run of every repo, with `state`, the times, `error` and
the repo's part of the report (layout.md, `reindex.Report`); `404` when no run has covered it.
The run writes the audit event `repo.reindex` when it starts.

## 5. System endpoints

`/-/healthz` (process up), `/-/readyz` (DB reachable, no other replica on it when it is SQLite
([index-schema.md](index-schema.md#coordination)), the replica's clock within 60 s of the DB's
([operations.md](../operations.md#clocks)), every configured registry answered `/v2/`
within the last minute), `/-/version` (JSON: version, commit, date).

Prometheus metrics are not on the public host: `server.metricsListen` (default
`127.0.0.1:9464`, the OpenTelemetry Prometheus exporter convention) serves `GET /metrics`, and
`/debug/pprof/` when `server.pprof` is on. The edge never routes to it; Prometheus scrapes it
inside the network. Issue #23.

## 6. Proxy and group repos

A proxy or a group repo ([ADR 0013](../adr/0013-proxy-and-group-repos.md),
[proxy-and-group.md](proxy-and-group.md)) is `/<format>/<repo>/` like a hosted one, with the
routes of its format: a client sees no difference, and the npm routes (ADR 0012) serve all three
kinds. What changes is who answers and what a write does.

| Request | proxy | group |
| --- | --- | --- |
| a read route of the format (a document, a file) | the upstream's document, cached, and the files it holds or fetches on a miss (proxy-and-group.md §4 to §7) | the members, in order, with hosted names hiding the upstream (§8) |
| a write route of the format (publish, unpublish, a dist-tag) | `405` with `Allow: GET, HEAD` | the same request on the group's hosted member, authorized there (rbac.md §7); `405` without one |
| `GET`, `HEAD` on `/v2/<format>/<repo>/…` (manifests, blobs, tags, referrers) | as on a hosted repo, for what it holds; `/v2` never fetches, but on an `oci` proxy, whose read route it is: a manifest and a blob the backend does not hold are fetched on a miss, a tag follows the upstream, and `tags/list` is the backend's without the keep-tags (proxy-and-group.md §11) | on an `oci` group, the answer of the one member its routes choose for the path, as at the member's own URL, `name` and `Link` naming the group's path; a path no route selects `404 NAME_UNKNOWN` (proxy-and-group.md §12). On any other group `404 NAME_UNKNOWN`: it has no OCI repository |
| `PUT` of a manifest on `/v2` | a referrer (manifest-profile.md §6) as on a hosted repo, with `write`; any other manifest `405 UNSUPPORTED` | on an `oci` group, `405 UNSUPPORTED` with `Allow: GET, HEAD`, a referrer included, once the principal may read the group, and the group's denial otherwise (rbac.md §3); on any other group `404 NAME_UNKNOWN` |
| blob uploads on `/v2` | as on a hosted repo, with `write` (the blobs of referrers) | as a `PUT` of a manifest. A mount into another repo whose `from` is an `oci` group's path mounts from the routed member (proxy-and-group.md §12.3) |
| `DELETE` of a manifest on `/v2`, and of a version on `/api/v1` | as on a hosted repo, with `delete`: the next miss fetches it again | on `/v2`, as a `PUT` of a manifest; on `/api/v1`, `404`: a group has no versions |
| `GET /v2/_catalog` | its OCI repositories | an `oci` group, to a principal who may read it, each member's repositories that the routes send to that member, as `oci/<group>/<path>`; any other group nothing |

A manifest pushed by a client with a `lajstrom.origin.*` annotation is `400 MANIFEST_INVALID`
on every manifest Lajstrom manages, a version's manifest and a referrer, on a hosted repo and on
a proxy one alike: only the fetcher writes them (manifest-profile.md §3.1). An `oci` repo's
pass-through is not checked: Lajstrom stores its manifests, a referrer among them, as they came,
and never indexes them.

Statuses that only a proxy has, on a read route (the full table is proxy-and-group.md §6): `502`
when the upstream fails and nothing is cached or held, when it refuses Lajstrom's credentials,
redirects from `https` to `http` or to an address that is not public, serves a file whose
checksum differs from its metadata, or a file that the plugin's `Validate` refuses; `504` when
it does not answer in `upstreams[].timeout`; `503` with `Retry-After` when it answers `429`;
`403` naming the bound for a file above `maxFileSize` or the registry's `maxBlobSize`; `403
DENIED` naming the list and the pattern for a name outside `allow` and `deny` (through a group,
a member that does not have it); `404` for one the upstream does not have, and a miss in
offline mode. A cached document is served instead of any `5xx` that the upstream causes. Messages name
the upstream by its configured name, never its URL.

On an `oci` proxy ([ADR 0018](../adr/0018-oci-proxy-repos-pull-through-from-any-registry.md),
proxy-and-group.md §11) the statuses are those of the distribution spec's JSON errors: `403
DENIED` for a path outside the lists, `400 NAME_INVALID` for a name the backend does not take,
`404 MANIFEST_UNKNOWN` or `BLOB_UNKNOWN` for what the upstream does not have (also its `401` or
`403` of a repository read after a token was granted, which is how Docker Hub and quay.io answer
for a repository that does not exist) and for a blob no held or cached manifest lists, `503
TOOMANYREQUESTS` with `Retry-After` for its `429` when no row is cached, `502` and `504` as
above. On an upstream that is Docker Hub (`upstreams[].dockerHub`) a path of one component is
`library/<path>` on every route. The query `ns` that containerd adds to a mirror's request
(`?ns=docker.io`) is ignored, as any query a route does not define, on a proxy and through an
`oci` group alike.

Through an `oci` group ([ADR 0019](../adr/0019-oci-groups-one-member-per-repository-path.md),
proxy-and-group.md §12) the route is decided after the principal's read on the group, from the
path as the client sent it, before a Docker Hub member's `library/` rule; the routed member's
statuses pass through, but for a path a proxy member's lists refuse, which is `404 NAME_UNKNOWN`
through the group. A scope of a group's path gets `pull` at most from `/v2/token` (rbac.md §4).

Management API (§4), in addition:

| Call | Needs |
| --- | --- |
| `POST /api/v1/repos` with `kind` `proxy` or `group` | the admin role, as every create |
| `PATCH` of `upstream.name`, of `group.members` or of `group.paths` | the admin role |
| `PATCH` of the other keys of `upstream` and `group`, and of `anonymousRead` | `manage` on the repo |
| `DELETE /api/v1/repos/<format>/<repo>/upstream-cache[?name=<name>]` | `manage` on a proxy repo: drops its `proxy_meta` rows, or the rows of one name, and their rendered documents; `204`, `400` on a repo that is not a proxy; audit `repo.cache.purge` |
| `PATCH /api/v1/repos/<format>/<repo>` with `{"kind": "hosted"}`, alone | the admin role: a proxy becomes a hosted repo that keeps what it holds, its dist-tags written to its journal (proxy-and-group.md §2.6); `200` with the resource, `400` on a group or a format without the hook, `409` while a fetch runs or when a group of it has a hosted member; on a hosted repo it resumes a conversion that stopped half way, else changes nothing; audit `repo.convert` |
| `POST /api/v1/repos/<format>/<repo>/prefetch` | `manage` on a proxy repo: fetches the versions of 1 to 100 names before a client asks, `{"names": […], "allVersions": false}` (proxy-and-group.md §5.1); on an `oci` proxy the images of 1 to 100 references below the repo, `{"images": […], "platforms": ["linux/amd64", …]}`, `platforms` required for a reference that names an index (§11.10); `200` with a result per version, or per image, platform and blob not held; `400` on a repo that is not a proxy, a format without the hook but `oci`, or the other format's body; `409` on an offline one; each fetch audited `proxy.fetch` as a miss's |
| `DELETE /api/v1/repos/<format>/<repo>` of a repo that is a member of a group | `409 CONFLICT` naming the groups, `force` or not |

The repo resource has `kind` and, by kind, `upstream` or `group` (proxy-and-group.md §2.4),
with the upstream's name only, never its URL or its credentials.

## 7. OIDC login `/-/oidc` (M6, #69)

The routes of rbac.md §8, served only while `auth.oidc.issuer` is set; without it every one is
`404`. They are not under `/api/` because a browser follows them: the login and its callback
answer redirects and HTML pages. The device routes answer JSON in the shape of RFC 8628 and of
OAuth 2.0 (RFC 6749 §5.2), not the envelope of §4: `lajstrom login` is an OAuth client. None
takes a credential; a request's `Authorization` header is ignored on them.

| Method and path | Behaviour |
| --- | --- |
| `GET /-/oidc/login[?user_code=]` | starts the authorization code flow: `302` to the IdP with `state`, `nonce` and the PKCE challenge, and the cookie `lajstrom_oidc` (rbac.md §8). With `user_code`, the login approves that device request: one that is unknown, expired or approved is `400` with the page of `/-/oidc/device`. `503` when the IdP's discovery does not answer, `Retry-After: 30` |
| `GET /-/oidc/callback?code=&state=` | the IdP's redirect: one redemption of the state, the code exchange, the ID token checks, the user and its groups. `200` with the page that shows the new personal access token once (`Cache-Control: no-store`, `Referrer-Policy: no-referrer`), or, for a device request, the page that says to return to the terminal. `400` with a page for every refusal of rbac.md §8; `503` when the IdP does not answer the exchange |
| `GET /-/oidc/device[?user_code=]` | the page where the person types or confirms the user code; its form is a `GET` of `/-/oidc/login?user_code=` |
| `POST /-/oidc/device/code` | starts a device request (RFC 8628 §3.1, `application/x-www-form-urlencoded`, its `client_id` and `scope` ignored): `200` `{"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval"}`; `429` with `Retry-After` beyond the throttle |
| `POST /-/oidc/token` | `grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=` (RFC 8628 §3.4): `200` `{"access_token", "token_type": "Bearer", "expires_in", "user"}` once the device request is approved, the personal access token minted then; else `400` `{"error", "error_description"}` with `authorization_pending`, `slow_down`, `expired_token`, `access_denied` or `invalid_grant`; another `grant_type` is `400` `unsupported_grant_type` |

Every answer of these routes has `Cache-Control: no-store`. The pages are HTML of Lajstrom's own,
with no script and `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'` (no `form-action`: the
device page's form ends in a redirect to the IdP),
and say nothing the IdP said beyond the reason in Lajstrom's words.
