# Configuration

Lajstrom reads one YAML file. `serve`, `migrate`, `reindex` and the `admin` commands (`repo`,
`user`, `binding`, `token`, `key`) load and validate it before they do anything else; `version`
and `admin deb metadata` do not need it.

```sh
lajstrom serve --config /etc/lajstrom/lajstrom.yaml
LAJSTROM_CONFIG=/etc/lajstrom/lajstrom.yaml lajstrom serve
```

`--config` defaults to `lajstrom.yaml` in the current directory, or to `$LAJSTROM_CONFIG` when
that is set. A commented example that works with the [dev stack](dev-stack.md) is
[deploy/compose/lajstrom.example.yaml](../deploy/compose/lajstrom.example.yaml): SQLite, Zot as
the registry `local`, distribution as `second`; `make run` uses it when
`deploy/compose/lajstrom.yaml` does not exist. With `LAJSTROM_DATABASE_DRIVER=postgres` and
`LAJSTROM_DATABASE_DSN='postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable'`
it uses the Postgres of the stack instead.

## How the configuration is built

Later steps win:

1. **Defaults** (the Default column below).
2. **The file.** Keys are camelCase. An unknown key is an error that names the key and the
   line, so a typo never passes silently. An empty file is allowed and leaves the defaults.
3. **Environment overrides** for the scalar keys that have a variable in the Env column. An
   empty variable counts as unset. Lists (`registries`, `repos`) cannot be set from the
   environment.
4. **Secrets**: each `registries[].auth.passwordEnv` is read from the environment. The master
   key of `keys.masterKeyEnv` or `keys.masterKeyFile` is not: `serve` and `admin key` read it
   when they start (see [keys](#keys)).
5. **Validation.** Every problem is reported at once, one line each, starting with the key:

   ```text
   lajstrom: config: lajstrom.yaml: invalid:
   server.listen: must not be empty
   repos[0].registry: "zot" is not a configured registry
   ```

The `--log-level` and `--log-format` flags override `log.level` and `log.format` for one run.

## Secrets

Secrets never go into the file. Keys ending in `Env` name the environment variable that holds
the secret: `registries[].auth.passwordEnv` and `keys.masterKeyEnv`. The master key can also come
from a file of its own, `keys.masterKeyFile`, for a secret mounted as a file. A `password` key in
the file is rejected. Lajstrom never logs a secret or puts one in an error message.

## What `serve` needs

Only `server.listen`. Everything else has a default or is optional:

```yaml
server:
  listen: ":8080"
```

`serve` opens the index database (`database`, by default the SQLite file `lajstrom.db` in the
current directory), applies its pending migrations and creates the [repos](#repos) of the file
that the database does not have, all before it listens; `/-/readyz` answers 503 while the
database does not. It also contacts the registries: at startup it asks each one
what it supports (see [capabilities](#registry-capabilities)) and logs the answer at info. A
registry that does not answer within 10 s is logged at error and `serve` starts anyway, so that
one backend down does not keep a replica from starting.

An upload is spooled to a temporary file for the length of its request, then pushed to the
registry and removed. The file goes to `$TMPDIR` when it is set, `/tmp` otherwise; there is no
configuration key for it. Give that directory room for the largest uploads one replica takes at
once (a tmpfs in [Compose](deploy/compose.md), an `emptyDir` on [Kubernetes](deploy/kubernetes.md)).

## Reference

### server

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `server.listen` | string, `host:port` | none, required | `LAJSTROM_SERVER_LISTEN` | TCP address to listen on. `":8080"` listens on every interface, `"127.0.0.1:8080"` on loopback only. |
| `server.externalURL` | `https://<host>[:port]` | empty | `LAJSTROM_SERVER_EXTERNAL_URL` | Base of every absolute URL Lajstrom renders: token realm, redirects, simple index links ([http-routing.md](spec/http-routing.md) §1). No path, query or fragment: Lajstrom owns the whole host. `http://` only for the dev stack on loopback (the `server.insecureHTTP` switch and TLS keys come with #50). Example: `https://art.example.com`. |
| `server.shutdownTimeout` | duration (`30s`, `2m`) | `30s` | none | How long in-flight requests get to finish after SIGINT or SIGTERM before their connections are closed. Must be positive. |
| `server.maxManifestSize` | bytes | `4194304` (4 MiB) | none | Largest manifest a `PUT /v2/<name>/manifests/<ref>` may carry, on every repo; a larger one is `413` ([http-routing.md](spec/http-routing.md) §2). The OCI facade reads a manifest into memory before it checks it, so this bounds the memory of one push; the distribution spec asks a registry to take at least 4 MiB. Must not be negative; `0` means the default. |
| `server.metricsListen` | string, `host:port` | `127.0.0.1:9464` | `LAJSTROM_SERVER_METRICS_LISTEN` | The metrics listener: `GET /metrics` for Prometheus, and `/debug/pprof/` with `server.pprof` ([operations.md](operations.md#metrics)). It is a listener of its own and never the public host: the main listener answers `404` on `/-/metrics` and `/metrics`, and the edge must not route to it. In a container set `0.0.0.0:9464` (or the pod IP) so that Prometheus reaches it, and keep the port off the ingress. Empty turns it off; it must not be `server.listen`. |
| `server.pprof` | bool | `false` | none | Serves Go's `/debug/pprof/` on the metrics listener, for a profile taken on purpose; it costs nothing until asked. |

### log

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `log.level` | `debug`, `info`, `warn`, `error` | `info` | `LAJSTROM_LOG_LEVEL` | Lowest level that is logged. Flag: `--log-level`. |
| `log.format` | `text`, `json` | `text` | `LAJSTROM_LOG_FORMAT` | `text` for terminals, `json` for log collectors. Flag: `--log-format`. |

Logs go to standard error. Every request is logged once with method, path (never the query string),
status, bytes, duration, request id and remote address.

### database

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `database.driver` | `sqlite`, `postgres` | `sqlite` | `LAJSTROM_DATABASE_DRIVER` | Engine of the index database ([index-schema.md](spec/index-schema.md)). |
| `database.dsn` | string | `file:lajstrom.db` | `LAJSTROM_DATABASE_DSN` | Data source name. SQLite: `file:/var/lib/lajstrom/lajstrom.db`. Postgres: `postgres://lajstrom@db:5432/lajstrom?sslmode=require`. A DSN with a password belongs in `LAJSTROM_DATABASE_DSN`, not in the file. The forms are below. |

**SQLite DSN.** A file name or a `file:` URI, e.g. `file:/var/lib/lajstrom/lajstrom.db`. Lajstrom
adds the connection parameters of `modernc.org/sqlite` itself: `_busy_timeout=5000` (a writer
waits up to 5 s for the lock), `_journal_mode=WAL` and `_synchronous=NORMAL` unless the DSN sets
them, and always `_foreign_keys=1` and `_txlock=immediate` (deletes cascade; a transaction takes
the write lock when it begins). Tests use an in-memory database,
`file:<name>?mode=memory&cache=shared`; it lives in one connection and is gone when the process
ends.

**Postgres DSN.** A URL, `postgres://user:password@host:5432/db?sslmode=require`, or a
keyword/value string, `host=db user=lajstrom dbname=lajstrom sslmode=require`, as pgx reads it
(it also honours the `PG*` environment variables). `pool_max_conns=N` sizes the connection
pool; the default is the number of CPUs, at least 4. Text columns use the `C` collation, so the
database's own collation does not matter.

`lajstrom migrate` applies the migrations the database does not have yet and prints them, and
`lajstrom serve` does the same on start; both take a lock, so replicas starting together are
safe. Lajstrom logs `database.driver`, never the DSN, and keeps the password out of its errors.

### registries

A list; each entry is one backend OCI registry. Repos refer to it by name.

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `registries[].name` | name | none, required | none | Unique; lowercase letters and digits separated by single `.`, `_` or `-`, at most 64 characters. Example: `local`, `ecr-prod`. |
| `registries[].type` | `generic`, `ecr` | `generic` | none | `generic` covers distribution, Zot, the GitLab container registry, Harbor and GHCR ([registries/](registries/README.md)); `ecr` is Amazon ECR (issue #36). |
| `registries[].url` | absolute `http(s)` URL | none, required | none | Base URL of the registry, e.g. `http://localhost:5000`. Must not contain credentials. |
| `registries[].prefix` | string | empty | none | Instance prefix of every OCI repository Lajstrom creates, e.g. `art/` gives `art/raw/scratch/…`. Must not start with `/` and must end with `/`. Clients never see it. |
| `registries[].auth.username` | string | empty | none | User for the registry. Set together with `passwordEnv`; leave both empty for anonymous access. |
| `registries[].auth.passwordEnv` | environment variable name | empty | none | Variable that holds the password. Startup fails when it is set here but the variable is unset or empty. Example: `ZOT_PASSWORD`. |
| `registries[].timeout` | duration (`30s`, `1m`) | `30s` | none | Bounds one request to the registry up to its response headers: connecting and the TLS handshake (each at most 10 s) and the wait for the headers. A blob upload or download streams for as long as it takes. Must not be negative; `0s` means the default. |
| `registries[].retries` | integer | `3` | none | Retries of a failed request: 408, 429, 5xx, timeouts and connection resets, with exponential backoff from 250 ms (or the `Retry-After` of a 429), at most 3 s. Only requests whose body can be sent again are retried; a streamed blob upload is sent once. `0` means the default; a negative value disables retries. |
| `registries[].capabilities.createOnPush` | bool | probed | none | Pushing to a new repository creates it. |
| `registries[].capabilities.referrers` | bool | probed | none | The registry serves the OCI referrers API. `false` makes Lajstrom use the `sha256-<digest>` tag schema without asking. |
| `registries[].capabilities.catalog` | bool | probed | none | The registry lists repositories (`/v2/_catalog`). `false` turns repository listing off without asking, for registries whose catalog is missing, slow or wrong; Lajstrom then uses its own list of repositories. |
| `registries[].capabilities.deleteManifest` | bool | probed | none | Manifests can be deleted. |
| `registries[].capabilities.immutableTags` | bool | probed | none | The registry itself refuses to re-point a tag. |
| `registries[].capabilities.blobRedirect` | bool | probed | none | A blob download may be answered with a `307` to the URL the registry redirects it to (a presigned URL of its object storage), so that the bytes bypass Lajstrom. Each download is one request to the registry, whose answer decides: a redirect to another origin goes to the client, a `200` or `206` streams. `false` makes Lajstrom follow every redirect of the registry and stream the bytes itself: a client never gets a URL of the registry, for storage that clients cannot reach. Per repo, `policy.blobRedirect: "never"` does the same ([api.md](api.md)). |
| `registries[].capabilities.maxBlobSize` | bytes | `0` | none | Largest blob the registry accepts; `0` means unknown. Must not be negative. |

#### Registry capabilities

At startup Lajstrom asks each registry what it supports, once, and keeps the answer: `GET /v2/`
(the registry answers, and accepts the credentials), `GET /v2/_catalog?n=1` (a catalog: 200 yes;
401, 403, 404, 405 no) and the referrers of a digest no manifest has in the repository
`<prefix>lajstrom-probe` (an OCI image index: the referrers API; a 404: the tag schema). The
`generic` type assumes create-on-push, manifest deletes and blob redirects, and no immutable
tags; a registry that answers a manifest delete with 405 turns `deleteManifest` off from then on.
Blob redirects need no probe: a download is one `GET` whose first answer is used as it is, a
redirect or the bytes, so a registry on filesystem storage costs nothing for the assumption.

What a probe cannot measure comes from a table of vendors, which Lajstrom tells apart by what a
registry says about itself: a `Gitlab-Container-Registry-Version` header on `GET /v2/` is the
GitLab container registry, the host `ghcr.io` is GHCR, and a Bearer challenge with
`service="harbor-registry"` on `GET /v2/` is Harbor. GHCR gets `catalog: false` (its
`_catalog` answers 200 with every repository of GHCR, which the probe cannot tell from a usable
one) and `deleteManifest: false` (every delete through `/v2` is 405), without asking; GitLab and
Harbor need nothing, the probe is right for them. A GitLab registry that answers
`Gitlab-Container-Registry-Database-Enabled: true` gets a warning at startup: with its database
it refuses a manifest whose `artifactType` it does not know, every one Lajstrom writes, unless
`REGISTRY_FF_DYNAMIC_MEDIA_TYPES` is on ([gitlab.md](registries/gitlab.md#media-types)). The
startup line names the vendor, `vendor=gitlab`, `ghcr`, `harbor` or `unknown`, beside the
capabilities. A registry the table does not know gets the probe alone.

Each key under `capabilities` replaces what was found and what the vendor table sets, for
registries that answer wrongly; a key that is set is not probed. There is no `catalog` key
outside `capabilities`. What was tested against which registry, and the configuration each one
needs, is in [registries/](registries/README.md).

### ingest

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `ingest.claimTTL` | duration (`10m`, `1h`) | `10m` | none | How long a publish's claim on a tag holds off a publish of other bytes to the same tag. Every publish claims its tag in the index before it writes the manifest to the registry and releases the claim once the index rows are written, so that two replicas cannot both write one tag ([index-schema.md](spec/index-schema.md), Coordination). A claim older than this, left by a replica that died mid-publish, is stale: the next publish of the tag takes it over, and `reindex` and the reconcile loop delete it. It must outlast one manifest `PUT` and one index transaction; blob uploads come before the claim and do not count. Must not be negative; `0s` means the default. |

### reconcile

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `reconcile.interval` | duration (`1h`, `30m`) | `1h` | none | How often each replica runs a reindex of every repo with prune, so that writes that went past Lajstrom (a push straight to the backend, a delete in the registry's console) reach the index ([operations.md](operations.md#reconcile)). The first run comes after a random part of the interval; a run that overlaps a run elsewhere waits for the next tick. `0s` turns the loop off. Must not be negative. |

The `ingest` and `reconcile` sections are additions to the shape in [layout.md](spec/layout.md)
§6.

### auth

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `auth.anonymousRead` | bool | `false` | none | Server-wide switch for anonymous read. When `false`, no repo is readable without credentials, whatever `repos[].anonymousRead` says. Enforced by RBAC (issue #45, [rbac.md](spec/rbac.md)). |

There is no key file for the bearer tokens of `/v2`: their signing key lives in the database,
encrypted with the master key ([ADR 0010](adr/0010-bearer-token-signing-key.md),
[keys](#keys)). A file that still sets `auth.tokenSigningKeyFile` is refused:
`auth.tokenSigningKeyFile is gone: the key lives in the database, ADR 0010`.

### keys

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `keys.masterKeyEnv` | environment variable name | empty | none | Variable that holds the master key, which encrypts the signing keys at rest: 64 hexadecimal characters (32 bytes), e.g. from `openssl rand -hex 32`. It has two readers: the apt signing keys of the repos and the key that signs the bearer tokens of `/v2`. Example: `LAJSTROM_MASTER_KEY`. See below for what happens without it. |
| `keys.masterKeyFile` | file path | empty | `LAJSTROM_MASTER_KEY_FILE` | File that holds the master key instead of the variable, in the same encoding; spaces and line breaks around the key are ignored, so `openssl rand -hex 32 > file` works. For a Compose secret or a Kubernetes Secret mounted as a file, e.g. `/run/secrets/lajstrom-master-key`. Setting it while the variable of `keys.masterKeyEnv` is set (and not empty) is a configuration error of every command. The file should be `0400` or `0600`: a mode that lets the group or others read or write it is used, with a warning at start. An addition to the shape in [layout.md](spec/layout.md) §6. |
| `keys.algorithm` | `ed25519` or `rsa4096` | `ed25519` | none | Algorithm of the signing keys made from now on (each apt repo gets one on its first signed request, or with `lajstrom admin key`). `ed25519` is an OpenPGP EdDSA key, which apt verifies since 2.0 (Debian 11, Ubuntu 20.04); `rsa4096` is for older clients. Keys made before keep their algorithm; rotate a repo's key to change it. |
| `keys.retireGrace` | duration (`720h`, `168h`) | `720h` | none | How long a key retired by a rotation keeps signing `InRelease` and `Release.gpg` beside the new key and stays in the repo's `key.asc`. Clients that pinned the old `key.asc` keep verifying until it ends; fetch `key.asc` again on every client before then ([operations.md](operations.md#rotation)). `0s` drops a retired key at once, for a key that must not be trusted any more. Must not be negative. It does not apply to the bearer-token key, whose retired key verifies for the token lifetime and the clock skew, 21 minutes. |
| `keys.tokenAlgorithm` | `ES256` or `EdDSA` | `ES256` | none | Algorithm of the bearer-token signing keys made from now on: `ES256` is ECDSA on P-256, `EdDSA` is Ed25519. A key keeps its algorithm; `lajstrom admin key rotate --token` makes a key of the new one ([operations.md](operations.md#the-bearer-token-key)). The token lifetime (20 minutes) and the clock skew tolerated between replicas (60 seconds) are fixed ([rbac.md](spec/rbac.md) §4). |

The master key is read once, when `serve` or `admin key` starts, from the file of
`keys.masterKeyFile` when it is set, otherwise from the variable `keys.masterKeyEnv` names:

- **Set and valid**: apt repos are signed (`InRelease`, `Release.gpg`, `key.asc`), each with its
  own key, made on first use and stored encrypted with the master key; the bearer tokens of
  `/v2` are signed with the token key of the database, made at the first start, the same on
  every replica.
- **Set but empty, or not 64 hexadecimal characters**: `serve` and `admin key` stop with an
  error that names the variable or the file and the format, never the value. So does a file
  that is missing or cannot be read: a configured file is never taken for "no master key".
- **Neither `keys.masterKeyFile` nor `keys.masterKeyEnv`, or the variable not set**: signing is off. `serve` logs a warning
  for every apt repo, whose `InRelease`, `Release.gpg` and `key.asc` are `404`; clients use
  `[trusted=yes]`. When the index already holds apt signing keys, `serve` stops instead: only
  the master key decrypts them, and their clients pinned them. Bearer tokens are signed with an
  ephemeral key in memory, with a warning: they do not survive a restart and are not shared
  between replicas. `admin key` always needs it.

[operations.md](operations.md#keys-and-apt-signing) has the procedures: generating and storing
the master key, distributing `key.asc`, rotating a key, the bearer-token key.

### repos

Optional. Repos to create at startup when the control plane does not have them, the config
bootstrap. Repos are also created through the [management API](api.md#repos) and with
`lajstrom admin repo`, and those need not be listed here.

On every start, after the migrations and before it listens, `serve` looks up each entry in the
index database:

- **Missing:** it is created exactly as `POST /api/v1/repos` would create it, with the same
  checks and one `repo.create` audit row whose details say `"source": "config"`. The
  configuration was validated already, so the one check that can still fail is the format: a
  repo whose format plugin is not compiled into the binary stops `serve` with an error naming
  the entry (`oci`, the pass-through format of the OCI facade, needs no plugin).
- **Present:** it is left as it is. When its `registry` or `anonymousRead` differ from the
  database, a warning names the repo and the field: the database wins, and a change goes
  through the API or `lajstrom admin repo`.

The bootstrap never deletes a repo and never changes one: removing an entry from this list
leaves the repo in place. Replicas that start together create each repo once; a replica whose
create loses to another's counts the repo as present.

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `repos[].name` | name | none, required | none | Repo name, unique within its format; same rule as registry names ([manifest-profile.md](spec/manifest-profile.md) §4.1). Example: `scratch`. |
| `repos[].format` | name | none, required | none | Plugin name: `raw`, `pypi`, `apt`, `maven`. |
| `repos[].registry` | name | none, required | none | One of `registries[].name`. |
| `repos[].anonymousRead` | bool | `false` | none | Readable without credentials, when `auth.anonymousRead` is `true`. |

`(format, name)` must be unique across `repos`.
