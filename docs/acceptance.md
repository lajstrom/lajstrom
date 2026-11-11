# PoC acceptance: what v0.1.0 must do, and how we check it

- Status: draft for the lead's review (2026-11-06). Owner: @gecube. This page changes only by a
  pull request; the roadmap's decision log records why.
- Related: [roadmap.md](roadmap.md) (scope and dates), [architecture.md](architecture.md) (the
  flows), the specs in [spec/](spec/), the issues in [plan/](plan/).
- Audience: the two people building it, and anyone judging whether the proof of concept holds.

The proof of concept is v0.1.0 as the roadmap defines it. This page turns that definition into
checks with an id, steps, an expected result and the issue that implements it. A check passes when
the expected result is observed on the reference environment of §2. v0.1.0 is accepted when every
check marked **P0** passes and every failed **P1** has an issue and a line in the roadmap's
decision log. Every check is automated in `test/e2e/` (#34) unless marked *manual*; the Friday
demos are these scripts.

The cut line: if the week-1 review (2026-11-13) moves maven (M3) after the MVP, §3.6 and the maven
rows elsewhere are deferred as a block. Nothing else moves.

## 1. Scope

### 1.1 What must work

| Area | Functions | Issues |
| --- | --- | --- |
| Core | `lajstrom serve`, `migrate`, `reindex`, `version`, `admin`; YAML config with env overrides and validation; TLS only (`externalURL` is `https://host`), TLS at the edge or in the binary; JSON logs with request ids and no secrets; `/-/healthz`, `/-/readyz`, `/-/version`; Prometheus on a separate listener; graceful shutdown that drains transfers | #7, #23, #50 |
| raw | `PUT`, `GET`, `HEAD`, `DELETE /raw/<repo>/<path…>`, `?digest=` pin, JSON listing, 307 to the registry when it supports redirect, `Range` | #13 |
| pypi | `twine upload`, `pip install --index-url`, PEP 503 HTML, PEP 691 JSON, PEP 658 metadata, name normalisation | #31, #32, #33 |
| apt | `PUT` upload of a `.deb`, `oras push` with the deb profile, signed `InRelease`, `Release` + `Release.gpg`, `Packages` (+gz, +xz), `by-hash`, `key.asc`, pool downloads; `apt-get update && apt-get install` on Ubuntu and Debian | #39, #40, #41, #42, #48 |
| maven (cut line) | `mvn deploy` of a multi-file release, `mvn dependency:get`, Gradle publish and resolve, `maven-metadata.xml`, checksum files, publication sessions that publish on commit only | #52, #53, #54, #55, #56 |
| OCI facade | `/v2`: token endpoint, blob uploads (monolithic, chunked, mount), blob and manifest reads, manifest `PUT` hook into ingest, `tags/list`, `_catalog`, referrers with tag-schema fallback, blob 307 redirect; `oras`, `crane`, `cosign` work | #24, #25, #26, #27, #28, #37 |
| Ingest | one write path; profile and plugin validation; tag claim before the registry write; idempotent re-push; 409 on a tag with other bytes; audit event | #12 |
| Index and reindex | SQLite (single node) and Postgres (HA) behind one `Store`; `reindex` rebuilds every derived row from the registry; `reindex --report` lists foreign manifests; the reconcile loop picks up drift | #10, #29, #30 |
| Auth | local users (argon2id), personal access tokens, deploy tokens, bindings with masks, roles reader/writer/maintainer/admin, anonymous read per repo, bearer tokens for `/v2`, the 401/404/403 rule of rbac.md §3, audit log | #43, #44, #45, #46, #26 |
| Management | `/api/v1` repos, users, tokens, bindings, audit, whoami; `admin` CLI for bootstrap; repos from the config file | #14, #47 |
| Providers | generic provider on oras-go against Zot and docker distribution; ECR adapter if the spike allows; capability matrix filled from spikes | #9, #17–#21, #35, #36 |
| Delivery | `goreleaser`: binaries, `ghcr.io/lajstrom/lajstrom` and Docker Hub images, SBOM, cosign signature; install guide; compose stacks `single` and `ha` | #57, #58, #15 |

### 1.2 Explicitly not in the PoC

Proxy and group repos, OIDC and groups, the web UI (its own repository, #70), docker image
passthrough and multi-arch stitching, vulnerability scanning, scheduled cleanup (retention, #72),
webhooks (#77), rpm, npm, terraform, FQDN mode, Harbor and GHCR adapters beyond the spikes, maven
SNAPSHOT, apt source packages, pypi yank. A check that needs one of these is not in this page,
but for M4's npm with its proxy and group repos, which come after v0.1.0 and have their own rows
outside its sign-off (§3.11).

Scanning and cleanup are external jobs in the PoC, not server features: a cron container with
`oras`, `crane` or `curl` against the public interfaces. The PoC keeps what such a job needs,
and the rows below prove it: enumeration through `GET /v2/_catalog` and `tags/list` (O-08,
O-09); referrers pushed with `subject` = the version's manifest and listed back on every backend
(O-07), so a scan report or an SBOM is content-addressed, idempotent to re-push and survives a
`reindex` (I-01); `DELETE /v2/<name>/manifests/<digest>` with index removal (R-07, E-09); a
deploy token with `write` for the scanner (S-04) and, because deploy tokens never carry `delete`
(rbac.md §2), a service user with a maintainer binding and a PAT for the cleanup job (S-03,
S-06); the audit log with `since` as a polling cursor (S-09). Both jobs are reconciliation
loops: every run recomputes "versions without a fresh report" or "versions past the policy" from
the registry and acts idempotently, so a missed run or a crashed job loses nothing.

## 2. Reference environment

Two compose stacks in `deploy/compose/` (#15, extended by #34):

| Stack | Lajstrom | Edge | Database | Registry | Use |
| --- | --- | --- | --- | --- | --- |
| `single` | 1 replica, `lajstrom serve` | none, `server.insecureHTTP: true` on loopback | SQLite | Zot | development, unit and integration tests |
| `ha` | 2 replicas, `lajstrom-a` and `lajstrom-b`, the release image | nginx with TLS from a local CA, round robin per request, `proxy_request_buffering off`, `client_max_body_size 0`, health check on `/-/readyz` | Postgres 16 | docker distribution v3 with S3 storage on SeaweedFS (blob redirect), plus Zot as the second registry | acceptance, HA, security |

Fixed names: host `art.test` (in `/etc/hosts` of the runner and of every client container), the
CA from `mkcert` installed in the client images, metrics on `127.0.0.1:9464` of each replica and
nowhere else.

Client containers, pinned by digest in `test/e2e/clients.env`: `ghcr.io/oras-project/oras`,
`gcr.io/go-containerregistry/crane`, `gcr.io/projectsigstore/cosign`, `python:3.13` (pip, twine,
uv), `ubuntu:24.04`, `debian:12`, `maven:3.9-eclipse-temurin-21`, `gradle:8-jdk21`.

Bootstrap, run by `test/e2e/bootstrap.sh` and repeated by hand for the manual checks:

```sh
make build && make dev-up STACK=ha
bin/lajstrom admin user create --name admin --admin          # a prompt for the password
# repos: one per format, one public
curl -u admin:… -X POST https://art.test/api/v1/repos -d '{"name":"assets","format":"raw","registry":"dist"}'
curl -u admin:… -X POST https://art.test/api/v1/repos -d '{"name":"public","format":"raw","registry":"dist","anonymousRead":true}'
curl -u admin:… -X POST https://art.test/api/v1/repos -d '{"name":"internal","format":"pypi","registry":"dist"}'
curl -u admin:… -X POST https://art.test/api/v1/repos -d '{"name":"ubuntu-internal","format":"apt","registry":"zot"}'
curl -u admin:… -X POST https://art.test/api/v1/repos -d '{"name":"releases","format":"maven","registry":"dist"}'
# principals: alice writer on */*, bob reader on pypi/*, a deploy token with write on raw/assets
```

Fixtures are built in process by `internal/testutil` (#22): a `.deb`, a wheel and an sdist, a jar
with its POM. Nothing binary is vendored.

Preconditions before any check: `GET https://art.test/-/readyz` is 200 on both replicas (the edge
exposes `/-/readyz` of each replica on `art.test:8443` and `:8444` for the HA checks), and
`curl http://127.0.0.1:9464/metrics` works from inside the replica's network namespace only.

## 3. Functional checks

Columns: id, what to do, what must happen, issue. Priority is P0 unless the id carries `(P1)`.

### 3.1 Core and operations

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| C-01 | `lajstrom serve --config lajstrom.yaml` with the `ha` config | listens, logs `listening` with `addr` and `externalURL`, `readyz` 200 within 5 s | #7 |
| C-02 | config with `externalURL: https://art.test/sub` | exit 2, the error names `server.externalURL` and says no path is allowed | #50 |
| C-03 | config with `externalURL: http://art.test` and no `insecureHTTP` | exit 2, the error says TLS only, `insecureHTTP` is for loopback | #50 |
| C-04 | config with `passwordEnv: ZOT_PASSWORD` and the variable unset | exit 2, the error names the variable, never a value | #7 |
| C-05 | stop the registry container, wait 70 s, `GET /-/readyz`; start it, wait 70 s | 503 with the registry name while down, 200 after | #23 |
| C-06 | `GET /-/version` | JSON with `version`, `commit`, `date` equal to the image labels | #7 |
| C-07 | run the whole e2e suite, then `grep` the logs of both replicas for every password, token secret and `Authorization` value used | no match; every line has `requestId`; `X-Request-Id` sent by a client appears unchanged (max 128 chars, else replaced) | #23 |
| C-08 | `GET /-/metrics` on `art.test`; `GET /metrics` on `127.0.0.1:9464` inside the container | 404 on the host; 200 with `lajstrom_build_info` and `lajstrom_http_request_duration_seconds` on the listener; no label value is a repo or package name | #23 |
| C-09 | start a 500 MiB `curl -T` to replica A, send `SIGTERM` to A after 2 s | the upload completes with 201, new connections to A are refused meanwhile, A exits 0 within `shutdownTimeout` | #50 |
| C-10 | `lajstrom migrate` on an empty Postgres, then again | first run creates the schema, second is a no-op; there are no down migrations ([index-schema.md](spec/index-schema.md#upgrades)) | #10 |
| C-11 (P1) | `lajstrom version --json` from the release binary | `version` is the git tag, `commit` the sha, `date` ISO 8601 UTC | #57 |

### 3.2 raw

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| R-01 | `curl -u alice -T file.txt https://art.test/raw/assets/docs/file.txt` | 201, `Location` is the same path, `Docker-Content-Digest` is the manifest digest; `GET` returns the exact bytes with `Content-Length`, `Content-Type` from the metadata layer, `ETag` = blob digest, `Last-Modified`; `HEAD` has the same headers and no body | #13 |
| R-02 | same `PUT` again with identical bytes | 201, same digest, one version in the index (`Existed`) | #12, #13 |
| R-03 | `PUT` different bytes to the same path, then `GET`; `GET ?digest=<old manifest digest>` | new bytes; the pinned `GET` returns the old bytes; both versions in the listing | #13 |
| R-04 | `GET /raw/assets/` and `GET /raw/assets/docs/` | `{"entries":[{"path","size","digest","created"}…]}`, directory prefixes with a trailing `/`, sorted by path | #13 |
| R-05 | `GET` a 200 MiB file on the `ha` stack (distribution + SeaweedFS) | 307 to a presigned URL that expires in 20 minutes (the s3 driver's `X-Amz-Expires=1200`); the bytes match; on the `single` stack (Zot) the body streams, and a `Range` of one byte range is `206` with that range: Zot sends no `Accept-Ranges` on `GET`, and Lajstrom passes on the backend's header and adds none (http-routing.md §2; docs/registries/zot.md) | #13, #27 |
| R-06 | `curl -r 1000-1999` on a streamed file | 206 with 1000 bytes; the backend received the `Range` | #13 |
| R-07 | `DELETE /raw/assets/docs/file.txt` as bob (reader), then as alice (writer), then as a maintainer | 403, 403, 204; after it `GET` is 404 and the manifests are gone from the backend when it can delete (else 501 and nothing changes) | #13, #61 |
| R-08 | `oras pull art.test/raw/assets/docs/file.txt:<tag>` from the backend repository | writes a file named `file.txt` | #13 |
| R-09 | `PUT /raw/assets/Docs/Readme.MD`, `/raw/assets/docs/readme.md`, `/raw/assets/с пробелом/файл.txt`, a 300-character path | `docs/readme.md` is 409 naming `Docs/Readme.MD` (a raw path is unique without regard to case: the first spelling wins); the other three are distinct versions; case preserved; the long path gets a hashed component; listing shows the original paths | #8, #13, #97 |
| R-10 | `PUT` an empty file | 201; `GET` returns 0 bytes with `Content-Length: 0` | #13 |

### 3.3 pypi

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| P-01 | `twine upload --repository-url https://art.test/pypi/internal/ dist/*` with a wheel and an sdist of `demo-pkg 1.0.0` | 200 twice; two manifests, variants `py3-none-any` and `sdist`; `GET /pypi/internal/simple/demo-pkg/` lists both with `#sha256=` | #33, #32 |
| P-02 | `pip install --index-url https://art.test/pypi/internal/simple/ demo-pkg` in `python:3.13`; the same with `uv pip install` | installs 1.0.0 from the wheel; `pip download --no-binary :all:` gets the sdist | #32 |
| P-03 | `GET …/simple/` with `Accept: application/vnd.pypi.simple.v1+json` and with `text/html` | JSON per PEP 691 and HTML per PEP 503 with the same files; `data-requires-python` and `data-core-metadata` present | #32 |
| P-04 | `GET …/simple/Demo_Pkg/` | 301 to `…/simple/demo-pkg/` | #32 |
| P-05 | `GET …/files/demo-pkg/1.0.0/demo_pkg-1.0.0-py3-none-any.whl.metadata` | the core metadata from the metadata layer, PEP 658 | #32 |
| P-06 | `twine upload` the same wheel again; then a wheel with the same name and different bytes | 200 idempotent; 409 and twine prints the server's message | #33, #12 |
| P-07 | upload as bob (reader) | 403; as anonymous 401 with the Basic challenge | #45 |
| P-08 (P1) | a wheel whose METADATA disagrees with its file name (`Version: 2.0` in `demo_pkg-1.0.0-…`) | 400 with the field named | #31 |

### 3.4 apt

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| A-01 | `curl -u alice -T hello_1.0-1_amd64.deb "https://art.test/apt/ubuntu-internal/upload?suite=jammy&component=main"` | 201; manifest tagged `1.0-1__jammy_2fmain_2famd64`, `lajstrom.variant: jammy/main/amd64`, with `lajstrom.deb.*` annotations from `control` and the query; `GET …/dists/jammy/main/binary-amd64/Packages` lists it with `Filename: pool/main/h/hello/hello_1.0-1_amd64.deb`, `SHA256`, `Size` | #42, #39, #110, #40 |
| A-02 | `oras push art.test/apt/ubuntu-internal/hello:1.0-1__jammy_2fmain_2famd64 …` with the deb profile (flow A of architecture.md) | 201 and the same index rows as A-01 | #25, #12, #42 |
| A-03 | `GET …/dists/jammy/InRelease` | clearsigned with the repo key; `gpg --verify` against `GET …/key.asc` succeeds; `Release` + `Release.gpg` verify too; `Acquire-By-Hash: yes` and `by-hash/SHA256/<hash>` resolve | #41, #40 |
| A-04 | in `ubuntu:24.04` and `debian:12`: `curl key.asc > /etc/apt/keyrings/…`, `deb [signed-by=…] https://art.test/apt/ubuntu-internal jammy main`, `apt-get update && apt-get install hello` | both succeed; `hello` runs; no `NO_PUBKEY`, no `Hash Sum mismatch` | #42, #48 |
| A-05 | upload a `.deb` for `arm64` and one for `all` arch to the suite of A-01, then fetch `binary-amd64/Packages` and `binary-arm64/Packages` | the `all` package is in both (the architectures of a suite are those of its packages) | #40 |
| A-06 | upload the same `.deb` again; upload a different `.deb` with the same name, version and arch to the same suite and component | 201 idempotent; 409; in another suite 409 as well (E-07) | #12, #42 |
| A-07 | `GET pool/main/h/hello/hello_1.0-1_amd64.deb` on both stacks | 307 or stream as in R-05; one blob for the suites that hold the file (E-07) | #42 |
| A-08 | a package with epoch `1:2.0-1` | tag `1_3a2.0-1__jammy_2fmain_2famd64`; `Packages` shows `Version: 1:2.0-1` | #8, #39, #110, #40 |
| A-09 (P1) | upload, then immediately `apt-get update` through the other replica | the new package is visible: `index_cache` was invalidated in the database | #40 |

### 3.5 OCI facade

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| O-01 | `oras push art.test/raw/assets/hello:1.0 file.txt` with the raw annotations, as alice (`oras login`) | 401 with `WWW-Authenticate: Bearer realm="https://art.test/v2/token",service="art.test"` first, then a token with `pull,push`, then 201; the manifest is indexed | #26, #25 |
| O-02 | `oras pull`, `oras manifest fetch`, `oras repo tags`, `crane ls art.test/raw/assets/hello` | the file, the manifest with `Docker-Content-Digest`, the tag list from the index | #24 |
| O-03 | `oras push` a manifest whose annotations say `lajstrom.name: other` for the path `hello`, one with an unknown `artifactType`, one without `lajstrom.schema`, one whose `org.opencontainers.image.created` is a year before the server's clock, one with a layer that was never uploaded, one 5 MiB of JSON | 400 `MANIFEST_INVALID` for the first four; the fourth before the registry is asked, with the message `org.opencontainers.image.created <time> is <duration> behind the server's clock, <now>: it may be at most 5m0s ahead and 1h0m0s behind; push again, with the current time` (the bounds are `server.maxCreatedAhead` and `server.maxCreatedBehind`); 400 `MANIFEST_BLOB_UNKNOWN`, 413 | #25, #8, #551 |
| O-04 | push tag `1.0` again with other bytes | 409 `TAG_INVALID` with the existing digest in the message; the registry tag still points at the first manifest | #25, #12 |
| O-05 | chunked upload (`oras` with `--chunk-size` or a scripted `POST`/`PATCH`/`PUT`) of 50 MiB through the edge | 201; the blob digest matches; on the `ha` stack the chunks went to different replicas (log check) | #24 |
| O-06 | `?mount=<digest>&from=raw/assets/hello` to `raw/public/hello` | 201 mount when the backend supports it, else a normal upload | #24 |
| O-07 | `cosign sign` and `cosign verify` of the manifest of O-01 on a backend with the referrers API (Zot) and one without (distribution without it) | both verify; the second uses the tag-schema fallback and `GET /v2/<name>/referrers/<digest>` returns the signature on both | #28, #37 |
| O-08 | `GET /v2/_catalog` as bob (reader on pypi/*) and as admin | bob sees only `pypi/*` repositories; admin sees all | #24, #45 |
| O-09 | `GET /v2/raw/assets/hello/tags/list?n=2`, then with `last=` | two tags and a `Link` header; the next page continues | #24 |
| O-10 | `GET /v2/raw/assets/hello/blobs/<digest>` | 307 on the `ha` stack, stream on `single` (as R-05) | #27 |
| O-11 | the OCI distribution conformance suite, pull and push categories, against an `oci` repo | passes | #24 |
| O-12 | token for `scope=repository:raw/assets/hello:pull,push` as bob | the token carries `pull` only; a push with it is 403 `DENIED` | #26, #45 |

### 3.6 maven (cut line)

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| M-01 | `mvn deploy` of `com.example:lib:1.2.0` with jar, pom, sources, javadoc and checksum files against `https://art.test/maven/releases` | every `PUT` 201; the `PUT maven-metadata.xml` commits one manifest with four file layers; checksum `PUT`s are verified, not stored | #54, #53 |
| M-02 | `mvn dependency:get -Dartifact=com.example:lib:1.2.0` from a clean `~/.m2`; a Gradle build that resolves it | resolves; `.sha1` and `.sha512` files match | #55, #56 |
| M-03 | `GET …/com/example/lib/maven-metadata.xml` | `latest`, `release`, `versions`, `lastUpdated` from the index; checksum files alongside | #55 |
| M-04 | deploy 1.2.0 again unchanged; then with a changed jar | 200 idempotent; 409 | #54 |
| M-05 | a `PUT` of `lib-1.2.0.jar.sha1` that does not match the jar | 400; the session stays open; a correct retry works | #54 |
| M-06 | start a deploy, stop after the jar; wait out the session's timeout (30 s for the test) | the session is expired, the version does not exist, the blobs are orphans the reconcile report lists; a new deploy of the version starts an empty session | #53 |
| M-07 | `GET …/lib/1.2.0/lib-1.2.0.jar` on both stacks | 307 or stream | #55 |

### 3.7 Index and reindex

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| I-01 | after every format has content: dump `packages`, `versions`, `files`, `oci_repos`; `lajstrom reindex`; dump again | identical rows (ids may differ, natural keys and every other column equal); `index_cache` is empty and the next `GET` of an index rebuilds it | #29 |
| I-02 | `oras push` a manifest without `lajstrom.schema` straight to the backend under `raw/assets/foreign`; `lajstrom reindex --report` | the row is in `oci_repos`, not in `versions`; the report names it; the cosign signature of O-07 is not in the report (a manifest with `subject` is a referrer, not a foreign manifest, #89) | #29 |
| I-03 | push a profile manifest straight to the backend (bypassing Lajstrom); wait one reconcile interval | the version appears in the index; the audit log has a `reconcile` event | #30 |
| I-04 | re-point a tag straight in the backend to another digest; wait one interval | the index keeps the first digest, the report lists the drift, nothing is re-pointed by us | #30, #12 |
| I-05 | delete a manifest straight in the backend; wait one interval | its rows are gone | #30 |
| I-06 | the store conformance suite on SQLite and on Postgres | passes on both in CI (#16) | #10 |
| I-07 (P1) | attach an existing prefix with 1 000 manifests (generated); `lajstrom reindex` | finishes; rate and memory in the log; the count matches | #29 |

### 3.8 Auth, RBAC, audit

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| S-01 | `lajstrom admin user create --name admin --admin` (#43), or `lajstrom admin bootstrap` with the bootstrap token (#440); then `POST /api/v1/users` and `/bindings` for alice, bob, a maintainer (#47) | 201s; `GET /api/v1/whoami` as each returns the principal (#43) | #43, #47 |
| S-02 | the matrix below, on the raw frontend and on `/v2` (push, pull, delete) | exactly the table | #45 |
| S-03 | `POST /api/v1/users/alice/tokens` (PAT, scopes `read`), use it to push | 403; with scopes unset it inherits alice's bindings | #44 |
| S-04 | a deploy token with `write` on `raw/assets`: push to `raw/assets`, push to `pypi/internal`, `DELETE` on `raw/assets` | 201, 404 (no access at all on that repo, rbac.md §3), 403 (it can read `raw/assets`, so the denial is visible) | #44, #45 |
| S-05 | revoke the PAT, retry within one second | 401 | #44 |
| S-06 | a PAT with `expiresAt` 1 s ahead; wait 2 s | 401; creating one with expiry over one year is 400 | #44 |
| S-07 | `anonymousRead: true` on `raw/public`: anonymous `GET`, anonymous `PUT`, anonymous `GET` on `raw/assets`, anonymous `GET` on `raw/missing` | 200, 401, 401, 401 (the last two are indistinguishable) | #45 |
| S-08 | bob (reader on pypi/*) `GET /raw/assets/…`, `GET /raw/missing/…` | 404 and 404 (no 403 leak of names) | #45 |
| S-09 | after the suite: `GET /api/v1/audit?repo=raw/assets` | one row per write with principal, action, repo, digest, client ip, request id; reads are not there | #46 |
| S-10 | `GET /api/v1/users` as bob | 403 (`system` resource) | #45, #47 |

RBAC matrix for S-02, expected status on the raw frontend (`/v2` maps 401 to `UNAUTHORIZED`, 404 to
`NAME_UNKNOWN`, 403 to `DENIED`):

| Principal | GET private | PUT private | DELETE private | GET missing repo |
| --- | --- | --- | --- | --- |
| anonymous | 401 | 401 | 401 | 401 |
| reader | 200 | 403 | 403 | 404 |
| writer | 200 | 201 | 403 | 404 |
| maintainer | 200 | 201 | 204 | 404 |
| admin | 200 | 201 | 204 | 404 |
| user with no binding on the repo | 404 | 404 | 404 | 404 |

### 3.9 Management API and CLI

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| G-01 | `POST /api/v1/repos` twice with the same name and format; once with `format: oci2`, once with an unknown `registry` | 201, 409 `CONFLICT`, 400 `INVALID` naming the field, 400 naming the registry | #14 |
| G-02 | `PATCH …/repos/raw/assets` with `anonymousRead: true`; `GET` | the change is visible on the next request on both replicas | #14 |
| G-03 | `repos:` in the config file with two repos; start twice | both exist after the first start; the second start changes nothing (idempotent merge) | #14 |
| G-04 | `DELETE …/repos/raw/assets` as bob (reader), as a maintainer of that repo | 403, 204 (`manage` covers it, http-routing.md §4); afterwards `GET /raw/assets/…` is 404 and a bearer token issued before is rejected on that repo | #14, #45 |
| G-05 | `POST …/repos/raw/assets/reindex` | 202, the reindex of that repo runs, the audit log has it | #14, #29 |
| G-06 | every error of `/api/v1` | `{"error":{"code","message"}}` with the seven codes of http-routing.md §4 only | #14, #47, #579 |
| G-07 (P1) | `lajstrom admin export` | JSON of the control plane without secrets (token hashes yes, passwords no, private keys as ciphertext) | #47 |

### 3.10 Delivery

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| D-01 | `goreleaser release --snapshot` | linux/amd64 and arm64 binaries, darwin, the two images, an SBOM, a cosign signature that `cosign verify` accepts with the published key | #57 |
| D-02 | `docker run ghcr.io/lajstrom/lajstrom:v0.1.0 version` | runs as a non-root uid, read-only root fs works, no shell required | #57 |
| D-03 *manual* | the install guide on a fresh VM with a public DNS name, TLS in Lajstrom itself | `curl -T` round trip works from a laptop within 30 minutes of reading | #58 |
| D-04 | `make dev-up STACK=single` and `STACK=ha` on macOS (OrbStack) and on the CI runner | both reach `readyz` 200 within 60 s | #15 |

### 3.11 npm, proxy and group repos (M4)

M4 comes after v0.1.0 ([roadmap.md](roadmap.md)): these rows are not part of the v0.1.0 sign-off
(§1.2). They are the demo of M4. `test/e2e/npm.sh` prints each id at the step that checks it
(`make e2e E2E=npm`). It runs in `node:26.13.0-slim` with npm 11, pnpm 10 and yarn 4, against a
hosted repo, a proxy of registry.npmjs.org and a group of both, all made through the API.

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| J-01 | `npm login` on a hosted repo with a user's password; `npm whoami` | a personal access token in `.npmrc`, listed for the user; `whoami` names the user | #308 |
| J-02 | `npm publish` of an unscoped package (1.0.0, 1.1.0, and 2.0.0-beta.1 `--tag beta`) and of a scoped one; `npm install` of both into an empty project with an empty cache | dist-tags `beta` and `latest` (1.1.0); the install resolves `latest` and `require()` loads it | #305, #306 |
| J-03 | `npm dist-tag add`, `ls`, an install by the tag, `rm`; `npm deprecate` and `npm deprecate ''` | the tag in the packument and in `ls`, gone after `rm`; the message in the packument and in `npm install`'s warning, gone after `''` | #307, #308 |
| J-04 | pnpm with a deploy token as `_authToken`: `pnpm whoami`, `pnpm publish` of a package that depends on J-02's, `pnpm add` with an empty store | `token:<id>`; the dependency resolved from the repo (`^1.0.0` to 1.1.0) | #305, #306 |
| J-05 | yarn 4: `yarn npm login`, `whoami`, `publish`, `tag add`, `list`, `remove`, `yarn add` with an empty cache | a token in `.yarnrc.yml`; the tags change in the packument; the packages of npm, pnpm and yarn installed | #305, #307, #308 |
| J-06 | `npm install chalk@4.1.2` through a proxy of npmjs.org, twice, each with an empty cache | the first fetches each tarball once (one `proxy.fetch` row each) and holds it in the registry; the second fetches none | #309, #310 |
| J-07 | npm, pnpm and yarn each publish through a group of the hosted repo and the proxy a scoped package that depends on `chalk@4.1.2`, with `registry=<group>` the one registry line, and install it with an empty cache (pnpm with the group's deploy token) | the versions land in the hosted repo, their `push` rows with `details.via`; `require()` loads `chalk@4.1.2`; every `dist.tarball` names the group; nothing is fetched again; a group's deploy token with `write` is 400 | #311 |
| J-08 | publish a name of npmjs.org (`is-number`) to the hosted repo; read its packument through the group; then `group.merge` of the name | the hosted versions only; with `merge`, the upstream's versions too | #311 |
| J-09 | `npm audit` through the proxy of a project with `minimist@1.2.0` and a scoped package of the hosted repo, first without a deny list, then with `deny` of the scope and of `minimist` | first npmjs.org's advisories of minimist; then none: the denied names never leave the server | #514 |
| J-10 | `npm unpublish` of a version as a writer, then as a maintainer; `npm unpublish <scoped> --force` | E403 and nothing deleted; the version gone with its dist-tag; the package 404 | #308 |
| J-11 | `npm dist-tag add` (`latest` below the highest release, a new tag) and `npm deprecate` through the group; delete the hosted repo, make it again, `POST …/reindex` | the delete is 409 while the repo is a member of the group; after the reindex every packument of the run is as before, dist-tags, deprecations and integrity; no orphan journal entry | #307, #311 |
| J-12 | the package of J-10's `--force`, after J-11's reindex | still 404: its journal went with it | #307, #308 |
| J-13 | `npm logout` | the token gone from `.npmrc` and revoked (401); the token of `yarn npm login` still valid | #308 |

## 4. High availability

The constraint: several replicas serve one registry and one database, nothing in process memory
outlives a request (layout.md §1 conventions). The `ha` stack has two replicas behind nginx, round
robin per request, so every multi-request protocol crosses replicas by construction. The script is
`test/e2e/ha.sh`; `docker kill` and `docker start` are driven from it.

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| H-01 | run the whole of §3 against the `ha` stack | every check passes unchanged, but X-04 through the round robin: the throttle of failed logins counts per replica ([auth.md](auth.md#failed-logins)), so X-04 runs on one replica past the edge; both upstreams served requests | #34 |
| H-02 | two `curl -T` of the same 100 MiB file to `raw/assets/big` at the same time, one to replica A and one to B (ports 8091 and 8092 bypass the round robin) | both 201 with the same digest; one blob and one manifest in the backend; one version in the index | #12, #24 |
| H-03 | two `oras push` of tag `race` with different bytes, one per replica, started within the same 100 ms (`&` and `wait`) | exactly one 201 and one 409 `TAG_INVALID`; `oras manifest fetch` from the backend returns the winner's digest; the index has the winner's digest; repeated 20 times | #12 |
| H-04 | a chunked upload: `POST` on A, `PATCH` chunks on B, final `PUT` on A, by script | 201; the digest matches | #24 |
| H-05 | `curl -T` 500 MiB to replica A; `docker kill lajstrom-a` after 100 MiB; retry the same `PUT` through the edge | the first fails with a connection error, the retry is 201; no version with a wrong digest in the index; no claim older than the TTL left after `ingest.claimTTL`; after `docker start lajstrom-a` its `/tmp` has no spool files | #12, #24 |
| H-06 | a loop of `pip install` every second; `docker restart postgres` | requests during the outage are 503 and `readyz` is 503, the loop recovers without restarting Lajstrom, no duplicate versions and no half-indexed version afterwards | #23, #10 |
| H-07 | the same loop; `docker restart registry` | 502 during the outage (`provider.ErrUnauthorized` or connection errors map to 502/503), recovery; the next reconcile reports nothing missing | #9, #30 |
| H-08 | maven deploy with the edge alternating replicas on every `PUT` (M-01 on the `ha` stack) | the session lives in the database; the commit on whichever replica sees the metadata `PUT` | #53 |
| H-09 | `POST …/reindex` on A and on B at once | one runs, the other answers 409 `CONFLICT` "already running" (advisory lock); the result equals I-01 | #29 |
| H-10 | the `pip install` loop; `docker restart lajstrom-a`, wait for `readyz`, then `lajstrom-b` | zero failed requests: nginx takes a replica out on `readyz` 503 and shutdown drains in-flight requests | #50 |
| H-11 | upload a `.deb` through A, `apt-get update` through B within one second | the package is visible (A-09) | #40 |
| H-12 | get a bearer token from A (`/v2/token`), use it on B | accepted: the signing key is in the database, not in a replica's memory or local file | #26 |
| H-13 | the same `index_cache` scope requested on A and B at once after an invalidation | both answer the same bytes; one `index_cache` row | #11, #40 |
| H-14 *note* | SQLite with two replicas | not supported, and detected: a second `serve` on the SQLite file of a live one exits with an error that names it (its row of `replicas`, [index-schema.md](spec/index-schema.md#coordination)), and a restart after a crash starts once the crashed one's heartbeat is 2 minutes old. The install guide says Postgres for more than one replica, and the Helm chart (post-MVP) refuses `replicas > 1` with SQLite | #58, #478 |

## 5. Security

The threat model (#63) is the written argument; these are the checks that must hold before it is
signed. `test/e2e/security.sh` automates the ones with a command; the rest are reviews.

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| X-01 | C-02, C-03; `curl http://art.test/` | TLS only; plain HTTP is refused by the edge (301 or closed), and the server will not start with a non-loopback `http` `externalURL` | #50 |
| X-02 | C-07 after the whole suite, plus a `PUT` with `Authorization` and `Cookie` headers set | no secret in any log line; `Cookie` is never logged either | #23 |
| X-03 | `SELECT hash FROM tokens` (index-schema.md); the API response that created the token | sha256 in the database; the secret is shown once in the creation response and never again (`GET` shows the hash prefix only) | #44 |
| X-04 | `SELECT password_hash FROM users` | argon2id with the parameters of rbac.md; 20 wrong passwords in a row for alice | the 11th and later answer 429 with `Retry-After` while a correct password for bob still works | #43, #50 |
| X-05 | S-07, S-08 and the matrix of §3.8 on the frontends and on `/v2` | the 401/404/403 rule holds everywhere: anonymous never learns whether a repo exists, a principal without access gets the same answer as for a missing repo | #45 |
| X-06 | raw `PUT` to `../../etc/passwd`, `%2e%2e/x`, `/raw/assets//x`, a path with `\0`, a 5 000-byte path; maven `PUT` with `..` in `groupPath`; apt `?suite=../x` | 400 every time; nothing written; the error names the rule | #13, #54, #42, #63 |
| X-07 | R-09 plus names `h0__abc`, `_5f`, `a__b` in raw and pypi | distinct packages with distinct OCI paths; `oras repo ls` on the backend shows no collision; the escape property tests of #8 run in CI | #8 |
| X-08 | O-03 (manifest hygiene) plus a manifest with a `subject` in another repo and one with 10 000 layers | 400 or 413; the facade never stores a manifest the profile rejects on a managed repo | #25 |
| X-09 | a `.deb` whose `control.tar.xz` inflates to 2 GiB; a wheel with a 500 MiB `METADATA`; a POM with a DTD and an external entity; a 3 GiB upload to a repo with `maxFileSize: 1 GiB` | 400 or 413 within seconds, RSS of the replica stays under 256 MiB, the external entity is not fetched | #39, #31, #52, #60, #63 |
| X-10 | config with `registries[].url` pointing at `169.254.169.254`; a `?mount=…&from=` naming a repo on another registry | the URL is used only for that registry's provider (no request parameter ever chooses a URL); the mount is refused with 400 | #9, #24, #63 |
| X-11 | a raw `PUT` whose file name contains `\r\n` or `%0d%0a`; `GET` with a crafted `Host` header | 400; `Location` and absolute URLs are built from `externalURL`, never from `Host` | #13, #63 |
| X-12 | 200 idle connections sending one header byte per 5 s; a 2 MiB request header | the connections are closed after `ReadHeaderTimeout` (10 s), 431 for the header; a parallel 1 GiB transfer is unaffected | #50 |
| X-13 | `OPTIONS` with `Origin: https://evil.test` on `/api/v1/repos` | no `Access-Control-Allow-Origin` header (CORS is off until #70 adds the allowlist) | #70 |
| X-14 | a deploy token used for `DELETE` and for `PATCH /api/v1/repos/…`; a PAT with `scopes: [read]` used for push | 403, 403, 403: deploy tokens never get `delete` or `manage`, PAT scopes only narrow | #44, #45 |
| X-15 | `DELETE /api/v1/audit/1`, `PATCH` on an audit row | 405: the audit log has no write API; `admin export` includes it | #46 |
| X-16 | `govulncheck`, `go-licenses`, CodeQL, the snippet scan, on the release commit | all clean; the allowlist of CONTRIBUTING.md holds | #57 |
| X-17 | D-02; `docker inspect` of the image | non-root, no setuid binaries, `readOnlyRootFilesystem` works with `/tmp` as the only writable mount (the request spool) | #57 |
| X-18 | `SELECT * FROM keys`; `GET …/key.asc`; `tcpdump` between the edge and a client during R-05 | GPG private keys are ciphertext under `masterKeyEnv`; `key.asc` is the public key only; backend credentials never reach a client; the presigned URL expires | #41, #27 |
| X-19 | the config file world-readable; `LAJSTROM_MASTER_KEY` unset with keys present | a warning at start for the file mode; exit 2 for the missing master key, naming the variable only | #41, #50 |
| X-20 *review* | `docs/security/threat-model.md` | every high finding fixed or tracked; `SECURITY.md` has the supported-versions table and a contact | #63 |

## 6. Corner cases

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| E-01 | the idempotent pairs: R-02, P-06, A-06, M-04, O-04 | same bytes always succeed with the same digest; other bytes under the same tag are always 409 with the existing digest; never a silent re-point | #12 |
| E-02 | two `PUT` of the same file to the same replica at the same moment | both 201 (one `Existed`), one version | #12 |
| E-03 | I-04, I-05: changes made straight in the backend | reported, not re-pointed; deleted manifests disappear from the index; the registry stays the truth | #30 |
| E-04 | I-02: a foreign manifest under a managed path | `oci_repos` only, listed by `--report`, never indexed, never served by a frontend; a manifest with `subject` (O-07) is a referrer and stays out of the report | #29, #89 |
| E-05 | pypi `Foo_Bar`, `foo-bar`, `foo.bar` uploads | one project `foo-bar`; `GET …/simple/Foo_Bar/` is 301 | #31 |
| E-06 | a wheel and an sdist of the same version | two manifests with different variants under one package; `pip` picks the wheel; `--no-binary` picks the sdist | #31 |
| E-07 | apt: the same package name in `jammy` and `noble`; the same `.deb` in two suites and in two components; another `.deb` under the pool file name of the first in `noble` | separate `Packages` files; per component one pool URL that the suites share (`pool/<component>/…`), so the two components are two URLs for one blob; the other `.deb` is 409 naming the version that holds the name (a pool holds one content per file name) | #40, #42 |
| E-08 | maven classifier `sources`, extension `pom` only (no jar), a version `1.0.0-rc.1` | all deploy and resolve; `maven-metadata.xml` lists `1.0.0-rc.1` and `release` is the newest non-prerelease | #52, #55 |
| E-09 | a backend without the referrers API, without `_catalog`, without delete, without blob redirect (per the capability matrix) | tag-schema fallback (O-07), `oci_repos` listing, `DELETE` 501 with the index unchanged, streaming with `Range` | #37, #61, #27 |
| E-10 | `org.opencontainers.image.created` 10 years ahead, 1970, with a `+03:00` offset | accepted, normalised to UTC in the index; `Last-Modified` follows it; the listing sorts by it | #8, #13 |
| E-11 | a tag of 129 characters, a 300-character package name, a name of `_` only | hashed tag (`_h` + 40 hex), hashed component (`h0_h` + 32 hex, ADR 0017), `_5f`; everything round-trips through `reindex` | #8 |
| E-12 | 1 000 versions of one pypi project; 1 000 tags of one OCI repo | `simple/<project>/` renders under 200 ms from `index_cache`; `tags/list` pages; `reindex` restores all | #32, #24, #29 |
| E-13 | delete a repo through the API while a client holds a bearer token for it; recreate it with the same name | the old token is refused (`NAME_UNKNOWN`), the new repo starts empty, the backend's old manifests are reported by `--report` under the old prefix | #14, #29 |
| E-14 | `raw/foo` and `pypi/foo` | two resources; bindings on `*/foo` cover both, on `raw/foo` one | #45 |
| E-15 | `HEAD` on every `GET` route of §3 | same status and headers, no body | #13, #32, #42, #55 |
| E-16 | `If-None-Match` and `If-Modified-Since` on indexes and files | 304 with `ETag`; `Packages` changes its `ETag` after an upload | #11, #40 |
| E-17 | a maven session open when both replicas restart | the session is still in the database; the next `PUT` continues it; it expires on schedule | #53 |
| E-18 | `X-Request-Id: <200 chars>`; `X-Request-Id` with a newline | replaced by a generated id; the client-sent id is never written to a header unchanged if it fails the check | #23 |
| E-19 | a `.deb` of 0 bytes; a wheel that is not a zip; a POM that is not XML | 400 naming the parser's complaint, never 500 | #39, #31, #52 |
| E-20 | the registry answers 401 to Lajstrom (wrong `passwordEnv`) | 502 to the client, `readyz` 503 naming the registry, no credential in the log | #9, #23 |
| E-21 | Postgres `max_connections` reached | 503 with `Retry-After`, recovery when connections free up | #10 |
| E-22 | a manifest with layers that exist in the backend under another repository only | 400 `MANIFEST_BLOB_UNKNOWN` unless mounted first (O-06); never a cross-repo read | #25 |

## 7. Non-functional

| Id | Steps | Expected | Issue |
| --- | --- | --- | --- |
| N-01 | 8 parallel 1 GiB uploads and downloads through the facade and raw on the `ha` stack, `process_resident_memory_bytes` from the metrics listener | RSS of each replica under 256 MiB throughout | #62 |
| N-02 | throughput of N-01 compared with the same transfers straight to the registry | at least 0.8 of direct for streams; downloads with 307 are not measured through us | #62 |
| N-03 | `simple/<project>/` with 1 000 files, `Packages` with 1 000 packages, `tags/list` | p95 under 200 ms from the cache, under 2 s on a cold cache | #32, #40 |
| N-04 | `lajstrom serve` start to `readyz` 200 | under 2 s with the schema in place | #7 |
| N-05 | CI: a docs-only PR; a code PR | under 5 minutes; under 15 minutes with integration and e2e | #16, #34 |
| N-06 (P1) | I-07 | reindex of 1 000 manifests under 2 minutes on the dev stack | #29 |

## 8. Sign-off procedure

The release issue (#59) carries this list with links to the runs. Steps, in order:

1. Tag the release candidate from `main` (`v0.1.0-rc.1`); `goreleaser` builds the images (D-01).
2. `make dev-up STACK=ha IMAGE=ghcr.io/lajstrom/lajstrom:v0.1.0-rc.1`; wait for both `readyz`.
3. `test/e2e/bootstrap.sh` creates the users, repos, tokens and bindings of §2.
4. `make e2e STACK=ha`: every script of §3 (`core.sh`, `raw.sh`, `pypi.sh`, `apt.sh`, `maven.sh` unless cut, `oras.sh`, `index.sh`, `auth.sh`, `api.sh`). A failure stops here.
5. `test/e2e/ha.sh`: H-01 to H-13, twenty iterations of H-03.
6. `test/e2e/security.sh`: the automated rows of §5; then the manual and review rows (X-20, D-03) with the names of who did them.
7. `test/e2e/corner.sh`: §6.
8. `test/load/run.sh`: §7, numbers pasted into `docs/operations.md`.
9. `make dev-down && make dev-up STACK=single` and `make e2e STACK=single`: the single-node path with SQLite and Zot.
10. The lead fills the table in #59: every P0 green; every red P1 has an issue and a line in the roadmap decision log; the threat model is merged.
11. Tag `v0.1.0`, release notes, announcement.

A check that cannot be run because the environment is missing something is a failure of §2, not
a skipped check.

## 9. Traceability

Every id above names its issue in the last column; the issue's acceptance criteria are the
implementation-level detail, this page is the user-level behaviour. When an issue is cut or moved,
the rows that name it get the same label in this page's next revision. Scripts live in
`test/e2e/` and print the ids they cover, so a run's log is the filled checklist.
