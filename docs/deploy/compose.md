# Deploy with Docker Compose

One host, three containers: Lajstrom, Postgres as its index database, and a registry that holds
every byte. [deploy/compose-production/](../../deploy/compose-production/) has the two files,
`compose.yaml` and `lajstrom.yaml`. It is not the [dev stack](../dev-stack.md): every service
has a password, only Lajstrom publishes a port, on loopback, for the TLS edge on the same host,
and Lajstrom runs as a user without privileges on a read-only root filesystem.

| Service | Image | Published | Keeps data in |
| --- | --- | --- | --- |
| `lajstrom` | `ghcr.io/lajstrom/lajstrom`, the release image (#57), user 65532, read-only root, all capabilities dropped | `127.0.0.1:8080` | nothing: `/tmp`, where a proxy fetch is spooled for one request, is a tmpfs |
| `postgres` | `postgres:17.11-alpine` | no | the volume `postgres-data`: the index, the control plane with it ([index-schema.md](../spec/index-schema.md)) |
| `registry` | `registry:3.1.2`, Basic auth with one user | no | the volume `registry-data`: every artifact |

A registry you already run (Zot, Harbor, ECR, the GitLab registry): delete the `registry` service,
its volume `registry-data`, its secret `registry-htpasswd` and the `registry:` entry under
`depends_on:` of `lajstrom` (without that, `docker compose` stops at `service "lajstrom" depends
on undefined service "registry"`). Then point `registries[0]` of `lajstrom.yaml` at it, with the
configuration its page in [registries/](../registries/README.md) gives; for a registry that
takes no password, delete its `auth:` too.

## 1. Secrets

Five files in `secrets/` (git-ignored), made once:

```sh
cd deploy/compose-production
umask 077
mkdir -p secrets
pg=$(openssl rand -hex 24)
reg=$(openssl rand -hex 24)
openssl rand -hex 32 > secrets/master-key
openssl rand -hex 32 > secrets/bootstrap-token   # the one-time token of the first admin (section 4)
printf '%s' "$pg" > secrets/postgres-password
docker run --rm --entrypoint htpasswd httpd:2.4.66-alpine -Bbn lajstrom "$reg" > secrets/registry.htpasswd   # bcrypt
cat > secrets/lajstrom.env <<EOF
LAJSTROM_DATABASE_DSN=postgres://lajstrom:$pg@postgres:5432/lajstrom?sslmode=disable
LAJSTROM_REGISTRY_PASSWORD=$reg
EOF
unset pg reg
sudo chown 65532:65532 secrets/master-key secrets/bootstrap-token   # on Linux: the image's user reads them (below)
```

| File | Read by | What |
| --- | --- | --- |
| `secrets/master-key` | `lajstrom`, as the Compose secret `lajstrom-master-key`, the file `/run/secrets/lajstrom-master-key` of `keys.masterKeyFile` | the [master key](../operations.md#the-master-key) |
| `secrets/bootstrap-token` | `lajstrom`, as the Compose secret `lajstrom-bootstrap-token`, the file `/run/secrets/lajstrom-bootstrap-token` of `auth.bootstrap.tokenFile` | the one-time token that makes the first admin ([operations.md](../operations.md#the-first-admin)); removed once the admin exists |
| `secrets/lajstrom.env` | Compose, into the environment of `lajstrom` | the DSN with the database password, the registry password |
| `secrets/postgres-password` | `postgres`, as the Compose secret `POSTGRES_PASSWORD_FILE` names | the same database password |
| `secrets/registry.htpasswd` | `registry`, as a Compose secret | the bcrypt line of the user `lajstrom` |

The master key is a file in the container and nowhere else; once the stack is up (section 3):

```sh
docker compose logs --no-log-prefix lajstrom | grep 'master key'
docker inspect "$(docker compose ps -q lajstrom)" --format '{{range .Mounts}}{{.Destination}} {{end}}'
docker inspect "$(docker compose ps -q lajstrom)" --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -c MASTER
```

```text
{"time":"2026-11-09T18:18:43.057765241Z","level":"INFO","msg":"apt repos are signed with keys under the master key","keys.masterKeyFile":"/run/secrets/lajstrom-master-key","keys.algorithm":"ed25519","keys.retireGrace":"720h0m0s"}
/etc/lajstrom/lajstrom.yaml /run/secrets/lajstrom-master-key
0
```

Compose bind-mounts a file secret with the owner and mode of the host's file (it ignores `uid`,
`gid` and `mode` for one, and says so), so the file must be readable by the image's user
65532. On Linux that takes the `chown` above, after which `sudo cat secrets/master-key` reads it
back. In Docker 29 on a Linux kernel (`docker:29-dind`), the same file owned by uid 1000 with
`0600` was `keys.masterKeyFile: open /run/secrets/lajstrom-master-key: permission denied`, and
owned by 65532 it was read. On macOS, OrbStack shows a shared file as owned by the container's
user, so the run of this page had no `chown`. Lajstrom warns at start when the file is open to
the group or others (`the master key file is open to more than its owner: make it 0400 or
0600`); `0600` or `0400` is quiet. The DSN and the registry password have no file option and
stay in the environment, where `docker inspect` shows them.

**Copy the master key somewhere apart from the host and from the database backups now**:
without it the signing keys in the database are ciphertext
([operations.md](../operations.md#the-master-key)).

## 2. Configuration

`lajstrom.yaml` is short, every key is in [configuration.md](../configuration.md):

- `server.externalURL`: the public URL of the TLS edge, `https://art.example.com` in the file.
  The realm of the bearer challenge of `/v2` is built from it, so `docker login` and `oras login`
  fail when it is wrong. To try the stack without an edge, on the loopback port, set
  `externalURL: "http://localhost:8080"` and add `insecureHTTP: true` under `server:`; an `http`
  URL without it stops the start (`server.externalURL: must be https`).
- `server.metricsListen: "0.0.0.0:9464"`: Prometheus on the Compose network scrapes it; the port
  is not published.
- `database.driver: postgres`; the DSN comes from `LAJSTROM_DATABASE_DSN`, since it carries the
  password.
- `keys.masterKeyFile: /run/secrets/lajstrom-master-key`: the master key, from the Compose
  secret.
- `registries[0]`: the registry service, under the prefix `lajstrom/`, with the user `lajstrom`
  and the password from `LAJSTROM_REGISTRY_PASSWORD`.
- `repos`: `raw/assets`, created at the first start; the database has every repo created later.

The image is `ghcr.io/lajstrom/lajstrom:v0.1.0` unless `LAJSTROM_IMAGE` says otherwise; pin a
digest there in production, `ghcr.io/lajstrom/lajstrom:vX.Y.Z@sha256:…`. `LAJSTROM_PORT`
changes the loopback port (default 8080). Both can go into a `.env` beside `compose.yaml`
(git-ignored, like `secrets/`).

`lajstrom.yaml` is bind-mounted as a file. After a change, `docker compose up -d
--force-recreate lajstrom` reads it again; a restart keeps seeing the old file when the editor
replaced it rather than wrote into it (`sed -i` and many editors do).

## 3. Start

```sh
docker compose up -d --wait
docker compose ps --format 'table {{.Service}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'
```

The run below, on 2026-11-11, used an image built from main with the `Dockerfile`
([release.md](../release.md) shows the build by hand), `LAJSTROM_IMAGE=lajstrom:walkthrough`,
`LAJSTROM_PORT=18150`, and `externalURL: "http://localhost:18150"` with `insecureHTTP: true`,
since no edge was in front. `up --wait` returned after 37 seconds, once Lajstrom's own health
check passed; each health check runs first 10 seconds after its container starts:

```text
SERVICE    IMAGE                   STATUS                    PORTS
lajstrom   lajstrom:walkthrough    Up 6 seconds (healthy)    127.0.0.1:18150->8080/tcp
postgres   postgres:17.11-alpine   Up 23 seconds (healthy)   5432/tcp
registry   registry:3.1.2          Up 23 seconds (healthy)   5000/tcp
```

Lajstrom starts once Postgres and the registry are healthy, applies the migrations, creates the
repos of the file and the bearer-token key, asks the registry what it supports, and listens
(`docker compose logs lajstrom`, JSON, a selection, without the times):

```text
{"level":"INFO","msg":"index migrations applied under the migration lock","database.driver":"postgres","applied":[1,2,3,4,5,6,7,8,9,10,11,12,13]}
{"level":"INFO","msg":"repo of the configuration created","repo":"raw/assets","registry":"local"}
{"level":"INFO","msg":"token signing key made","fingerprint":"816b40ab693fd79a6fa6538130a7e8d6eca385631f0ec9c8a878dfa2152520db","algorithm":"ES256"}
{"level":"INFO","msg":"registry capabilities","registry":"local","vendor":"unknown","createOnPush":true,"referrers":false,"catalog":true,"deleteManifest":true,"immutableTags":false,"blobRedirect":true,"maxBlobSize":0}
{"level":"INFO","msg":"listening","addr":"[::]:8080","externalURL":"http://localhost:18150","tls":false}
{"level":"INFO","msg":"metrics listening","addr":"[::]:9464","pprof":false}
```

### The health check

The image has no shell and no HTTP client, so the `healthcheck:` of the `lajstrom` service runs
the binary itself: `lajstrom healthcheck --url http://127.0.0.1:8080` GETs `/-/readyz` within
3 seconds and exits 0 on `200`, 1 otherwise, with one line. It reads no configuration and no
secret ([operations.md](../operations.md#readiness)). By hand:

```sh
docker compose exec -T lajstrom /lajstrom healthcheck --url http://127.0.0.1:8080
docker compose exec -T lajstrom /lajstrom healthcheck --url http://127.0.0.1:8080 --live   # /-/healthz
```

```text
http://127.0.0.1:8080/-/readyz: 200 ok
http://127.0.0.1:8080/-/healthz: 200 ok
```

`/-/readyz` is `503` while the database does not answer or the registry has not answered for a
minute. With the registry stopped (`docker compose stop registry` at 18:25:47), the service was
`unhealthy` by 18:27:09: a minute for `/-/readyz`, then three failed checks 10 seconds apart.
Docker does not restart an unhealthy container; the edge that checks `/-/readyz` stops sending
it requests. Its health log (`docker inspect --format '{{range .State.Health.Log}}…'`):

```text
1 lajstrom: unhealthy: http://127.0.0.1:8080/-/readyz: 503 a registry has not answered for a minute
```

After `docker compose start registry` it was `healthy` again within 20 seconds.

## 4. The first admin and more repos

No admin exists yet, so `serve` made the token of `secrets/bootstrap-token` valid at start
([operations.md](../operations.md#the-first-admin)); it logged that, never the token.
`lajstrom admin bootstrap` exchanges it for the admin: in the container it reads the token from
the secret, asks the server of its configuration and prompts for the password. The other `admin`
commands run in the container too, against the same database, with the same environment; a
function saves typing:

```sh
docker compose exec lajstrom /lajstrom --config /etc/lajstrom/lajstrom.yaml admin bootstrap --name admin \
  --token-file /run/secrets/lajstrom-bootstrap-token
lj() { docker compose exec -T lajstrom /lajstrom --config /etc/lajstrom/lajstrom.yaml "$@"; }
lj admin repo create oci/images --registry local
```

```text
{"uid":"01a1c155-57ca-7c43-a38e-9595784a4098","name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-09T17:22:38.154331Z"}
{"format":"oci","name":"images","registry":"local","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"2026-11-09T17:22:38.363197Z","updatedAt":"2026-11-09T17:22:38.363197Z"}
```

The token is dead now. Remove it: delete `secrets/bootstrap-token`, the two
`lajstrom-bootstrap-token` entries of `compose.yaml` and `auth.bootstrap` of `lajstrom.yaml`, and
`docker compose up -d`; until then every start warns that the token is set but no longer valid.
`lj admin user create --name admin --admin --password-stdin` makes the admin without a token,
straight in the database; it is also how a lost admin password is recovered
([operations.md](../operations.md#a-lost-password)). [auth.md](../auth.md) has the rest: users,
bindings, tokens for CI.

## 5. Push and pull

Through the edge, with `https://art.example.com`; here to the loopback port, with the default
`LAJSTROM_PORT` and `externalURL: "http://localhost:8080"` (section 2), hence `--plain-http`.
The run had 18150 in both places. `pw` holds the admin's password:

```sh
read -rs pw
printf 'hello compose\n' > hello.txt
curl -s -u "admin:$pw" -o /dev/null -w '%{http_code}\n' -T hello.txt http://localhost:8080/raw/assets/docs/hello.txt   # 201
curl -s -u "admin:$pw" http://localhost:8080/raw/assets/docs/hello.txt                                              # hello compose

printf '%s\n' "$pw" | oras login --plain-http -u admin --password-stdin localhost:8080   # Login Succeeded
oras push --plain-http --artifact-type application/vnd.example.notes localhost:8080/oci/images/hello:1 hello.txt
oras pull --plain-http -o out localhost:8080/oci/images/hello:1                                          # out/hello.txt
```

```text
Pushed [registry] localhost:8080/oci/images/hello:1
ArtifactType: application/vnd.example.notes
Digest: sha256:1dfc701d5c1ef2260ab925b42526f16f4b400e86f400769398a73cac0e6d1cd6
…
Pulled [registry] localhost:8080/oci/images/hello:1
Digest: sha256:1dfc701d5c1ef2260ab925b42526f16f4b400e86f400769398a73cac0e6d1cd6
```

Both land in the registry under the prefix, and nowhere else:

```sh
docker compose exec -T registry sh -c 'ls /var/lib/registry/docker/registry/v2/repositories/lajstrom/*/*'
```

```text
/var/lib/registry/docker/registry/v2/repositories/lajstrom/oci/images:
hello

/var/lib/registry/docker/registry/v2/repositories/lajstrom/raw/assets:
docs
```

## 6. Back up

What must be backed up is the database and the master key; the registry has its own backup, or
none if its storage is replicated ([operations.md](../operations.md#disaster-recovery)). A
dump of the running database, in Postgres's custom format; it holds the password hashes, the
token hashes and the encrypted keys, so only its owner reads it (`*.dump` is git-ignored here):

```sh
umask 077
docker compose exec -T postgres pg_dump -U lajstrom -d lajstrom --format=custom > "lajstrom-$(date -u +%Y%m%dT%H%M%SZ).dump"
```

Restoring it into a new, empty database, then `migrate` and `reindex` before Lajstrom starts,
was rehearsed on this stack. The volume is `<project>_postgres-data`: `lajstrom_postgres-data`
with the name of `compose.yaml`; the run used the project name `walkthrough-compose`, so its
volume was `walkthrough-compose_postgres-data`. The `docker volume rm` deletes the database:
read the dump first. `reindex` reports the `oci` repo's manifests as foreign: they are listed,
never indexed ([operations.md](../operations.md#what-a-run-does)).

```sh
docker compose exec -T postgres pg_restore --list < lajstrom-….dump | grep -c 'TABLE DATA'   # 21 in the run
docker compose stop lajstrom
docker compose rm -f -s postgres && docker volume rm lajstrom_postgres-data
docker compose up -d --wait postgres
docker compose exec -T postgres pg_restore -U lajstrom -d lajstrom --no-owner --exit-on-error < lajstrom-….dump
docker compose run --rm -T lajstrom migrate --config /etc/lajstrom/lajstrom.yaml
docker compose run --rm -T lajstrom reindex --config /etc/lajstrom/lajstrom.yaml --report
docker compose up -d --wait lajstrom
```

## 7. Upgrade and remove

A new release: back up the database (section 6), set `LAJSTROM_IMAGE` to it and
`docker compose up -d`; `serve` applies the migrations of the new version on start. A contract
migration that requires a release you skipped is refused; [operations.md](../operations.md#one-node)
says what to do. This has not been tried: there is no release yet.
`docker compose down` stops and removes the containers and keeps the volumes;
`docker compose down -v` deletes the index and every artifact.

## What was not verified

- An edge in front, TLS and `externalURL` with `https`: #129 describes the edge.
- A host other than macOS with OrbStack 2.2.5 (Docker 29.8.2, Compose 5.5.0): on Linux the
  secret files are bind-mounted with mode 0600 and read by root inside `postgres` and
  `registry`, which Docker's default allows; rootless Docker or user namespaces were not tried.
- An external registry instead of the `registry` service with a password: the run tried the
  steps at the top of the page only with the dev stack's Zot, which takes none
  (`url: "http://host.docker.internal:5000"`, no `auth:`). The registry pages list what was
  tested against each one.
