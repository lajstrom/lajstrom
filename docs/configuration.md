# Configuration

Lajstrom reads one YAML file. `serve`, `migrate`, `reindex` and the `admin` commands (`repo`,
`user`, `binding`, `token`, `key`) load and validate it before they do anything else; `version`
and `admin deb metadata` do not need it, nor does `admin retention`, which asks a server
(`--url`, [operations.md](operations.md#retention)). `healthcheck` reads `server.listen` and
`server.tls` from it and nothing else, and does not open it with `--url`
([operations.md](operations.md#readiness)); `admin bootstrap` reads the same two, and does not
open it with `--server` ([auth.md](auth.md#bootstrap-the-first-admin)); `registry probe` loads
it for `--registry` only.

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
   empty variable counts as unset. Lists (`registries`, `upstreams`, `repos`) cannot be set
   from the environment.
4. **Secrets**: each `registries[].auth.passwordEnv` is read from the environment. The master
   key of `keys.masterKeyEnv` or `keys.masterKeyFile` is not: `serve` and `admin key` read it
   when they start (see [keys](#keys)).
5. **Validation.** Every problem is reported at once, one line each, starting with the key:

   ```text
   lajstrom: config: lajstrom.yaml: invalid:
   server.listen: must not be empty
   repos[0].registry: "zot" is not a configured registry
   ```

   A configuration that cannot be read or is invalid ends every command with exit status `2`;
   other errors with `1`. `healthcheck` is the exception: it exits `1` on every failure, since
   Docker reserves `2` in a health check.

The `--log-level` and `--log-format` flags override `log.level` and `log.format` for one run.

## Secrets

Secrets never go into the file. Keys ending in `Env` name the environment variable that holds
the secret: `registries[].auth.passwordEnv` and `keys.masterKeyEnv`. The master key can also come
from a file of its own, `keys.masterKeyFile`, for a secret mounted as a file. A `password` key in
the file is rejected. Lajstrom never logs a secret or puts one in an error message.

## What `serve` needs

`server.listen` and `server.externalURL`, the URL clients use; only the dev stack, with
`server.insecureHTTP: true`, may leave the URL empty (#248). Everything else has a default or is
optional:

```yaml
server:
  listen: ":8080"
  externalURL: "https://art.example.com"
```

`serve` opens the index database (`database`, by default the SQLite file `lajstrom.db` in the
current directory), applies its pending migrations and creates the [repos](#repos) of the file
that the database does not have, all before it listens; `/-/readyz` answers 503 while the
database does not. It also contacts the registries: at startup it asks each one
what it supports (see [capabilities](#registry-capabilities)) and logs the answer at info, then
pings each one for readiness. A registry that does not answer is logged at error and `serve`
starts anyway, so that one backend down does not keep a replica from starting; the question
and the ping wait up to 10 s each, one after the other, so the listener opens up to 20 s late,
and `/-/readyz` answers `503` until every registry has answered
([operations.md](operations.md#readiness)). The same holds after the start: with several
registries, any one that has not answered for a minute makes every replica not ready, and an
edge or a readiness probe that asks `/-/readyz` takes them all out, the repos on the other
registries included.

An upload to a frontend (raw, apt, Maven, PyPI, npm) streams into the registry and takes no disk
([ADR 0014](adr/0014-uploads-stream-into-the-registry.md)).
A `/v2` upload streams to the registry and takes no disk, but for the chunks of
[`registries[].uploads.spoolUnknownLength`](#uploads), and so does a proxy repo's fetch. Those
chunks go to `$TMPDIR` when it is set, `/tmp` otherwise; there is no configuration key for it (a tmpfs in
[Compose](deploy/compose.md), an `emptyDir` on [Kubernetes](deploy/kubernetes.md)).

## Reference

### server

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `server.listen` | string, `host:port` | none, required | `LAJSTROM_SERVER_LISTEN` | TCP address to listen on. `":8080"` listens on every interface, `"127.0.0.1:8080"` on loopback only. |
| `server.externalURL` | `https://<host>[:port]` | none, required but with `server.insecureHTTP` | `LAJSTROM_SERVER_EXTERNAL_URL` | Base of the absolute URLs Lajstrom renders: the token realm, `<externalURL>/v2/token`, and npm's `dist.tarball` ([http-routing.md](spec/http-routing.md) §1); redirects and the `Location` of an upload are paths, and the links of the PyPI simple index are relative. A new value needs npm's documents rendered again: run `lajstrom reindex` ([reverse-proxy.md](reverse-proxy.md#serverexternalurl)). Its host is the `service` of the token flow, the issuer and audience of every bearer token. Required (#248): without it the realm and the audience would come from the request's `Host`, which the client chooses, and a server restored from another's database would take the other's tokens for that `Host`. Only with `server.insecureHTTP: true`, the dev stack, may it be empty; the realm is then `https://<Host>/v2/token` for a request over TLS and `http://` otherwise. No path, query or fragment (a trailing `/` is allowed): Lajstrom owns the whole host. Clients see Lajstrom over TLS only, so the scheme is `https`, whether TLS ends at the edge or in Lajstrom (`server.tls`); `http://` only for a loopback host (`localhost`, `127.0.0.1`, `::1`) with `server.insecureHTTP: true`, the dev stack. Example: `https://art.example.com`. |
| `server.insecureHTTP` | bool | `false` | none | Allows an `http://` `server.externalURL` on a loopback host, or none, for the dev stack on one machine. Without it, an `http` URL or an empty one stops the start; with it, an `http` URL on any other host still does. It changes nothing else: Lajstrom listens on plain HTTP unless `server.tls` is set. With it on, no `server.tls` and a `server.listen` host that is not loopback (`127.0.0.0/8`, `::1`, `localhost`; an empty host, `":8080"`, is every interface), `serve` logs a warning at start: passwords in Basic auth would cross the network in clear text. It does not refuse, because a container listens on `:8080` behind a port published on loopback (#746). |
| `server.tls.certFile` | path | empty | none | PEM certificate chain, the server's certificate first, for TLS in Lajstrom itself on a single host: `server.listen` then speaks TLS 1.2 and later, with HTTP/2. Set together with `keyFile`; both empty (the default) is plain HTTP, for TLS at the edge. The files are read at start: a renewed certificate takes a restart. The metrics listener stays plain HTTP. |
| `server.tls.keyFile` | path | empty | none | PEM private key of `certFile`. |
| `server.shutdownDelay` | duration | `5s` | none | How long `serve` keeps serving after SIGTERM, SIGINT or SIGHUP before the listener closes: `/-/readyz` answers `503` at once and every connection closes after its next response, while the edge stops sending new requests to this replica. The edge's time to notice goes here: on Kubernetes, the removal of the endpoint; behind nginx, longer than `fail_timeout` by more than a second ([reverse-proxy.md](reverse-proxy.md#restarts)). The orchestrator's grace period must exceed it plus `server.shutdownTimeout` ([operations.md](operations.md#shutdown)). `0s` closes the listener at once, which suits a single replica: there is no other to send to. Must not be negative. |
| `server.shutdownTimeout` | duration (`30s`, `2m`) | `30s` | none | The drain after `server.shutdownDelay`: the listener closes, and in-flight requests, uploads included, get this long to finish. Then the requests still running are cancelled and their connections closed, and the process exits with an error. A second signal stops it at once ([operations.md](operations.md#shutdown)). Must be positive. |
| `server.maxManifestSize` | bytes | `4194304` (4 MiB) | none | Largest manifest a `PUT /v2/<name>/manifests/<ref>` may carry, on every repo; a larger one is `413` ([http-routing.md](spec/http-routing.md) §2). The OCI facade reads a manifest into memory before it checks it, so this bounds the memory of one push; the distribution spec asks a registry to take at least 4 MiB. Must not be negative; `0` means the default. |
| `server.maxCreatedAhead` | duration | `5m` | none | How far `org.opencontainers.image.created` of a version pushed to `/v2` may be after the server's clock. It is the version's publish time, which the rules of `lajstrom admin retention` read ([operations.md](operations.md#retention)); a later one is `400 MANIFEST_INVALID` naming the time and the clock ([http-routing.md](spec/http-routing.md) §2). The frontends write the server's own time. Must not be negative; `0s` means the default. |
| `server.maxCreatedBehind` | duration | `1h` | none | How far that time may be before the server's clock, likewise. `oras push` writes the time before it uploads the files, so this must outlast the upload of the largest push over the slowest link; a push refused for it goes through when it is sent again, with the files in the registry already. Must not be negative; `0s` means the default. |
| `server.metricsListen` | string, `host:port` | `127.0.0.1:9464` | `LAJSTROM_SERVER_METRICS_LISTEN` | The metrics listener: `GET /metrics` for Prometheus, and `/debug/pprof/` with `server.pprof` ([operations.md](operations.md#metrics)). It is a listener of its own and never the public host: the main listener answers `404` on `/-/metrics` and `/metrics`, and the edge must not route to it. In a container set `0.0.0.0:9464` (or the pod IP) so that Prometheus reaches it, and keep the port off the ingress. Empty turns it off; it must not be `server.listen`. |
| `server.pprof` | bool | `false` | none | Serves Go's `/debug/pprof/` on the metrics listener, for a profile taken on purpose; it costs nothing until asked. |
| `server.stallTimeout` | duration | `1m` | none | How long a request body or a response may make no progress, no byte read from the client or taken by it, before the connection is closed. The bound moves with every read and write: a transfer of any size passes while it moves, and the time the server spends on the registry or the index between two reads is not counted. Keep it below the body timeout of the registry and of any proxy in front of it (nginx's `client_body_timeout` is `60s` by default): a `/v2` upload streams through, and one whose client pauses longer than the registry waits fails, though Lajstrom would have waited ([operations.md](operations.md#requests-from-clients)). `0s` turns it off; must not be negative. |
| `server.maxConcurrentUploads` | integer | `32` | none | How many uploads one replica runs at once: `POST`, `PUT` and `PATCH` to the frontends and to `/v2`, not to `/api/` or `/v2/token`. The next one is answered `503` with `Retry-After: 5` before its body is read (on `/v2` with the error `UNAVAILABLE`, which docker retries). An upload holds a connection to the registry; one to a frontend holds a chunk of 8 MiB and the bounded metadata of its format in memory ([operations.md](operations.md#performance)), and a `/v2` chunk that [`uploads.spoolUnknownLength`](#uploads) spools is a temporary file of its size. So the default bounds what a replica holds of each (#246; [operations.md](operations.md#requests-from-clients)). `0` means no limit; must not be negative. |
| `server.spoolBudget` | bytes | `0`, no budget | none | How many bytes the `/v2` chunks that [`uploads.spoolUnknownLength`](#uploads) spools may hold in `$TMPDIR` at once on one replica, of every registry (#427). A chunk's length is unknown until it ends, so it takes its bytes from the budget as it writes them; a chunk that would pass the budget is answered `503 UNAVAILABLE` with `Retry-After: 5` (docker retries it), its file is removed at once, and nothing of it has reached the registry: the session keeps its offset, so the same `PATCH` passes later. A closing `PUT` refused so ends its session, as any failure of its last chunk does. Set it below the room of `$TMPDIR`, the `sizeLimit` of an `emptyDir` ([deploy/kubernetes.md](deploy/kubernetes.md)), so that a full disk never evicts the pod. Nothing else spools. Must not be negative. |
| `server.uploadSessions.perRepo` | integer | `256` | none | How many live upload sessions of `/v2` one principal may hold in one repo, on one replica; a new session beyond is `429 TOOMANYREQUESTS` with `Retry-After`, naming the bound and this key ([http-routing.md](spec/http-routing.md) §2). A session is live while a request on it runs and for 5 minutes after the last one ended: clients do not cancel the sessions of a failed push, and a retry opens new ones, so an idle session takes no slot, and stays resumable until it expires an hour after its last request. Its `DELETE`, its closing `PUT` and an error that ends it free the slot at once. The principal is a user, whose personal access tokens act as the user, or a deploy token: CI runners that share one credential share its bound. 256 is about 50 runners pushing five blobs each at once into one repo. Per replica: behind N replicas a principal may hold up to N times as many ([operations.md](operations.md#requests-from-clients)). `0` means no bound; must not be negative. |
| `server.uploadSessions.perPrincipal` | integer | `1024` | none | How many live upload sessions of `/v2` one principal may hold in all repos together, on one replica, counted as for `perRepo`; a new session beyond is `429 TOOMANYREQUESTS` naming this key. It bounds a principal that may write to many repos. `0` means no bound; must not be negative. |
| `server.trustedProxies` | list of CIDR prefixes | empty | none | The peers whose `X-Forwarded-For` is believed: the edge in front of Lajstrom ([ADR 0011](adr/0011-client-address-behind-a-proxy.md)). When the peer of a connection is in the list, the client is read from `X-Forwarded-For` from the right: trusted entries are skipped and the first one that is not is the client (the leftmost when all are trusted); an entry with a port is read without it, and one on that walk that is not an address makes the header untrusted. Otherwise, and with an empty list, the peer is the client and no forwarded header is read. A bare address is one host (`/32`, `/128`). There is no implicit trust, of private ranges or of anything else: in a cluster that would make every pod a proxy. `0.0.0.0/0` and `::/0` mean "every peer is a proxy", which is safe only when nothing but the edge can reach the port (a NetworkPolicy that admits only the ingress); otherwise any client can name any address. The client address is the access log's `remoteAddr`, with the edge as `proxy`, and the key of the per-address throttles of failed logins and `/v2/token` ([auth.md](auth.md#failed-logins)); with an empty list those throttles are off, since every client behind an edge would share its address, and the startup log says so; with `server.tls` set, Lajstrom terminates TLS itself, the peer is the client, and an empty list turns them on with the peer (#248). Example: `["10.42.0.0/16"]`, the pod CIDR for an ingress controller that runs as a Deployment. To find what to list, send one request through the edge while the list is empty and read the `remoteAddr` of its access log line: without its port, that is the edge as Lajstrom sees it ([reverse-proxy.md](reverse-proxy.md#the-client-address)). |

### log

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `log.level` | `debug`, `info`, `warn`, `error` | `info` | `LAJSTROM_LOG_LEVEL` | Lowest level that is logged. Flag: `--log-level`. |
| `log.format` | `text`, `json` | `text` | `LAJSTROM_LOG_FORMAT` | `text` for terminals, `json` for log collectors. Flag: `--log-format`. |

Logs go to standard error. Every request is logged once with method, path (never the query string,
and a token's secret in it redacted), status, bytes, duration, request id, remote address and
principal, and `proxy` when the peer is a trusted proxy: the remote address is then the client
that `X-Forwarded-For` names ([`server.trustedProxies`](#server)).

### database

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `database.driver` | `sqlite`, `postgres` | `sqlite` | `LAJSTROM_DATABASE_DRIVER` | Engine of the index database ([index-schema.md](spec/index-schema.md)). |
| `database.dsn` | string | `file:lajstrom.db` with `sqlite`; none with `postgres`, required | `LAJSTROM_DATABASE_DSN` | Data source name. SQLite: `file:/var/lib/lajstrom/lajstrom.db`. Postgres: `postgres://lajstrom@db:5432/lajstrom?sslmode=require`. A DSN with a password belongs in `LAJSTROM_DATABASE_DSN`, not in the file. The forms are below; a DSN in the form of the other driver's is a configuration error: a `postgres://` or `postgresql://` URL, or `host=…` pairs, with `sqlite`; a `file:` DSN, `:memory:` or a file name with `postgres`. With `postgres` and no DSN, as when `LAJSTROM_DATABASE_DSN` is unset, every command that loads the configuration stops with `database.dsn: must be set with database.driver postgres`. |

**SQLite DSN.** A file name or a `file:` URI, e.g. `file:/var/lib/lajstrom/lajstrom.db`. A
relative name, such as the default `file:lajstrom.db`, is resolved against the working directory
of the process, as SQLite resolves it, not against the directory of the configuration file: run
`lajstrom admin` and `reindex` where `serve` runs, or give an absolute path. An error opening the
file and the first line of `serve` (`database.file`) name the absolute path. `admin` and
`reindex` do not create a file that does not exist; they stop with `no database at <path>
(database.dsn): run lajstrom migrate there, or point --config/database.dsn at the server's`.
`migrate` and `serve` create it. Lajstrom
adds the connection parameters of `modernc.org/sqlite` itself: `_busy_timeout=5000` (a writer
waits up to 5 s for the lock), `_journal_mode=WAL` and `_synchronous=NORMAL` unless the DSN sets
them, and always `_foreign_keys=1` and `_txlock=immediate` (deletes cascade; a transaction takes
the write lock when it begins). SQLite serves one replica: a `serve` that finds another live
one on the file refuses to start and names it ([index-schema.md](spec/index-schema.md#coordination)).
Tests use an in-memory database,
`file:<name>?mode=memory&cache=shared`; it lives in one connection and is gone when the process
ends.

**Postgres DSN.** A URL, `postgres://user:password@host:5432/db?sslmode=require`, or a
keyword/value string, `host=db user=lajstrom dbname=lajstrom sslmode=require`, as pgx reads it
(it also honours the `PG*` environment variables). `pool_max_conns=N` sizes the connection
pool; the default is the number of CPUs, at least 4. Text columns use the `C` collation, so the
database's own collation does not matter. Behind a connection pooler (PgBouncer, Odyssey, the
Pooler of CloudNativePG) and on databases that speak the Postgres protocol,
[deploy/databases.md](deploy/databases.md) has what works and what to set: in transaction mode
without prepared statement support, `default_query_exec_mode=exec`; no `search_path` or
`options` in the DSN; `pool_max_conns` for each replica.

`lajstrom migrate` applies the migrations the database does not have yet and prints them, and
`lajstrom serve` does the same on start; both take a lock, so replicas starting together are
safe. Which release runs on which schema during an upgrade is in
[operations.md](operations.md#upgrades). Lajstrom logs `database.driver`, and the file of a SQLite
DSN, never the DSN, and keeps the password out of its errors.

### registries

A list; each entry is one backend OCI registry. Repos refer to it by name.

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `registries[].name` | name | none, required | none | Unique; lowercase letters and digits separated by single `.`, `_` or `-`, at most 64 characters. Example: `local`, `ecr-prod`. |
| `registries[].type` | `generic`, `ecr` | `generic` | none | `generic` covers every registry of [registries/](registries/README.md) but ECR: distribution, Zot, the GitLab container registry, Harbor, GHCR, Gitea and Forgejo, Nexus and JFrog; `ecr` is Amazon ECR, with the keys `registries[].ecr.*` below ([registries/ecr.md](registries/ecr.md)). |
| `registries[].url` | absolute `http(s)` URL | none, required | none | Base URL of the registry, `http(s)://<host>[:port]`, e.g. `http://localhost:5000`: no path, query or fragment (a trailing `/` is allowed), since the API is at `/v2/` of the host and Lajstrom adds `/v2` itself. A project or an owner, the `myorg` of `ghcr.io/myorg`, goes in `prefix` (`myorg/`). Must not contain credentials. A URL that breaks a rule is a configuration error, naming the key. |
| `registries[].prefix` | string | empty | none | Instance prefix of every OCI repository Lajstrom creates, e.g. `art/` gives `art/raw/scratch/…`. Must not start with `/` and must end with `/`. Clients never see it. Each component keeps to the grammar of the names Lajstrom writes ([ADR 0017](adr/0017-the-grammar-of-oci-names.md)): lowercase letters and digits separated by single `.`, `_` or `-`, which Gitea and Forgejo check, and none ends in `blobs`, `.uploads` or `.sync` or is `index.json`, `oci-layout`, `manifests` or `referrers`, nor is `tags` before a component that starts with `list`: Zot leaves such names out of its catalog and its garbage collection, and Harbor routes them elsewhere. Startup refuses a prefix that breaks a rule, naming the component. The prefix counts in the length of a name: distribution and GitLab take 255 bytes with it, Harbor 245 (its project included), ECR 256, so the names below it get 255 bytes less the prefix's length (245 less on Harbor, 256 less on ECR), `art/` leaving 251. A publish whose name is longer is `400` before anything is pushed, with the name's length and the limit and not the prefix: an upload of a frontend, a maven session, a `/v2` manifest or blob upload (`NAME_INVALID`); a proxy repo answers such a file `403` before it downloads anything ([quirks.md](registries/quirks.md#case-and-length)). |
| `registries[].auth.username` | string | empty | none | User for the registry. Set together with `passwordEnv`; leave both empty for anonymous access, and for type `ecr`, whose credentials come from the AWS SDK. The credentials go to the registry's host only: a bearer challenge whose token realm is on another host name (another port is fine) that `tokenRealmHosts` does not list, or is `http://` for a registry on `https://`, is refused with an error that names the registry and the realm's host, and a blob redirect from `https` to `http` is refused. |
| `registries[].auth.passwordEnv` | environment variable name | empty | none | Variable that holds the password. Startup fails when it is set here but the variable is unset or empty. Example: `ZOT_PASSWORD`. |
| `registries[].tokenRealmHosts` | list of host names | the vendor table's | none | Host names, besides the registry's own, that the token realm of its bearer challenge may be on, so that the credentials go there: exact names or IP addresses, no scheme, port, path or wildcard. A realm on a listed host must be `https://`. Lajstrom adds the hosts its vendor table knows without configuration: `gitlab.com` for `registry.gitlab.com` (GitLab.com), `auth.docker.io` for `registry-1.docker.io` and `docker.io` (Docker Hub); a list adds to them. oras-go still refuses a realm on another host name that is or resolves to a loopback, link-local or cloud metadata address. Example: `[gitlab.example.com]` for a GitLab whose registry is `registry.gitlab.example.com`. |
| `registries[].timeout` | duration (`30s`, `1m`) | `30s` | none | Bounds one request to the registry up to its response headers: connecting and the TLS handshake (each at most 10 s) and the wait for the headers. A blob upload or download streams for as long as it takes. Must not be negative; `0s` means the default. |
| `registries[].retries` | integer | `3` | none | Retries of a failed idempotent request (`GET`, `HEAD`, `DELETE`, the `PUT` of a manifest): `429`, `5xx` but `501` and `505`, timeouts, connections refused, reset or closed without an answer; never another `4xx`. The wait doubles from 250 ms with jitter, at most 3 s, or is the `Retry-After` of a `429` or `503` when that is 3 s or less. The requests of an upload session are sent once; a blob that Lajstrom uploads itself from a file or from memory is uploaded again from the start ([operations.md](operations.md#requests-to-the-registries)). `0` means the default; a negative value disables retries. |
| `registries[].capabilities.createOnPush` | bool | probed | none | Pushing to a new repository creates it. |
| `registries[].capabilities.referrers` | bool | probed | none | The registry serves the OCI referrers API. `false` makes Lajstrom use the `sha256-<digest>` tag schema without asking. |
| `registries[].capabilities.catalog` | bool | probed | none | The registry lists repositories (`/v2/_catalog`). `false` turns repository listing off without asking, for registries whose catalog is missing, slow or wrong; Lajstrom then uses its own list of repositories. |
| `registries[].capabilities.deleteManifest` | bool | probed | none | Manifests can be deleted. |
| `registries[].capabilities.immutableTags` | bool | probed | none | The registry itself refuses to re-point a tag. |
| `registries[].capabilities.blobRedirect` | bool | probed | none | A blob download may be answered with a `307` to the URL the registry redirects it to (a presigned URL of its object storage), so that the bytes bypass Lajstrom. Each download is one request to the registry, whose answer decides: a redirect to another origin goes to the client, a `200` or `206` streams. `false` makes Lajstrom follow every redirect of the registry and stream the bytes itself: a client never gets a URL of the registry, for storage that clients cannot reach. Per repo, `policy.blobRedirect: "never"` does the same ([api.md](api.md)). |
| `registries[].capabilities.maxBlobSize` | bytes | `0` | none | Largest blob the registry accepts; `0` leaves the provider's: on type `ecr` ECR's quota on a layer, 52,000 MiB; on type `generic` the vendor table's, GHCR's 10 GB, and unknown, no bound, for the others. A `/v2` blob upload above it, or above the repo's `maxFileSize` when that is smaller, is `413 SIZE_INVALID` before it streams, on every repo, `oci` ones included (#246). Must not be negative. |
| `registries[].capabilities.consistency` | `strong`, `eventual` | the vendor table's: `eventual` for type `ecr`, for GHCR and for Quay, `strong` for distribution, Zot and GitLab, unknown otherwise | none | When the registry's reads see a write it acknowledged ([registries/README.md](registries/README.md#consistency)). `strong`: at once. `eventual`: a manifest, a blob or a tag can be `404` for a while after its `201`, as on ECR. Every publish reads back what it pushed before the version is indexed ([`ingest.visibilityTimeout`](#ingest)): on a `strong` registry the manifest alone, once; on any other its tag and blobs too. An `eventual` registry without `consistencyBound` gets a warning at startup. |
| `registries[].capabilities.consistencyBound` | duration (`5s`) | the vendor table's: `130s` for Quay, else `0s` | none | The longest lag measured on an `eventual` registry, from the `201` of a write until every read sees it; [`lajstrom registry probe`](registries/README.md#measuring-a-registry-lajstrom-registry-probe) measures it (the row "Read-after-write lag"). `0s` means unknown. A bound longer than `ingest.visibilityTimeout` replaces it for this registry. Must not be negative, and not set with `consistency: strong`. |
| `registries[].capabilities.maxNameDepth` | integer | the vendor table's: `1` for Docker Hub, `4` for GitLab behind its token service, `0` otherwise | none | The most components the registry takes in a repository name below `prefix`; `0` means no limit, and set here leaves the vendor table's. Every name Lajstrom writes has three or more, `<format>/<repo>/<package path>`, and a publish whose name is deeper is `400` before anything is pushed, naming the limit: an upload of a frontend, a maven session, a `/v2` manifest or blob upload (`NAME_INVALID`); a proxy repo answers such a file `403` before it downloads anything. Reads and deletes are not checked: a package stored deeper before the limit was set still downloads, and only its new versions are refused. A limit below 3 is an error line in the log at startup: no package can be stored there. Docker Hub takes `<namespace>/<repository>` only, so nothing of Lajstrom's; behind GitLab, the token service grants a project path and two components more, which is a project per Lajstrom repo, `<prefix><format>/<repo>`, and two components of package path ([gitlab.md](registries/gitlab.md#behind-gitlab-read-not-measured)): with a project per format set `3`, and with a GitLab that grants more, that depth. Behind a proxy that rewrites the realm of the challenge, the table cannot tell GitLab's token service: set the depth yourself. Must not be negative. |
| `registries[].uploads.spoolUnknownLength` | bool | `false` | none | A `/v2` chunk of unknown length (chunked encoding, no `Content-Length`) into a session of the registry that already holds bytes is spooled to a temporary file in `$TMPDIR` for its one request, then sent with its `Content-Range`: for registries that refuse such a chunk streamed, Gitea and Forgejo with 400 "Stream uploads after first write are not allowed" ([gitea.md](registries/gitea.md#uploads)), JFrog with 416 ([jfrog-jcr.md](registries/jfrog-jcr.md#uploads-and-digests)). The file is bounded by the repo's `policy.maxFileSize` where it has one (a chunk past it is `413` and the session ends), unlinked as soon as it is made, closed when the request ends, whether it succeeded, failed or the client went away, and never served from. Off, every chunk streams, and such a chunk fails with the registry's 4xx, or on a registry with a chunk bound (`uploads.chunkSize`, GHCR's 4 MiB) with a `400` before it is sent; nothing is stored. The vendor table never turns it on; see [uploads](#uploads). |
| `registries[].uploads.chunkSize` | bytes | the vendor table's: `4194304` (4 MiB) for GHCR, else `8388608` (8 MiB) | none | The size of the chunks, each a `PATCH` with its `Content-Range`, of the uploads that Lajstrom streams into the registry itself ([ADR 0014](adr/0014-uploads-stream-into-the-registry.md)); one chunk per upload is held in memory. `0` takes the vendor table's. Set it for a registry that takes less in one request than 8 MiB and that the table cannot tell by its host: GHCR on another host name, or a proxy in front of the registry with a smaller body limit; `lajstrom registry probe` measures the largest chunk (the row "Upload: chunk size limit"). Where this or the vendor table sets a size, it bounds a client's `/v2` chunk too: one above it goes as `PATCH`es of this size, and the client sees one chunk; one of unknown length into a session that holds bytes is `400 BLOB_UPLOAD_INVALID` before it is sent, unless `spoolUnknownLength` spools it; a first write of unknown length, docker's one streamed `PATCH`, goes as it came (#697). Elsewhere a client's chunk goes as it came. Between `0` and `268435456` (256 MiB). |
| `registries[].ecr.region` | AWS region | from the URL, else the SDK's | none | Type `ecr` only, as every `ecr` key. The region of the registry; empty takes the one in the URL's host (`<account>.dkr.ecr.<region>.amazonaws.com`), else the AWS SDK's (`AWS_REGION`, the profile). The credentials are the SDK's default chain: the environment, the shared profile, web identity (IRSA), the instance or task role; `auth` stays empty ([registries/ecr.md](registries/ecr.md)). |
| `registries[].ecr.endpoint` | absolute `http(s)` URL | the SDK's | none | Endpoint of the ECR API instead of the SDK's for the region: a VPC endpoint, or the moto mock of the integration tests (`http://localhost:5010`). `url` stays the registry's base URL, `https://<account>.dkr.ecr.<region>.amazonaws.com`. |
| `registries[].ecr.imageTagMutability` | `IMMUTABLE`, `IMMUTABLE_WITH_EXCLUSION`, `MUTABLE`, `MUTABLE_WITH_EXCLUSION` | `IMMUTABLE` | none | Tag mutability of the repositories Lajstrom creates (`CreateRepository`); `capabilities.immutableTags` follows it. A repository that exists keeps its own. |
| `registries[].ecr.imageTagMutabilityExclusions` | list of wildcard filters | empty | none | The exclusion filters of a `*_WITH_EXCLUSION` mutability, required with one and refused otherwise; e.g. `["sha256-*"]` keeps the tags of the referrers tag schema and of cosign mutable under `IMMUTABLE_WITH_EXCLUSION`. |
| `registries[].ecr.encryption` | `AES256`, `KMS`, `KMS_DSSE` | `AES256` | none | Encryption of the repositories Lajstrom creates. |
| `registries[].ecr.kmsKey` | KMS key ARN, ID or alias | empty | none | Key of `KMS` or `KMS_DSSE` encryption; empty is the AWS managed key of ECR. Refused with `AES256`. |
| `registries[].ecr.forceDelete` | bool | `false` | none | A repository delete also removes a repository that still holds images; `false` lets ECR refuse it (`409`). |

Two registries on one URL (the same scheme, host and port) must not have `prefix`es that nest,
one the other or beginning with it, the empty prefix included: with `""` and `raw/x/`, repo
`raw/x` of the first and path `raw/y/pkg` would be repo `raw/y` of the second. Such a pair is
refused, naming both.

#### Registry capabilities

At startup Lajstrom asks each registry what it supports, once, and keeps the answer: `GET /v2/`
(the registry answers, and accepts the credentials), `GET /v2/_catalog?n=1` (a catalog: 200
with a JSON catalog yes; a 200 without one, 401, 403, 404, 405 no) and the referrers of a digest no manifest has in the repository
`<prefix>lajstrom-probe` (an OCI image index: the referrers API; a 404: the tag schema). The
`generic` type assumes create-on-push, manifest deletes and blob redirects, and no immutable
tags; a registry that answers a manifest delete with 405 turns `deleteManifest` off from then on.
Blob redirects need no probe: a download is one `GET` whose first answer is used as it is, a
redirect or the bytes, so a registry on filesystem storage costs nothing for the assumption.

What a probe cannot measure comes from a table of vendors, which Lajstrom tells apart by what a
registry says about itself: a `Gitlab-Container-Registry-Version` header on `GET /v2/` is the
GitLab container registry, the host `ghcr.io` is GHCR, a Bearer challenge with
`service="harbor-registry"` on `GET /v2/` is Harbor, `X-Jfrog-Version` on it is JFrog's
Artifactory or JCR, `Server: Nexus/…` or a Basic challenge with `realm="Sonatype Nexus Repository
Manager"` is Nexus Repository, and a Bearer challenge with `service="container_registry"` and
`scope="*"` is Gitea, or Forgejo when its own `GET /api/forgejo/v1/version` answers 200 with a
version (it answers `GET /v2/` as Gitea does), `X-ZOT-API-CLIENT` in its
`Access-Control-Allow-Headers` is Zot, `X-Sparse-Manifest-Support` on `GET /v2/`, or the host
`quay.io`, is Project Quay, Red Hat Quay or quay.io, and `Docker-Distribution-Api-Version` with
none of these, on a host that is not Docker Hub's, Quay's, ACR's, Google's or AWS's, is
distribution. GHCR gets `catalog: false` (its `_catalog` answers 200 with every repository of
GHCR, which the probe cannot tell from a usable one) and `deleteManifest: false` (every delete
through `/v2` is 405), without asking. Quay gets `catalog: false` (its `_catalog` answers 200
with no repositories to a client that sends no credentials, which is how the catalog is asked),
`referrers: false` (it has the API, but the tag schema answers right where the API does not: the
API is 400 for a subject that does not exist and keeps listing a deleted referrer) and
`consistency: eventual` within `130s` (it caches a tag list for 120 s,
[quay.md](registries/quay.md#consistency)). Harbor, Gitea and Forgejo need nothing: the probe is
right for them. The depth of a name is the table's too: a
host `docker.io` or below it is Docker Hub, which takes one component below the prefix
(`maxNameDepth: 1`), and a GitLab whose Bearer challenge names GitLab's token service, a realm
`…/jwt/auth`, takes four (`maxNameDepth: 4`); the GitLab registry alone, without GitLab, takes
any depth. Nexus depends on the format of the
repository, which the referrers probe tells: the `docker` format, without the referrers API, gets
`deleteManifest: false`, since a delete by digest leaves every tag that names the manifest
([nexus.md](registries/nexus.md#delete)); the `oci` format answers every ranged `GET` of a blob
with 500, so Lajstrom asks it for whole blobs and serves a client's range with the whole blob
(`200`, no `Accept-Ranges`). On the `docker` format, also turn off the hosted repository's
`strictContentTypeValidation` ("Strict Content Type Validation" in the UI, where it is on for a
new repository): with it on, Nexus refuses a blob whose content type it cannot tell from its
first bytes, such as one that starts with eight zero bytes, with `400 BLOB_UPLOAD_INVALID`, and
the client gets `502` ([nexus.md](registries/nexus.md#again-after-352)). Nothing in the vendor
table can work around that setting. JFrog answers `HEAD` of a blob with 200 in an image path where
`GET` is 404 ([jfrog-jcr.md](registries/jfrog-jcr.md#blob-reads-head-and-get)), so Lajstrom
asks whether a blob exists with a `GET` of its first byte. Quay's token service refuses the
action `delete` with 400 and lets a `push` token delete
([quay.md](registries/quay.md#tokens-and-the-delete-action)), so Lajstrom asks it for `push`
where it would ask `delete`; every other registry is asked for `delete`. A GitLab registry that answers
`Gitlab-Container-Registry-Database-Enabled: true` gets a warning at startup: with its database
it refuses a manifest whose `artifactType` it does not know, every one Lajstrom writes, unless
`REGISTRY_FF_DYNAMIC_MEDIA_TYPES` is on ([gitlab.md](registries/gitlab.md#media-types)). The
startup line names the vendor, `vendor=gitlab`, `ghcr`, `dockerhub`, `harbor`, `nexus`, `gitea`, `jfrog`, `zot`, `quay`, `distribution` or
`unknown`, beside the capabilities, and a line `registry needs workarounds` says when blobs are
read whole, their existence asked with `GET`, or a delete's token asked with `push`. A registry the table does not know gets the probe alone.
To see what a registry does before attaching it, beyond these answers, `lajstrom registry probe`
measures its row of the capability matrix in a scratch prefix and deletes what it pushed
([registries/README.md](registries/README.md#measuring-a-registry-lajstrom-registry-probe)).

Each registry is also strongly or eventually consistent, or unknown: the table says `eventual`
for GHCR, whose tag was seen `404` a fraction of a second after its push, and for Quay within
`130s`, whose tag lists are cached, and the `ecr` type
says it for ECR; distribution, Zot and GitLab are `strong`, as `lajstrom registry probe` saw
every write of theirs at once on 2026-11-11; nothing else is classified until it is measured.
The startup line says
`consistency=strong`, `eventual`, `eventual within <bound>` or `unknown`, and warns about an
`eventual` registry without `capabilities.consistencyBound`.

Each key under `capabilities` replaces what was found and what the vendor table sets, for
registries that answer wrongly; a key that is set is not probed. There is no `catalog` key
outside `capabilities`. What was tested against which registry, and the configuration each one
needs, is in [registries/](registries/README.md).

#### Uploads

`registries[].uploads.spoolUnknownLength` is off by default because Lajstrom runs on ephemeral
disks (a tmpfs in [Compose](deploy/compose.md), an `emptyDir` on [Kubernetes](deploy/kubernetes.md)):
by default nothing on `/v2` writes to disk, and `$TMPDIR` needs room for the frontends' uploads
only. The chunk it spools is one that docker, containerd and oras do not send: they stream one
`PATCH` per blob into an empty session, which every registry takes, and that `PATCH` is never
spooled. Turn it on for a Gitea, Forgejo or JFrog registry whose clients send several streamed
`PATCH`es, or close with a chunked `PUT` after a `PATCH`, and give `$TMPDIR` room for one chunk
per such upload at once, or set [`server.spoolBudget`](#server) below its room. When the vendor table names a registry `gitea` or `jfrog` and the key is
off, `serve` logs a hint at start that names the key.

The uploads that Lajstrom streams into the registry itself, a raw `PUT`, an apt upload and a
Maven file ([ADR 0014](adr/0014-uploads-stream-into-the-registry.md)), go in chunks of 8 MiB,
each a `PATCH` with its `Content-Range`; on GHCR, which refuses a `PATCH` over 4 MiB
([ghcr.md](registries/ghcr.md#upload-sessions)), the vendor table makes them 4 MiB, and
`registries[].uploads.chunkSize` sets another size. That size also bounds a client's `/v2`
chunk, which is split into `PATCH`es of it (#697); without one, a client's chunk goes as it came.

### upstreams

A list; each entry is a registry that [proxy repos](spec/proxy-and-group.md) of one format serve
and keep what they fetched from, as Verdaccio's `uplinks`. A proxy repo refers to it by name
(`repos[].upstream.name`); the management API never takes an upstream's URL or credentials, so
only the operator decides where Lajstrom sends requests ([ADR 0013](adr/0013-proxy-and-group-repos.md)).

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `upstreams[].name` | name | none, required | none | Unique; the grammar of `registries[].name`. Example: `npmjs`. |
| `upstreams[].format` | plugin name, or `oci` | none, required | none | The format it serves: a plugin of this build that has proxy repos (`serve` checks), or `oci`, a registry of the distribution API that an `oci` proxy repo pulls through from ([proxy-and-group.md](spec/proxy-and-group.md) §11, [ADR 0018](adr/0018-oci-proxy-repos-pull-through-from-any-registry.md)). Only a proxy repo of this format names it. |
| `upstreams[].url` | absolute `http(s)` URL | none, required | none | Base of every request, e.g. `https://registry.npmjs.org/`. A path is allowed; credentials, a query and a fragment are not. `http` is accepted with a warning at start: what it serves is not authenticated. An `oci` upstream's URL is the registry's root, `https://registry-1.docker.io`, `https://ghcr.io`: no path, since the API is at `/v2/` of the host. |
| `upstreams[].auth.username` | string | empty | none | With `passwordFile` or `passwordEnv`: HTTP Basic, and on an `oci` upstream the credentials of its token realm. |
| `upstreams[].auth.passwordFile` | path | empty | none | File that holds the password of `username`. |
| `upstreams[].auth.passwordEnv` | environment variable name | empty | none | Variable that holds the password of `username`, as `registries[].auth.passwordEnv`: startup fails when it is set here and the variable is unset or empty. Not with `passwordFile`. Example: `DOCKERHUB_TOKEN`, a Docker Hub personal access token. |
| `upstreams[].auth.tokenFile` | path | empty | none | File that holds a bearer token (`Authorization: Bearer`), npm's `_authToken`; on an `oci` upstream a static token sent instead of the token flow. Not with `username`. |
| `upstreams[].auth.ecr` | map | unset | none | An `oci` upstream on Amazon ECR: its credential is ECR's authorization token, which `GetAuthorizationToken` gives the AWS SDK's default chain (the environment, the shared profile, web identity as with IRSA, the container or instance role), sent as Basic `AWS:<token>` and replaced an hour before its 12 hours end, as for `registries[].type: ecr`. Nothing secret in the file. Not with another key of `auth`; refused on another format. Example: `auth: {ecr: {}}` ([registries/ecr.md](registries/ecr.md)). |
| `upstreams[].auth.ecr.region` | AWS region | from the URL, else the SDK's | none | The region of the ECR API that gives the token; empty takes the one in the URL's host (`<account>.dkr.ecr.<region>.amazonaws.com`), else the AWS SDK's (`AWS_REGION`, the profile). |
| `upstreams[].auth.ecr.endpoint` | absolute `http(s)` URL | the SDK's | none | Endpoint of the ECR API instead of the SDK's for the region: a VPC endpoint, or moto in the tests. |
| `upstreams[].auth.gcp` | map, no keys | unset | none | An `oci` upstream on Google Artifact Registry or Container Registry: its credential is an access token of the workload's service account from the GCP metadata server (GKE Workload Identity, Compute Engine, Cloud Run), sent as Basic `oauth2accesstoken:<token>` and replaced 4 minutes before it expires. The metadata server is `169.254.169.254`, or the host of `GCE_METADATA_HOST`, and is reached without the egress proxy. Not with another key of `auth`; refused on another format. Example: `auth: {gcp: {}}`. A service account key instead is Basic: `username: _json_key` with the key file as `passwordFile`. |
| `upstreams[].tokenRealmHosts` | list of host names | the vendor table's | none | An `oci` upstream's: the host names, besides its own, that the realm of its bearer challenge may be on, over `https`, so that its credentials go there; as `registries[].tokenRealmHosts`, with the vendor table's `auth.docker.io` for Docker Hub added. A realm elsewhere is refused (`502`) with the realm's host in the log. Refused on another format. |
| `upstreams[].dockerHub` | bool | `true` when the URL's host is `registry-1.docker.io` or `docker.io`, else `false` | none | An `oci` upstream's: it is Docker Hub or a mirror of it, so a path of one component is `library/<path>` on every `/v2` route, and `nginx` is stored once, as `library/nginx`. Refused on another format. |
| `upstreams[].timeout` | duration | `30s` | none | Bounds the wait for the response headers (connecting and TLS at most 10 s each) and every wait for the next bytes of a body. Must not be negative; `0s` means the default. |
| `upstreams[].retries` | integer | `2` | none | Retries of a request, as `registries[].retries` defines them: `429`, `5xx` but `501` and `505`, timeouts, connections refused or reset; the wait doubles from 250 ms, at most 3 s. On an `oci` upstream, also the resumes in a row of a blob's body that get no byte: a body that breaks is asked again with `Range` from the bytes read (proxy-and-group.md §11.5). `0` means the default; a negative value disables retries. |
| `upstreams[].revalidateTimeout` | duration | `5s` | none | An `oci` upstream's: bounds, whole, the `HEAD` that asks it for a tag whose cached row expired. It is sent once, without retries, and when it fails or gets no answer within this time the cached digest is served as it is ([proxy-and-group.md](spec/proxy-and-group.md) §11.4), so a client gets an answer before containerd's 30 s wait for the response headers is over. A tag without a row has nothing to fall back on and keeps `timeout` and `retries`: with the defaults, an upstream that accepts the connection and never answers costs it 90 s. Above `timeout`, each request still waits at most `timeout` for its headers. Must not be negative; `0s` means the default; refused on another format. |
| `upstreams[].maxMetadataSize` | bytes | `67108864` (64 MiB) | none | Largest document the upstream may answer for one name; a larger one is not cached. Must not be negative; `0` means the default. |
| `upstreams[].maxConcurrentTransits` | integer | unset: no cap | none | An `oci` upstream's: how many requests of one blob a replica streams from the upstream at once while the blob's copy into the backend runs, each with a `GET` of its own, verified against the digest, stored nowhere (a transit, [proxy-and-group.md](spec/proxy-and-group.md) §11.8). A request beyond the cap waits for the copy and is served from the backend, as one did before transits. `-1` or unset: no cap, as distribution's proxy does; `0`: no transit; below `-1` is refused, and so is the key on another format. The cap is per replica: with N replicas, up to N times as many. Each transit costs one more transfer from the upstream (egress; Docker Hub does not count blob `GET`s). |

The credential files are read when `serve` starts, as `keys.masterKeyFile` is: a file that is
missing, unreadable or empty stops `serve`, and one that the group or others may read is used,
with a warning. Spaces and line breaks around the value are ignored, and a rotated secret takes a
restart. A cloud credential (`auth.ecr`, `auth.gcp`) is fetched at the first request that needs
it and replaced before it expires; one that cannot be had fails the request with `502` unless a
cached answer serves it ([proxy-and-group.md](spec/proxy-and-group.md) §11.1). The credentials go to the origin (scheme, host and port) of `url` only, never to a host
a redirect or a document names; a client's own credentials are never forwarded.

The connections to an upstream go through the egress proxy of the environment, `HTTPS_PROXY`,
`HTTP_PROXY` and `NO_PROXY`, as the connections to the registries do; a registry that must not go
through it belongs in `NO_PROXY`. A repo that names an upstream the file no longer has is served
as if it were offline, with an error at every start naming the repo and the upstream.

npmjs.org, a hosted npm repo, a proxy of npmjs.org that never asks it for the scope `@acme`,
and a group of both, the one URL of the clients ([quickstart/npm.md](quickstart/npm.md) §9 and
§10):

```yaml
upstreams:
  - name: npmjs
    format: npm
    url: "https://registry.npmjs.org/"
repos:
  - {name: internal, format: npm, registry: local}
  - {name: npmjs, format: npm, registry: local, kind: proxy, upstream: {name: npmjs, deny: ["@acme/*"]}}
  - {name: all, format: npm, kind: group, group: {members: [internal, npmjs]}}
```

An `oci` proxy of Docker Hub, with a personal access token that raises its pull limit
(`docker pull <lajstrom>/oci/hub/nginx:1.27`), and one of ghcr.io without credentials:

```yaml
upstreams:
  - name: dockerhub
    format: oci
    url: "https://registry-1.docker.io"
    auth: {username: acme, passwordEnv: DOCKERHUB_TOKEN}
  - name: ghcr
    format: oci
    url: "https://ghcr.io"
repos:
  - {name: hub, format: oci, registry: local, kind: proxy, upstream: {name: dockerhub, metadataTTL: 1h}}
  - {name: ghcr, format: oci, registry: local, kind: proxy, upstream: {name: ghcr}}
```

A group of a hosted `oci` repo and the Docker Hub proxy, one pull address for both
(`docker pull <lajstrom>/oci/all/acme/api:1.4`, `docker pull <lajstrom>/oci/all/nginx:1.27`): each
member serves the paths of its patterns, the most specific pattern winning
([proxy-and-group.md](spec/proxy-and-group.md) §12):

```yaml
repos:
  - {name: internal, format: oci, registry: local}
  - {name: all, format: oci, kind: group, group: {members: [internal, hub], paths: {internal: ["acme/*"], hub: ["*"]}}}
```

### ingest

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `ingest.claimTTL` | duration (`10m`, `1h`) | `10m` | none | How long a publish's claim on a tag holds off a publish of other bytes to the same tag. Every publish claims its tag in the index before it writes the manifest to the registry and releases the claim once the index rows are written, so that two replicas cannot both write one tag ([index-schema.md](spec/index-schema.md), Coordination). A claim older than this, left by a replica that died mid-publish, is stale: the next publish of the tag takes it over, and `reindex` and the reconcile loop delete it. It must outlast one manifest `PUT` and one index transaction; blob uploads come before the claim and do not count. Must not be negative; `0s` means the default. |
| `ingest.visibilityTimeout` | duration (`30s`) | `30s` | none | How long a publish reads back what it pushed until the registry serves it, before it answers `503` with `Retry-After: 30` ([operations.md](operations.md#an-eventually-consistent-registry)). The version is indexed only once the registry serves its manifest (and, on a registry not classified `strong` in `registries[].capabilities.consistency`, its tag and blobs); a registry that serves at once costs a `HEAD` or a few. A referrer waits twice as long, until its subject lists it too. A registry's `capabilities.consistencyBound` raises it. Must not be negative; `0s` means the default. |

### reconcile

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `reconcile.interval` | duration (`1h`, `30m`) | `1h` | none | How often each replica runs a reindex of every repo with prune, so that writes that went past Lajstrom (a push straight to the backend, a delete in the registry's console) reach the index ([operations.md](operations.md#reconcile)). The first run comes after a random part of the interval; a run that overlaps a run elsewhere waits for the next tick. `0s` turns the loop off. Must not be negative. |
| `reconcile.graceAge` | duration (`1h`, `24h`) | `1h` | none | How long prune keeps a row whatever the registry answers: a version published, or seen in the registry by a run, less than this ago is never removed. It holds for the loop, `lajstrom reindex` and the API's reindex. A row goes only once it is this old and the registry answered `404 MANIFEST_UNKNOWN` to its manifest twice, at least 10 minutes apart ([operations.md](operations.md#on-an-eventually-consistent-registry)). Keep the default when Lajstrom reads the registry at the endpoint it writes to; set `24h` or more when reads go through a replica or a cache (ECR replication, a pull-through cache, ACR geo-replicas). `0s` means the default. Must not be negative. |

The `upstreams`, `ingest` and `reconcile` sections are additions to the shape in
[layout.md](spec/layout.md) §6.

### auth

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `auth.anonymousRead` | bool | `false` | none | Server-wide switch for anonymous read. When `false`, no repo is readable without credentials, whatever `repos[].anonymousRead` says. Enforced by RBAC (issue #45, [rbac.md](spec/rbac.md)). |
| `auth.bootstrap.tokenFile` | file path | empty | `LAJSTROM_BOOTSTRAP_TOKEN_FILE` | File that holds the bootstrap token, the one-time token that `lajstrom admin bootstrap` exchanges for the first admin ([auth.md](auth.md#bootstrap-the-first-admin)): 32 to 256 bytes without whitespace that you make, spaces and line breaks around it ignored, so `openssl rand -hex 32 > file` works; a Kubernetes Secret or a Compose secret mounted as a file. `serve` reads it at start while no enabled user has an admin binding, and stops on a file it cannot read or a token it refuses; once an admin exists it is not read, and while the file exists a warning at start says to remove it. Empty, or a file that does not exist: there is no token, and a warning at start says how to make the first admin. `serve` never makes a token and never logs one. |
| `auth.oidc.issuer` | URL | empty | none | The OIDC identity provider's issuer, exactly as its tokens' `iss` and its discovery document's `issuer` name it ([auth.md](auth.md#oidc-login-and-groups), [rbac.md](spec/rbac.md) §8). Empty: OIDC is off and `/-/oidc/` is `404`. https; http only with `server.insecureHTTP`. Needs `server.externalURL`: the redirect URI to register at the IdP is `<server.externalURL>/-/oidc/callback`. |
| `auth.oidc.clientID` | string | empty | none | The client's id at the IdP; required with the issuer. |
| `auth.oidc.clientSecretFile` | file path | empty | `LAJSTROM_AUTH_OIDC_CLIENT_SECRET_FILE` | File that holds the client's secret, one line, a Kubernetes or Compose secret mounted as a file; required with the issuer. `serve` reads it at start and never logs it; it is sent to the token endpoint with `client_secret_basic`. |
| `auth.oidc.scopes` | list | `[openid, profile, email, groups]` | none | The scopes of the login; `openid` is sent whatever this says. Use the scope that puts the groups in the ID token at your IdP. |
| `auth.oidc.usernameClaim` | string | `preferred_username` | none | The claim that names the user a first login creates; it must be a user name. A later login keeps the name. |
| `auth.oidc.groupsClaim` | string | `groups` | none | The claim of the groups, a list of strings (or one string); `""` reads no groups. Each login replaces the user's groups. |
| `auth.oidc.tokenLifetime` | duration | `24h` | none | The lifetime of the personal access token a login mints; the groups of the login count as long. Positive, at most `8760h`. |
| `auth.bootstrap.ttl` | duration | `24h` | none | How long the token of the file is valid once `serve` made it valid. Used or expired, it is not made valid again: the file needs a new token. Must be positive. |

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
| `keys.kms` | key URI | empty | none | A key in AWS KMS or GCP Cloud KMS that becomes the apt and rpm signing key of the repos whose key is made from now on, instead of a key Lajstrom makes: a repo's first key, and `lajstrom admin key rotate` without `--kms` or `--local` ([ADR 0020](adr/0020-repo-signing-keys-in-a-cloud-kms.md)). The private key never leaves the KMS: Lajstrom keeps the OpenPGP public key it builds around the KMS's public key and, sealed with the master key, the key's name, and sends the KMS the digest of each signature. `awskms:///` followed by a key id, a key ARN, `alias/<name>` or an alias ARN, e.g. `awskms:///alias/lajstrom-apt` (an alias is resolved to its key when a repo's key is made from it); or `gcpkms://projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>/cryptoKeyVersions/<n>`, which must name a version. The key signs with SHA-256: an AWS key of usage `SIGN_VERIFY` and spec `RSA_2048`, `RSA_3072`, `RSA_4096` (recommended: every apt and dnf verifies it) or `ECC_NIST_P256`; a GCP version of `RSA_SIGN_PKCS1_2048_SHA256`, `RSA_SIGN_PKCS1_3072_SHA256`, `RSA_SIGN_PKCS1_4096_SHA256` or `EC_SIGN_P256_SHA256`, enabled. Another key, Ed25519 included, is refused when a repo's key is made from it, with an error naming its spec. `keys.algorithm` does not apply to it. Keys made before keep theirs; rotate a repo to move it onto the KMS key. The master key is still required for signing. Credentials: AWS's default chain, as for `registries[].ecr`, the region of the ARN or else `AWS_REGION`, the endpoint of `AWS_ENDPOINT_URL_KMS`, else `AWS_ENDPOINT_URL`, else the region's; GCP's metadata server, as for `upstreams[].auth.gcp`. A value of neither form is a configuration error. [operations.md](operations.md#keys-in-a-kms) has the permissions, the failures and the runbook. |
| `keys.tokenAlgorithm` | `ES256` or `EdDSA` | `ES256` | none | Algorithm of the bearer-token signing keys made from now on: `ES256` is ECDSA on P-256, `EdDSA` is Ed25519. A key keeps its algorithm; `lajstrom admin key rotate --token` makes a key of the new one ([operations.md](operations.md#the-bearer-token-key)). The token lifetime (20 minutes) and the clock skew tolerated between replicas (60 seconds) are fixed ([rbac.md](spec/rbac.md) §4). |

The master key is read once, when `serve` or `admin key` starts, from the file of
`keys.masterKeyFile` when it is set, otherwise from the variable `keys.masterKeyEnv` names:

- **Set and valid**: apt repos are signed (`InRelease`, `Release.gpg`, `key.asc`), each with its
  own key, made on first use and stored encrypted with the master key, or held in the KMS of
  `keys.kms`; the bearer tokens of
  `/v2` are signed with the token key of the database, made at the first start, the same on
  every replica.
- **Set but empty, or not 64 hexadecimal characters**: `serve` and `admin key` stop with an
  error that names the variable or the file and the format, never the value. So does a file
  that is missing or cannot be read: a configured file is never taken for "no master key".
- **Neither `keys.masterKeyFile` nor `keys.masterKeyEnv`, or the variable not set**: signing is off, `keys.kms` or not. `serve` logs a warning
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
  configuration was validated already, so the checks that can still fail are the format and
  the policy: a repo whose format plugin is not compiled into the binary (`oci`, the
  pass-through format of the OCI facade, needs no plugin), or whose policy the API refuses,
  stops `serve` with an error naming the entry.
- **Present:** it is left as it is. When its `registry`, `kind`, `anonymousRead`, `policy`,
  `upstream` or `group` differ from the database, a warning names the repo and the field: the
  database wins, and a change goes through the API or `lajstrom admin repo`. `upstream` and
  `group` are compared with their defaults filled in: `{name: npmjs}` matches a proxy whose
  `metadataTTL` is the default `10m`.

The bootstrap never deletes a repo and never changes one: removing an entry from this list
leaves the repo in place. Replicas that start together create each repo once; a replica whose
create loses to another's counts the repo as present.

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `repos[].name` | name | none, required | none | Repo name, unique within its format; same rule as registry names ([manifest-profile.md](spec/manifest-profile.md) §4.1), and not a name that Zot's storage or Harbor's routes keep for themselves ([ADR 0017](adr/0017-the-grammar-of-oci-names.md)): one that ends in `blobs`, `.uploads` or `.sync`, or is `index.json`, `oci-layout`, `manifests`, `referrers` or `tags`. Example: `scratch`. |
| `repos[].format` | name | none, required | none | Plugin name: `raw`, `pypi`, `apt`, `maven`, `npm`; or `oci`, the pass-through format of the OCI facade, which has no plugin ([quickstart/oras.md](quickstart/oras.md)). Another name stops `serve`: `repos[0]: format: "rpm" is not a format this server has`. |
| `repos[].registry` | name | none, required but on a group | none | One of `registries[].name`. A group has none. |
| `repos[].kind` | `hosted`, `proxy`, `group` | `hosted` | none | What the repo serves ([proxy-and-group.md](spec/proxy-and-group.md) §1): what is published to it, what it fetched from an upstream, or its members under one URL. Set at creation and never changed. A proxy and a group need a format whose plugin has them (npm), or `oci`, which needs none. |
| `repos[].upstream` | map | none | none | A proxy repo's upstream, with the keys of the management API ([proxy-and-group.md](spec/proxy-and-group.md) §2.2): `name`, one of `upstreams[].name` of the repo's format (required), `metadataTTL` (`10m`), `negativeTTL` (`2m`), `offline` (`false`), `allow` and `deny` (lists of name patterns). Required on a proxy repo, refused on another kind. Example: `{name: npmjs, deny: ["@acme/*"]}`. |
| `repos[].group` | map | none | none | A group's settings, with the keys of the management API ([proxy-and-group.md](spec/proxy-and-group.md) §2.3): `members`, the names of 1 to 16 repos of the group's format in the order they are read, each hosted or proxy and at most one hosted (required), and `merge` (name patterns whose versions are merged even when the hosted member holds them). An `oci` group may have several hosted members, has no `merge`, and has `paths` instead (required): each member's name to the patterns of the paths it serves, `<path>`, `<prefix>/*` or `*`, every member at least one and no pattern on two members (§12.1). Required on a group, refused on another kind; a group has no `registry` and no `policy`. The bootstrap creates the groups after every other repo of the list. Example: `{members: [internal, npmjs]}`. |
| `repos[].anonymousRead` | bool | `false` | none | Readable without credentials, when `auth.anonymousRead` is `true`. |
| `repos[].policy` | map | none | none | The repo's policy, with the keys and values of the management API ([api.md](api.md#policy)): `maxFileSize` (bytes; by default npm 512 MiB, pypi and maven 4 GiB, apt and raw 8 GiB, oci 16 GiB), `blobRedirect` (`auto` by default, or `never`), and on a maven repo `maven.sessionTimeout` (`30m` by default, `1m` to `24h`), `maven.maxSessionFiles` (`256` by default, `1` to `999`: the files one deploy's session may store) and `maven.versionPolicy` (`release` by default, or `snapshot`: a repo of SNAPSHOT builds, which takes no release). The API checks it when the bootstrap creates the repo, and a policy it refuses stops `serve` naming the entry and the key (`repos[0]: policy.maven: only a maven repo has it, not a raw repo`). Example: `{maxFileSize: 104857600, maven: {sessionTimeout: 2h, maxSessionFiles: 512}}`, or for a repo of SNAPSHOT builds `{maven: {versionPolicy: snapshot}}`. |

`(format, name)` must be unique across `repos`.
