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
https://<host>/-/oidc/...               OIDC login: browser and device flows (§7, M6); token exchange for CI (§7, #937)
http://<metricsListen>/metrics        Prometheus, a separate listener, never on the public host
https://<host>/.well-known/terraform.json   Terraform's service discovery (§3, terraform)
https://<host>/ui/...                   (after the MVP)
https://<repo>.<domain>/...             FQDN mode: the tree of one repo, rewritten to the path form (§1.1)
```

Reserved first segments: `v2`, `api`, `ui`, `-`, `.well-known`, `oci`, `lajstrom`, and every
registered format name. `oci` is the format of the pass-through repos: a repo `oci/<repo>` holds
arbitrary OCI artifacts that pass through `/v2` unchanged, indexes nothing and has no frontend, so
no plugin may take the name and the management API creates such a repo without one. `lajstrom`
is the first component of Lajstrom's own artifacts in the registry, the snapshots of the control
plane at `lajstrom/control-plane` below the instance prefix (manifest-profile.md §9, #76) and the
deny list at `lajstrom/policy/denylist` (§10, #101): no format may take it, so that no repo's OCI
repositories are below it. No route serves it, `/v2` included: a name there is `<format>/<repo>/…`
of a repo that exists, and no repo has the format `lajstrom`.
A format name is lowercase ASCII letters and digits with single hyphens between them
(`^[a-z0-9]+(-[a-z0-9]+)*$`) and never one of the other reserved segments.
A repo name is unique within its format; the pair `<format>/<repo>` is the resource for RBAC.
The optional FQDN mode (§1.1, #75) gives a repo a host of its own, `<repo>.<domain>`, and
rewrites its requests to the path form before they are authenticated and routed.

Lajstrom owns the whole host: nothing lives under a path prefix. OCI clients address a registry
as `host[:port]/<name>` and always request `https://<host>/v2/…`, and Terraform discovery reads
`/.well-known/terraform.json` at the host root, so a prefix cannot work for them, and the server
has one tree. `server.externalURL` is `https://<host>[:port]` without a path, and a reverse proxy
in front of Lajstrom must not rewrite paths.

Clients see Lajstrom over TLS only; there is no scheme to choose. docker refuses a plain-HTTP
registry, pip and Maven refuse a plain-HTTP index by default, and bearer tokens must not travel
in clear. TLS terminates in one of two places: at the edge (an ingress or a reverse proxy, the
default in Kubernetes; Lajstrom then listens on plain HTTP inside the network and trusts
`X-Forwarded-For` only from `server.trustedProxies`, and reads no other forwarded header but the
client certificate of `auth.clientCertificates.forwardedHeader`, from the same peers: rbac.md §9),
or in Lajstrom itself for a single host (`server.tls.certFile` and `server.tls.keyFile`; ACME
after the MVP), which then asks for a client certificate when `auth.clientCertificates.bundles`
has one (rbac.md §9). Plain HTTP to clients is for the dev stack on loopback only, behind an explicit
`server.insecureHTTP: true`.
`server.trustedProxies`, a list of CIDR prefixes (ADR 0011), came with #130; the TLS keys and
`server.insecureHTTP` came with the hardening issue (#50).

`server.externalURL` is the base for every absolute URL Lajstrom renders: the `WWW-Authenticate`
realm and npm's `dist.tarball` (§3, npm), so a new value needs npm's documents rendered again
(`lajstrom reindex`); on a repo host (§1.1) npm's `dist.tarball` follows that host. Redirects
and the `Location` of an upload are paths, the file links of the PyPI simple index are relative,
and apt `Release` is path-relative and needs none. Its host is the
`service` of the token flow, the `iss` and `aud` of every bearer token. It is required: only with
`server.insecureHTTP: true`, the dev stack, may it be empty (#248). Then the realm and the service
come from the request's `Host`, and the realm is `https://` when the request came over TLS
(`server.tls`), `http://` otherwise.

### 1.1 FQDN mode (#75)

With `server.fqdn.domain` set ([configuration.md](../configuration.md#server); it needs
`server.externalURL`, and an `http://` one the domain `localhost`), every repo also has a host of
its own, and a client may take that host as the root of the repo: npm's
`registry=https://internal.art.example.com/`, pip's
`--index-url https://internal.art.example.com/simple/`. Lajstrom rewrites such a request to the
path form before it authenticates and routes it, and nothing below the rewrite tells the two
apart: the path form keeps working beside it, for the same repo, with the same credentials,
permissions and documents.

```
https://<repo>.<domain>/<path>            → /<format>/<repo>/<path>, the format of the one repo of that name
https://<repo>.<format>.<domain>/<path>   → /<format>/<repo>/<path>
```

**Which host is what.** The `Host` header decides, lower-cased, without its port and a trailing
dot; no forwarded header is read (§1), so an edge in front passes `Host` through. The host of
`server.externalURL`, the domain itself and every host not below the domain are the main host,
served as without the mode. A host below the domain is a repo host or nothing:

- one label, `<repo>.<domain>`: the repo of that name among the formats the server serves. `oci`
  repos, which have no frontend, are not counted. A name that repos of two formats hold has no
  host of this form, whichever was created first: creating `maven/payments` takes
  `payments.<domain>` from `npm/payments`, whose clients then need the second form;
- two labels, `<repo>.<format>.<domain>`, `<format>` a format the server serves: the repo of that
  format;
- the repo label is a DNS label: lowercase letters, digits and single hyphens, at most 63 bytes. A
  repo whose name has a `.` or a `_`, or is 64 bytes long, has no host; the path form serves it.
  Its name is not mapped to another, which could be the name of another repo.

Any other host below the domain (more labels, a format the server does not serve, an unknown
repo, a name two formats hold, a label that is no repo name) is `404`, with the message
`no repo is served at the host <host>` followed by the forms above, and the reasons a repo has no
host; the anonymous principal gets `401` with the Basic challenge instead, as for a repo it may
not read (rbac.md §3). The answer comes after authentication, so credentials that are refused
are `401` first, as on every route.

**A repo host serves the tree of its repo, and all of it.** Every path is rewritten,
`/-/…`, `/v2/…`, `/api/…` and `/.well-known/…` included: npm's own routes are below `/-/`, and an
npm package may be named `v2`. The management API, the OCI facade with its token endpoint, the
system endpoints and the OIDC login are on the main host only.

- The OCI facade stays host-wide: an image or an artifact is `<main host>/<format>/<repo>/<path>`.
  On a repo host `/v2/` is a path of the repo, so docker and oras find no registry there. The
  realm and the service of the bearer challenge, and the `iss` and `aud` of every token, are
  those of `server.externalURL`, whatever the `Host`.
- Terraform discovers `/.well-known/terraform.json` on a host; on a repo host that is
  `/terraform/<repo>/.well-known/terraform.json`, a route of the terraform plugin, whose
  `modules.v1` is the root of the repo host and whose module addresses keep the repo as their
  namespace (§3, terraform).

**URLs on a repo host** follow it. Their host is built from the repo name, the form the request
used and the domain, their scheme and port are those of `server.externalURL`; nothing of them
comes from the request's `Host`.

- A `Location` that is a path below `/<format>/<repo>/`, the redirect of a frontend or of the
  router, or the `Location` of an upload, loses `/<format>/<repo>`: pip is sent to
  `/simple/my-pkg/`, not to `/pypi/internal/simple/my-pkg/`.
- npm's `dist.tarball` is `https://<repo host>/<name>/-/<file>`. The packument is rendered and
  kept under `server.externalURL` as on the main host (§3, npm, one `index_cache` row for both)
  and served with `<externalURL>/npm/<repo>/` after `"tarball":"` written as the repo host's
  root; its `ETag` is the digest of the bytes served. Of a group it is the group's host.
- The links that are relative already stay so: the PyPI simple index, apt `Release`, the rpm
  repodata. The `Location` of a blob's `307` is the registry's own URL, as on the main host.
- Terraform's `X-Terraform-Get` is a path, which the terraform plugin writes without
  `/terraform/<repo>` on a repo host, as a `Location` is.

A request on a repo host of the first form looks its repo up once per format the server serves,
of the second form once; the router then looks it up as on the main host.

**TLS.** The main host, `*.<domain>` and, for the second form, `*.<format>.<domain>` of each format
used, in one certificate or in several, at the edge; a wildcard DNS record for each. Lajstrom's own
`server.tls` holds one certificate and sends it whatever the SNI name, so with the mode on it is a
certificate with those names; `serve` warns at start when it does not cover `<repo>.<domain>`.

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
| `GET /v2/token?service=&scope=repository:<format>/<repo>/<pkg>:pull,push` | Basic auth (user:password, user:PAT, or deploy token) → JWT bearer with the scopes the caller is allowed (rbac.md §4); a `push` that the caller has as `attach` only is granted as `attach`. Anonymous: scopes limited to `pull` on repos with anonymous read. Without an `Authorization` header, a client certificate that verifies is the credential, the principal `cert:…`; a Basic password that is a service-account token of a configured Kubernetes cluster is that token, the principal `k8s:…`, whatever the user name (rbac.md §9). A Basic password `ljs_ci_…` is a CI token, the principal `ci:…`, whatever the user name (rbac.md §11). |
| `POST /v2/token` | the OAuth2 form of the password grant (RFC 6749 §4.3), which containerd posts before it tries `GET` whenever it has a secret (#908): `application/x-www-form-urlencoded` (parameters such as `charset` allowed) with `grant_type=password`, `username`, `password`, `service` and `scope`, which may repeat and hold several scopes separated by spaces, as `GET`'s; `client_id` and any other field are ignored. `username` and `password` are checked as the user name and the secret of Basic credentials are (a password, a PAT, a deploy token under any user name, a Kubernetes service-account token under any user name: rbac.md §9, or a CI token under any user name: rbac.md §11), with the same throttles of failed logins; the rate limit of `/v2/token` counts `GET` and `POST` alike. The credential is the form's: an `Authorization` header is not read. The token, the scopes, their bound, their reduction, the service and the answer are `GET`'s for that principal (rbac.md §4); there is no `refresh_token`. The form is read before the credential is checked, and its errors are OAuth2's (RFC 6749 §5.2, `{"error", "error_description"}`), with nothing counted as a failed login: another `grant_type`, `refresh_token` included, is `400 unsupported_grant_type`; another media type, no `grant_type`, `username` or `password`, or one of them or `service` more than once, is `400 invalid_request`; a body over 8 KiB is `413 invalid_request`, before it is read when its `Content-Length` says so. What `GET` answers too keeps `GET`'s answer: a refused credential is `401` with `WWW-Authenticate: Basic realm="lajstrom"`, a throttle `429` with `Retry-After`, more than 100 repositories `400 UNSUPPORTED`. Any other method of `/v2/token` is `405 UNSUPPORTED`. |
| `HEAD/GET /v2/<name>/manifests/<ref>` | proxied to the backend, which is asked with the manifest media types of oras-go (OCI and Docker manifests and indexes); the answer has the manifest's own `Content-Type`, `Docker-Content-Digest` and `Content-Length`. The client's `Accept` converts nothing: a manifest of a type it did not list is returned all the same. `404 MANIFEST_UNKNOWN`. On a repo with `policy.enforcePull: true` (#101), a manifest that policy refuses is `403 DENIED` once the reference has resolved, with the reason in the message and `detail` (policy.md §6.5); without it, and for every blob, tag list and referrers request, policy refuses no read. |
| `PUT /v2/<name>/manifests/<ref>` | requires `write`, or `attach` (rbac.md §2), checked before the body is read; with `attach` and without `write`, a manifest that is no referrer (below) is `403 DENIED` once the body is parsed, naming `write`, and nothing is stored (#89). **hook**: body is parsed, on every repo, an `oci` one included (`Content-Length` required, else `411 MANIFEST_INVALID`; at most `server.maxManifestSize`, default 4 MiB, else `413 MANIFEST_INVALID`; at most 1000 layers, else `400 MANIFEST_INVALID`); for a non-`oci` repo it must be an OCI image manifest that satisfies the manifest profile (an image index or a Docker manifest is `400 MANIFEST_INVALID` naming the media type the profile wants), its `org.opencontainers.image.created` must be at most `server.maxCreatedAhead` (5 minutes) after the server's clock and `server.maxCreatedBehind` (an hour) before it, unless the repo has the manifest already, else `400 MANIFEST_INVALID` naming the time and the clock (the time is refused, never replaced); every layer must be in the backend (`400 MANIFEST_BLOB_UNKNOWN` naming the digest; on a registry not classified `strong` a layer it does not serve is read again for `ingest.visibilityTimeout` first), and it is passed to `ingest.Ingest` with the `<path>` below the repo and the tag; on success the manifest is stored and indexed, `201` with `Location`, `Docker-Content-Digest` (also for the idempotent repeat of the same manifest) and, when the manifest has a `subject`, `OCI-Subject`. A push by digest is accepted when the annotations yield a tag, `Tag(lajstrom.version, lajstrom.variant)` (manifest-profile.md §4.2), which ingest sets in the backend; the `Location` names that tag; without `lajstrom.version` it is `400 MANIFEST_INVALID`. Errors: `403 DENIED` for a manifest whose coordinates or digest policy refuses (policy.md §6.3, #101), on every repo, an `oci` one included, before anything is claimed or pushed, the reason in the message; `413 SIZE_INVALID` for a file layer above the repo's `maxFileSize` (policy, #60), naming the limit; `400 MANIFEST_INVALID` with the rule that failed in `detail`, and, when the `artifactType` is oras's default `application/vnd.unknown.artifact.v1`, the hint to pass `--artifact-type application/vnd.lajstrom.<type>.v1` in the message; `409 TAG_INVALID` (tag exists with another digest, named in the message and in `detail.existing`; or the package path is another spelling's, named in the message, or is being published under another spelling: manifest-profile.md §4.1); `404 NAME_UNKNOWN` (repo not configured); `503 UNAVAILABLE` with `Retry-After: 30` when the registry took the manifest and did not serve it back within `ingest.visibilityTimeout`: nothing is indexed, the claim on the tag stays, and the same `PUT` again completes it; the message names what the registry does not serve yet, and so does `detail`: `name` (the `<name>` of the request), `digest` (the manifest's) and `missing`, with `manifest: true` when the manifest is not served by its digest, `tag` when the tag does not name it yet, `blobs` (the digests not served, at most the first 20 in digest order), `moreBlobs` (how many more blobs are not served, past those `blobs` names) and, for a referrer, `referrersOf` (the subject whose referrers do not list it), each only when it is missing (#565); the message, too, names at most 20 blobs and counts the rest, `… 981 more blobs` (#693). On an `oci` repo the manifest passes through unchanged (an OCI or Docker manifest or index media type that the body's `mediaType` does not contradict), by tag or by digest, and `oci_repos` records the repository; a push by tag holds the tag lock of the manifest (index-schema.md, Coordination). **Referrers** (manifest-profile.md §6): on a managed repo, a manifest with a `subject` and an `artifactType` that is not `application/vnd.lajstrom.*`, pushed by digest, or a manifest under cosign's legacy tag `sha256-<hex>.sig`, `.att` or `.sbom`; on an `oci` repo, any manifest with a `subject`. The facade checks `write` or `attach` and the blobs (`400 MANIFEST_BLOB_UNKNOWN`) and hands it to `ingest.PutReferrer`, which indexes nothing but a version's attachments (manifest-profile.md §6.1, after the registry served the referrer back, never refusing it for them): on a managed repo the subject must be a manifest of the repository and any other tag is refused (`400 MANIFEST_INVALID`), and the push runs under the referrers lock of the subject, the tag lock that the delete of a version holds, with the backend's referrers API or without: the subject is resolved again under it, so a referrer pushed while its subject is deleted is either deleted with it or refused after it (`400 MANIFEST_INVALID`, #806); on an `oci` repo the push runs under that lock without the referrers API (index-schema.md, Coordination). The answer is `201` with `Location`, `Docker-Content-Digest` and `OCI-Subject` for a manifest with a subject, whatever the backend sent. |
| `DELETE /v2/<name>/manifests/<ref>` | requires `delete`, checked before the reference is looked up; `ingest.Delete` (#61). On a managed repo a referrer whose attachment another attachment of the same version names as its `input` (a scan report computed from this SBOM, manifest-profile.md §6.2), by the referrer's digest or by the digest of one of its layers, is `409 DENIED` while the index has that attachment, naming the referrers that name it in the message and in `detail.reports`, and nothing changes; an attachment of the referrer itself (a layer of the same cosign legacy manifest) does not count, and the delete of the version takes them all (#89). On a managed repo a reference that names a version, its tag or its digest, deletes the version: its referrers first, each with its own before it (cosign's legacy manifests under `sha256-<hex>.sig`, `.att` and `.sbom`, the tag-schema index `sha256-<hex>`, the referrers the backend lists), then the manifest, then its rows in the index, its package's when it was the last version, and the repo's rendered indexes (`index_cache`), with one audit row `delete` that counts the referrers. Any other manifest of the repository (a referrer, a foreign manifest, any manifest of an `oci` repo) is deleted with its referrers; a referrer's attachment rows go with it (index-schema.md, Attachments), and nothing else of the index changes; the audit row is written all the same. On an `oci` repo, a delete that leaves the repository without a tag then drops its `oci_repos` row (#748). Without the backend's referrers API, the `sha256-<hex>` index of a referrer's subject is written again without it, under the referrers lock of the subject (index-schema.md, Coordination). A tag names the manifest it holds. On an `oci` repo a delete by tag of a manifest that other tags also name is `409 DENIED` naming them, and nothing changes (#284); by digest the manifest goes with every tag that names it: the distribution API has no untag. On a managed repo a manifest has one tag (manifest-profile.md §4.2). The delete counts the tags and deletes under the tag lock of the manifest (index-schema.md, Coordination), so that a tag pushed meanwhile is either counted or pushed after the delete. `202 Accepted`. A backend that cannot delete manifests (`Capabilities.DeleteManifest` false, or one that refuses the first delete: the first request that changes anything is a plain delete) is `405 UNSUPPORTED` naming the backend, and nothing changes; with `DeleteManifest` false, a reference that names a version whose OCI repository the backend does not know (its manifest and its tag list answer 404 without `MANIFEST_UNKNOWN`, reindex's `repositoryUnknown`) deletes the version's rows and writes its audit row, under the same locks and conditions, and nothing is written to the backend (#692). `404 MANIFEST_UNKNOWN` for a reference that names nothing, `400 DIGEST_INVALID` or `TAG_INVALID` for one that is neither; a delete the backend refuses for this manifest alone (Zot keeps a manifest that an image index lists) is `409 DENIED`. `DELETE …/blobs/<digest>` stays `405 UNSUPPORTED`: unused blobs are the backend's garbage collection's. Retention (`lajstrom admin retention`, #72) deletes through the same path, via `POST …/versions:delete` (§4). |
| `POST /v2/<name>/blobs/uploads/`, `PATCH`, `PUT …?digest=`, `GET`, `DELETE` | requires `write` or `attach` (the blobs of a referrer; rbac.md §2). Proxied upload session (monolithic and chunked, a single `POST ?digest=` too): Lajstrom drives the backend's own session and streams; it never buffers a blob on disk or in memory, unless the operator turns on `registries[].uploads.spoolUnknownLength` for that registry: then a chunk of unknown length (no `Content-Length`: chunked encoding) into a backend session that already holds bytes is spooled to a temporary file for one request, to learn its length and send it with `Content-Range`, bounded by the repo's `maxFileSize`, removed at once, never served from ([configuration.md](../configuration.md#registries)). It hashes the bytes as they pass and holds a blob's last byte back until the hash is known (#368), so that a blob whose bytes do not have the digest of its closing `PUT` or `POST ?digest=` never completes in the backend, whether the backend checks digests or not: a blob in one request is hashed in the algorithm of its digest, one in chunks in sha256, whose state the row keeps between chunks. A mount streams nothing and is not hashed. The upload id in the `Location` Lajstrom answers is opaque and does not depend on the replica: it is the key of a `sessions` row (kind `blob-upload`, one hour after the last request) that holds the backend's session, so any replica continues it; the backend's location never reaches the client. `Range` is what the backend holds. `?mount=&from=` is passed to the backend when `from` is in the same repo, or in another repo of the same registry that the principal may read; otherwise, and without `from`, the answer is a plain session. Errors: `404 BLOB_UPLOAD_UNKNOWN` (unknown, expired, another repo's or principal's id; a chunk after which the backend holds bytes the hash did not see, part of an earlier chunk that failed: the session ends), `416 BLOB_UPLOAD_INVALID` (a `Content-Range` that does not start at the offset), `413 SIZE_INVALID` (a blob above its bound, the repo's `maxFileSize`, else its format's default, `oci` 16 GiB (manifest-profile.md §5), or the registry's `capabilities.maxBlobSize` when that is smaller, the message naming the bound: a declared length, `Content-Length` or `Content-Range`, that would carry the session past it, before anything streams, or a body of unknown length cut where the bytes the session holds pass it, counted across its `PATCH`es and the closing `PUT`; the session ends), `429 TOOMANYREQUESTS` with `Retry-After` (#246: a new session, a `POST` without `?digest=` or a mount that does not mount, while the principal holds `server.uploadSessions.perRepo` live sessions of the repo, 256 by default, or `server.uploadSessions.perPrincipal` live sessions of all repos, 1024 by default, on the replica that answers; the message names the bound and its key, and `0` turns a bound off. A session is live while a request on it runs and for 5 minutes after the last one ended; an idle session takes no slot and stays resumable until it expires, and a request on a session that exists is never refused for the bound. The `DELETE` of a session, its closing `PUT` and an error that ends it free its slot at once. `Retry-After` is the seconds until one of the sessions counted stops counting, at most 5 while a request on one of them runs. Per replica, as the throttles of failed logins: a replica counts the sessions it has answered a request of, and one that another replica ended counts until the bound is reached and the index says it is gone. The anonymous principal opens none: it only reads, rbac.md §1), `400 DIGEST_INVALID` (the bytes do not have the digest, the backend refuses it, a chunked upload is closed with a digest that is not sha256, or the backend holds bytes the hash did not see: the session ends), `503 UNAVAILABLE` with `Retry-After: 5` (#427: a chunk that `uploads.spoolUnknownLength` spools would pass `server.spoolBudget`, the bytes the spooled chunks of the replica may hold at once; nothing of it reached the backend, and the session keeps its offset). A closing `PUT` that fails once its last chunk has begun to stream ends the session too. |
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
is over its limit. A registry that refuses a write for a limit or a quota of its own
(`provider.ErrLimit`, #357: ECR's `LimitExceededException`; a `403` or `DENIED` that speaks of a
quota or of exceeding a limit, Harbor's project quota; a `403` to a `PATCH` or to the closing
`PUT` of an upload session it had opened, Gitea's `LIMIT_SIZE_CONTAINER`), on any route, is
`403 DENIED`: the message names the limit, and nothing of the registry's own; docker and oras
show it and do not retry, and the same push fails until the operator raises the limit or frees
room. A registry that refuses Lajstrom's credentials (`401`, `403`, `UNAUTHORIZED`, `DENIED`,
but a limit's) is `502`, and any other `5xx` of the registry stays a `5xx`, with the status
text only.

Mount of a package under a repo: the name after `/v2/` is split as `<format>/<repo>/<rest>`; the
repo must exist in the control plane, else `404 NAME_UNKNOWN`. Auto-creation of repos on push is
not supported (an explicit `POST /api/v1/repos` is one call). A manifest `PUT` and the `POST` that
starts a blob upload are `400 NAME_INVALID` when the registry does not take the OCI repository
name, before anything goes to it (`provider.Capabilities.CheckName`): a name with more components
below the instance prefix than `capabilities.maxNameDepth` (#599), or longer than the registry
takes, 255 bytes with the instance prefix on distribution and most registries, 256 on ECR
(`MaxNameLength`, #661). A name of a repository the index does not have yet is also `400
NAME_INVALID` where the registry creates no repository whose name ends in a build route,
`…/build`, `…/build/<x>` or `…/build/<x>/logs|status` (`Capabilities.CheckNew`,
`capabilities.buildRoutes`, Quay, #662); a package or an `oci` repository the index has keeps
taking pushes there. The message names the limit, not the prefix.

An `oci` repo takes any manifest and blob as they come, within the bounds of the rows above (a
manifest's length and layers, a blob's size), and indexes nothing beyond its repository (the OCI
distribution-spec conformance suite runs against one, `test/conformance/`). Docker image
passthrough with what it needs beyond that (recognising images for the UI, multi-arch stitching) is
after the MVP; in the MVP only the paths above are exercised, with `oras` and `cosign` as the
OCI-native clients tested (`test/e2e/oras.sh`, `cosign.sh`).

### 2.1 Encrypted repos (#114, ADR 0009)

On a repo whose `policy.encryption.mode` is `at-rest` the facade encrypts what it stores and
answers every client in the form the client pushed (manifest-profile.md §11): the client's
manifest under the digest it computed, each blob by its plaintext digest, decrypted. The stored
manifest and the ciphertext stay reachable by their own digests, for an OCI client that holds a
recipient's key. The rows of §2 hold, with these changes:

| Method and path | Behaviour on an encrypted repo |
| --- | --- |
| `HEAD/GET /v2/<name>/manifests/<ref>` | a tag, or a digest that `blob_map` maps at the path (`kind` `manifest`): the manifest as the client had it, the stored one with the change of manifest-profile.md §11 undone, `Docker-Content-Digest` its digest and `Content-Length` its length. The stored manifest's own digest: the stored manifest as it is. A manifest stored without the change (before the repo turned `at-rest` on, or one without layers) is answered as it is under every reference. A stored manifest that cannot be answered as the client had it, a layer whose row `blob_map` lacks, is `500` naming the layer: reindex writes the row again |
| `PUT /v2/<name>/manifests/<ref>` | the hook of §2, on the manifest as the client sent it: every layer must have been uploaded into the OCI repository encrypted, a row of `blob_map` at the path (`400 MANIFEST_BLOB_UNKNOWN` naming the digest otherwise); the empty config is pushed in plain when the registry lacks it; ingest validates the plaintext and stores the manifest changed (manifest-profile.md §11) under the tag. The answer's `Docker-Content-Digest` and the digest of its `Location` are the client's digest, also for the idempotent repeat, whose stored manifest may differ from the one this push would store; `409 TAG_INVALID` names the client's digest of the existing manifest in `detail.existing`. A referrer (§2) is stored with its layers encrypted, its config in the clear (pushed decrypted when the backend has only its ciphertext) and its subject's stored digest, under the referrers lock of the stored subject; `OCI-Subject` is the subject as the client named it. A referrer under cosign's legacy tag, which names its subject by the client's digest, is `400 MANIFEST_INVALID`: an encrypted repo takes referrers through the referrers API (`cosign --registry-referrers-mode=oci-1-1`). `blob_map` gets the manifest's row in the transaction that indexes it |
| `POST`, `PATCH`, `PUT …/blobs/uploads/…` | every byte is encrypted on its way to the backend's session (AES-256-CTR from the session's offset; a data key and a nonce per session, kept in the row, the data key sealed with the master key); `Range` counts the bytes the backend holds, as many as the client sent. The hash of the client's digest is over the plaintext, the last byte held back until it is known (§2); the backend's session is closed with the ciphertext's digest. The answer names the client's digest. The close writes the blob's row of `blob_map` (the path, both digests, the size, the wrapped private options and the public options), so that a manifest that names the blob can be stored on any replica. A blob in one request is closed with its HMAC; a session that got bytes in more than one request reads its blob back from the backend once after the close, to compute it. A row that cannot be written after the backend's close is `500` and ends the session: the client uploads again. A server without the master key cannot encrypt: the start of a session, a monolithic upload and a mount are `500` naming the missing key, and nothing is written |
| `POST …/blobs/uploads/?mount=<digest>&from=<name>` | a `from` in the same repo, a row of `blob_map` at `from`'s path for `<digest>` as plaintext: the backend mounts its ciphertext, and the row is written at the target path. A `from` in another repo, or a digest without a row: a plain session (`202`), the client uploads and the blob is encrypted |
| `HEAD /v2/<name>/blobs/<digest>` | a plaintext digest with a row of `blob_map` at the path: the backend's `HEAD` of the ciphertext, answered with the plaintext digest as `Docker-Content-Digest` and `ETag`, and the size. Any other digest: §2, as the registry has it |
| `GET /v2/<name>/blobs/<digest>` | a plaintext digest with a row at the path: `format.ServeBlob` decrypts the ciphertext as it streams, never a `307`; a whole blob goes out with its last byte held back until the HMAC and the plaintext digest match, and is aborted otherwise (guarantees.md, INV-40); a `Range` goes to the backend as it is and is decrypted from the first byte of the backend's `Content-Range`, `206` with that `Content-Range`, not authenticated, as a range of a plain blob is not hashed. Any other digest, a ciphertext's included: §2, as the registry has it, a `307` allowed |
| `GET /v2/<name>/referrers/<digest>` | the referrers of the stored manifest that `<digest>` maps to (itself when `blob_map` has no row), each descriptor with the digest of its manifest as the client had it and the annotations of that manifest; keys referrers (manifest-profile.md §11.1) are left out |
| `DELETE /v2/<name>/manifests/<ref>` | a digest that `blob_map` maps names the stored manifest; the rows of `blob_map` of a deleted manifest go in the transaction of its rows |

An `oci` repo, which passes manifests through, and a group, which stores nothing, never encrypt:
the API refuses `at-rest` on them ([api.md](../api.md#policy)). A blob that a group serves from an
encrypted member is decrypted with the member's key.

## 3. Protocol frontends (MVP formats)

Authentication on all frontend endpoints: HTTP Basic with a user password, a PAT or a deploy
token, the same credentials as `/v2/token`; or `Authorization: Bearer` with a PAT or a deploy
token, which every route but `/v2` takes (npm's `_authToken` sends it). Anonymous `GET` on repos
with `anonymousRead: true`.
Writes require `write` on `<format>/<repo>`.

A document that a frontend renders, served with `format.ServeDocument` (apt's `dists/`, rpm's
`repodata/`, pypi's simple pages, maven's `maven-metadata.xml` and checksums, terraform's
`versions`), is validated by its `ETag` alone (#1255): `If-None-Match` with it is `304`, and an
`If-Range` with it gives the range. Its `Last-Modified`, to the second, may be that of the render
before a publish: `If-Modified-Since` is never `304`, `If-Unmodified-Since` is not read, and an
`If-Range` with a date gives the whole document. apt asks with `If-Modified-Since` alone, and so
fetches `InRelease` (or `Release` and `Release.gpg`) whole on every `apt-get update`, and an index
only when the new `Release` lists another hash for it.

A request that Lajstrom sends the registry for a frontend and that the registry refuses with a
status no other rule names (#494; `format.StatusOf` is the one table): a `413` stays `413`, the
file is larger than the registry takes; a `429` or `503` is `503` with the registry's
`Retry-After`, `1` when it sent none, as an open circuit breaker is: the registry throttles
Lajstrom's own credential, which every client shares, not this client; a refusal for a limit
or a quota of the registry (`provider.ErrLimit`, §2) is `403` with the message, which names the
limit; any other `4xx`, such as
a `400 MANIFEST_INVALID` for a manifest Lajstrom built, is `502`: the client sent its file, not
the request the registry refused. A `5xx` says its status text only, and no answer carries the
registry's URL, realm or message. The one exception is a publish that the registry took and did
not serve within `ingest.visibilityTimeout` (`*ingest.NotVisibleError`, #667). It is `503` with
`Retry-After: 30`, as on `/v2` (§2), and its message follows the status text with what the
registry does not serve yet, each only when it is missing: the OCI name and the manifest's
digest, then the manifest, the tag, at most 20 blobs and how many more, and the subject whose
referrers do not list it; the same request again completes it (`format.Explainer`).

A download of a file of a version that policy refuses, and an upload of such a version, are `403`
in the format's error answer, the message naming the version and the reason (`requests 2.32.0 is
denied by policy: CVE-2026-1234`, `… is quarantined until 2026-11-18T10:00:00Z`), before any
request to the registry; the version is left out of the format's indexes (policy.md §6, #101).

On an encrypted repo (§2.1) every file a frontend stores is encrypted on its way into the
registry, its metadata layer too, and every file `GET` of an encrypted layer is decrypted by
`format.ServeBlob` as it streams: never a `307`, whatever `policy.blobRedirect` says; a whole file
with its last byte held back until the HMAC matches; a `Range` decrypted from the first byte of the
registry's `Content-Range`. `ETag`, `Last-Modified`, `Docker-Content-Digest` and the indexes do not change: the
indexes carry the files' own checksums (manifest-profile.md §11). A read that needs a data key
that the repo's keys do not open is `500`; one whose key is in a KMS that does not answer is `503`
with `Retry-After: 5`, as a signature is (ADR 0020).

### raw

| Method and path | Behaviour |
| --- | --- |
| `GET /raw/<repo>/<path…>` | newest version of the file at `<path>` (the most recently created), read as the blob `GET` of the facade (§2, `format.ServeBlob`): one request to the backend with the client's `Range`, `307` to the backend's storage when it redirects and `policy.blobRedirect` allows it, else the backend's `200`, `206` or `416` streamed. `ETag` is the blob digest (`If-None-Match` is `304` without a request), `Last-Modified` the version's `created`, `Docker-Content-Digest` the manifest digest. `?digest=sha256:…` pins a version. `Content-Type` from the metadata layer or sniffed. A `PUT` of bytes an older version already holds is the idempotent repeat of that version and does not make it the newest: to roll back, `DELETE` the newer version or pin `?digest=`. |
| `HEAD` | same headers, from the backend's `HEAD`; no body, never a `307` |
| `PUT /raw/<repo>/<path…>` | upload one file (body). Creates a new version (tag) for different bytes; the path now resolves to it. `201` with `Location`, `Docker-Content-Digest` and a JSON body `{"path", "version", "digest", "size", "created"}` (`digest` the manifest's, as `Docker-Content-Digest`), also when it indexes the manifest that an earlier `PUT` of the same bytes left in the registry, failed after its push. The same bytes again are a success that stores nothing (#1129): `200` with the same headers and body, naming the version that holds them (its manifest digest and `created`), when the index has it or another `PUT` of the bytes stores it while this one waits for that `PUT`'s claim on the tag ([index-schema.md](index-schema.md), Coordination); never `409`. A claim of the tag that neither stores nor goes within `ingest.visibilityTimeout` is `503` with `Retry-After: 30`, and nothing is stored. `409` when the path exists in another spelling (the message names it). |
| `DELETE /raw/<repo>/<path…>` | requires `delete`; removes all versions of the path, each as the `DELETE` of its manifest on `/v2` deletes it, its referrers with it (after the MVP: one version). `204`; `501` when the backend cannot delete manifests; a failure after the first version names the versions deleted before it |
| `GET /raw/<repo>/` and any directory prefix with trailing `/` | JSON listing (`{"entries":[{"path","size","digest","created"}]}`), recursive: one entry per version of every path below the prefix, at any depth, sorted by path and then `created`; `digest` is the manifest digest, the value `?digest=` takes. HTML after the MVP |

Model: package = path, version = first 16 hex of the content sha256, variant none. `oras push`
to `/v2/raw/<repo>/<escaped path>:<version>` works with the same annotations.

### pypi

Implements PEP 503 (simple index HTML), PEP 691 (JSON simple API, content negotiation), PEP 658
metadata files, PEP 592 yanked files (#81), and the legacy upload API that `twine` uses, for a
hosted repo; a proxy and a group repo answer the same routes as ADR 0013 says (below), a group
passing its uploads to its hosted member. PEP 694 (the upload API 2.0) is a draft and not
implemented.

| Method and path | Behaviour |
| --- | --- |
| `GET /pypi/<repo>/simple/` | project list |
| `GET /pypi/<repo>/simple/<project>/` | file list of the project (normalised name; non-normalised redirects `301`). Links point to `/pypi/<repo>/files/…` with `#sha256=`; `data-requires-python` set, and `data-dist-info-metadata`/`data-core-metadata` (in JSON `core-metadata`/`dist-info-metadata`) on a wheel only: a build may change an sdist's `PKG-INFO` (PEP 643 `Dynamic`), and PEP 658 is for metadata the processing does not modify, as PyPI does. Every file of a yanked version (below) has `data-yanked`, the reason HTML-escaped or `""` without one, and in JSON `yanked` is the reason, or `true` without one; any other file has no `data-yanked` and `yanked: false`. |
| `GET /pypi/<repo>/files/<project>/<version>/<filename>` | the distribution file (`307` or stream) |
| `GET /pypi/<repo>/files/<project>/<version>/<filename>.metadata` | the core metadata of a wheel (PEP 658), from the metadata layer; `404` for an sdist |
| `POST /pypi/<repo>/` | legacy upload: `multipart/form-data` with `:action=file_upload`, `protocol_version=1`, `content`, metadata fields. One file = one manifest (variant = the wheel's build tag, when it has one, and tag set, or `sdist`: manifest-profile.md §4.2). `200` on success; `400` on validation errors; `409` if the file name exists with a different digest, or another file holds its version and variant (a wheel with a build tag uploaded before #726, in the earlier form of its variant), with `File already exists` and the name of the file there. |

**Yank** (PEP 592, #81). A version is yanked and unyanked through the management API (§4), since
no client has a yank route (PyPI's is its web UI); each is an entry of the package's journal
(manifest-profile.md §8), so reindex restores it. A yank applies to every file of the version,
those uploaded after it too, as PyPI yanks releases. The files stay served: pip and uv skip a
yanked version unless a requirement pins it with `==` or `===`, and then install it with a
warning. Every yank and unyank is an audit row of its own, action `pypi.yank`, `details.op`
`yank` or `unyank`, with the entry's digest. A version deleted keeps its yank in the journal: the
same version uploaded again is yanked until it is unyanked. A pypi journal manifest is never
pushed nor deleted through `/v2`, as npm's (npm, Through `/v2`).

**Proxy** (ADR 0013, proxy-and-group.md, #67). The upstream is a simple index:
`upstreams[].url` is its root as pip's `--index-url` names it (`https://pypi.org/simple/`), and
the key of a name is its PEP 503 normalized form.

- The page of a project is asked at `<project>/` below that URL, with `Accept:
  application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html;q=0.2,
  text/html;q=0.01`. An answer that is a JSON object is read as the JSON form, of the project
  (another `name` is a document that does not parse, proxy-and-group.md §6); any other as the HTML
  form: each `<a>` with an `href`, the file name the last segment of its path, the sha256 that of
  its fragment (`#sha256=…`), `data-requires-python`, `data-core-metadata` or else
  `data-dist-info-metadata`, `data-yanked`; an HTML comment is not read.
- `proxy_meta` keeps, under the one kind `simple.json`, the files the proxy serves, as a page of
  the JSON form: a wheel or an sdist (`.tar.gz`, `.zip`) of the project whose file name the
  upload route would take, with an `http` or `https` URL and its sha256, each file name once,
  and one sdist per version, the first `.tar.gz`, else the first `.zip` (the variant `sdist`
  holds one file, manifest-profile.md §4.2). Another file (`.egg`, `.exe`, one without a sha256)
  is not listed. A URL is kept as the upstream wrote it, without its fragment; one relative to
  the page is asked relative to the page.
- The page lists those files in the upstream's order, each with `url`
  `../../files/<project>/<version>/<filename>`, relative to the page as a hosted repo's, the
  version the file name's; `hashes` its sha256; `requires-python` and `yanked` as the upstream
  gave them; `size` and `upload-time` when it gave them; and on a wheel `core-metadata` and
  `dist-info-metadata` with the sha256 the upstream announced, which PEP 658 makes the sha256 of
  the `METADATA` inside the wheel. A file the repo holds has the index's sha256, size and core
  metadata, as on a hosted repo's page, and its `created` as `upload-time` when the upstream gave
  none. `meta.api-version` is `1.1` when every file has its size, else `1.0`; `versions` are the
  files' versions. The HTML form is that page converted on each request, its `ETag` the sha256
  of its bytes. Without a cached page (proxy-and-group.md §7) the page is what the repo holds, as
  a hosted repo's.
- A file is fetched on its first request, from its URL and checked against its sha256
  (proxy-and-group.md §5); its `upload-time` is its version's `created`. `<filename>.metadata`
  of a wheel is served from the index once the repo holds the wheel, which a request of it
  fetches as a request of the wheel does; of an sdist it is `404`, and nothing is fetched. The
  first request of a file waits for its fetch and its ingest before its first byte: pip waits
  15 s for a byte (`--timeout`) and retries a request 5 times, each retry waiting on the same
  fetch, and uv 30 s (`UV_HTTP_TIMEOUT`), so the first install of a larger file needs a longer
  timeout.
- The project list is the projects the repo holds, rendered for each request. An upload is `405`
  (§6). The management API yanks the versions of a hosted repo only (§4): a proxy's page has the
  upstream's yanks.

**Group** (ADR 0013, proxy-and-group.md §8, #68). A group has the routes above:

- The page of a project merges the JSON pages of the members that take part (§8.1, §8.2), the
  hosted member's own page and a proxy member's of above, in member order: `files` is their union
  by file name, a file that several members list as the first lists it; `versions` their union,
  in order; `meta.api-version` `1.1` when every page is of `1.1`, else `1.0`. The links stay
  relative to the page, so they name the group. The HTML form is converted from it, as a
  proxy's. `ETag` is the group's (§8.5) for the JSON form, the sha256 of its bytes for the HTML
  one.
- A file, and a wheel's `.metadata`, come from the first member taking part that holds the file
  or, for a proxy member, fetches it (§8.3), and are served from that member as from a hosted
  repo.
- The project list is the union of the members' (a proxy member's: the projects it holds),
  rendered for each request.
- An upload is the hosted member's, with `read` on the group and `write` on the member (rbac.md
  §7); `405` without a hosted member.

Client config: `pip install --index-url https://host/pypi/<repo>/simple/ …`,
`twine upload --repository-url https://host/pypi/<repo>/ dist/*`.

### apt

Implements the Debian repository format (`Release`, `InRelease`, `Packages`, `Sources`,
`Contents-<arch>`) for binary and source packages (#79). Suites and components are upload metadata
(annotations), not separate repos. A group of hosted apt repos has the same routes (#68, below).

| Method and path | Behaviour |
| --- | --- |
| `GET /apt/<repo>/dists/<suite>/InRelease` | clearsigned Release (per-repo GPG key) of the bytes of the `Release` of the same render; from `index_cache`, made on the first `GET` after the render and kept beside the `Release`. Signed by the repo's active key and, for `keys.retireGrace` after a rotation, by the retired key too. `404` when the server has no master key (`keys.masterKeyEnv` or `keys.masterKeyFile`): the `Release` is unsigned then, and clients mark the source `[trusted=yes]`. When a key that signs is held in a KMS ([ADR 0020](../adr/0020-repo-signing-keys-in-a-cloud-kms.md)): `503` with `Retry-After: 5` while the KMS does not answer, times out or throttles, and `500` when it refuses (an active key's access denied, disabled or deleted); either way nothing is stored and nothing unsigned is served, and a retired key whose KMS refuses is left out of the signatures |
| `GET /apt/<repo>/dists/<suite>/Release`, `Release.gpg` | `Release`: rendered with every `Packages`, `Sources` and `Contents-<arch>` of the suite in one render on the first `GET` after a publish or a delete (both invalidate the repo's documents) and kept with them in `index_cache`, so its hashes are those of the files served. `Origin: Lajstrom`, `Label` the repo, `Suite` and `Codename` the suite, `Date`, `Architectures`, `Components`, `Acquire-By-Hash: yes`, and `MD5Sum`, `SHA1` and `SHA256` of every `Packages`, `Sources`, `.gz` and `.xz`, and every `Contents-<arch>` and `.gz`. `Architectures` are those of the binary packages, never `source`, as in the Debian archive's `Release`. `Release.gpg`: the armored detached signatures of the same `Release` bytes, by the keys that sign `InRelease`; `404` without a master key; `503` and `500` as `InRelease` for a key held in a KMS. Suites, components and architectures come from the versions (`lajstrom.deb.suite`, `.component`, `.arch`), with no configuration; a suite without versions is `404` |
| `GET /apt/<repo>/dists/<suite>/<component>/binary-<arch>/Packages`, `Packages.gz`, `Packages.xz` | package index: the versions of the suite and component whose architecture is `<arch>` or `all`, sorted by name and version. The architectures of a suite are the architectures of its versions, `all` among them when it has `all` packages: `binary-all` holds the `all` packages alone, and they are also in the index of each concrete architecture, which apt merges without a warning. apt on a host of an architecture the suite has no package for reads `binary-all` alone, so the `all` packages install there (#725); the `Release` has no `No-Support-for-Architecture-all`, with which apt skips `binary-all`. A suite with `all` packages only has `binary-all` alone. Every component has an index for every architecture of the suite, empty when it has no package for it. `by-hash/SHA256/<hash>` serves the `Packages`, `.gz` or `.xz` of that directory whose SHA256 the current `Release` lists (apt uses it when `Acquire-By-Hash: yes`); a hash of an older render is `404`, and apt falls back to the file by name. A path the `Release` does not list is `404` |
| `GET /apt/<repo>/dists/<suite>/<component>/source/Sources`, `Sources.gz`, `Sources.xz` | source index (#79), for every component of a suite that has a source version (`lajstrom.deb.arch` `source`), empty for a component without one; a suite without source versions has none, and its `Release` lists none. One paragraph per source version of the suite and component, sorted by name and version: the fields of its `.dsc` as written (its metadata layer, manifest-profile.md §2), `Source` written as `Package`, then `Files`, `Checksums-Sha1` and `Checksums-Sha256` with a line for the `.dsc` itself first and then the lines of the `.dsc`'s own fields, and `Directory: pool/<component>/<p>/<source>`, the pool directory of the binaries of the same source. What `apt-get source` and `apt-get build-dep` read from a `deb-src` line. `by-hash/SHA256/<hash>` serves them as it serves a `Packages` |
| `GET /apt/<repo>/dists/<suite>/<component>/Contents-<arch>`, `Contents-<arch>.gz` | file index (#79), for every component and architecture of the suite that `Packages` has, the architectures of its binary versions: a line per path that a package of the component and of `<arch>` or `all` holds, the path, spaces, and the comma-separated names of the packages that hold it, each qualified with its control file's `Section` (`usr/bin/hello  utils/hello`), sorted by path, a name once however many versions hold the path. `Contents-all` holds the `all` packages alone, those of the concrete architectures hold them too, as `Packages` does. The paths are the `files` of the version's metadata layer (manifest-profile.md §2), every member of its `data.tar` that is not a directory; a version without `files` (uploaded before #79, or pushed without it) is listed by the paths of its `md5sums`, which lack its symbolic links and usually its conffiles. What apt-file reads through apt's index target `$(COMPONENT)/Contents-$(ARCHITECTURE)`. `<component>/by-hash/SHA256/<hash>` serves the `Contents-<arch>` or `.gz` of the component whose SHA256 the current `Release` lists, as `by-hash` does for `Packages` |
| `GET /apt/<repo>/pool/<component>/<p>/<source>/<filename>` | the `.deb`, the `Filename` of its `Packages` paragraph, or a file of a source package, the `.dsc` or a file it names, in the `Directory` of its `Sources` paragraph. The directory is the source package's, as the Debian archive lays out a pool: `<source>` is the name of the control file's `Source` field, or the package name without one, and `<p>` its first letter, or its first four when it starts with `lib` (`libf/libfoo/libfoo2_2.1-3_amd64.deb`); `<filename>` is `<package>_<version without the epoch>_<arch>.deb`, or for a source package the `.dsc`'s name, `<source>_<version without the epoch>.dsc`, and the names the `.dsc` gives its files (`hello_2.10.orig.tar.gz`, `hello_2.10-3.debian.tar.xz`). Resolved by file name: the versions of the component that hold the file, one per suite (several for an `.orig.tar.*` that two revisions of a source share), whose `<p>/<source>` is the path (else `404`; a version without metadata in the index is taken on the file name), read as the blob `GET` of the facade (§2, `format.ServeBlob`): `307` or stream, `Range`, `ETag` the blob digest, `Last-Modified` and `Docker-Content-Digest` those of the newest such version, `Content-Type: application/vnd.debian.binary-package` for a `.deb`, `text/plain; charset=utf-8` for a `.dsc`, `application/gzip`, `application/x-xz`, `application/x-bzip2`, `application/x-lzma` or `application/zstd` for a file of those compressions, `application/pgp-signature` for an `.asc`, `application/octet-stream` otherwise; `HEAD` from the backend's `HEAD`. The same file in two suites of a component is one URL, in two components two URLs for one blob. Versions that hold one file name with two blobs are `500`, logged with both digests for the operator: the upload refuses another content under a file name, an `oras push` cannot be checked |
| `GET /apt/<repo>/key.asc` | ASCII-armored public key of the repo (`signed-by=`): one public key block with the active key, made on the first request when the repo has none, and the keys retired within `keys.retireGrace`; `application/pgp-keys`, not cached. `404` without a master key. It asks a KMS only to make the repo's first key under `keys.kms`, `503` with `Retry-After: 5` while that KMS does not answer |
| `PUT /apt/<repo>/upload?suite=<suite>&component=<component>[&arch=<arch>]` | convenience upload of one `.deb` (body). Equivalent to `oras push` with the deb profile: the deb plugin builds the manifest from the control file and the query. `suite` and `component` are required, lowercase letters, digits, `.`, `+` and `-`, the first a letter or a digit (`400` naming the one missing or malformed); `arch`, when given, must be the control file's `Architecture`. A body above the repo's `maxFileSize` is `413`, one that is not a `.deb` `400`, and so is a `.deb` whose `Architecture` is `source`, the architecture of source packages. The body streams into the registry ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)): its head, up to the end of the control member, is read first for the package name, so a control member over 4 MiB is `400` before the rest is read; a refused upload stores nothing. `201` with `Location` (the file in the pool), `Docker-Content-Digest` (the manifest digest) and a JSON body `{"package", "version", "architecture", "suite", "component", "filename", "digest", "tag"}`. The same bytes again are the idempotent repeat, `201` with the same digest; the same bytes in another suite or component are a new version over the same blob. `409` for other bytes under the same name, version and architecture in the same suite and component (the tag holds another manifest), and for other bytes under a pool file name that a version of another suite, component or epoch holds, naming that version: a Debian pool holds one content per file name |
| `POST /apt/<repo>/upload?suite=<suite>&component=<component>` | upload of one source package (#79): a `multipart/form-data` body with a part per file, each with a `filename`, the field names free; the first file is the `.dsc` (at most 1 MiB, signed or not: its OpenPGP signature is kept and not checked), and the others are files it names, in any order. `suite` and `component` as for `PUT`. Equivalent to `oras push` of the source profile (manifest-profile.md): the manifest is built from the `.dsc` and the query. Each file streams into the registry as it arrives ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)), into a publication session of the source and version (`format.Sessions`), and is checked against the size, the MD5 of `Files`, the SHA1 of `Checksums-Sha1` and the SHA256 of `Checksums-Sha256` that the `.dsc` gives it: `400` naming the file and the checksum that differ. A first part that is not a `.dsc` that parses, with `Format`, `Source`, `Version`, `Files` and `Checksums-Sha256`, is `400`, and so is a part the `.dsc` does not name, a name twice, or a `.dsc` that names more than 64 files. A file the `.dsc` names and the body lacks is taken from the pool when a version of the same source holds that file name with that SHA256 (the `.orig.tar.*` of an earlier revision: its blob is in the source's OCI repository already); else `400` naming it. Every part is bounded by the repo's `maxFileSize` (`413`). The pool file name rule of `PUT` holds for every file: `409` for a file name that a version of another suite, component, source version or epoch holds with other bytes, naming that version. Nothing is published unless every file arrived and passed: the session is aborted on any failure, and a client that goes away publishes nothing. `201` with `Location` (the `.dsc` in the pool), `Docker-Content-Digest` (the manifest digest) and a JSON body `{"package", "version", "architecture", "suite", "component", "filename", "digest", "tag"}`, `package` the source name, `architecture` `source`, `filename` the `.dsc`. The same files again are the idempotent repeat, `201` with the same digest; other files under the same source, version, suite and component are the `409` of the tag |

**Group** (ADR 0013, proxy-and-group.md §8.6, #68). An apt group's members are hosted apt repos,
and the group has the routes above, its documents its own:

- The suites of the group are those of its members. A suite's documents are rendered from the
  versions of every member that has the suite, merged as below, as a hosted repo's are rendered
  from its own versions: its components and architectures are those of the versions merged, and
  every rule of the routes above holds (`binary-all`, an index for every component and
  architecture, `Sources` when a version merged is a source one, `Contents-<arch>` from the
  file lists). The `Release` has `Label` the group's name and `Description` `apt/<group>
  <suite>`, the hashes of the documents the group serves, `Date` the time of the render and no
  `Valid-Until`, as a hosted repo's. A suite that no member has is `404`.
- A version merged: of the versions of one package, version and architecture (`source` for a
  source package) in one suite and component, the first member's, in member order. A version of
  a member is left out when an earlier member holds one of its pool files (its `.deb`, or its
  `.dsc` and the files it names) under the same file name with other bytes, in any suite or
  component: the group, as a hosted repo, holds one content per pool file name, and a file its
  indexes list is the file its pool serves.
- The documents are rendered on the first `GET` after a change of a member's documents or of
  the group's members, and kept in `index_cache` under the group (proxy-and-group.md §8.6);
  they are served as a hosted repo's, by name and by hash, `ETag` the sha256 of their bytes.
- `InRelease` and `Release.gpg` are signed with the group's own keys, and `key.asc` is the
  group's keyring, made on its first request as a hosted repo's: a client's `signed-by=` names
  the group's key, not a member's. The statuses of a master key that is not set and of a key
  held in a KMS are those above.
- `pool/<component>/<p>/<source>/<filename>` is the file of the first member, in member order,
  whose own pool route resolves the path, served from that member's registry as there; `404`
  when no member has it.
- `PUT` and `POST …/upload` go to the group's member when it has one, with `read` on the group
  and `write` on the member (rbac.md §7), `Location` naming the member; a group of several
  members answers `405` with `Allow: GET, HEAD` (proxy-and-group.md §8.4), once the principal may
  read it.

Client config: `deb [signed-by=/etc/apt/keyrings/<repo>.asc] https://host/apt/<repo> <suite> <component>`, and
`deb-src [signed-by=/etc/apt/keyrings/<repo>.asc] https://host/apt/<repo> <suite> <component>` for the source packages;
a group is `<repo>` with its own `key.asc`.

### maven

Implements the Maven 2 repository layout: release versions, and the builds of SNAPSHOT versions
in a repo whose `policy.maven.versionPolicy` is `snapshot` ([api.md](../api.md#policy), #80). A
repo takes one kind or the other (ADR 0004): a release repo refuses a SNAPSHOT build, a snapshot
repo a release, each with `400` naming the key, at the first file of a deploy before it is stored
and on every publish, `/v2` included (manifest-profile.md §5). A change of the policy applies to
the deploys after it; the versions the repo holds stay, served as before.

A SNAPSHOT build is a version of its own (manifest-profile.md §4.2): Maven 3 and Gradle deploy
`1.0-SNAPSHOT` as unique snapshot versions, each deploy's files named with its timestamp and
build number, `lib-core-1.0-20261114.101112-3.jar`, in the directory of the SNAPSHOT version,
`com/example/lib-core/1.0-SNAPSHOT/`. Each deploy is one more build, under its own tag, and
`1.0-SNAPSHOT` is never a tag: the latest build is what the `maven-metadata.xml` of the
SNAPSHOT version names, rendered from the index. Builds are deleted as versions are, by the
management API and `lajstrom admin retention --keep-builds` ([operations.md](../operations.md#retention)).

| Method and path | Behaviour |
| --- | --- |
| `GET /maven/<repo>/<groupPath>/<artifactId>/<version>/<file>` | artifact, POM, or checksum (`.sha1`, `.md5`, `.sha256`, `.sha512` computed at ingest and stored in the metadata layer). A build's file is at `<base>-SNAPSHOT/<artifactId>-<base>-<timestamp>-<n>[-<classifier>].<extension>`. The non-unique name of a SNAPSHOT version, `<base>-SNAPSHOT/<artifactId>-<base>-SNAPSHOT[-<classifier>].<extension>` (and its checksums), is the file of that classifier and extension in the latest build that has one, by Maven's version order (the timestamp, then the build number), looked up at each request and never a tag: Maven asks for it when the version's `maven-metadata.xml` has no entry for the file, and it is what a `curl` of the latest build fetches. `404` when no build has it |
| `GET /maven/<repo>/<groupPath>/<artifactId>/maven-metadata.xml` (+ checksums) | rendered from the index: `latest`, `release`, `versions`, `lastUpdated`. The builds of a SNAPSHOT version are listed once, as the SNAPSHOT version, `1.0-SNAPSHOT`, which may be `latest` and is never `release` |
| `GET /maven/<repo>/<groupPath>/<artifactId>/<base>-SNAPSHOT/maven-metadata.xml` (+ checksums) | the builds of the SNAPSHOT version, rendered from the index into `index_cache` (scope `<groupId>/<artifactId>/<base>-SNAPSHOT`) as Maven 3 writes the document: `modelVersion="1.1.0"`, `groupId`, `artifactId`, `version` (the SNAPSHOT version), and `versioning` with `snapshot` (the `timestamp` and `buildNumber` of the latest build), `lastUpdated` (that build's timestamp as `yyyyMMddHHmmss`) and `snapshotVersions`: one `snapshotVersion` per classifier and extension that a build has, from the newest build that has it, with its `classifier` (absent when none), `extension`, `value` (the build's version) and `updated` (the build's timestamp), sorted by extension, then classifier. This is the document Maven's own merge of the deploys leaves; it depends on the rows alone, so every replica and every reindex renders the same bytes. `404` without a build |
| `PUT /maven/<repo>/<groupPath>/<artifactId>/<version>/<file>` | adds the file to the publication session `(repo, g, a, v)` of the principal, opened by the first file; another principal's open session of the version is `409`, and so is a `groupId:artifactId` whose package path another spelling holds (the message names it; manifest-profile.md §4.1), before the file is stored. A session holds at most the repo's `maven.maxSessionFiles` files, 256 by default ([api.md](../api.md#policy)): another file is `400` naming the bound and that key, before it is stored, while a file of the session sent again replaces it (#246). The bound counts the files the session stores, signatures (`.asc`, `.asc.asc`) and `.sigstore.json` included; a checksum file, of any file (a `.asc` or a `.module` too), and `maven-metadata.xml` with its checksums are never stored and never counted. It is a bound of one session, not of the version: a deploy of a version that exists opens a session of its own, which commits as the idempotent repeat when its files are the version's and is `409` otherwise. A file of a SNAPSHOT build goes to the session of the build, the version of its file name (`g:a:1.0-20261114.101112-3`); a file named with the SNAPSHOT version itself (`lib-core-1.0-SNAPSHOT.jar`, Maven 2's non-unique snapshots) is `400`, and so is a file of the kind the repo's policy does not take, before either is stored. A checksum file (`.md5`, `.sha1`, `.sha256`, `.sha512`) is verified against the file it names, as the session computed it when the file streamed in, and is never stored: `400` when it differs, or when its file was not uploaded first. One that differs also keeps the session from committing until its file is uploaded again: mvn logs the `400` of a checksum and goes on to `maven-metadata.xml`, which is then `400` naming the file, and nothing is published. The file streams into the registry ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)): nothing of it is written to a local disk, and one refused while it arrives (`maxFileSize`, a body shorter than its `Content-Length`) is not stored. `201`. |
| `PUT /maven/<repo>/<groupPath>/<artifactId>/maven-metadata.xml` (+ checksum PUTs) | **commit** of the principal's session of the version the document is for, its `release` (else `latest`), which mvn and Gradle set to the version they deploy; never another version it lists, whose session would be an unfinished deploy. Builds the manifest with all files, ingests, closes the session: `201`. Idempotent re-PUT of an identical version: `200`, also without a session when the version is in the index. Different bytes for an existing version: `409`, and the session is aborted. No session and no such version: `400`. A document for a SNAPSHOT version commits nothing, as the version's own document committed its build: `200` when the index has a build of it, `400` otherwise. The checksums of `maven-metadata.xml` are accepted and not checked, and the metadata of a group (plugin prefixes) is accepted and not kept: Lajstrom renders its own. |
| `PUT /maven/<repo>/<groupPath>/<artifactId>/<base>-SNAPSHOT/maven-metadata.xml` (+ checksum PUTs) | **commit** of the principal's session of the build the document is for: the one its `versioning/snapshot` names, `<base>-<timestamp>-<buildNumber>`, which mvn and Gradle set to the build they deploy, else the newest `value` of its `snapshotVersions`. Its `version`, when it has one, must be the directory's (`400`). Then as the artifact's document: `201`, `200` for the repeat, `409` and the session aborted for other bytes under the build, `400` without a session or a build. Mvn and Gradle PUT it after the files and before the artifact's document. |

A session expires 30 minutes after its last request, or after the repo's `maven.sessionTimeout`
(policy, #60, `1m` to `24h`), and its files are dropped: nothing is published but by the commit. There is no `seal`,
which would publish what had arrived, a version missing files (decided on #53). Client config:
`<repository>` with `<url>https://host/maven/<repo></url>` and a `<server>` entry with
user/token in `settings.xml`.

**Proxy** (ADR 0013, [proxy-and-group.md](proxy-and-group.md), #67). A proxy repo of maven
mirrors a Maven 2 repository, its upstream's URL the root of the layout
(`https://repo.maven.apache.org/maven2/`), and has the `GET` routes above:

- A `maven-metadata.xml`, of an artifact (`proxy_meta` kind `maven-metadata`) or of a SNAPSHOT
  version (kind `maven-metadata:<base>-SNAPSHOT`), is the upstream's at the same path, kept and
  asked again as proxy-and-group.md §4 says, and served as Lajstrom writes the Maven metadata
  model: `groupId`, `artifactId`, `version`, `versioning` (`latest`, `release`, `versions`,
  `lastUpdated`, `snapshot`, `snapshotVersions`) and `plugins`, each when the upstream's has it.
  So the plugin prefixes of a group, which have the path of an artifact's document
  (`org/apache/maven/plugins/maven-metadata.xml`), pass through. The versions the repo's
  `policy.maven.versionPolicy` does not take are left out (a release repo lists no SNAPSHOT
  version, a snapshot repo no release), and `latest` and `release` are the upstream's while they
  are listed, else chosen among the versions listed as a hosted repo chooses them. A SNAPSHOT
  version's document in a release repo is `404`, and so is a file of a version of the kind the
  repo's policy does not take: nothing is asked of the upstream for them. Without a document
  (proxy-and-group.md §7) it is what the repo holds, rendered as a hosted repo's. Its `.md5`,
  `.sha1`, `.sha256` and `.sha512` are of the bytes served.
- A file of a version is fetched on its first request (proxy-and-group.md §5). The document that
  lists it is the upstream's `.sha1` of the file (kind `file:<version directory>/<file name>`,
  `file:1.2.0/lib-core-1.2.0.jar`), the lowercase hex of its first word; a `404` there is the
  file's `404` and a negative row. Its sha1 is the checksum the fetcher verifies. A file whose
  `.sha1` the upstream does not have is `404`, but a signature (`.asc`, of any file): one that the
  upstream publishes without a `.sha1` is fetched and served unchecked, as the upstream serves it,
  over the transport of every fetch (TLS to an `https` upstream) and within the same bounds
  (`policy.maxFileSize`, `capabilities.maxBlobSize`), and its manifest has no
  `lajstrom.origin.checksum` (manifest-profile.md §3.1); a forged signature fails its
  verification by itself. A signature with a `.sha1` is checked as every file. Each file fetched is a version of its own, of that one file, whose variant is its
  extension and classifier (manifest-profile.md §4.2): the repo holds the files that clients
  asked for, not every file of the version.
- A checksum file of a file (`.md5`, `.sha1`, `.sha256`, `.sha512`) is served from the metadata
  layer of the file the repo holds, as on a hosted repo; a file it does not hold is fetched
  first.
- The non-unique name of a SNAPSHOT version is the file of the build that the SNAPSHOT
  version's document names for that classifier and extension (`snapshotVersions`, else
  `snapshot`'s timestamp and build number), fetched as that build's file. `404` when it names
  none.
- A `PUT` is `405` with `Allow: GET, HEAD`.

**Group** (ADR 0013, proxy-and-group.md §8, #68). A group has the routes above:

- The `maven-metadata.xml` of an artifact merges the members' documents that take part (§8.1,
  §8.2), in member order: `versions` their union in Maven's version order, `latest` the greatest
  of the members' `latest` in that order, `release` likewise of their `release`, `lastUpdated`
  the latest, the plugins their union by prefix, a prefix the first member's; `groupId` and
  `artifactId` the first member's. A SNAPSHOT version's document takes `snapshot` and
  `lastUpdated` from the member whose latest build is the newest (its timestamp, then its build
  number), and `snapshotVersions` is their union by classifier and extension, each the one with
  the newest `updated`, the first member's on a tie. A hosted member's documents are rendered
  from its index as for its own URL. The checksum files of a document are of the merged bytes.
- A file comes from the first member taking part that holds it (a hosted member's version holds
  it without a variant) or, for a proxy member, that fetches it (§8.3); its checksum files from
  the metadata layer of the version that member holds. The non-unique name of a SNAPSHOT version
  is resolved through the merged document. A proxy member does not have a SNAPSHOT version's
  document that its policy does not take; one asked for a file of a version its policy does not
  take refuses it at the ingest (proxy-and-group.md §5, step 9), and the group answers from the
  other members.
- A `PUT`, a file, a checksum or a `maven-metadata.xml`, is the hosted member's, with `read` on
  the group and `write` on the member (rbac.md §7), in the member's publication sessions; `405`
  without a hosted member.

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
  group's own, ADR 0013). A new `externalURL` needs the documents reset (`reindex`). On a repo
  host it is served under that host, `https://<repo host>/<name>/-/<file>` (§1.1).
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
format's: `400 MANIFEST_INVALID`) nor deleted (`409 DENIED`: an entry goes with its package;
the same for a pypi package's journal);
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
| `GET /rpm/<repo>/repodata/repomd.xml.asc` | the armored detached signatures of the bytes of the same render's `repomd.xml`, one per key that signs (the repo's active key and, for `keys.retireGrace` after a rotation, the retired one), made on the first `GET` after the render and kept beside it; signed again when the `Signer` no longer calls them current, as apt's `InRelease`. `application/pgp-signature`. `404` without a master key (`keys.masterKeyEnv`): clients set `repo_gpgcheck=0` then. `503` with `Retry-After: 5`, or `500`, as apt's `InRelease` for a key held in a KMS ([ADR 0020](../adr/0020-repo-signing-keys-in-a-cloud-kms.md)): nothing unsigned is served |
| `GET /rpm/<repo>/repodata/repomd.xml.key`, `GET /rpm/<repo>/key.asc` | the ASCII-armored keyring of the repo, as apt's `key.asc`: the active key, made on the first request when the repo has none, and the keys retired within `keys.retireGrace`; `application/pgp-keys`, not cached. zypper fetches `repomd.xml.key` beside a `repomd.xml.asc` by itself; dnf takes either as `gpgkey=`. `404` without a master key; `503` as apt's `key.asc` for a first key under `keys.kms` |
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

### terraform (#66, ADR 0023)

Implements Terraform's module registry protocol (`modules.v1`), which Terraform and OpenTofu
read, for a hosted repo: the service discovery, the versions of a module and its download, whose
archive Lajstrom serves itself. A repo is a Terraform namespace: the module address is
`<host>/<repo>/<name>/<system>`, `art.example.com/infra/vpc/aws`. `<name>` is letters, digits,
`-` and `_`, 1 to 64 of them, beginning and ending with a letter or a digit; `<system>` is
lowercase letters and digits, 1 to 64: Terraform's grammar of an address. A repo whose name has a
`.` is no namespace: Terraform refuses its addresses before it asks, and the routes serve the
repo to other clients all the same. The provider registry protocol (`providers.v1`),
`login.v1` (`terraform login`), `.zip` archives, proxy repos of registry.terraform.io and groups
are not served. OpenTofu 1.10 and later also installs a module from an `oci` repo, with an
`oci://` source through the facade (§2), without this protocol.

| Method and path | Behaviour |
| --- | --- |
| `GET /.well-known/terraform.json` | the service discovery of the host, to any principal: `{"modules.v1":"/terraform/"}`, `application/json`. Terraform reads it over `https://` at the root of the host of a module address. No other service is listed. On a repo host (§1.1) the path is the repo's, below |
| `GET /terraform/<repo>/.well-known/terraform.json` | `read`. The service discovery of a repo host: `{"modules.v1":"/"}`, the root of the repo host; on the main host, which no client asks, `{"modules.v1":"/terraform/<repo>/"}`. A module address on a repo host keeps the repo as its namespace, `infra.art.example.com/infra/vpc/aws`, so Terraform asks the routes of `<namespace>` below |
| `GET /terraform/<repo>/<name>/<system>/versions` | `read`. `{"modules":[{"versions":[{"version":"1.2.0"},…]}]}`: every version of the module, by SemVer precedence, lowest first, pre-releases included (Terraform installs one only when a constraint names it), `application/json`. From `index_cache` (scope the package path, kind `versions`), rendered on the first `GET` after a publish or a delete, as raw's listing. The module is looked up by its package path (manifest-profile.md §4.1), so `VPC/aws` finds `vpc/aws`. `404` for a module the repo does not have |
| `GET /terraform/<repo>/<name>/<system>/<version>/download` | `read`. `204` with `X-Terraform-Get`, the path of the version's archive (the next rows), `/terraform/<repo>/<name>/<system>/<version>/<file>`, which Terraform resolves against the URL of the download; on a repo host it is written without `/terraform/<repo>` (§1.1). For a principal that is not anonymous the path ends with `?token=<token>`, a bearer token of rbac.md §4 that downloads this version and nothing else, for 20 minutes: Terraform fetches the archive with go-getter, which sends the credentials of `~/.netrc` and not those of the `credentials` block. `<version>` is a version the versions route lists, `1.2.0`; `404` for one the module does not have |
| `GET /terraform/<repo>/<namespace>/<name>/<system>/versions`, `GET /terraform/<repo>/<namespace>/<name>/<system>/<version>/download` | the two rows above, where `<namespace>` is the repo's own name: the routes Terraform asks on a repo host. Any other `<namespace>` is `404` |
| `GET /terraform/<repo>/<name>/<system>/<version>/<file>`, `HEAD` | the archive: `<file>` is the title of the version's file layer, `<name>-<system>-<version>.tar.gz` (any other is `404`), read as the blob `GET` of the facade (§2, `format.ServeBlob`): `307` or stream, `Range`, `ETag` the blob digest, `Last-Modified` and `Docker-Content-Digest` those of the version, `Content-Type: application/gzip`; `HEAD` from the backend's `HEAD`. It needs `read` on the repo, or, in place of credentials, the `token` of the query that a download of this version wrote, checked before the version is looked up: a token that does not verify, has expired or downloads another version counts as none, and the answer is the denial of rbac.md §3 (`401` with the Basic challenge to the anonymous principal) |
| `PUT /terraform/<repo>/<name>/<system>/<version>` | `write`. One module archive (body), a gzip-compressed tar. Equivalent to `oras push` with the terraform profile: the plugin builds the manifest from the path. `<version>` is a SemVer 2.0.0 version without build metadata and without a leading `v` (`1.2.0`, `2.0.0-rc.1`), else `400`: Terraform orders `1.0.0+a` and `1.0.0+b` as one version, and asks for a version as its library prints it. A `<name>` or `<system>` outside the grammar above is `400`. A body that is not a gzip-compressed tar archive whose entries are paths without a leading `/` or a `..` component, of regular files, directories and links whose target is such a path, a regular file among them, is `400`, at most 2^18 entries and 1 GiB unpacked; a body above the repo's `maxFileSize` is `413`. The body streams into the registry ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)) and passes the archive's checks on its way; a refused upload stores nothing. `201` with `Location` (the archive's path), `Docker-Content-Digest` (the manifest digest) and a JSON body `{"namespace", "name", "system", "version", "filename", "digest", "tag"}`. The same bytes again are the idempotent repeat, `201` with the same digest. `409` for other bytes under a version of the module (the tag holds another manifest), and for a name another spelling of which the repo holds, naming it (manifest-profile.md §4.1) |

A version is immutable while it exists. It is deleted as in every format:
`DELETE /api/v1/repos/terraform/<repo>/packages/<name>%2F<system>/versions/<version>` or the
`DELETE` of its manifest on `/v2`; the next versions document is rendered without it. `oras push`
to `/v2/terraform/<repo>/<name>/<system>:<tag>` with the terraform profile works too.

Client config: a `credentials` block in the CLI configuration (`~/.terraformrc`, or the file of
`TF_CLI_CONFIG_FILE`), whose token, a personal access token or a deploy token of the repo with
`read`, Terraform sends as `Authorization: Bearer`; or the variable `TF_TOKEN_art_example_com`.
None for a repo with anonymous read.

```hcl
credentials "art.example.com" {
  token = "<token>"
}
```

```hcl
module "vpc" {
  source  = "art.example.com/infra/vpc/aws"
  version = "~> 1.2"
}
```

## 4. Management API `/api/v1`

JSON, bearer (PAT) or Basic, or `DPoP` with the token of a web UI's sign-in (below). Shapes are in [api.md](../api.md) and `internal/api/openapi.yaml`;
listed here so frontends do not invent their own. What each call needs, in the actions of rbac.md
§2 (decided on #136, 2026-11-09):

| Calls | Needs |
| --- | --- |
| reading a repo's content: `GET /api/v1/repos` (lists only the repos the caller may read), `GET …/repos/<format>/<repo>`, `GET …/packages` and `GET …/packages/<name>/versions` (#72), `GET …/packages/<name>` (#958), `GET …/packages/<name>/versions/<version>` (#136), `GET …/versions/<version>/attachments` (#89), `GET …/versions/<version>/manifest` and `…/referrers` (#951) | `read` on the repo; a repo the caller may not read is `404`, as on the frontends (rbac.md §3) |
| searching: `GET /api/v1/search` (#956) | nothing; it searches the repos the caller may read, as `GET /api/v1/repos` lists them, and a repo it may not read adds no hit (rbac.md §3) |
| changing a repo: `PATCH`, `DELETE`, `POST …/reindex`, `GET …/reindex`, the deploy tokens of the repo | `manage` on the repo |
| deleting versions: `DELETE …/repos/<format>/<repo>/packages/<name>/versions/<version>` (#61), `POST …/repos/<format>/<repo>/versions:delete` (#72) | `delete` on the repo, checked before the body is read; a repo the caller may not read is `404` |
| yanking and unyanking a pypi version: `PUT` and `DELETE …/repos/pypi/<repo>/packages/<name>/versions/<version>/yank` (#81) | `write` on the repo, as an npm deprecation; a repo the caller may not read is `404` |
| a repo's webhooks: `GET`/`POST …/repos/<format>/<repo>/webhooks`, `GET`/`PATCH`/`DELETE …/webhooks/<id>`, `POST …/webhooks/<id>/secret`, `POST …/webhooks/<id>/test`, `GET …/webhooks/<id>/deliveries` (#77) | `manage` on the repo, decided before the repo is looked up (rbac.md §3); the webhooks whose mask is exactly the repo |
| a repo's open publication sessions: `GET …/repos/<format>/<repo>/sessions` (#960) | `manage` on the repo, decided before the repo is looked up (rbac.md §3) |
| every token, `GET /api/v1/tokens` (#959), and the registries, `GET /api/v1/registries` (#952) | the admin role, checked before anything else (rbac.md §3) |
| a repo's policy: `POST /api/v1/policy/rules` and `DELETE …/rules/<id>` of a rule whose mask is exactly the repo, `POST` and `DELETE …/packages/<name>/versions/<version>/deny` (#101) | `manage` on the repo; a rule's permission is decided once the body is read, a deny's before the repo is looked up |
| `GET /api/v1/policy/rules?repo=`, `GET /api/v1/policy/verdicts`, `GET /api/v1/policy/denylist` (#101) | `read` on the repo; the lists hold the rows of the repos the caller may read |
| creating a repo, and the system resources: users, bindings, audit, the deploy tokens over a mask (`/api/v1/deploy-tokens`, #911; the role checked before the body is read), the webhooks of any mask under `/api/v1/webhooks` (#77), the policy rules of any other mask and the list of every rule (`policy`, #101) | the admin role (rbac.md §2; a `maintainer` on `*/*` is not enough) |
| the personal access tokens of a user | the user themself, within what the credential allows, or the admin role |
| the requests that wait for a second admin: `GET /api/v1/approvals`, `GET …/<id>`, `POST …/<id>/approve`, `POST …/<id>/reject` (#1285) | the admin role, checked before anything else; an approval takes the admin's password or a web UI's fresh sign-in through OIDC, and an admin other than the requester (rbac.md §13) |
| `GET /api/v1/whoami`, `GET /api/v1/info` (#136), `GET /api/v1/openapi.yaml`, `GET /api/v1/policy/key` (#101) | nothing |
| `POST /api/v1/bootstrap` (#440) | nothing: the one-time bootstrap token in the body is the credential; `404` once an enabled user has an admin binding, and while `auth.localPasswords` is `false` (rbac.md §6) |
| `POST /api/v1/login` (#70) | nothing: the password, or the OIDC code and its verifier, in the body is the credential, with a DPoP proof of the key the token is bound to; the request's `Authorization` is ignored |
| a user's own password: `POST /api/v1/users/<name>/password` with `currentPassword` (#1281) | nothing: the current password in the body is the credential, checked as a Basic password is, with the throttles and the lockout; the request's `Authorization` is ignored (rbac.md §1, §3) |

| Resource | Endpoints |
| --- | --- |
| info | `GET /api/v1/info` |
| repos | `GET /api/v1/repos[?format=]`, `POST /api/v1/repos`, `GET/PATCH /api/v1/repos/<format>/<repo>`, `DELETE /api/v1/repos/<format>/<repo>[?force=true]`, `POST …/reindex`, `GET …/reindex`, `DELETE …/upstream-cache[?name=]` (§6) |
| packages, versions | `GET /api/v1/repos/<format>/<repo>/packages[?prefix=&q=&attr=&facet=&sort=&order=&limit=&cursor=]`, `GET …/packages/<name>`, `GET …/packages/<name>/versions[?limit=&cursor=&without=]`, `GET …/packages/<name>/versions/<version>[?variant=]`, `GET …/packages/<name>/versions/<version>/attachments[?variant=&kind=&predicateType=&verified=&limit=]`, `GET …/packages/<name>/versions/<version>/manifest[?variant=]`, `GET …/packages/<name>/versions/<version>/referrers[?variant=&artifactType=]`, `DELETE …/packages/<name>/versions/<version>[?variant=&force=]`, `POST /api/v1/repos/<format>/<repo>/versions:delete`, `PUT` and `DELETE …/packages/<name>/versions/<version>/yank` (pypi) |
| search | `GET /api/v1/search?q=[&format=&limit=&cursor=]` (#956) |
| users | `GET /api/v1/users[?kind=&limit=&cursor=]`, `POST /api/v1/users`, `GET/PATCH/DELETE /api/v1/users/<name>`, `POST …/password`, `POST …/unlock`; a service account is a user of kind `service` (rbac.md §1). `POST …/password` takes `{"password"}` from the admin role, or `{"currentPassword", "password"}`, the user's own change (#1281, rbac.md §1, Local passwords): `204`; a password that breaks a rule of `auth.passwords` is `400` naming the rule, and in the user's own change one of its last four passwords is `400` too. `PATCH` takes `mustChange`, `true` or `false`, for a user with a password (`400` otherwise), and a user whose password must be changed is answered with `mustChange: true`, left out otherwise. While `auth.localPasswords` is `false`, a `password` in `POST /api/v1/users` and `POST …/password` are `400`, naming the setting (rbac.md §1, Local passwords). `POST …/unlock` (#1280) ends the user's lock of the lockout and sets its count of failures to `0`: `204`, with a `user.unlock` audit row when a lock was in force, and without one when none was. A user is answered with `lastSeenAt`, its last activity (`null` before any record), and, while `auth.disableInactiveUsersAfter` is not `0`, `inactive: true` when it is inactive (rbac.md §1, Inactive users); while `auth.lockout.duration` is not `0`, a locked user with `lockedUntil`, the end of its lock, left out otherwise. None of the three is taken in a body, and none moves the user's revision: its `ETag` names what a `PATCH` changes, and a `304` of `If-None-Match` may stand for an older `lastSeenAt`, `inactive` and `lockedUntil` (the revisions below) |
| tokens | `GET/POST /api/v1/users/<name>/tokens` (PAT), `DELETE …/tokens/<id>`; `GET/POST /api/v1/repos/<format>/<repo>/deploy-tokens`, `DELETE …/<id>`; the deploy tokens over a mask (rbac.md §1): `GET /api/v1/deploy-tokens[?repoMask=]`, `POST /api/v1/deploy-tokens` (`repoMask` in the body), `DELETE /api/v1/deploy-tokens/<id>`. A deploy token's expiry is within `auth.deployTokens.maxTTL` when it is set, a service account's within `auth.serviceAccounts.maxTokenTTL` (rbac.md §1, #1286): created without an expiry, it expires at the bound; an `expiresAt` or `expiresIn` beyond is `400` naming the field, the bound and the latest expiry. Every view of a token answers a live one beyond the bound of its kind with `beyondMaxTTL: true`, and leaves the field out otherwise, and a token of a web UI's sign-in through OIDC with `signIn`, `{authTime, amr, acr}`, each when kept (rbac.md §10, #1276). With `auth.approval: second-admin`, a `POST /api/v1/deploy-tokens` whose `scopes` hold `write` is `202` with the request that waits and the token's `secret`, which is no credential until the request is approved (approvals, below; rbac.md §13). `POST /api/v1/users/<name>/tokens` and `POST /api/v1/deploy-tokens` take a password as the request's credential, or a web UI's fresh sign-in through OIDC in its place (rbac.md §12, #1323) |
| bindings | `GET /api/v1/bindings[?principal=&limit=&cursor=]`, `POST /api/v1/bindings`, `DELETE /api/v1/bindings/<id>`; a binding's principal is `user:<name>`, `user:<uid>`, `group:<name>`, or a `cert:` or `k8s:` principal or pattern (rbac.md §3, §8, §9); a `ci:` principal has none (rbac.md §11). With `auth.approval: second-admin`, a `POST` of a binding of the role `admin` or on `*/*` is `202` with the request that waits (approvals, below; rbac.md §13), and makes no binding until it is approved |
| approvals (#1285, rbac.md §13) | `GET /api/v1/approvals[?state=&limit=&cursor=]`, newest first, `GET /api/v1/approvals/<id>`, `POST …/<id>/approve`, `POST …/<id>/reject`. A request is `{id, kind, change, changeReason, state, requestedBy, requestedAt, expiresAt}`, with `decidedBy`, `decidedAt`, `decisionReason` and `result` once decided: `kind` `binding` or `deploy-token`, `change` the binding (`principal`, `role`, `repoMask`) or the deploy token (`name`, `repoMask`, `scopes`, `expiresAt`), `state` `pending`, `approved`, `rejected` or `expired`, `result` what the approval made (`binding:<id>`, `token:<id>`). The `POST` of a change that waits answers `202` with the request and its `Location`, and for a deploy token its `secret`, shown once; the same change pending twice is `409`. `approve` and `reject` take no body, and answer `200` with the request decided; an approval by the requester, or without the admin's password or a fresh sign-in, is `403`; a request decided or expired, and an approval whose change no longer holds (the principal gone, the binding made meanwhile, the requester disabled), are `409`; an unknown id `404`. A request has no revision, and the routes ignore `If-Match` |
| audit | `GET /api/v1/audit[?repo=&principal=&action=&state=&since=&until=&limit=&cursor=]` |
| publication sessions | `GET /api/v1/repos/<format>/<repo>/sessions[?limit=&cursor=]` |
| every token | `GET /api/v1/tokens[?kind=&user=&repo=&repoMask=&live=&limit=&cursor=]` |
| registries | `GET /api/v1/registries` |
| policy (#101, [policy.md](policy.md)) | `GET /api/v1/policy/rules[?repo=]`, `POST /api/v1/policy/rules`, `DELETE /api/v1/policy/rules/<id>`, `GET /api/v1/policy/verdicts?digest=&repo=`, `POST` and `DELETE /api/v1/repos/<format>/<repo>/packages/<name>/versions/<version>/deny[?variant=]`, `GET /api/v1/policy/denylist`, `GET /api/v1/policy/key` |
| CI trust policies (rbac.md §11, #937) | `GET /api/v1/ci/policies`, `POST /api/v1/ci/policies`, `GET/PUT/DELETE /api/v1/ci/policies/<name>`, `POST …/<name>/renew`, `POST …/<name>/approve`; the admin role. A policy is `{name, issuer, claims, grants, expiresAt}`, answered with `source` (`config` or `api`), `state` (`active`, `pending` or `expired`; the configuration's `active`) and, for the API's, `renewal` (`{expiresAt}`, a renewal that waits), `createdBy`, `createdAt`, `updatedBy`, `updatedAt`, `approvedBy` and `approvedAt`, and its revision as `ETag` (the revisions below). `POST` answers `201` with its `Location`; `PUT` replaces `issuer`, `claims`, `grants` and `expiresAt`, its body's `name`, when present, the path's; `renew` takes `{expiresAt}` or no body; `approve` takes no body and requires `If-Match`, the ETag of the revision approved (`400` without, or with `*`); `DELETE` `204`. A rule of rbac.md §11 broken, and an `expiresAt` of a policy that reads, past, or more than 180 days ahead, are `400` naming the field; a step of a policy that writes missing (a fresh sign-in with a second factor, a mask within `auth.ci.apiWriteRepos`, an approver who did not make the change) `403`, but a web UI's sign-in through OIDC that is not fresh or shows no second factor, which is asked for more: `401` with `insufficient_user_authentication` (below; rbac.md §10, #1296); a change, renewal or approval of a policy of the configuration, a `POST` with a name that is taken, a renewal of a policy that reads or waits, an approval when nothing waits or after the expiry, and a change made meanwhile, `409`; an unknown name `404` |
| webhooks (#77, [webhooks.md](webhooks.md)) | a repo's: `GET/POST /api/v1/repos/<format>/<repo>/webhooks`, `GET/PATCH/DELETE …/webhooks/<id>`, `POST …/webhooks/<id>/secret`, `POST …/webhooks/<id>/test`, `GET …/webhooks/<id>/deliveries[?limit=&cursor=]`; every mask: the same under `/api/v1/webhooks` |
| whoami | `GET /api/v1/whoami`; for a user, its current groups too (rbac.md §8) |
| bootstrap | `POST /api/v1/bootstrap` |
| login | `POST /api/v1/login` |
| the API's description | `GET /api/v1/openapi.yaml` |

Errors: `{"error": {"code": "NOT_FOUND|CONFLICT|UNAUTHENTICATED|FORBIDDEN|INVALID|UNAVAILABLE|INTERNAL", "message": "…"}}`.
A `401` (no credentials, or credentials refused, with the Basic challenge, or the DPoP challenge
of a web UI's token, below, or a sign-in asked for more, below) is `UNAUTHENTICATED`,
a `403` (the caller lacks the action) `FORBIDDEN` (#579), and so is the `403` of a write the
registry refuses for a limit or a quota of its own (`provider.ErrLimit`, §2; a prefetch), whose
message names the limit. A `429` (a throttle of failed logins,
with `Retry-After`; #130) is `UNAVAILABLE`: try again later. A `412` (a conditional delete that
found the version changed, or an `If-Match` that names no revision the resource has) is
`CONFLICT`, as `409` is. There is no eighth code.

Revisions (#796): a repo, a user, a webhook and a CI trust policy of the API (#937) have a
revision, the `revision` of their row (index-schema.md), which every write of the row moves but
the record of a user's activity (`lastSeenAt`, above) and the writes of the lockout (`lockedUntil`,
above). The
`GET` of one answers it as a strong, opaque `ETag`, and so do the `201` of its `POST` and the `200`
of its `PATCH`, or of a policy's `PUT`, renewal and approval. `If-Match` on its `PATCH`, `PUT`,
`DELETE`, renewal and approval makes the change conditional: it is made only while the resource
has a revision the header lists (strong comparison), or exists (`*`), else `412 CONFLICT` with the
`ETag` now, and nothing changes; the comparison is in the transaction that writes, so that of
two changes with one tag one is made. `If-None-Match` on the `GET` answers `304` when it lists
the `ETag` (weak comparison) or is `*`. Without the headers nothing changes from before; a value
that does not parse is `400`. Lists, bindings and tokens have no revision, and their routes
ignore the headers; a version's `DELETE` keeps its own `If-Match` of digests (below).

A reason (#1285, rbac.md §13): a control-plane change, as rbac.md §13 lists them, takes
`changeReason`, 1 to 1024 bytes of UTF-8 without control characters, as a field of its JSON body,
or as the query parameter `?changeReason=` on a route without a body (a `DELETE`, an approval, a
rejection, an unlock). It is never `reason`, which a policy rule's and a deny's bodies hold with
their own meaning. One outside its bounds, or both forms in one request, is `400` naming it.
With `auth.requireReason: true`, a change without one is `400 INVALID` naming `changeReason`,
after the role is checked and before anything is written. The audit row has it as
`details.changeReason` (rbac.md §5).

Lists of a repo's content (`read`): `GET …/packages` pages the packages, by name unless
`?sort=` says otherwise (below); `GET …/packages/<name>/versions` pages the versions,
newest first by the publish time of their first variant, each with its variants (`variant`,
`tag`, manifest `digest`, `publishedAt` and the facts below), `publishedAt` and `lastModified`
(of the first and the newest variant), its `pointers` (npm dist-tags) and, on a yanked pypi
version only, `yanked` (`{"reason": "…"}`, `""` without one). The publish time is
`org.opencontainers.image.created`, `versions.created_at`, which reindex rebuilds. Both pages are
`?limit=` and `?cursor=`; the cursor holds the place of the page's last row (a name, or a sort's
value and a name; a time and a version), never an offset.

The package list (#955) takes these too, and every one that is given holds:

- `?prefix=`: the packages whose name starts with it;
- `?q=`: the packages whose name, or the summary of one of whose manifests
  (`versions.summary`, manifest-profile.md §2), contains it: its ASCII letters in either case,
  every other character as it is;
- `?attr=<key>:<value>`, repeatable: the packages with a manifest whose `attrs` (`versions.attrs`)
  have the key with the value. The key is the annotation's full key (`lajstrom.deb.suite`), and
  the first `:` ends it. The values given for one key are alternatives; the keys all hold, on
  one manifest;
- `?sort=`: `name` (the default), `updated` (the publish time of the package's newest
  manifest), `versions` (how many versions it has) or `size` (the size of the file layers of all
  its manifests), with `?order=` `asc` or `desc`, by default `asc` for `name` and `desc` for the
  others. Packages with the same value follow by name, in the same order; names compare as
  bytes;
- `?facet=<key>`, repeatable: the answer adds `facets`, for each key the values the manifests of
  the repo have for it, each with the number of packages that have a manifest with it, in byte
  order, at most 1000 values per key. They count the whole repo: the other parameters do not
  change them.

At most 16 `attr` and 16 `facet`. A key that is not `lajstrom.` followed by letters, digits,
`.`, `_` and `-`, an `attr` without `:`, a `sort` or an `order` that is none of its values, and a
cursor of another sort are `400`.

A package of the list has `name`, `nameOci` (`packages.name_oci`), `createdAt` and `updatedAt`
(the publish times of its first and its newest manifest), `latestVersion` (below),
`latestVariants` (the variants of that version but `""`, in byte order; `[]` when it is `null`),
`lastPublished` (the first version of its versions list, `null` without versions), `versions`
and `manifests` (how many of each it has), `size` (the sizes of the file layers of all its
manifests added up, a blob two manifests share twice), `summary` and `attrs` of its head
manifest, the first variant in byte order of `latestVersion`, or of `lastPublished` when it is
`null`, and `pointers`: the pointers of its journal state that name one of its versions, each
`{"kind", "name", "version"}`, by kind and name (npm's dist-tags; `[]` for the other formats).
`summary` is `null` when the head manifest has none. It is all read from the derived tables, so
reindex rebuilds it. A group has no packages: its list is empty, its facets too.

`latestVersion` is the version that the format's client installs by default, among the
package's versions, and `null` when it installs none:

- npm: the `latest` dist-tag as the packument has it (§3, npm): the journal's when the package
  has its version, else the highest release by SemVer precedence, else the highest version;
- maven: the `release` of the artifact's `maven-metadata.xml` (§3, maven), the highest version
  in Maven's order that is not a pre-release; `null` when every version is one;
- pypi: the highest by PEP 440 of the versions that are PEP 440 versions, as pip picks one: of
  those that are not pre-releases (a pre- or a development release), or of all of them when
  every one is; then the highest that is not yanked (the journal's state), or the highest
  yanked one when all are;
- apt and rpm: the highest by dpkg's order and by rpm's (`[epoch:]version[-release]` by
  rpmvercmp) of the versions they read;
- raw, whose client has none: `lastPublished`.

One package (`read`, #958): `GET …/packages/<name>`, with `<name>` as the delete below has it,
answers the package's row of that list, `ociRepository` (as for one version, below) and
`latestMetadata`, the metadata layer of the head manifest (`null` when it is absent or omitted
for size). `404` when the repo or the package does not exist, and for a group.

`?without=<kind>` on the versions (#89) keeps the variants that have no attachment of that kind
in `attachments` (index-schema.md), whatever its verification: `signature`, `sbom`, `scan`,
`provenance`, `attestation` or `other` (manifest-profile.md §6.1), each `?without=` one more
condition that the variant must meet (`?without=sbom&without=signature`: neither). A version is
listed with those of its variants, and its times and its place in the order are theirs, so the
cursor of such a page is for the same filter. Another value is `400`. It is a job's question,
"which versions has no scanner seen yet", answered from the derived tables: an attachment pushed
past Lajstrom counts after the next reindex of its repo.

The facts of a variant in the list of versions (#1132) are the index's, so reindex rebuilds them:
`artifactType`, `attrs` (`versions.attrs`, by annotation key) and `degraded` of its `versions`
row; `size`, its `files` rows' sizes added up, and `fileCount`, their number; and `attachments`,
its rows in `attachments` (index-schema.md) counted by `kind`: `signature`, `sbom`, `scan`,
`provenance`, `attestation` and `other` are always there, `0` without rows, and a kind a newer
binary wrote is counted under its own name. A degraded variant has them all: they come from its
manifest's annotations and layers, not from its metadata layer. The facts of a page are one
statement of the index over the page's variants, never one per row.

One version (`read`, #136): `GET …/packages/<name>/versions/<version>[?variant=]`, with
`<name>` and `<version>` as the delete below has them, answers the version's row of that list
(`version`, `publishedAt`, `lastModified`, `pointers`, `variants` with their facts, and `yanked`
when it has one) and `ociRepository`, the OCI
repository below the instance prefix that holds the manifest of its first variant: the `<name>`
of §2, `<format>/<repo>/<package path>`, whose package path is `index.ManifestPath` of the
variant's row, its `oci_repo`, else the package's `name_oci`. The variants are in the order of
their names; those of a version split across two forms of its package path (index-schema.md,
Invariants and rules) may be at both. Each variant adds its own `ociRepository` and the rest of
what the index has of its manifest: `metadata` (the metadata layer; `null` when it is absent or
omitted for size), `indexedAt` and `files`, its file layers in manifest order, each with `name`,
`mediaType`, `digest`, `size` and `attrs` (the layer's annotations).
`?variant=` keeps one variant (`""` is the variant of a format that has none); the version's times
and pointers stay those of all its variants. It is all read from
the derived tables (index-schema.md), so reindex rebuilds it. The manifest itself and its referrers
are live registry reads and not part of it: they are the two routes below (#951), and
`/v2/<ociRepository>/…` of a variant serves both too (§2). A file's protocol URL is not part of
it either: the client builds it from the format, as for its usage snippets. The answer is `404`
when the repo, the package, the version or the variant does not exist, and for a group, which has
no versions.

The attachments of one version (`read`, #89):
`GET …/packages/<name>/versions/<version>/attachments[?variant=&kind=&predicateType=&verified=&limit=]`,
with `<name>`, `<version>` and `?variant=` as the version above, answers
`{"attachments": [...], "truncated": false}`: the rows of the version's variants in
`attachments` (index-schema.md), its signatures, SBOMs, scan reports and attestations,
newest first, every result kept. The order is `createdAt` descending, the rows without one
after the others, then by `digest` and `layer`. Each row has `variant`, `subject` (the
variant's manifest digest), `digest` (the referrer's), `layer`, `mediaType`, `artifactType`,
`kind`, `predicateType`, `size`, `annotations`, `verification` and `indexedAt`, and when it has
them `tag` (cosign's legacy tag), `tool`, `toolVersion`, `database`, `input`, `signedBy` (the
trust entry of a verified row), `reason` (why it is not verified) and `createdAt`. `?kind=` and
`?predicateType=` keep the rows with that value, `?verified=true` the verified rows and
`?verified=false` the others; a `kind` or `verified` that is none of its values is `400`. At
most `?limit=` rows, 1000 by default and at most, the newest; `truncated` says that there are
more. It is read from the derived tables, so reindex rebuilds it, and a referrer pushed past
Lajstrom is listed after the next reindex of its repo. `404` as for the version. The referrers
themselves stay on `/v2/<ociRepository>/referrers/<digest>` (§2) and `…/referrers` below; this
list is their labelled index, without a registry read, and the list of versions counts its rows
(#1132).

The manifests of one version (`read`, #951):
`GET …/packages/<name>/versions/<version>/manifest[?variant=]`, with `<name>`, `<version>` and
`?variant=` as the version above, answers `{"manifests": [...]}`, one entry per variant in
variant order, each with `variant`, `digest`, `mediaType`, `size` and `manifest`, the manifest as
JSON. The index gives each variant's digest and OCI repository; the manifest is read through the
provider (`Repository.Resolve` and `Fetch` by that digest, at most `server.maxManifestSize`) and
checked against the digest. Its exact bytes are `/v2/<ociRepository>/manifests/<digest>` (§2).
An answer of one variant has `ETag`, its digest quoted, and `If-None-Match` with it is `304`
without a registry read; an answer of several has none. Every answer has `Cache-Control:
no-cache`. `404` as for the version, and when the registry does not have a variant's manifest,
naming its digest; `503 UNAVAILABLE` when the registry does not answer.

The referrers of one version (`read`, #951):
`GET …/packages/<name>/versions/<version>/referrers[?variant=&artifactType=]`, with `<name>`,
`<version>` and `?variant=` as the version above, answers `{"referrers": [...]}`: the manifests
whose `subject` is a variant's manifest, per variant in variant order and in the provider's
order within one, each with `variant`, `subject` (the variant's manifest digest), `digest`,
`mediaType`, `artifactType`, `size` and `annotations` (`{}` without them), as the provider lists
them: `Repository.Referrers`, the backend's referrers API or the `sha256-<hex>` tag schema where
it has none (`Capabilities.Referrers`, manifest-profile.md §6), both with `referrers.alwaysTag`.
`?artifactType=` keeps the referrers of that type. It is live, one entry per referrer manifest: a
referrer pushed past Lajstrom, or one whose rows ingest could not write, is there before a
reindex indexes it. A variant whose OCI repository the registry does not have has none. `404` as
for the version; `503 UNAVAILABLE` when the registry does not answer.

Search (#956): `GET /api/v1/search?q=[&format=&limit=&cursor=]` searches the repos the caller
may read, as `GET /api/v1/repos` lists them, of `?format=` when it is given. A repo it may not
read adds no hit, and nothing in the answer tells whether one exists (rbac.md §3); a group adds
none either, its members are searched as repos of their own. It answers `{"hits": [...],
"next": "…"}`, the hits in ranks, and within a rank in byte order of the repo's format, the repo's
name, the package's name, then the version, the variant and the file's name:

1. when `q` is a digest's prefix, `sha256:` or not followed by 8 to 64 hex digits in either case:
   the manifests whose digest starts with it (`versions.digest`), then the file layers whose
   digest does (`files.digest`), each a hit of `kind` `version`;
2. the packages whose name is `q`, then those whose name starts with it, then those whose name
   contains it, then those with a manifest whose summary contains it, each a hit of `kind`
   `package` in the first of these ranks it is in. They compare as `?q=` of the package list does.

A `version` hit has `format`, `repo`, `name`, `version`, `variant`, `digest` (the manifest's) and
`matched`, `manifest` or `file`; a file's adds `file`, `{"name", "digest"}`. A `package` hit has
`format`, `repo`, `name`, and `latestVersion`, `summary` and `updatedAt` as the package list has
them. The page is `?limit=` and `?cursor=`, the cursor the last hit's rank and place; there is no
`total`. `q` is required, at most 256 bytes: none or a longer one is `400`. It reads the derived
tables only.

Server information (nothing, #136): `GET /api/v1/info` answers what a client needs before it
signs in:

- the build: `version`, `commit` and `date`, as `/-/version` has them;
- `externalURL`: `server.externalURL`, or `""` on the dev stack, which may leave it empty;
- `anonymousRead`: `auth.anonymousRead`, the server-wide switch;
- `formats`: the format plugins the server serves, by `name`, each with its `artifactType`.
  `oci` repos need no plugin and are not listed;
- `authModes`: how a person signs in. `password` is a local user's password, over Basic and to
  `POST /api/v1/login`, left out while `auth.localPasswords` is `false` (rbac.md §1, #1280);
  `token` is a personal access token; `oidc`, while `auth.oidc` is on, the
  OIDC login (#69);
- `login`: `{"tokenLifetime", "idleTimeout"}`, the seconds of `auth.login.tokenLifetime` and of
  `auth.login.idleTimeout` (`0`: off; #1277): the server has `POST /api/v1/login`, whose token is
  DPoP-bound, and with `oidc` in `authModes` its `/-/oidc/login` takes `return_to`,
  `code_challenge` and `dpop_jkt` (§7). A server without them answers no `login`, and one of a
  release before #1277 no `idleTimeout`, which is off. A UI warns the person before the idle bound
  (rbac.md §10): the server may refuse up to a minute before it;
- `adminMFA`: `true` while `auth.admin.requireMFA` is on (rbac.md §12, #1276), left out
  otherwise: the admin role counts only for a web UI's sign-in through OIDC with a second factor;
- `approval`: `auth.approval`, `none` or `second-admin` (rbac.md §13, #1285), and
  `requireReason`: `auth.requireReason`, so that a UI asks for a reason and expects the `202` of a
  change that waits;
- `caller`: the request's principal, as `GET /api/v1/whoami` answers it, and `admin`, whether
  the request's credential reaches the admin role (a personal access token with scopes does not,
  nor, under `adminMFA`, any credential but a web UI's sign-in with a second factor); and
  `adminNeedsMFA: true` when the caller has an `admin` binding that the request's credential does
  not reach under `adminMFA`, left out otherwise, so that a UI offers a sign-in with a second
  factor.

Unlike whoami, a request without credentials gets `200`, with the caller `anonymous`. Credentials
that are refused get `401`, as on every route.

A browser (#136): the API sets no cookie and reads none. A web UI signs in with
`POST /api/v1/login` (below), which answers a personal access token bound to a key that the
browser holds (DPoP, RFC 9449), or by creating one: `POST /api/v1/users/<name>/tokens` with the
user's password over Basic, since a token creates none, or with a fresh sign-in through OIDC
(rbac.md §12). It sends the bound token as
`Authorization: DPoP` with a proof of the key on every request (below), any other token as
`Authorization: Bearer`, and signing out revokes it (`DELETE …/tokens/<id>`, with the token
itself). A token the person already has is sent as a bearer. The OIDC login (#69) mints a
personal access token too, and the UI gets a bound one from `POST /api/v1/login` with the code its
callback hands over (§7). Its cookie `lajstrom_oidc` has the path `/-/oidc/` and never
authenticates `/api/v1`.

- **Where the token is kept** is the UI's choice. Any script of the origin can read any storage
  of it. No answer of Lajstrom on that origin runs a script ([operations.md](../operations.md),
  What Lajstrom answers browsers). A bound token is of no use without its key, which the UI keeps
  where no script can read it out (a non-extractable WebCrypto key): a script of the origin can
  still sign proofs with it while the page is open.
- **The browser's own Basic handling.** The UI sends its requests with `credentials: "omit"`, so
  that the Basic challenge of a `401` opens no browser dialog and no cached Basic credentials go
  along.
- **No CSRF token.** The browser adds a bearer to no request by itself. The one ambient
  credential is cached Basic, and `server.crossOrigin` refuses it on an unsafe request from
  another site, but for an origin of `server.corsOrigins` on `/api/` (below).

Same origin (#136): the UI is served from the host of `server.externalURL`, by its own
container at `/ui/` behind the same edge (§1; lajstrom-ui ADR 0001). While `server.corsOrigins`
is empty, the default, that is the only origin: no answer of `/api/v1` has an
`Access-Control-*` header, and `OPTIONS` is `404 NOT_FOUND`, as is every method a route does not
have. A page of another origin can neither read an answer nor send a request with a bearer.

Another origin (#949): `server.corsOrigins` (configuration.md) lists the origins of a UI served
from elsewhere, object storage behind a CDN for one, exactly as a browser sends them in
`Origin`. On every path under `/api/` and on `/-/version`, on the main host:

- a request whose `Origin` is a listed origin gets, on its answer, whatever its status, the
  `401`, `403` and `429` of the middleware included: `Access-Control-Allow-Origin` with that
  origin, and `Access-Control-Expose-Headers: Date, Retry-After, WWW-Authenticate,
  X-Request-Id` (the challenge of a DPoP refusal, RFC 9449 §7.1, is in `WWW-Authenticate`;
  `Date` is the server's clock, by which a UI on a laptop whose clock is off sets the `iat` of
  its proofs, as no nonce does it, rbac.md §10);
- its preflight, an `OPTIONS` with `Access-Control-Request-Method`, is `204` before
  authentication, whatever the path and the method it asks for, with
  `Access-Control-Allow-Origin`, `Access-Control-Allow-Methods: GET, HEAD, POST, PUT, PATCH,
  DELETE`, `Access-Control-Allow-Headers: Authorization, Content-Type, DPoP` (the proof of a
  web UI's sign-in, #1170) and `Access-Control-Max-Age: 600`;
- no answer has `Access-Control-Allow-Credentials`: the UI sends its bearer with
  `credentials: "omit"`, and no page of another origin reads an answer that cached Basic
  credentials or a cookie made;
- `server.crossOrigin` lets a listed origin's unsafe requests through on `/api/`, as the server's
  own. On every other path they are refused as another site's.

While the list is not empty, every answer of these paths has `Vary: Origin`. An origin the list
does not have gets no `Access-Control-*` header, and its `OPTIONS` is `404`, as with an empty
list. The frontends, `/v2`, `/-/oidc/` and the other system endpoints answer no CORS to any
origin: a link to a file or to the login is a navigation, which needs none.

Signing in (#70, [ADR 0022](../adr/0022-web-ui-sign-in-with-short-lived-tokens.md), rbac.md
§10): `POST /api/v1/login` takes no credential; the request's `Authorization` header is ignored,
so that a UI whose stored token was revoked is not refused before it signs in. It takes a DPoP
proof (RFC 9449 §4) in the header `DPoP`, checked as below but for `ath`, which it needs not:
the token it answers is bound to the proof's key. A login without a proof, or whose proof is
refused or used already, is `400 INVALID`, the message saying why, before its credential is
checked. Its body is one of:

- `{"username", "password"}`: a local user's password, checked as Basic credentials are, with
  the same throttles of failed logins and the same lockout (rbac.md §1). A refusal is
  `401 UNAUTHENTICATED`, whatever the reason (an unknown, disabled or password-less user, a wrong
  password, a locked account, a token in place of the password), its message the same whatever
  the reason, saying while `auth.lockout.duration` is set that 10 wrong passwords in a row lock a
  password, and for how long; a throttled account or address is `429` with `Retry-After`. While
  `auth.localPasswords` is `false`, every password is `401`, the message saying that passwords
  are off. A right password whose user must change it (rbac.md §1, #1281) is `403 FORBIDDEN`,
  the one `403` of a login, and the UI offers the change;
- `{"code", "codeVerifier"}`: the code that the callback of an OIDC login with `return_to` handed
  to the UI (§7), and the PKCE verifier (RFC 7636, 43 to 128 characters of `A-Z a-z 0-9 - . _ ~`)
  whose S256 challenge started that login; the proof's key must be the one whose thumbprint that
  login named in `dpop_jkt` (RFC 9449 §10). A code that is unknown, used, expired, of another
  verifier or of another key is `400 INVALID`, and a wrong verifier or key uses the code up; a
  user disabled or deleted since the callback is `401`.

A body with neither, both, or another field is `400`. Either way the answer is `201` with the
new personal access token as `POST …/tokens` answers it (`CreatedToken`: `id`, `user`, `name`,
`expiresAt`, `dpopJkt`, …, and `secret`, shown once), with `tokenType: "DPoP"` (the `token_type`
of RFC 9449 §5), and its `Location`, `/api/v1/users/<name>/tokens/<id>`. The token has no scopes
and expires after `auth.login.tokenLifetime` (configuration.md), and after
`auth.oidc.tokenLifetime` for an OIDC code when that is shorter. There is no refresh: the UI signs
in again. No answer sets a cookie.

The DPoP-bound token (RFC 9449, decided on #1170, 2026-11-16): the token of `POST /api/v1/login`
is bound to its proof's key; `dpopJkt` is that key's JWK SHA-256 thumbprint (RFC 7638, base64url;
the `cnf.jkt` of RFC 9449 §6.1). It authenticates on `/api/` only, as `Authorization: DPoP
<token>` with one proof in the header `DPoP` on every request:

- the proof is a JWT whose header has `typ` `dpop+jwt`, `alg` `ES256`, the one algorithm taken,
  and `jwk`, a public P-256 key (`kty` `EC`, `crv` `P-256`, `x`, `y`; a private member is
  refused), and whose signature verifies with that key; at most 4096 bytes;
- its claims: `htm`, the request's method; `htu`, the request's URI without its query and
  fragment, that is `server.externalURL` and the request's path, the URL that the browser sees
  behind the edge (with `server.externalURL` empty, the dev stack, the request's `Host`), the two
  compared after the syntax- and scheme-based normalization of RFC 3986 §6.2.2 and §6.2.3 (the
  case of the scheme and the host, percent-encoding, dot segments, the default port, an empty
  path as `/`); `iat`, within 5 minutes of the server's clock, before or after; `jti`, 1 to 256
  bytes, used once (below); `ath`, the base64url SHA-256 of the token;
- the proof's key is the token's: its thumbprint is the token's `dpopJkt`;
- a proof is used once on any replica: the sha256 of the key's thumbprint and the `jti` is kept
  until `iat` and 5 minutes (index-schema.md, `dpop_proofs`), and a proof found there is
  refused;
- no server nonce (RFC 9449 §9): no answer has `DPoP-Nonce`, and a `nonce` claim is ignored. A
  UI whose clock is off takes the server's from the `Date` of an answer, exposed to a listed
  origin (above), and sets `iat` by it.

A refused proof is `401 UNAUTHENTICATED` with `WWW-Authenticate: DPoP
error="invalid_dpop_proof", error_description="<why>", algs="ES256"`; a token that is unknown,
revoked, expired, of a disabled user, bound to another key or to none (a token that is not a
sign-in's, sent as `DPoP`) is `401` with `WWW-Authenticate: DPoP error="invalid_token",
algs="ES256"`. A token that passes those and whose proof verifies, but that is idle (rbac.md §10,
`auth.login.idleTimeout`, #1277), is `401 UNAUTHENTICATED` with `WWW-Authenticate: DPoP
error="invalid_token", error_description="idle", algs="ES256"`, the message saying that the
sign-in was idle, and the server revokes it. The UI reads `idle` to say why it signs the person
out: a token that is unknown, revoked or expired, sent as `DPoP`, has no `error_description`.
A token that passes those, whose proof verifies and whose sign-in through OIDC does not meet a
step that needs a fresh one or one with a second factor (rbac.md §10, §11, §12; #1276, #1296) is
`401 UNAUTHENTICATED` with `WWW-Authenticate: DPoP error="insufficient_user_authentication",
error_description="<what is missing>", algs="ES256"` (RFC 9470 §3), with `max_age=600` when the
step needs a fresh sign-in, the message saying what is missing; the token is not revoked, and the
UI signs in again with `prompt=login` (§7) and repeats the request. Any other credential that
such a step refuses gets `403 FORBIDDEN`. A bound token is refused
everywhere else, `401` as credentials refused: as
`Authorization: Bearer` or in Basic credentials on `/api/`, with
`WWW-Authenticate: DPoP error="invalid_token", …` (RFC 9449 §7.2); on the frontends, `/v2` and
`/v2/token`, in any form, with the route's challenge. `Authorization: DPoP` outside `/api/` is
refused alike. A bound token is so of no use to docker, pip or npm, nor to whoever copies it off
the browser without the key.

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

Yanking a pypi version (`write`, #81): `PUT …/packages/<name>/versions/<version>/yank`, with
`<name>` and `<version>` as the delete above has them and an optional body `{"reason"}` (1 to
4096 bytes of UTF-8; no body, or a `reason` absent, `null` or `""`, for none), appends a `yank` to the package's journal
(manifest-profile.md §8); `DELETE …/versions/<version>/yank` appends an `unyank`. Both answer
`204`, and both are idempotent: a yank of a version yanked with the same reason, or an unyank of
a version that is not yanked, appends nothing and writes no audit row; a yank with another reason
appends one that replaces it. A yank names the whole version: `?variant=` is `400`. `404
NOT_FOUND` when the repo has no such package or version; `400 INVALID` on a repo that is not a
hosted pypi repo, or for a reason that breaks the rule; `503 UNAVAILABLE` with `Retry-After`
when the registry does not serve the new entry within the visibility timeout, and the retry
completes it. The audit row is the journal's (`pypi.yank`, §3, pypi), with the request id in
`details`, as an npm deprecation's.

Reindex of one repo (`manage`): `POST …/reindex` with an optional body `{"prune", "verify"}`,
both `false` by default (as `lajstrom reindex --repo`), starts the run on the replica that
received it and answers `202` with the run
(`id`, `state`, `source`, `principal`, `replica`, `startedAt`), before the run reads the
registry; a running run that overlaps it, on any replica or in the CLI, is `409 CONFLICT` naming
where it runs and since when (index-schema.md, Coordination). `GET …/reindex` answers the latest
run that covered the repo, its own or a run of every repo, with `state`, the times, `error` and
the repo's part of the report (layout.md, `reindex.Report`); `404` when no run has covered it.
The run writes the audit event `repo.reindex` when it starts.

A repo (`read`, #957): `GET /api/v1/repos` and `GET …/repos/<format>/<repo>` answer, beside
the repo's settings, what the request may do on it and what the index holds of it, read when the
request is answered:

- `permissions`: the actions of rbac.md §2 that the evaluation of rbac.md §3 allows the
  request's principal on the repo, its credential's scopes included: of `read`, `write`,
  `delete` and `manage`, those it has, in that order. A group's are its own, whatever its
  members allow (rbac.md §7).
- `stats`: what the derived tables hold of the repo (index-schema.md), so that reindex rebuilds
  it: `packages`; `versions`, the distinct versions of its packages; `manifests`, its rows of
  `versions`, one per variant of a version; `files`, their file layers; `size`, the sum of those
  layers' sizes, a blob of two manifests counted twice and the metadata layer not at all, which
  is not what the registry stores; `lastPublishedAt`, the newest publish time of its manifests
  (`versions.created_at`), `null` without one. A group and an `oci` repo index no versions and
  have no `stats`.
- `signingKeys`: an apt or rpm repo's OpenPGP keys (index-schema.md, `keys`), their public facts
  only: the active key first, then the retired ones, the most recently retired first, each with
  `fingerprint` (uppercase hexadecimal), `algorithm` (`ed25519`, `rsa2048`, `rsa3072`, `rsa4096`
  or `ecdsa-p256`), `createdAt` and `retiredAt` (`null` for the active key). It is `[]` before
  the repo's first signed document makes its first key, and on a server that signs nothing. A
  repo of another format has no `signingKeys`.

The list reads `stats` for all its repos in one query, not one per repo. The state of the repo's
reindex is not part of it: `GET …/reindex` answers that, to `manage`.

A repo's open publication sessions (`manage`, #960):
`GET …/repos/<format>/<repo>/sessions[?limit=&cursor=]` pages, by id, the rows of `sessions`
(index-schema.md) of the repo whose state is `open` and whose time is not up: the deploys of a
maven repo that wait for their `maven-metadata.xml` (§3, maven), never a blob upload of the
facade (§2). Each has `id`; `key`, `<groupId>:<artifactId>:<version>`; `principal`, `user:<uid>`
or `token:<id>`, and for a user `principalName`, its name now; `files`, the files stored so
far in the order they first came, each with `name` (its path below the repo), `digest` and
`size`; `refused`, when there are any, the files a checksum file disagreed with, by name, with
the reason; `committing`, `true` while a commit runs; `createdAt`, `updatedAt` (its last change)
and `expiresAt`. A session that committed, was aborted or expired is not listed: the audit log
has the publish, and the others' files were dropped. A repo of a format that opens no session,
and a group, have none; a repo that does not exist is `404`. No route aborts a session: it ends
with its commit, or at `expiresAt`.

Every token (the admin role, #959):
`GET /api/v1/tokens[?kind=&user=&repo=&repoMask=&live=&limit=&cursor=]` pages, by id, which is
the order of their creation, every token: the personal access tokens of every user, a deleted
user's under the name it had, the deploy tokens of every repo and those over a mask, the revoked and expired ones
included. Each is the resource of the other token lists ([api.md](../api.md#tokens)): never
its secret, and of its hash `hashPrefix` only. `kind` keeps `pat` or `deploy`; `user` the
personal access tokens of the live user of that name or uid; `repo`, `<format>/<repo>`, that
repo's own deploy tokens, not those over a mask that matches it; `repoMask` the deploy tokens
over that mask as it was written (`oci/*` does not list a token over `*/*`); `live=true` those
neither revoked nor expired when the request is answered; with `beyondMaxTTL` on each live token
beyond the bound of its kind (rbac.md §1, #1286), `kind=deploy&live=true` lists what a new
`auth.deployTokens.maxTTL` leaves to replace. The filters add up. A user or a repo
that does not exist has none; a `kind` that is neither, a `live` that is not a boolean and a
`repo` or `repoMask` that is not two segments are `400`. The personal access token a web UI
creates at a sign-in (above) is listed as any other: the server does not tell them apart.

The registries (the admin role, #952, #953): `GET /api/v1/registries` answers
`{"replica", "registries": [...]}`, the registries of the configuration by name, as the replica
that answers knows them, and that replica (`replicas.id`, index-schema.md). Each has:

- `name`, `type` (`generic` or `ecr`), `url` and `prefix`, as `registries[]` has them; never
  `auth`, nor any part of a credential;
- `vendor`, the product the provider's probe detected, as `lajstrom registry probe` names it
  (`unknown` for one that the vendor table does not know), and `support`, the tier of that
  vendor in this release ([registries/README.md](../registries/README.md#support-tiers)):
  `ci`, `manual`, `hosted` or `untested`; both `null` before a probe of the replica answered;
- `capabilities`: the provider's capabilities (layout.md, `provider.Capabilities`) after the
  vendor table and `registries[].capabilities`, as ingest and the facade use them; `null` before
  a probe of the replica succeeded;
- `repos`: the repos on the registry, `<format>/<name>`, by format and name;
- `status`: what the replica's readiness ping (§5) and the registry's circuit breaker know:
  `checkedAt`, the time of the last ping, `null` for a provider that is not pinged;
  `answeredAt`, the last ping answered `200`, `null` before one; `latencyMs`, how long the last
  ping took; `message`, the error of the last ping when it failed, as the provider words it,
  which names neither a credential nor the instance prefix, else `null`; `breaker`, `closed`,
  `open` or `half-open`; and `state`: `down` when no ping was answered `200` for a minute, as
  `/-/readyz` counts it, or the breaker is open; else `degraded` when the last ping failed or
  the breaker is half-open; else `ok`.

The answer sends no request to a registry. `/-/readyz` stays public and without detail (§5).

Webhooks (#77; events, payload and delivery in [webhooks.md](webhooks.md)). Under a repo
(`manage`), a webhook's mask is the repo: `POST …/webhooks` takes `{"url", "events",
"active", "description"}` (`active` `true` by default) and answers `201` with the webhook and its
`secret`, the one time it is shown; `GET …/webhooks` lists the webhooks of that exact mask, by id;
`GET …/webhooks/<id>` answers one, `404` for an id that is not of the repo; `PATCH` changes `url`,
`events`, `active` and `description` (`mask` is `400`, it cannot change); `DELETE` deletes it and
its delivery log, `204`; `POST …/webhooks/<id>/secret` makes a new secret, answered once, and the
old one signs nothing from then on; `POST …/webhooks/<id>/test` sends a `webhook.ping` now and
answers `200` with the row of the delivery log it wrote, `delivered` or `failed`; `GET
…/webhooks/<id>/deliveries` pages its delivery log, newest first. Under `/api/v1/webhooks` (the
admin role, checked before the body is read) the same routes take any mask in the body of `POST`,
`{"mask", …}`, and list every webhook. A URL that webhooks.md §5 refuses is `400` naming `url`;
`POST` and `…/secret` without a master key, and `…/test` too, are `501 UNAVAILABLE` naming
`keys.masterKeyEnv`.

Policy (#101; rules, verdicts and the deny list in [policy.md](policy.md) §7). `POST
/api/v1/policy/rules` takes `{"repoMask", "name", "version", "variant", "effect", "reason",
"expiresAt"}` and answers `201` with the rule; a field that policy.md §1.3 refuses is `400` naming
it. `DELETE /api/v1/policy/rules/<id>` answers `204`. `GET /api/v1/policy/rules` lists by id.
`GET /api/v1/policy/verdicts` without `digest` and `repo` is `400`. `POST …/versions/<version>/deny`
takes `{"reason", "validUntil"}` and answers `201` with the verdicts it attached; a version, or a
variant, that the repo does not have is `404`; without the policy key `501 UNAVAILABLE` naming
`keys.masterKeyEnv`. `DELETE …/deny` answers `204`, `404` when the version has no verdict of
source `lajstrom:manual`. `GET /api/v1/policy/denylist` answers `application/json`, `GET
/api/v1/policy/key` `application/x-pem-file`, `404` without the policy key.

## 5. System endpoints

`/-/healthz` (process up), `/-/readyz` (DB reachable, no other replica on it when it is SQLite
([index-schema.md](index-schema.md#coordination)), the replica's clock within 60 s of the DB's
([operations.md](../operations.md#clocks)), every configured registry answered `/v2/`
within the last minute), `/-/version` (JSON: version, commit, date; with the CORS headers of §4
for an origin of `server.corsOrigins`).

Prometheus metrics are not on the public host: `server.metricsListen` (default
`127.0.0.1:9464`, the OpenTelemetry Prometheus exporter convention) serves `GET /metrics`, and
`/debug/pprof/` when `server.pprof` is on. The edge never routes to it; Prometheus scrapes it
inside the network. Issue #23.

## 6. Proxy and group repos

A proxy or a group repo ([ADR 0013](../adr/0013-proxy-and-group-repos.md),
[proxy-and-group.md](proxy-and-group.md)) is `/<format>/<repo>/` like a hosted one, with the
routes of its format: a client sees no difference, and the npm routes (ADR 0012), the maven
and the pypi routes serve all three kinds. What changes is who answers and what a write does.

| Request | proxy | group |
| --- | --- | --- |
| a read route of the format (a document, a file) | the upstream's document, cached, and the files it holds or fetches on a miss (proxy-and-group.md §4 to §7) | the members, in order, with hosted names hiding the upstream (§8) |
| a write route of the format (publish, unpublish, a dist-tag) | `405` with `Allow: GET, HEAD` | the same request on the group's hosted member, authorized there (rbac.md §7); `405` without one, and with several (an apt group, proxy-and-group.md §8.4) |
| `GET`, `HEAD` on `/v2/<format>/<repo>/…` (manifests, blobs, tags, referrers) | as on a hosted repo, for what it holds; `/v2` never fetches, but on an `oci` proxy, whose read route it is: a manifest and a blob the backend does not hold are fetched on a miss, a tag follows the upstream, and `tags/list` is the backend's without the keep-tags (proxy-and-group.md §11) | on an `oci` group, the answer of the one member its routes choose for the path, as at the member's own URL, `name` and `Link` naming the group's path; a path no route selects `404 NAME_UNKNOWN` (proxy-and-group.md §12). On any other group `404 NAME_UNKNOWN`: it has no OCI repository |
| `PUT` of a manifest on `/v2` | a referrer (manifest-profile.md §6) as on a hosted repo, with `write` or `attach`; any other manifest `405 UNSUPPORTED` | on an `oci` group, `405 UNSUPPORTED` with `Allow: GET, HEAD`, a referrer included, once the principal may read the group, and the group's denial otherwise (rbac.md §3); on any other group `404 NAME_UNKNOWN` |
| blob uploads on `/v2` | as on a hosted repo, with `write` or `attach` (the blobs of referrers) | as a `PUT` of a manifest. A mount into another repo whose `from` is an `oci` group's path mounts from the routed member (proxy-and-group.md §12.3) |
| `DELETE` of a manifest on `/v2`, and of a version on `/api/v1` | as on a hosted repo, with `delete`: the next miss fetches it again | on `/v2`, as a `PUT` of a manifest; on `/api/v1`, `404`: a group has no versions |
| `GET /v2/_catalog` | its OCI repositories | an `oci` group, to a principal who may read it, each member's repositories that the routes send to that member, as `oci/<group>/<path>`; any other group nothing |

apt has groups and no proxies: an apt group's members are hosted apt repos, and it serves
documents it renders from their versions and keeps (§3, apt, **Group**; proxy-and-group.md §8.6).

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

## 7. OIDC login `/-/oidc` (M6, #69), token exchange for CI (#937)

The routes of rbac.md §8, served only while `auth.oidc.issuer` is set; without it every one is
`404`, but `POST /-/oidc/token` while `auth.ci.issuers` has an issuer (rbac.md §11), which then
answers the token exchange and a device grant `400 unsupported_grant_type`. They are not under `/api/` because a browser follows them: the login and its callback
answer redirects and HTML pages. The device routes answer JSON in the shape of RFC 8628 and of
OAuth 2.0 (RFC 6749 §5.2), not the envelope of §4: `lajstrom login` is an OAuth client. None
takes a credential; a request's `Authorization` header is ignored on them.

| Method and path | Behaviour |
| --- | --- |
| `GET /-/oidc/login[?user_code=]` | starts the authorization code flow: `302` to the IdP with `state`, `nonce` and the PKCE challenge, and the cookie `lajstrom_oidc` (rbac.md §8). With `user_code`, the login approves that device request: one that is unknown, expired or approved is `400` with the page of `/-/oidc/device`. `503` when the IdP's discovery does not answer, `Retry-After: 30` |
| `GET /-/oidc/login?return_to=&code_challenge=&code_challenge_method=S256&dpop_jkt=[&prompt=login]` | the same login for a web UI (rbac.md §10): `return_to`, an absolute URL of at most 2048 bytes on the origin of `server.externalURL` or of `server.corsOrigins`, without a fragment, `code_challenge`, the S256 challenge of the UI's PKCE verifier (43 characters of base64url), and `dpop_jkt`, the JWK SHA-256 thumbprint of the DPoP key the UI redeems the code with (RFC 9449 §10, 43 characters of base64url), are kept on the login request; `prompt=login` asks the IdP to authenticate the person again, `prompt=login` and `max_age=600` on the redirect (a step-up, rbac.md §10). A `return_to` on another origin, a challenge or a thumbprint that is not one, a method that is not `S256`, one of the three without the others, any with `user_code`, a `prompt` that is not `login`, and a `prompt` without them are `400` with a page |
| `GET /-/oidc/callback?code=&state=` | the IdP's redirect: one redemption of the state, the code exchange, the ID token checks, the user and its groups. `200` with the page that shows the new personal access token once (`Cache-Control: no-store`, `Referrer-Policy: no-referrer`), or, for a device request, the page that says to return to the terminal, or, for a login with `return_to`, `302` to `<return_to>#code=<code>`, the code that `POST /api/v1/login` redeems (§4). `400` with a page for every refusal of rbac.md §8, a login with `return_to` too; `503` when the IdP does not answer the exchange |
| `GET /-/oidc/device[?user_code=]` | the page where the person types or confirms the user code; its form is a `GET` of `/-/oidc/login?user_code=` |
| `POST /-/oidc/device/code` | starts a device request (RFC 8628 §3.1, `application/x-www-form-urlencoded`, its `client_id` and `scope` ignored): `200` `{"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval"}`; `429` with `Retry-After` beyond the throttle |
| `POST /-/oidc/token` | `grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=` (RFC 8628 §3.4): `200` `{"access_token", "token_type": "Bearer", "expires_in", "user"}` once the device request is approved, the personal access token minted then; else `400` `{"error", "error_description"}` with `authorization_pending`, `slow_down`, `expired_token`, `access_denied` or `invalid_grant`. `grant_type=urn:ietf:params:oauth:grant-type:token-exchange` (RFC 8693 §2.1) while `auth.ci.issuers` has an issuer: below. Another `grant_type`, or one whose configuration is off, is `400` `unsupported_grant_type` |

**The token exchange** (rbac.md §11) is an `application/x-www-form-urlencoded` body of at most
16 KiB (`413 invalid_request` beyond) with `grant_type`, `subject_token` (the CI platform's OIDC
token) and `subject_token_type`, `urn:ietf:params:oauth:token-type:jwt` or
`urn:ietf:params:oauth:token-type:id_token`, each once. It takes no client authentication.
`requested_token_type`, when sent, is `urn:ietf:params:oauth:token-type:access_token`;
`audience` and `resource` are not read. The answers:

- `200` `{"access_token": "ljs_ci_…", "issued_token_type":
  "urn:ietf:params:oauth:token-type:access_token", "token_type": "Bearer", "expires_in",
  "principal", "policies"}`, `principal` the CI token's `ci:` principal and `policies` the names
  of the policies that matched;
- `400 invalid_request`: another media type, a parameter missing or sent twice, another
  `subject_token_type` or `requested_token_type`, or an `actor_token`;
- `400 invalid_scope`: a `scope` that is not empty: a CI token is not narrowed;
- `400 invalid_grant`: a subject token that is not a JWT of a configured issuer, that does not
  verify, whose project breaks rbac.md §11, or that no policy matches. The description says which,
  and for the last the token's issuer, project, ref, environment and event (`pipeline_source` on
  GitLab), never a policy. Each is a `token.exchange` row of `outcome` `refused`, rate-limited
  (rbac.md §11);
- `503` `{"error": "temporarily_unavailable"}` with `Retry-After: 30` while the issuer's
  discovery document or keys never loaded;
- `429` with `Retry-After` beyond the rate limit of `/v2/token` (rbac.md §1), against which every
  `POST /-/oidc/token` counts.

Every answer of these routes has `Cache-Control: no-store`. The pages are HTML of Lajstrom's own,
with no script and `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'` (no `form-action`: the
device page's form ends in a redirect to the IdP),
and say nothing the IdP said beyond the reason in Lajstrom's words.
