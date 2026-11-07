# Registries tested with Lajstrom

What each backend registry was tested with, and what it answered. The capability columns are
`provider.Capabilities` as the provider reported them after its probe (`docs/configuration.md`,
"Registry capabilities"), and what the integration test then saw. Re-run the test and update
this page when a registry version or the provider changes; a registry that is not here is not
tested.

## `generic` (internal/provider/generic)

Tested on 2026-11-07 with `TestIntegration` (`internal/provider/generic/integration_test.go`):
push a blob, push a manifest with a tag in one PUT, resolve, fetch, exists, list tags, attach a
referrer (a manifest with `subject`) and list it, `OpenBlob` (the first answer of one `GET`: a
redirect to follow, or the bytes; with redirect off, the bytes; a range, 206; a range past the
end, 416), `StatBlob`, list repositories under a random prefix, delete the referrer and the
manifest. Each run works in `raw/it-<random>/…`, once without and once with the instance
prefix `lajstrom-it/`. The blob column says what a blob `GET` of the facade or a frontend
becomes (`format.ServeBlob`): one request to the registry either way.

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| distribution | 3.1.2 (`registry:3`, `sha256:38907510…ff768c`), filesystem storage, delete enabled | none | yes | **no**: `/referrers/` is a plain 404; referrers listed through the `sha256-<digest>` tag schema | yes, `n`/`last` pagination | yes | no (not probed) | no redirect: streamed, one request; `Accept-Ranges: bytes`, a range 206 | pass, with and without prefix |
| distribution | 3.1.2, filesystem storage, `REGISTRY_STORAGE_DELETE_ENABLED=false`, htpasswd | Basic | yes | no (tag schema) | yes | **no**: the first manifest delete answered 405 `UNSUPPORTED`, DeleteManifest turned false; the delete step skipped | no | no redirect: streamed, one request | pass (delete skipped) |
| distribution | 3.1.2, `s3` storage driver on MinIO 2025-07-23 (`bitnamilegacy/minio:2025.7.23-debian-12-r5`), delete enabled: the s3-minio profile of the dev stack, the fallback | none | yes | no (tag schema) | yes | yes | no (not probed) | **yes**: `GET` and `HEAD` answer 307 to a URL MinIO signs for 20 minutes (`X-Amz-Expires=1200`); `OpenBlob` returns it, and with redirect off follows it and streams from MinIO, a range 206 from MinIO; `StatBlob` follows the `HEAD`'s | pass, with and without prefix |
| distribution | 3.1.2, `s3` storage driver on SeaweedFS 4.52 (`chrislusf/seaweedfs:4.52`, `sha256:57877e32…43ce59`, `weed server -s3`), delete enabled: the s3 profile of the dev stack | none | yes | no (tag schema) | yes | yes | no (not probed) | **yes**: `GET` and `HEAD` answer 307 to a URL signed for 20 minutes (`X-Amz-Expires=1200`), which SeaweedFS enforces; `OpenBlob` returns it, and with redirect off follows it and streams from SeaweedFS, a range 206 from SeaweedFS; `StatBlob` follows the `HEAD`'s | pass, with and without prefix |
| Zot | v2.1.23 (`zot-minimal-linux-arm64`, `sha256:d8ec72e9…0571f3`), default configuration | none | yes | **yes**: the probe got an empty OCI image index for a repository that does not exist | yes | yes | no (not probed) | no redirect: streamed, one request | pass, with and without prefix |
| Zot | v2.1.23, htpasswd | Basic | yes | yes | yes | yes | no | no redirect: streamed, one request | pass (with prefix) |
| Zot | v2.1.23 (`zot-minimal:v2.1.23`, the multi-arch index `sha256:59cb5740…cd3458`, arm64 as above), the dev stack: `dedupe`, `gc` every 10 min (`deploy/compose/zot/config.json`) | none | yes | yes | yes | yes | no (not probed) | no redirect: streamed, one request; a range 206, but no `Accept-Ranges` on a `GET` (its `HEAD` has one) | pass, with and without prefix |
| GitLab container registry | v4.43.0 (`cng/gitlab-container-registry:v4.43.0-gitlab`, `sha256:fe9c6741…f8222e`), filesystem storage, delete enabled; metadata on the filesystem (A) or in its database (B, as GitLab.com): the gitlab profile of the dev stack (`make dev-up PROFILES=gitlab`) | none | yes | **no**: `/referrers/` is a plain 404 in both; tag schema | yes, `n`/`last` pagination, `n` at most 1000 | yes | no option in the registry: GitLab's rules come in GitLab's tokens, and with one every digest delete is 401 `PROTECTED_MANIFEST` | no redirect: streamed, one request; `Accept-Ranges: bytes`, a range 206 | A: pass, with and without prefix. B: **every Lajstrom manifest refused**, 400 `unknown media type` for the `artifactType`, unless `REGISTRY_FF_DYNAMIC_MEDIA_TYPES=true`; then pass |
| GHCR ([ghcr.md](ghcr.md)) | ghcr.io on 2026-11-07, no version published (`registry/2.0`) | bearer from `/token`, which is the PAT in base64 | yes, on the first manifest; until then a blob answers `HEAD` 200 and `GET` 404 | **no**: `/referrers/` is 404 `MANIFEST_UNKNOWN`; the tag schema | **wrong**: `_catalog` is 200 with all of GHCR, so the probe says yes; `catalog: false`, and the packages REST API lists an organisation (#74) | **no**: 405 `UNSUPPORTED`, DeleteManifest turned false; the REST API deletes versions (#74) | no | **yes**: `GET` is 307 to `pkg-containers.githubusercontent.com` (an Azure SAS, 5 to 10 min), `HEAD` 200; `OpenBlob` returns it, and with redirect off follows it, a range 206 | **fail**, with the prefix `lajstrom-extras/`: list repositories (the catalog walk), upload sessions, a blob read before the first manifest |

Notes:

- The [dev stack](../dev-stack.md) runs distribution as in the first row (plus
  `REGISTRY_LOG_LEVEL=info`) on `localhost:5001` and Zot as in the last row on `localhost:5000`:
  `LAJSTROM_TEST_REGISTRY=localhost:5001` and `=localhost:5000` after `make dev-up`. Both passed
  the test there on 2026-11-07, with and without the prefix. `make dev-up PROFILES=s3` adds the
  distribution on SeaweedFS of the fourth row on `localhost:5002`, `PROFILES=s3-minio` the
  fallback on MinIO of the third row on `localhost:5006`; both passed, side by side.
- The `registry:3` image enables deletes in its default configuration
  (`/etc/distribution/config.yml`), unlike distribution's own default.
- distribution 3.1.2 has no referrers API: the routes do not exist, so the probe and oras-go
  get a plain-text 404 and use the tag schema. Lajstrom lists referrers either way.
- On filesystem storage neither registry redirects a blob download: the first answer is the
  bytes, streamed as they come, so a download is one request to the registry. distribution on S3
  redirects `GET` and `HEAD` to the object store; `TestIntegrationS3Redirect`
  (`internal/facade`, `LAJSTROM_TEST_REGISTRY_S3=localhost:5002`, or `=localhost:5006` for the
  fallback on MinIO) checks the 307 of the facade and of `/raw/`, that the bytes behind the
  `Location` have the blob's digest, a range after the redirect, `HEAD` (200: the provider
  follows), and that `policy.blobRedirect: never` streams. Its presigned URLs name the endpoint
  distribution talks to; the dev stack makes that `localhost:9000` (SeaweedFS) or
  `localhost:9001` (MinIO) on both sides ([dev-stack.md](../dev-stack.md#the-s3-profile)).
- Ranges, both registries on the filesystem: one range is 206 with `Content-Range`; several
  are 206 `multipart/byteranges`, which `format.ServeBlob` does not ask for (it sends one
  range, or none); a range past the end is 416 `bytes */<size>`; a malformed one is 416 too.
- `TestIntegrationUploads` (#24) on distribution on S3 passes but for one step: the cancel of a
  session after a refused digest answers 500 (`s3aws: append to zero-size path … unsupported`),
  so such a session stays in the bucket until distribution purges old uploads. SeaweedFS and the
  fallback on MinIO, side by side (#118), give the same answer and message: the cause is the s3
  driver of distribution, not the store. On both a status `GET` of that session is 500 too, and
  the steps after it pass. Through Lajstrom the facade cancels the backend session itself after
  the refused digest, logs the 500 (`facade: cancel backend upload session`), and the client's
  `DELETE` is 404 `BLOB_UPLOAD_UNKNOWN`.
- Bearer token authentication is covered by unit tests against a fake token endpoint, not yet
  against a real registry with a token server.
- GHCR: the commands and answers, the 4 MiB chunk limit and the other upload deviations, the
  overrides it needs, what the provider must do differently, and the packages the tests left
  in `lajstrom-extras` are in [ghcr.md](ghcr.md).
- Without credentials, both registries with htpasswd answer the probe with 401 and a Basic
  challenge; `Capabilities` then fails with `provider.ErrUnauthorized` (no credentials
  configured), which `serve` logs at startup.
- The GitLab container registry, both variants, the online garbage collector (it deletes an
  untagged referrer 24 h after its push), a test of GitLab's immutable tags through its tokens,
  and the commands: [gitlab.md](gitlab.md).

## Commands

The registries ran in containers (OrbStack on macOS, arm64):

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
`LAJSTROM_TEST_REGISTRY_S3=localhost:5002 go test -race -count=1 -tags integration -run TestIntegrationS3Redirect -v ./internal/facade/`.

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
