# Dev stack

`make dev-up` starts the two registries and the database that Lajstrom is developed and tested
against, the same on every machine: Zot, docker distribution and Postgres, in containers, from
[deploy/compose/compose.yaml](../deploy/compose/compose.yaml). It is for development only: both
registries take anonymous pushes, and the Postgres password is `lajstrom`.

## What runs

| Service | Image | Port | Used as |
| --- | --- | --- | --- |
| `zot` | `ghcr.io/project-zot/zot-minimal:v2.1.23` | `localhost:5000` | registry `local` of the [example configuration](../deploy/compose/lajstrom.example.yaml); the repo `raw/scratch` lives here |
| `distribution` | `registry:3.1.2` | `localhost:5001` | registry `second` |
| `postgres` | `postgres:17.11-alpine` | `localhost:5432` | database `lajstrom`, user `lajstrom`, password `lajstrom` |

The ports are published on `127.0.0.1` only. Each service keeps its data in a named volume
(`lajstrom-dev_zot-data`, `lajstrom-dev_distribution-data`, `lajstrom-dev_postgres-data`), so
a restart keeps what was pushed and the index. The registry versions are those of
[registries/](registries/README.md), where what each one supports is listed.

- **Zot**: the minimal build, so no extensions (no search, no UI, no metrics), from
  [deploy/compose/zot/config.json](../deploy/compose/zot/config.json). The referrers API is
  built in (`distSpecVersion` 1.1.1). `dedupe` is on; garbage collection runs every 10 minutes
  and removes blobs that no manifest has referred to for an hour. No authentication: anyone
  on the machine can push and pull. Zot accepts OCI media types only, not Docker's (it has no
  `http.compat`): `docker push` of a Docker-format image is refused, and `crane append` needs
  `--oci-empty-base`.
- **distribution**: the configuration of the `registry:3` image, filesystem storage, deletes
  enabled (`REGISTRY_STORAGE_DELETE_ENABLED=true`), log level `info`. It has no referrers API:
  Lajstrom lists referrers through the `sha256-<digest>` tag schema on it. It accepts Docker and
  OCI media types.
- **Postgres**: 17, with the database, user and password of `.env` (all `lajstrom` by
  default), created when the volume is.

## The s3 profile

`make dev-up PROFILES=s3` adds a registry whose blob downloads are a `307` to its object storage,
which the filesystem registries above never answer (#27): distribution on SeaweedFS (#118), two
services and a one-shot.

| Service | Image | Port | Used as |
| --- | --- | --- | --- |
| `distribution-s3` | `registry:3.1.2` | `localhost:5002` | distribution on the `s3` storage driver, bucket `distribution`; deletes enabled |
| `seaweedfs` | `chrislusf/seaweedfs:4.52` | `localhost:9000` | the object store, its S3 gateway; access key `lajstrom`, secret key `lajstrom-dev` |
| `seaweedfs-bucket` | the same | not published | makes the bucket `distribution` when it is missing, then exits |

A blob `GET` or `HEAD` on distribution-s3 answers `307` with a URL signed for 20 minutes
(`X-Amz-Expires=1200`, the s3 driver's default), which SeaweedFS enforces: past it, `403`
"Request has expired". `curl -L` downloads the blob from SeaweedFS, and a `Range` through the
redirect is `206`. distribution signs its redirects for the endpoint it talks to
(`REGISTRY_STORAGE_S3_REGIONENDPOINT`), and the signature covers the host, so the endpoint must
be a name that works inside the registry's container and on the machine alike. The trick:
distribution-s3 runs in the network namespace of seaweedfs (`network_mode: service:seaweedfs`),
so it reaches the S3 gateway as `localhost:9000`, and seaweedfs publishes port 9000 on the
machine, where `localhost:9000` is the gateway too. The presigned URLs then name
`http://localhost:9000/distribution/docker/registry/v2/blobs/…`, which a client on the machine
follows. The port of distribution-s3 is published on the seaweedfs service for the same reason.
`LAJSTROM_S3_PORT` changes the port on both sides and in the endpoint, since they must agree; a
client in another container (CI service containers, the `ha` stack) needs an endpoint name of its
own. SeaweedFS has no option that does it otherwise: `-s3.domainName` is for bucket names in the
host, and `-s3.externalUrl` adds a host the gateway checks signatures against, while the URL still
names the host distribution signed for.

seaweedfs runs `weed server -s3`: the master, a volume server, the filer and the S3 gateway in one
process, the gateway on port 9000 with the one identity of
[deploy/compose/seaweedfs/s3.json](../deploy/compose/seaweedfs/s3.json) (`Admin`, `Read`, `Write`);
compose.yaml comments the other flags. The image makes no bucket, and `s3.bucket.create` fails on
one that exists: seaweedfs-bucket, in the same network namespace, runs `weed shell` once seaweedfs
is healthy and creates `distribution` only when `s3.bucket.list` does not name it, as the bucket
hook of SeaweedFS's Helm chart does. distribution-s3 starts once seaweedfs-bucket has exited 0;
`docker compose -f deploy/compose/compose.yaml ps -a` shows it as `Exited (0)`. The health check of
seaweedfs asks the gateway's `/healthz` with `wget` (the image is Alpine); the gateway starts last,
5 s after the master. From an empty volume `make dev-up PROFILES=s3` returns in about 15 s, once
the image is pulled (270 MB to download, 1 GB on disk: besides `weed` it carries SeaweedFS's Rust
volume server, maintenance worker and vector engine); with the bucket there, in about 8 s.
SeaweedFS keeps its data, the filer's metadata with it, in the volume
`lajstrom-dev_seaweedfs-data`; distribution-s3 keeps nothing of its own.

**MinIO as a fallback.** `make dev-up PROFILES=s3-minio` starts the same distribution on MinIO,
the store of the profile before SeaweedFS: `distribution-s3-minio` on `localhost:5006` and `minio`
(`bitnamilegacy/minio:2025.7.23-debian-12-r5`, the Bitnami build: `minio/minio` is no longer on
Docker Hub) on `localhost:9001`, with the same user, password and bucket. The image creates the
bucket itself, so there is no one-shot; its health check asks `/minio/health/live`, and its data
is in the volume `lajstrom-dev_minio-data`. The trick is the same: distribution-s3-minio runs in
the network namespace of minio and reaches it as `localhost:9001`, which minio publishes, and the
presigned URLs name `http://localhost:9001/distribution/…`. `LAJSTROM_MINIO_PORT` and
`LAJSTROM_DISTRIBUTION_S3_MINIO_PORT` move its two ports; they differ from those of the s3
profile, so `PROFILES=s3,s3-minio` runs both. The integration tests run against both stores:

```sh
make dev-up PROFILES=s3,s3-minio # the three services and those of both profiles
LAJSTROM_TEST_REGISTRY_S3=localhost:5002 go test -race -tags integration -run TestIntegrationS3Redirect ./internal/facade/   # SeaweedFS
LAJSTROM_TEST_REGISTRY_S3=localhost:5006 go test -race -tags integration -run TestIntegrationS3Redirect ./internal/facade/   # MinIO
LAJSTROM_TEST_REGISTRY=localhost:5002 go test -race -tags integration -run 'TestIntegration$' ./internal/provider/generic/
LAJSTROM_TEST_REGISTRY=localhost:5006 go test -race -tags integration -run 'TestIntegration$' ./internal/provider/generic/
LAJSTROM_TEST_REGISTRY=localhost:5000 LAJSTROM_TEST_REGISTRY_S3=localhost:5002 go test -race -tags integration -run TestAptGetInstall ./internal/format/deb/
LAJSTROM_TEST_REGISTRY=localhost:5000 LAJSTROM_TEST_REGISTRY_S3=localhost:5006 go test -race -tags integration -run TestAptGetInstall ./internal/format/deb/
make dev-down RESET=1            # every profile, and the volumes
```

A Lajstrom registry for each in `deploy/compose/lajstrom.yaml`:

```yaml
  - name: s3
    type: generic
    url: "http://localhost:5002"     # distribution on SeaweedFS: blob GETs are 307 to localhost:9000
  - name: s3-minio
    type: generic
    url: "http://localhost:5006"     # distribution on MinIO: blob GETs are 307 to localhost:9001
```

## The gitlab profile

`make dev-up PROFILES=gitlab` starts the GitLab container registry, the fork of distribution that
GitLab.com runs, in its two variants (#117; the measurements, the garbage collector and what the
provider must do differently are in [registries/gitlab.md](registries/gitlab.md)):

| Service | Image | Port | Used as |
| --- | --- | --- | --- |
| `gitlab` | `registry.gitlab.com/gitlab-org/build/cng/gitlab-container-registry:v4.43.0-gitlab` | `localhost:5003` | variant A, metadata on the filesystem; filesystem storage, deletes enabled, no auth |
| `gitlab-db` | the same | `localhost:5004` | variant B, the metadata database, as GitLab.com runs it, with the online garbage collector; the container applies the schema migrations and starts |
| `gitlab-postgres` | `postgres:17.11-alpine` | not published | the database of variant B: user, password and database `registry` |

With its defaults, variant B refuses every manifest Lajstrom writes (its `artifactType` is not in
the registry's table of known media types): `LAJSTROM_GITLAB_DYNAMIC_MEDIA_TYPES=true` lets it add
unknown types on push, and `LAJSTROM_GITLAB_GC_REVIEWAFTER=-1` makes its garbage collector review
at once instead of 24 h after an event; both in the shell of `make dev-up` or in `.env`.
`PROFILES=s3,gitlab` starts both profiles.

```sh
make dev-up PROFILES=gitlab
LAJSTROM_TEST_REGISTRY=localhost:5003 make test-integration   # variant A
LAJSTROM_GITLAB_DYNAMIC_MEDIA_TYPES=true make dev-up PROFILES=gitlab
LAJSTROM_TEST_REGISTRY=localhost:5004 make test-integration   # variant B
make dev-down RESET=1
```

## The ha profile

`make dev-up PROFILES=ha` adds the `ha` stack of [acceptance.md](acceptance.md) §4, which
[test/e2e/ha.sh](../test/e2e/ha.sh) runs against (#226): two replicas of Lajstrom, built from the
working tree, round robin behind nginx, on one Postgres and one distribution. Each request through
the edge goes to the next replica, so a protocol of several requests crosses replicas by
construction.

| Service | Image | Port | Used as |
| --- | --- | --- | --- |
| `ha-nginx` | `nginx:1.30.6-alpine` | `localhost:8090` | the edge, the URL clients use and the `externalURL` of both replicas |
| `lajstrom-a`, `lajstrom-b` | `lajstrom-dev-ha`, built from the working tree | `localhost:8091`, `localhost:8092` | the replicas, published so that a test can choose one past the edge |
| `ha-postgres` | `postgres:17.11-alpine` | not published | the index of both: database, user and password `lajstrom` |
| `ha-distribution` | `registry:3.1.2` | `localhost:5007` | the registry `local` of both; no referrers API |

The profile shares nothing with the three services above: its own database, registry and volumes
(`lajstrom-dev_ha-postgres-data`, `lajstrom-dev_ha-distribution-data`), and a network of its own,
`ha` (`172.31.226.0/24`), with fixed addresses: nginx `.10`, the replicas `.11` and `.12`.

- **The replicas** run the image of [deploy/compose/ha/Dockerfile](../deploy/compose/ha/Dockerfile):
  `go build ./cmd/lajstrom` in `golang:1.27.3-alpine`, on the distroless base of the release image.
  `pull_policy: build` rebuilds it on every `make dev-up`, so the stack always serves the working
  tree: `make dev-up PROFILES=ha` took 29 s the first time, with the images pulled and the Go
  modules still to download, and 7 s after a change of code, which recreates both replicas. Both
  read
  [deploy/compose/ha/lajstrom.yaml](../deploy/compose/ha/lajstrom.yaml): Postgres, the registry,
  `externalURL` `http://localhost:8090` with `insecureHTTP: true` (loopback,
  [configuration.md](configuration.md#server)), `trustedProxies: ["172.31.226.10/32"]`, nginx, text
  logs, no reconcile loop, the repos of `test/e2e/run.sh`, and a fixed master key, the hex of
  `lajstrom-ha-dev-stack-not-secret`. `LAJSTROM_CONFIG` names the file, so `lajstrom admin` in a
  replica works on the same database. Both start at once on an empty database: one applies the
  migrations under the migration lock, and the other logs `repo of the configuration was created
  meanwhile, by another replica`. The health check is `lajstrom healthcheck`.
- **nginx** runs [deploy/nginx/lajstrom.conf](../deploy/nginx/lajstrom.conf) through
  [deploy/compose/ha/nginx.sed](../deploy/compose/ha/nginx.sed), which holds every difference: the
  two replicas as the upstream instead of `lajstrom:8080`, and `listen 80` instead of
  `listen 443 ssl` with its two certificate lines. The rest is the file as an operator deploys it:
  bodies unbuffered both ways, the timeouts, the forwarded headers, the limit on `/v2/token` (20 a
  second per address, a burst of 40), and the connection and bandwidth limits, commented.
  `http2 on` stays, and on plain HTTP nginx answered HTTP/1.1 and HTTP/2 with prior knowledge on the
  same port. nginx starts once both replicas are healthy; its health check asks `/-/readyz`
  through itself.

There is no admin; make one in a replica:

```sh
make dev-up PROFILES=ha
docker compose -f deploy/compose/compose.yaml exec -T lajstrom-a /lajstrom admin user create --name admin --admin --password-stdin <<<"$password"
curl -u "admin:$password" http://localhost:8090/api/v1/whoami
test/e2e/ha.sh                 # testing.md
make dev-down                  # the profile stops with the rest; RESET=1 removes its volumes too
```

The replicas believe the `X-Forwarded-For` of nginx only, so their access log names the client
nginx saw, with nginx as `proxy`, and an address the client forged is not believed. A `PATCH`
sent through the edge with `X-Forwarded-For: 203.0.113.9`, as nginx and then a replica logged it
(under OrbStack every connection from the machine comes from the network's gateway, `.1`):

```text
172.31.226.1 - admin-1794260228 [09/Nov/2026:21:37:08 +0000] "PATCH /api/v1/repos/raw/scratch HTTP/1.1" 200 189 "-" "curl/8.7.1" "203.0.113.9"
time=2026-11-09T21:37:08.350Z level=INFO msg=request method=PATCH path=/api/v1/repos/raw/scratch status=200 bytes=189 duration=3.510191ms requestId=demo-1794260228-2 remoteAddr=172.31.226.1 principal=user:01a1c23e-587e-7ce0-9ed0-76efdf716ffe proxy=172.31.226.10:59564
```

`LAJSTROM_HA_PORT` moves the edge, and the replicas' `externalURL` with it; `LAJSTROM_HA_A_PORT`,
`LAJSTROM_HA_B_PORT` and `LAJSTROM_HA_DISTRIBUTION_PORT` the others. The address of nginx is in
`ha/lajstrom.yaml` and in `compose.yaml`: change both together. `docker image rm lajstrom-dev-ha`
removes the image, which `make dev-down` leaves.

## Commands

```sh
make dev-up            # start, and return once all three are healthy
make dev-ps            # the services, their health and ports
make dev-logs          # follow the logs (Ctrl-C stops following, not the stack)
make dev-down          # stop and remove the containers, of every profile; the volumes stay
make dev-down RESET=1  # the same, and remove the volumes
make dev-up PROFILES=s3  # the three, and distribution on SeaweedFS (below)
make dev-up PROFILES=s3-minio  # the three, and distribution on MinIO, the fallback (below)
make dev-up PROFILES=gitlab  # the three, and the GitLab container registry (below)
make dev-up PROFILES=ha  # the three, and two replicas of Lajstrom behind nginx (below)
make dev-up PROFILES=ecr  # the three, and moto with a distribution for the ECR provider (registries/ecr.md)
```

`make dev-up` returns when the health checks pass (`docker compose up -d --wait`): in 2 to 3 s
once the images are pulled, measured on an Apple silicon Mac with OrbStack; the first run also
pulls the three images, about 650 MB on disk. `docker compose -f deploy/compose/compose.yaml ps`
then shows the three services as `healthy`. The health checks:

- Zot: `zot healthcheck`, which probes `/readyz`. The image has no shell and names its binary
  after the architecture (`zot-linux-arm64-minimal`, `zot-linux-amd64-minimal`); the check
  runs `/proc/1/exe`, which is that binary, since Zot runs as PID 1.
- distribution: `wget -qO- http://127.0.0.1:5000/v2/` (the image is Alpine).
- Postgres: `pg_isready -h 127.0.0.1` with the user and the database. Over TCP, because on the
  first start the image initialises the database behind a server that listens on the socket
  only.

They run every second until the service is healthy, then every 30 s; each check of a registry
is a line in its log.

## Reset

`make dev-down RESET=1` removes the containers and the volumes, the object stores' too; the next
`make dev-up` starts with empty registries and a database without tables, which `make run`
migrates. Only `RESET=1` removes the volumes; `make dev-down` alone never does.

## Settings

Copy [deploy/compose/.env.example](../deploy/compose/.env.example) to `deploy/compose/.env`
(not committed) to change a host port (`LAJSTROM_ZOT_PORT`, `LAJSTROM_DISTRIBUTION_PORT`,
`LAJSTROM_PG_PORT`, for the s3 profile `LAJSTROM_DISTRIBUTION_S3_PORT` and
`LAJSTROM_S3_PORT`, for the s3-minio profile `LAJSTROM_DISTRIBUTION_S3_MINIO_PORT` and
`LAJSTROM_MINIO_PORT`, for the gitlab profile `LAJSTROM_GITLAB_PORT` and
`LAJSTROM_GITLAB_DB_PORT`, and for the ha profile `LAJSTROM_HA_PORT`, `LAJSTROM_HA_A_PORT`,
`LAJSTROM_HA_B_PORT` and `LAJSTROM_HA_DISTRIBUTION_PORT`, and for the ecr profile `LAJSTROM_MOTO_PORT` and
`LAJSTROM_ECR_DISTRIBUTION_PORT`) or the Postgres credentials (`LAJSTROM_PG_DB`, `LAJSTROM_PG_USER`,
`LAJSTROM_PG_PASSWORD`). A variable set in the shell wins over the file. When you change a port,
change what points at it too: the registry URL in your `deploy/compose/lajstrom.yaml`,
`LAJSTROM_TEST_REGISTRY`, the DSN. New Postgres credentials take effect on a fresh volume
(`make dev-down RESET=1 && make dev-up`).

## Lajstrom against the stack

```sh
make dev-up
make run    # SQLite (lajstrom.db in the current directory), Zot as "local", raw/scratch
```

`make run` serves [deploy/compose/lajstrom.example.yaml](../deploy/compose/lajstrom.example.yaml),
or `deploy/compose/lajstrom.yaml` when you have one (an edited copy, not committed). The example
keeps the index in SQLite so that `make run` also works without Docker: a registry that does
not answer is logged at startup and `serve` starts anyway. The Postgres of the stack is one
line away:

```sh
LAJSTROM_DATABASE_DRIVER=postgres LAJSTROM_DATABASE_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' make run
```

apt repos are signed when `serve` has a master key. The example names the variable
`LAJSTROM_MASTER_KEY` (`keys.masterKeyEnv`); export it before `make run`, and keep the value for
the next runs, since the keys made under it only decrypt with it:

```sh
export LAJSTROM_MASTER_KEY=$(openssl rand -hex 32)
make run
```

Without it apt repos are unsigned: `InRelease`, `Release.gpg` and `key.asc` are `404`, and an
apt client needs `deb [trusted=yes] …`. Once a key was made, `serve` does not start without the
master key; `make dev-down RESET=1` does not help there, since the keys are in `lajstrom.db`:
delete that file for a fresh start. [operations.md](operations.md#keys-and-apt-signing) has the
rest: the client line, `key.asc`, rotation.

A repo on distribution: `bin/lajstrom admin repo create raw/second --registry second --config deploy/compose/lajstrom.example.yaml`,
then `test/e2e/raw.sh http://localhost:8080 second`.

## Pushing by hand

[oras](https://oras.land) needs `--plain-http`; `crane` talks plain HTTP to `localhost` on its
own.

```sh
oras push --plain-http localhost:5000/t:1 README.md          # Zot
oras push --plain-http localhost:5001/t:1 README.md          # distribution
oras pull --plain-http -o out localhost:5000/t:1           # writes out/README.md

crane append --oci-empty-base -f <(tar -cf - README.md) -t localhost:5000/t:1
crane manifest localhost:5000/t:1
crane ls localhost:5001/t
crane catalog localhost:5000
```

Without oras installed, its image works on OrbStack and Linux, where `--network host` reaches
the published ports:

```sh
docker run --rm --network host -v "$PWD:/w" -w /w ghcr.io/oras-project/oras:v1.3.6 push --plain-http localhost:5000/t:1 README.md
```

## Tests

```sh
make test                                              # unit tests, no stack needed
LAJSTROM_TEST_REGISTRY=localhost:5000 make test-integration   # against Zot
LAJSTROM_TEST_REGISTRY=localhost:5001 make test-integration   # against distribution
LAJSTROM_TEST_REGISTRY=localhost:5000 LAJSTROM_TEST_REGISTRY_S3=localhost:5002 make test-integration   # and the 307 of distribution on S3 (PROFILES=s3)
LAJSTROM_TEST_REGISTRY=localhost:5000 LAJSTROM_TEST_REGISTRY_S3=localhost:5006 make test-integration   # the same on MinIO (PROFILES=s3-minio)
LAJSTROM_TEST_REGISTRY=localhost:5000 LAJSTROM_TEST_ECR_ENDPOINT=http://localhost:5010 LAJSTROM_TEST_ECR_REGISTRY=localhost:5011 make test-integration   # and the ECR provider against moto (PROFILES=ecr)
LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' go test -race ./internal/index/...
make e2e                                               # the end-to-end scenarios, on a server of their own
test/e2e/raw.sh http://localhost:8080 scratch          # one scenario, with make run in another terminal
```

The integration tests work in repositories with a random name and the Postgres tests in a schema
of their own, so they can run again and side by side without a reset. [testing.md](testing.md)
describes every level and what it needs.

CI runs the same three images, on the same ports, as service containers of the `integration`
job ([CONTRIBUTING.md](../CONTRIBUTING.md#ci-overview)); Zot there runs on the configuration built
into its image, since a service container cannot mount `config.json` from the checkout. CI does
not run the s3 and s3-minio profiles (the tests that need them skip without
`LAJSTROM_TEST_REGISTRY_S3`), nor the gitlab, ha and ecr profiles (the ECR test skips without
`LAJSTROM_TEST_ECR_ENDPOINT` and `LAJSTROM_TEST_ECR_REGISTRY`).

## Docker on macOS and Linux

- **OrbStack**: the daemon may be asleep; `orb start` wakes it (`docker info` fails until then).
- **Docker Desktop**: works as is. On macOS the AirPlay Receiver also listens on port 5000;
  when `make dev-up` reports the port taken, turn it off (System Settings, General, AirDrop &
  Handoff) or set `LAJSTROM_ZOT_PORT` in `.env`.
- **Linux**: Docker Engine with the Compose plugin. The health checks use `start_interval`,
  which needs Docker Engine 25 or later.

Every image tag is a multi-architecture index that serves `linux/amd64` and `linux/arm64`, so
Intel and Apple silicon machines and the CI runners get the same versions. The digest of each
index is in a comment in `compose.yaml`; when you bump a tag, update the digest and the same
service of the `integration` job in `.github/workflows/ci.yml` (Zot also in the `e2e` job),
re-run the integration tests against the new registry and update
[registries/](registries/README.md).
