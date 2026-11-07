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

## Commands

```sh
make dev-up            # start, and return once all three are healthy
make dev-ps            # the services, their health and ports
make dev-logs          # follow the logs (Ctrl-C stops following, not the stack)
make dev-down          # stop and remove the containers; the volumes stay
make dev-down RESET=1  # the same, and remove the volumes
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

`make dev-down RESET=1` removes the containers and the three volumes; the next `make dev-up`
starts with empty registries and a database without tables, which `make run` migrates. Only
`RESET=1` removes the volumes; `make dev-down` alone never does.

## Settings

Copy [deploy/compose/.env.example](../deploy/compose/.env.example) to `deploy/compose/.env`
(not committed) to change a host port (`LAJSTROM_ZOT_PORT`, `LAJSTROM_DISTRIBUTION_PORT`,
`LAJSTROM_PG_PORT`) or the Postgres credentials (`LAJSTROM_PG_DB`, `LAJSTROM_PG_USER`,
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
LAJSTROM_TEST_POSTGRES_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable' go test -race ./internal/index/...
test/e2e/raw.sh http://localhost:8080 scratch          # with make run in another terminal
```

The integration tests work in repositories with a random name and the Postgres tests in a schema
of their own, so they can run again and side by side without a reset.

CI runs the same three images, on the same ports, as service containers of the `integration`
job ([CONTRIBUTING.md](../CONTRIBUTING.md#ci-overview)); Zot there runs on the configuration built
into its image, since a service container cannot mount `config.json` from the checkout.

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
service of the `integration` job in `.github/workflows/ci.yml`, re-run the integration tests
against the new registry and update [registries/](registries/README.md).
