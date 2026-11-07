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

Reserved first segments: `v2`, `api`, `ui`, `-`, `.well-known`, and every registered format name.
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
for the dev stack on loopback only, behind an explicit `server.insecureHTTP: true`. The
configuration keys and checks are part of the hardening issue (#50).

`server.externalURL` is the base for every absolute URL Lajstrom renders (simple index links,
`WWW-Authenticate` realm, apt `Release` is path-relative and needs none).

## 2. OCI facade `/v2`

Implements the OCI distribution spec against the backend through the provider, with the OCI
repository name `<format>/<repo>/<package path>` (the instance prefix is added by the provider and
never visible to clients).

| Method and path | Behaviour |
| --- | --- |
| `GET /v2/` | `200` if authenticated, else `401` with `WWW-Authenticate: Bearer realm="<externalURL>/v2/token",service="<host>"` |
| `GET /v2/token?service=&scope=repository:<format>/<repo>/<pkg>:pull,push` | Basic auth (user:password, user:PAT, or deploy token) → JWT bearer with the scopes the caller is allowed (rbac.md §4). Anonymous: scopes limited to `pull` on repos with anonymous read. |
| `HEAD/GET /v2/<name>/manifests/<ref>` | proxied to the backend. `Accept` passed through. |
| `PUT /v2/<name>/manifests/<ref>` | **hook**: body is parsed; for a non-`oci` repo it must satisfy the manifest profile and is passed to `ingest.Ingest`; on success the manifest is stored and indexed, `201` with `Docker-Content-Digest`. Errors: `400 MANIFEST_INVALID`, `409 TAG_INVALID` (tag exists with another digest), `404 NAME_UNKNOWN` (repo not configured). |
| `DELETE /v2/<name>/manifests/<ref>` | requires `delete`; deletes in the backend (if `Capabilities.DeleteManifest`) and removes index rows. After the MVP: retention. |
| `POST /v2/<name>/blobs/uploads/`, `PATCH`, `PUT …?digest=` | proxied upload session (monolithic and chunked). Lajstrom streams; it never buffers a blob on disk. |
| `HEAD /v2/<name>/blobs/<digest>` | proxied |
| `GET /v2/<name>/blobs/<digest>` | `307` to `Provider.BlobRedirectURL` when available, otherwise streamed from the backend |
| `GET /v2/<name>/tags/list` | from the index for Lajstrom-managed repos (fast, consistent), from the backend for `oci` repos |
| `GET /v2/<name>/referrers/<digest>` | provider referrers or tag-schema fallback |
| `GET /v2/_catalog` | from the control plane, filtered by RBAC |

Mount of a package under a repo: the name after `/v2/` is split as `<format>/<repo>/<rest>`; the
repo must exist in the control plane, else `404 NAME_UNKNOWN`. Auto-creation of repos on push is
not supported (an explicit `POST /api/v1/repos` is one call).

Docker image passthrough (`oci` repos) is after the MVP; in the MVP only the paths above are
exercised, with `oras`, `cosign` and `helm` as the OCI-native clients tested.

## 3. Protocol frontends (MVP formats)

Authentication on all frontend endpoints: HTTP Basic with a user password, a PAT or a deploy
token; the same credentials as `/v2/token`. Anonymous `GET` on repos with `anonymousRead: true`.
Writes require `write` on `<format>/<repo>`.

### raw

| Method and path | Behaviour |
| --- | --- |
| `GET /raw/<repo>/<path…>` | newest version of the file at `<path>`; `307` to the blob when the provider supports redirect, else stream. `?digest=sha256:…` pins a version. `Content-Type` from the metadata layer or sniffed. |
| `HEAD` | same, no body |
| `PUT /raw/<repo>/<path…>` | upload one file (body). Idempotent for identical bytes. Creates a new version (tag) for different bytes; the path now resolves to it. `201` with `Location` and `Docker-Content-Digest`. `409` when the path exists in another spelling (the message names it). |
| `DELETE /raw/<repo>/<path…>` | requires `delete`; removes all versions of the path (after the MVP: one version) |
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
| `GET /apt/<repo>/dists/<suite>/InRelease` | clearsigned Release (per-repo GPG key); from `index_cache` |
| `GET /apt/<repo>/dists/<suite>/Release`, `Release.gpg` | detached variant |
| `GET /apt/<repo>/dists/<suite>/<component>/binary-<arch>/Packages`, `Packages.gz`, `Packages.xz` | package index; `all` packages are included in every arch index; `by-hash/SHA256/<hash>` supported (apt uses it when `Acquire-By-Hash: yes`) |
| `GET /apt/<repo>/pool/<component>/<p>/<package>/<filename>` | the `.deb`; `<p>` = first letter (or `lib` + letter) as apt expects; resolved by filename → digest; `307` or stream |
| `GET /apt/<repo>/key.asc` | ASCII-armored public key of the repo (`signed-by=`) |
| `PUT /apt/<repo>/upload?suite=<suite>&component=<component>` | convenience upload of one `.deb` (body). Equivalent to `oras push` with the deb profile. |

Client config: `deb [signed-by=/etc/apt/keyrings/<repo>.asc] https://host/apt/<repo> <suite> <component>`.
Source packages (`dsc`, `Sources`) are after the MVP.

### maven

Implements the Maven 2 repository layout for release versions. SNAPSHOT versions are after the
MVP (they will be a repo option, not a separate format).

| Method and path | Behaviour |
| --- | --- |
| `GET /maven/<repo>/<groupPath>/<artifactId>/<version>/<file>` | artifact, POM, or checksum (`.sha1`, `.md5`, `.sha256`, `.sha512` computed at ingest and stored in the metadata layer) |
| `GET /maven/<repo>/<groupPath>/<artifactId>/maven-metadata.xml` (+ checksums) | rendered from the index: `latest`, `release`, `versions`, `lastUpdated` |
| `PUT /maven/<repo>/<groupPath>/<artifactId>/<version>/<file>` | adds the file to the publication session `(repo, g, a, v)`; checksum files are verified against the session and not stored as layers. `201`. |
| `PUT /maven/<repo>/<groupPath>/<artifactId>/maven-metadata.xml` (+ checksum PUTs) | **commit** of the session: builds the manifest with all files, ingests, closes the session. Idempotent re-PUT of an identical version: `200`. Different bytes for an existing version: `409`. |

Sessions expire after `maven.sessionTimeout` (default 30 min): policy `seal` (commit what is
there) or `abort` (drop), per repo; default `abort`. Client config: `<repository>` with
`<url>https://host/maven/<repo></url>` and a `<server>` entry with user/token in `settings.xml`.

## 4. Management API `/api/v1`

JSON, bearer (PAT) or Basic. All endpoints require `manage` on the resource or the system admin
role. Shapes are fixed in the API issue (M2-09); listed here so frontends do not invent their own.

| Resource | Endpoints |
| --- | --- |
| repos | `GET/POST /api/v1/repos`, `GET/PATCH/DELETE /api/v1/repos/<format>/<repo>`, `POST …/reindex` |
| users | `GET/POST /api/v1/users`, `GET/PATCH/DELETE /api/v1/users/<name>`, `POST …/password` |
| tokens | `GET/POST /api/v1/users/<name>/tokens` (PAT), `DELETE …/tokens/<id>`; `GET/POST /api/v1/repos/<format>/<repo>/deploy-tokens`, `DELETE …/<id>` |
| bindings | `GET/POST /api/v1/bindings`, `DELETE /api/v1/bindings/<id>` |
| audit | `GET /api/v1/audit?repo=&principal=&since=` |
| whoami | `GET /api/v1/whoami` |

Errors: `{"error": {"code": "NOT_FOUND|CONFLICT|FORBIDDEN|INVALID|UNAVAILABLE|INTERNAL", "message": "…"}}`.

## 5. System endpoints

`/-/healthz` (process up), `/-/readyz` (DB reachable, every configured registry answered `/v2/`
within the last minute), `/-/version` (JSON: version, commit, date).

Prometheus metrics are not on the public host: `server.metricsListen` (default
`127.0.0.1:9464`, the OpenTelemetry Prometheus exporter convention) serves `GET /metrics`, and
`/debug/pprof/` when `server.pprof` is on. The edge never routes to it; Prometheus scrapes it
inside the network. Issue #23.
