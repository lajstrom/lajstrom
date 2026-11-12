# GHCR (ghcr.io)

Measured on 2026-11-07 against `ghcr.io`, GitHub's hosted service, which publishes no version
(every answer says `docker-distribution-api-version: registry/2.0`; the edge was `fra`). From
macOS arm64 with curl 8.7.1 and oras 1.3.6 (`ghcr.io/oras-project/oras:v1.3.6` in a container),
and Lajstrom (oras-go 2.7.0) at c8384ad, then again at 2b1ecd0, the main of #27 (`OpenBlob`,
`StatBlob`), which merged during the run. Everything was pushed to the test
organisation, `ghcr.io/lajstrom-extras/…`; nothing to `ghcr.io/lajstrom/`. The credentials were
the `gh` CLI's token of `gecube` (a classic OAuth token: `write:packages`, and `delete:packages`
from the middle of the run, see "Delete"), used as `$(gh auth token)` and never printed.

## The row

`lajstrom registry probe` measures every row of this table but the listing API, auth, rate
limits and the garbage collector, which still need a person;
[README.md](README.md#measuring-a-registry-lajstrom-registry-probe) says how.

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| GHCR | ghcr.io on 2026-11-07, no version published (`registry/2.0`); prefix `lajstrom-extras/` | bearer from `/token`, which is the PAT in base64 | yes, on the first manifest: a repository with blobs only is no package, and its blobs answer `GET` with 404 | **no**: `/referrers/<digest>` is 404 `MANIFEST_UNKNOWN`, no `OCI-Subject`; the tag schema works, and oras 1.3.6 falls back to it | **wrong**: `_catalog` answers 200 with all of GHCR, so the probe says yes; set `catalog: false`; the packages REST API lists an organisation | **no** through `/v2` (405 `UNSUPPORTED`, DeleteManifest turned false); yes through the REST API, by version, with `delete:packages` | no: a tag re-points with 201 | **yes**: `GET` is 307 to `pkg-containers.githubusercontent.com` (an Azure SAS behind Fastly, good for 5 to 10 min), `HEAD` 200 without one; `OpenBlob` returns it, and with redirect off follows it, a range 206 from the CDN | **fail**: list repositories, upload sessions, a blob read before the first manifest |

## The probe

The provider test logged, for `https://ghcr.io` with the prefix `lajstrom-extras/`, at both
commits:

```
level=INFO msg="registry capabilities" registry=integration createOnPush=true referrers=false catalog=true deleteManifest=true immutableTags=false blobRedirect=true maxBlobSize=0
```

`referrers=false` is right: the probe's 404 carries `MANIFEST_UNKNOWN`, not `NAME_UNKNOWN`.
`catalog=true` is wrong for Lajstrom (see "Catalog"), and `deleteManifest=true` turns false at
the first delete (405). The configuration GHCR needs:

```yaml
registries:
  - name: ghcr
    type: generic
    url: https://ghcr.io          # https only: port 80 answers 301
    prefix: <org>/
    auth:
      username: <user>
      passwordEnv: LAJSTROM_GHCR_TOKEN  # a classic PAT with write:packages
    capabilities:
      catalog: false             # the catalog is all of GHCR
      referrers: false           # what the probe finds; saves the request
      deleteManifest: false      # /v2 deletes are 405; deletes go through the REST API (#74)
      maxBlobSize: 10000000000   # documented: 10 GB per layer; measured up to 100 MiB
```

Since #35 the provider's vendor table sets `catalog: false` and `deleteManifest: false` for the
host `ghcr.io` without asking (the catalog is not probed), and the line says `vendor=ghcr`; the
two keys may stay in the configuration. Since #246 it sets `maxBlobSize: 10000000000` too, which
bounds a `/v2` blob upload before it streams.

## Measurements

The commands use two shell helpers. The token request is the one place the PAT travels, and
`printf` is a shell builtin, so the PAT is in no process list; the bearer reaches curl the same
way:

```sh
REPO=lajstrom-extras/it-b0ef9443
T=$(printf 'user = "gecube:%s"\n' "$(gh auth token)" |
  curl -sS -K - "https://ghcr.io/token?service=ghcr.io&scope=repository:$REPO:pull,push" | jq -r .token)
c() { printf 'header = "Authorization: Bearer %s"\n' "$T" | curl -sS -K - "$@"; }
```

Responses are trimmed: `date`, `x-github-request-id`, `strict-transport-security` and the
other security headers are left out.

### Token

```
GET /token?service=ghcr.io&scope=repository:lajstrom-extras/it-b0ef9443:pull,push  (Basic gecube:$(gh auth token))
HTTP/2 200
{"token": <56 characters>}        no expires_in, no issued_at
```

The token is not a JWT: it is the PAT in base64, byte for byte (compared in the shell, nothing
printed), the same for `pull`, for `pull,push`, for another repository and for a scope in
another organisation. GHCR takes the base64 PAT as a bearer without the token endpoint too
(`GET /v2/` 200). So the bearer does not expire, is not narrowed by the scope, and is as secret
as the PAT. Without credentials, the token request for a private repository is 403 `DENIED`;
for a public one (`oras-project/oras`) it gives a 56-character opaque token.

`http://ghcr.io` answers every request with `301 Moved Permanently` to `https://`.

### Create on push

```
HEAD /v2/lajstrom-extras/it-b0ef9443/blobs/sha256:0e830b07…  (before anything was pushed)
HTTP/2 404

POST /v2/lajstrom-extras/it-b0ef9443/blobs/uploads/
HTTP/2 202
docker-upload-uuid: 8.7b0fe7a6-5e20-4781-9f82-630ae4efc464
location: /v2/lajstrom-extras/it-b0ef9443/blobs/upload/8.7b0fe7a6-5e20-4781-9f82-630ae4efc464
range: 0-0

PUT <location>?digest=sha256:0e830b07…  (43 bytes)
HTTP/2 201
docker-content-digest: sha256:0e830b07…
location: /v2/lajstrom-extras/it-b0ef9443/blobs/sha256:0e830b07…

POST /v2/lajstrom-extras/it-b0ef9443/blobs/uploads/?digest=sha256:44136fa3…  (the empty config, {})
HTTP/2 201

PUT /v2/lajstrom-extras/it-b0ef9443/manifests/v1  (Content-Type: application/vnd.oci.image.manifest.v1+json)
  artifactType application/vnd.lajstrom.raw.v1, config application/vnd.oci.empty.v1+json,
  one layer application/vnd.lajstrom.raw.file.v1 with org.opencontainers.image.title,
  annotations lajstrom.schema, .format, .repo, .name, .version, org.opencontainers.image.created
HTTP/2 201
docker-content-digest: sha256:698904a6…
location: /v2/lajstrom-extras/it-b0ef9443/manifests/sha256:698904a6…
```

Nothing in our profile is refused. `GET` of the manifest by tag gives the pushed bytes, with
`content-type: application/vnd.oci.image.manifest.v1+json`, `docker-content-digest` and an
`etag` of the digest; `HEAD` of a blob answers with the layer's media type as its
`content-type` (`application/vnd.lajstrom.raw.file.v1`). The package `it-b0ef9443` appeared,
private, with the manifest; a repository that only received blobs does not become a package
(the upload tests and the mount target below left none).

A blob uploaded to a repository that has no manifest yet answers `HEAD` 200 and `GET` 404
`BLOB_UNKNOWN`, at once and 5 and 20 seconds later; in a repository that has a manifest, the
same upload is readable at once (`GET` 307, then the bytes):

```
POST /v2/lajstrom-extras/it-b0ef9443-unref/blobs/uploads/?digest=sha256:…  → 201
HEAD /v2/lajstrom-extras/it-b0ef9443-unref/blobs/sha256:…                  → 200
GET  /v2/lajstrom-extras/it-b0ef9443-unref/blobs/sha256:…                  → 404 {"errors":[{"code":"BLOB_UNKNOWN","message":"blob unknown to registry"}]}
```

### Referrers

```
PUT /v2/lajstrom-extras/it-b0ef9443/manifests/sha256:e129c938…  (artifactType …test.signature.v1, subject sha256:698904a6…)
HTTP/2 201
docker-content-digest: sha256:e129c938…                no OCI-Subject header

GET /v2/lajstrom-extras/it-b0ef9443/referrers/sha256:698904a6…
HTTP/2 404
{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}

GET /v2/lajstrom-extras/lajstrom-probe/referrers/sha256:0000…0000  (the probe; no such repository)
HTTP/2 404
{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}
```

The same with `?artifactType=`. GHCR adds no `sha256-<digest>` tag of its own. oras 1.3.6 falls
back to the tag schema:

```sh
gh auth token | docker run -i --rm -v "$PWD:/work" -w /work ghcr.io/oras-project/oras:v1.3.6 \
  push ghcr.io/lajstrom-extras/it-b0ef9443:oras-v1 --artifact-type application/vnd.lajstrom.raw.v1 \
  hello.txt:application/vnd.lajstrom.raw.file.v1 -u gecube --password-stdin
# ArtifactType: application/vnd.lajstrom.raw.v1, Digest: sha256:672d5afc…
… attach --artifact-type application/vnd.lajstrom.test.signature.v1 ghcr.io/lajstrom-extras/it-b0ef9443:oras-v1 sig.txt …
# Attached to … @sha256:672d5afc…, Digest: sha256:1e2b2c44…
… discover --format tree ghcr.io/lajstrom-extras/it-b0ef9443:oras-v1 …
# └── application/vnd.lajstrom.test.signature.v1
#     └── sha256:1e2b2c44…
```

and `tags/list` then holds `sha256-672d5afc…`.

### Catalog

```
GET /v2/_catalog?n=1  (a bearer for registry:catalog:*)
HTTP/2 200
link: </v2/_catalog?last=0-0eth0%2Fdevsecops-lab%2Fmyapp&n=1>; rel="next"
{"repositories":["0-0eth0/devsecops-lab/myapp"]}

GET /v2/_catalog?n=5&last=lajstrom-extras%2F
{"repositories":["lajstrom-extras/it-b0ef9443","lak65746-source/dark-smp","lak882/seti",…]}
```

The catalog is all of GHCR as the token sees it, other users' repositories and our private one
alike, sorted, with `n`/`last` pagination and at most 1000 names a page (`n=10000` gave 1000).
Without a token it is 401 with a challenge; an anonymous token for `registry:catalog:*` is
403 `DENIED`. The probe asks for `n=1`, gets 200 and says `catalog=true`, and
`ListRepositories` walks from the first name of GHCR (see "Our tests").

The packages REST API lists one organisation:

```sh
gh api '/orgs/lajstrom-extras/packages?package_type=container' --jq '.[] | {name, visibility}'
# {"name":"it-b0ef9443","visibility":"private"}
gh api -i '/orgs/lajstrom-extras/packages?package_type=container&per_page=2&page=2'
# Link: <https://api.github.com/organizations/353821447/packages?package_type=container&per_page=2&page=3>; rel="next", … rel="prev", … rel="first", … rel="last"
gh api '/orgs/lajstrom-extras/packages/container/raw%2Fit-bc2fa015%2Fpkg/versions' --jq '.[] | {id, name, tags: .metadata.container.tags}'
# {"id":1521017556,"name":"sha256:bf9676b3…","tags":["sha256-f8d4d0f4…"]}   the referrers index of the tag schema
# {"id":1521017524,"name":"sha256:93f0eab2…","tags":[]}                      the referrer
# {"id":1521017406,"name":"sha256:f8d4d0f4…","tags":["v1"]}
```

Pagination is by `page` and `per_page` with a `Link` header (`per_page` × `page` at most 10000,
documented). A package name is the repository below the organisation, slashes and all, and it
must be escaped in the path: `raw/it-bc2fa015/pkg` unescaped is 404. Every manifest is a
version whose `name` is its digest, with its tags in `metadata.container.tags`.

### Tags

```
GET /v2/lajstrom-extras/it-b0ef9443/tags/list?n=2
HTTP/2 200
link: </v2/lajstrom-extras/it-b0ef9443/tags/list?last=v2&n=2>; rel="next"
{"name":"lajstrom-extras/it-b0ef9443","tags":["v1","v2"]}
```

The order is the order of creation, not lexical (`v1, v2, v3, v4, empty-layer, oras-v1,
sha256-…`), and `last` is a position: `last=v4` gives `empty-layer, oras-v1, sha256-…`, and a
`last` that is not a tag (`a`, `zzz`) gives `"tags":null`, as does a page past the end. A large
`n` (1000000) is 200.

### Immutable tags

None. `PUT …/manifests/v1` with another manifest answers 201 and `v1` then resolves to the new
digest; GitHub documents no immutability option for containers.

### Delete

Through `/v2`, everything is 405:

```
DELETE /v2/lajstrom-extras/it-b0ef9443/manifests/sha256:e129c938…   (also by tag v4, also a blob, also an upload session)
HTTP/2 405
{"errors":[{"code":"UNSUPPORTED","message":"The operation is unsupported."}]}
```

Through the REST API, by version id, which needs `delete:packages` and, documented, admin rights:

```sh
gh api -X DELETE /orgs/lajstrom-extras/packages/container/it-b0ef9443/versions/1520997114
# HTTP/2.0 204 No Content
```

A 403 was expected here, since the CLI token had no `delete:packages` when the run started;
by the time of the call it had (`X-Oauth-Scopes: …, delete:packages, …`, and
`gh auth status` agreed), so the 403 was not seen. The deleted version (the untagged referrer)
answered `HEAD` 404 through `/v2` at once, was listed under `versions?state=deleted`, and came
back with `POST …/versions/1520997114/restore` (204; `HEAD` 200 again). There is no endpoint
to delete a tag alone, nor one to change a package's visibility (`PATCH` of the package is
404); visibility is set in the web UI.

### Blob redirect

```
GET /v2/lajstrom-extras/it-b0ef9443/blobs/sha256:0e830b07…   (redirects not followed)
HTTP/2 307
content-type: application/vnd.lajstrom.raw.file.v1
location: https://pkg-containers.githubusercontent.com/ghcrblobs08/blobs/sha256:0e830b07…?<query stripped>
```

The query is an Azure user-delegation SAS (`sv, se, sig, sp, spr, sr, skoid, sktid, skt, ske,
sks, skv`) plus `hmac`; the storage account in the path varies (`ghcrblobs08`, `ghcrblobs19`).
The URL needs no `Authorization` (`GET` 200 with the bytes, served by Fastly as
`application/octet-stream`, `accept-ranges: bytes`; a `Range` request through the redirect is
206). The expiry `se`, sampled every 20 s for 4 minutes, stays fixed and jumps 10 minutes
ahead once about 5 minutes are left: a URL is good for 305 to 598 s after the 307. `HEAD` of a
blob is 200 with no redirect. At 2b1ecd0, `OpenBlob` with redirect on returns the 307's URL,
which a plain `GET` downloads; with redirect off it follows to the CDN (without Lajstrom's
credentials) and streams, a range is 206 with `Content-Range` and a range past the end 416;
`StatBlob` is the `HEAD`'s 200 with the size and the digest.

### Upload sessions

- The session `Location` is a path on `ghcr.io`, `/v2/<name>/blobs/upload/<n>.<uuid>` (singular
  `upload`), the same after every `PATCH`; `docker-upload-uuid` repeats the id. Not another
  host.
- `PATCH` with `Content-Range` appends (`range: 0-9`, then `0-19`); out of order is 416
  `REQUESTED_RANGE_NOT_SATISFIABLE` "out of order chunk".
- A `PATCH` larger than 4 MiB is refused: 4194304 bytes is 202, 4194305 is 416 "the request
  body is too large and exceeds the maximum permissible limit of 4.00MiB". The limit is per
  request (two chunks of 4 MiB make an 8 MiB blob).
- A `PATCH` without `Content-Range` starts the session over: after `PATCH 0-9` a second
  `PATCH` of 10 bytes without `Content-Range` (streamed without `Content-Length`, or with it)
  answers `range: 0-9`, and the closing `PUT` takes the digest of the second chunk alone. As
  the only chunk it works, and is not held to 4 MiB (8 MiB streamed: 202, then `PUT` 201).
- The closing `PUT ?digest=` takes the last chunk as its body with `Content-Range`; without
  `Content-Range` it is 404 `BLOB_UPLOAD_INVALID`.
- A wrong digest is 404 `BLOB_UPLOAD_INVALID`, not 400 `DIGEST_INVALID`.
- `GET <location>` (status) is `303 See Other` to
  `https://github.com/-/v2/packages/container/package/<name>%2Fblobs%2Fupload%2F<id>`, a web
  page that answers 404, with or without credentials, and with no challenge; `HEAD` is 405.
  GHCR has no upload status.
- `DELETE <location>` (cancel) is 405 `UNSUPPORTED`.
- The 0-byte blob (`sha256:e3b0c442…`) cannot be uploaded (`PUT` or `POST ?digest=`: 404
  `BLOB_UNKNOWN`), but `HEAD` and `GET` answer 200 with 0 bytes and its digest (`HEAD` even in
  a repository that never existed), and a manifest with it as a layer is 201.
- Mount: `POST …/blobs/uploads/?mount=<digest>&from=lajstrom-extras/it-b0ef9443` into a new
  repository is 201; without `from`, and for an absent blob, 202 with a session.

### Size

Documented: 10 GB per layer and a 10-minute timeout per upload. Measured: a 100 MiB blob in
one `POST ?digest=` took 4.0 s (26 MB/s), 201. Nothing larger was pushed.

### Rate limits

No `ratelimit` or `Retry-After` header on any `/v2` answer, anonymous or not, and no 429 in
the curl runs; the tests saw no error a 429 would give, though the provider retries a 429
without a word (the catalog walk paged for 111 s without an error before the deadline). The
REST API answers with `X-Ratelimit-Limit: 5000` (`X-Ratelimit-Resource: core`).

### Anonymous pull

The packages GHCR creates in the organisation are private, and our private package without
credentials is 401 with a challenge, the token request 403. No package of ours could be made
public: the REST API has no visibility endpoint, and the UI was not used. A public package of
someone else (`oras-project/oras:v1.3.6`) pulls anonymously: the token request without
credentials gives a token, and `HEAD` of the index is 200.

## Our tests

```sh
LAJSTROM_TEST_REGISTRY=ghcr.io LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-extras/ LAJSTROM_TEST_REGISTRY_TLS=1 \
  LAJSTROM_TEST_REGISTRY_USERNAME=gecube LAJSTROM_TEST_REGISTRY_PASSWORD="$(gh auth token)" \
  go test -race -count=1 -tags integration -run TestIntegration -v ./internal/provider/generic/
```

and the same for `./internal/ingest/`, `./internal/format/raw/` and `./internal/facade/`. The
integration tests of those three build the registry URL as `"http://" + addr` and ignore
`LAJSTROM_TEST_REGISTRY_TLS`. Against GHCR that is port 80, which answers 301 to https. By the
code of oras-go 2.7.0, its auth client sends the authenticated retry only to the https URL of
a same-host upgrade and caches no token for it, so the PAT (the bearer is the PAT) should not
cross port 80; but net/http turns every `POST`, `PUT` and `PATCH` into a `GET` on a 301, so no
push can work. With the PAT at stake on a code reading, they were not run that way with
credentials; without credentials, `./internal/ingest/` fails at once with 403 `DENIED` behind
the 301. They were run with `go test -overlay` over copies whose only change is the scheme
(`https://` when `LAJSTROM_TEST_REGISTRY_TLS=1`), plus, once, a `t.Log` of the facade's log in
a failure message.

Everything ran at c8384ad first and again at 2b1ecd0, with the same results where the tests
are the same; #27 did not change the probe, the listing, the upload sessions or ingest. At
2b1ecd0 the provider test ran without its list step
(`-run 'TestIntegration$/^(push_blob|…|delete)$'`), which walks GHCR's catalog for two minutes
and had failed at c8384ad.

| Test | Result | Whose |
| --- | --- | --- |
| provider `TestIntegration` | push blob, push manifest and tag, resolve, fetch, tags, referrers (through the tag schema), open blob (a redirect to `https://pkg-containers.githubusercontent.com/ghcrblobs18/…`; followed with redirect off), open blob with a range (206, and 416 past the end), stat blob: pass. delete: skip, the first delete answered 405 and DeleteManifest turned false. **list repositories** (c8384ad): **fail**, `context deadline exceeded` after 111 s, the walk at `_catalog?last=77997%2Fkolla%2Ftrove-conductor&n=100`, still in the names that begin with a digit | GHCR: a catalog of all of GHCR behind a 200. Ours: the probe takes the 200 for a usable catalog, and the walk starts at the first name instead of at the prefix |
| provider `TestIntegrationUploads` | **session across providers: fail**, the first `PATCH` from the second provider: "request body is not rewindable" | ours, see below |
| the same, one step a run (`-run 'TestIntegrationUploads/^<step>$'`, c8384ad) | monolithic and last chunk of unknown size: **fail**, not rewindable (ours). empty blob: **fail**, 404 `BLOB_UNKNOWN` (GHCR). wrong digest: **fail**, 404 `BLOB_UPLOAD_INVALID` comes back as `ErrNotFound`, not `content.ErrMismatchedDigest` (GHCR answers off the spec; our mapping). chunk out of order: pass (416). cancel: **fail**, 405 `UNSUPPORTED` (GHCR). The download by digest alone and the mount need the first step's blob and say nothing alone; the mount works with curl | as given |
| ingest `TestIntegration` | **fail**: "ingest: fetch the metadata layer sha256:45d54887… from raw/it-844f32cb/tool: … provider: not found" | GHCR: a blob of a repository without a manifest answers `GET` 404. Ours to adapt: the pipeline reads the metadata layer back from the registry before it pushes the manifest, and every package's first version is in a new repository |
| raw `TestIntegration` | **fail**: `PUT it-375746e2/Docs/File Name.txt` = 400 "format: invalid: … fetch the metadata layer … provider: not found" | GHCR, as for ingest. Ours: a registry's 404 for a blob that was just pushed reaches the client as a 400 |
| facade `TestIntegrationUploadAcrossReplicas` | **fail**: `PATCH` on B 500, logged as "append to upload …: request body is not rewindable" | ours, see below |
| facade `TestIntegrationMonolithicAbandoned` | **fail**: the retry on B is 201, then "the backend does not have sha256:97a20d27…: provider: not found" | GHCR: the blob of a repository without a manifest |
| facade `TestIntegrationPassThrough` | pass (blobs, manifest by tag and digest, `HEAD` and `GET` of a blob, `tags/list`, `_catalog` of the facade) | |
| facade `TestIntegrationRawPush` | pass: the raw route answered 307 to `pkg-containers.githubusercontent.com`; another manifest under the tag is 409 (the index's claim; GHCR would have re-pointed the tag); a missing annotation 400 | |
| facade `TestIntegrationBlobReads` (2b1ecd0) | **fail**: `GET`, a range and a range past the end of a blob pushed without a manifest are 404; `HEAD` and `If-None-Match` pass | GHCR: the blob of a repository without a manifest |
| facade `TestIntegrationS3Redirect` (2b1ecd0) | skip (`LAJSTROM_TEST_REGISTRY_S3` not set) | |

"Not rewindable": a provider whose token cache is empty (the second provider of the test, a
second replica of the facade) primes before a request with a streamed body by asking for the
session's status, so that the 401 and the token come before the body. GHCR answers that `GET`
with a 303 to github.com and no challenge (the client follows it; `Authorization` is dropped on
the cross-host hop, as Go does), the cache stays empty, the `PATCH` or `PUT` with the body gets
the 401, and oras-go cannot send the body again. `HEAD` of a blob, `GET …/tags/list`, `HEAD`
of a manifest, a bodyless `POST …/blobs/uploads/` and a bodyless `PATCH` of the session all
answer 401 with the Bearer challenge.

## What the provider must do differently

- **No catalog**: `catalog: false`. The probe cannot tell GHCR's global catalog from a useful
  one; a catalog walk would at least have to start at the prefix (`last=`) and stop past it,
  which GHCR honours. The repository list per organisation (or user) is
  `GET /orgs/{org}/packages?package_type=container` with `page`/`per_page` and `Link`, package
  names escaped: the GHCR adapter of #74.
- **Delete through the REST API** (#74): digest to version id through `GET …/versions` (the
  version's `name` is the digest), then `DELETE …/versions/{id}`, with `delete:packages` and
  admin rights; a tag cannot be deleted alone; a deleted version can be restored for 30
  days. `/v2` deletes are 405: `deleteManifest: false` saves the first refusal.
- **Credentials**: the bearer is the PAT in base64, for every scope and without expiry, so the
  token cache holds the PAT, and a log line with the bearer would carry the PAT. https only;
  the integration tests that build `http://` must honour `LAJSTROM_TEST_REGISTRY_TLS` to run
  against GHCR at all.
- **Upload sessions** (the facade's path):
  - prime with a request GHCR challenges (`HEAD` of a blob, for one), not the session's `GET`,
    or send bodies that can be replayed;
  - there is no status: the offset comes from the session Lajstrom keeps, not from GHCR;
  - cancel is 405: unsupported, the session is left to expire;
  - chunks of at most 4 MiB, each with `Content-Range`, because a `PATCH` without
    `Content-Range` throws away what the session held: done. The uploads that Lajstrom streams
    itself, a raw `PUT`, an apt upload, a Maven file (ADR 0014), go in chunks of 4 MiB on GHCR,
    by the vendor table, or of `registries[].uploads.chunkSize` (#558). A client's `/v2` chunk
    above that is split into `PATCH`es of that size, sent one after the other, and the client
    sees one chunk (#697). A chunk of unknown size after the first write is spooled first with
    `registries[].uploads.spoolUnknownLength`, and refused without it before anything is sent:
    `400 BLOB_UPLOAD_INVALID` with a hint that names the key, as Gitea answers. Docker's one
    streamed `PATCH` into an empty session goes as it came, which GHCR takes whole (8 MiB,
    above), and a monolithic `POST ?digest=` is not held to 4 MiB (100 MiB, above). Checked on
    a fake with GHCR's limit, not against GHCR;
  - 404 `BLOB_UPLOAD_INVALID` on the closing `PUT` is a digest mismatch here;
  - the 0-byte blob is never uploaded: GHCR already "has" it everywhere.
- **No blob read before the first manifest**: in a repository without a manifest, `GET` of a
  blob is 404 although `HEAD` is 200. Ingest checks the metadata layer as it streams it to the
  registry, instead of reading it back.
- **Blob redirect**: nothing to change: `OpenBlob` hands the 307 to clients as it is (no
  credentials needed, `Range` works, the URL is good for at least 5 minutes) and follows it
  without credentials when redirects are off.
- **Tags**: no immutability: the claim and the 409 of ingest do the work, as the facade test
  showed. `tags/list` is in creation order and `last` is a position: follow the `Link`, never
  make up a `last`.
- **Size**: 10 GB per layer and 10 minutes per upload, documented; not measured above 100 MiB.

## Packages left in lajstrom-extras

```sh
gh api --paginate '/orgs/lajstrom-extras/packages?package_type=container&per_page=100' --jq '.[].name'
```

```
it-b0ef9443
raw/it-bc2fa015/pkg
raw/it-bc2fa015/other
oci/images/it-ae229be8
raw/assets/it-e3f276ed/hello.txt
raw/it-898d8ac1/pkg
oci/images/it-fc556235
raw/assets/it-3a5240c1/hello.txt
```

All private. The repositories that only received blobs (the upload tests, the failed ingest
and raw runs, `it-b0ef9443-other`, `it-b0ef9443-unref`) are no packages and are not listed;
the REST API cannot reach their blobs. The deletion, with a token that has `delete:packages`
and admin rights on the organisation (the CLI token has the scope since this run):

```sh
for p in it-b0ef9443 raw/it-bc2fa015/pkg raw/it-bc2fa015/other oci/images/it-ae229be8 \
  raw/assets/it-e3f276ed/hello.txt raw/it-898d8ac1/pkg oci/images/it-fc556235 raw/assets/it-3a5240c1/hello.txt; do
  gh api -X DELETE "/orgs/lajstrom-extras/packages/container/$(jq -rn --arg p "$p" '$p | @uri')"
done
```

The loop ran on 2026-11-07 after the run, with that token: the eight packages are gone and the
organisation lists none. The blob-only repositories expire on their own.
