# Registries tested with Lajstrom

What each backend registry was tested with, and what it answered. The capability columns are
`provider.Capabilities` as the provider reported them after its probe (`docs/configuration.md`,
"Registry capabilities"), and what the integration test then saw. Re-run the test and update
this page when a registry version or the provider changes; a registry that is not here is not
tested.

## `generic` (internal/provider/generic)

Tested on 2026-11-07 with `TestIntegration` (`internal/provider/generic/integration_test.go`):
push a blob, push a manifest with a tag in one PUT, resolve, fetch, exists, list tags, attach a
referrer (a manifest with `subject`) and list it, `BlobRedirectURL`, list repositories under a
random prefix, delete the referrer and the manifest. Each run works in `raw/it-<random>/…`,
once without and once with the instance prefix `lajstrom-it/`.

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| distribution | 3.1.2 (`registry:3`, `sha256:38907510…ff768c`), filesystem storage, delete enabled | none | yes | **no**: `/referrers/` is a plain 404; referrers listed through the `sha256-<digest>` tag schema | yes, `n`/`last` pagination | yes | no (not probed) | no redirect: `BlobRedirectURL` saw 200 and streams | pass, with and without prefix |
| distribution | 3.1.2, filesystem storage, `REGISTRY_STORAGE_DELETE_ENABLED=false`, htpasswd | Basic | yes | no (tag schema) | yes | **no**: the first manifest delete answered 405 `UNSUPPORTED`, DeleteManifest turned false; the delete step skipped | no | no redirect | pass (delete skipped) |
| Zot | v2.1.23 (`zot-minimal-linux-arm64`, `sha256:d8ec72e9…0571f3`), default configuration | none | yes | **yes**: the probe got an empty OCI image index for a repository that does not exist | yes | yes | no (not probed) | no redirect: `BlobRedirectURL` saw 200 and streams | pass, with and without prefix |
| Zot | v2.1.23, htpasswd | Basic | yes | yes | yes | yes | no | no redirect | pass (with prefix) |

Notes:

- The `registry:3` image enables deletes in its default configuration
  (`/etc/distribution/config.yml`), unlike distribution's own default.
- distribution 3.1.2 has no referrers API: the routes do not exist, so the probe and oras-go
  get a plain-text 404 and use the tag schema. Lajstrom lists referrers either way.
- Neither registry redirected a blob download with filesystem storage. The redirect branch
  (307 to object storage) is covered by unit tests only; a registry on S3 or another object
  store should be added here when one is tested.
- Bearer token authentication is covered by unit tests against a fake token endpoint, not yet
  against a real registry with a token server.
- Without credentials, both registries with htpasswd answer the probe with 401 and a Basic
  challenge; `Capabilities` then fails with `provider.ErrUnauthorized` (no credentials
  configured), which `serve` logs at startup.

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

The test, once per registry:

```sh
LAJSTROM_TEST_REGISTRY=localhost:55000 go test -race -count=1 -tags integration -run TestIntegration -v ./internal/provider/generic/
LAJSTROM_TEST_REGISTRY=localhost:55000 LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/ go test -race -count=1 -tags integration -run TestIntegration -v ./internal/provider/generic/
LAJSTROM_TEST_REGISTRY=localhost:55002 LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom LAJSTROM_TEST_REGISTRY_PASSWORD="$PASSWORD" \
  go test -race -count=1 -tags integration -run TestIntegration -v ./internal/provider/generic/
```

`LAJSTROM_TEST_REGISTRY_TLS=1` switches to HTTPS. The test does not clean up what the backend
cannot delete; remove the containers afterwards:

```sh
docker rm -f lajstrom-dist9 lajstrom-zot9 lajstrom-dist9-auth lajstrom-zot9-auth
```
