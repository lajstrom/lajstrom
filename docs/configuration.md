# Configuration

Lajstrom reads one YAML file. `serve`, `migrate` and `reindex` load and validate it before they
do anything else; `version` and `admin` do not need it.

```sh
lajstrom serve --config /etc/lajstrom/lajstrom.yaml
LAJSTROM_CONFIG=/etc/lajstrom/lajstrom.yaml lajstrom serve
```

`--config` defaults to `lajstrom.yaml` in the current directory, or to `$LAJSTROM_CONFIG` when
that is set. A commented example that works with the dev stack is
[deploy/compose/lajstrom.example.yaml](../deploy/compose/lajstrom.example.yaml); `make run` uses
it when `deploy/compose/lajstrom.yaml` does not exist.

## How the configuration is built

Later steps win:

1. **Defaults** (the Default column below).
2. **The file.** Keys are camelCase. An unknown key is an error that names the key and the
   line, so a typo never passes silently. An empty file is allowed and leaves the defaults.
3. **Environment overrides** for the scalar keys that have a variable in the Env column. An
   empty variable counts as unset. Lists (`registries`, `repos`) cannot be set from the
   environment.
4. **Secrets**: each `registries[].auth.passwordEnv` is read from the environment.
5. **Validation.** Every problem is reported at once, one line each, starting with the key:

   ```text
   lajstrom: config: lajstrom.yaml: invalid:
   server.listen: must not be empty
   repos[0].registry: "zot" is not a configured registry
   ```

The `--log-level` and `--log-format` flags override `log.level` and `log.format` for one run.

## Secrets

Secrets never go into the file. Keys ending in `Env` name the environment variable that holds
the secret: `registries[].auth.passwordEnv` and `keys.masterKeyEnv`. A `password` key in the file
is rejected. Lajstrom never logs a secret or puts one in an error message.

## What `serve` needs

Only `server.listen`. Everything else has a default or is optional:

```yaml
server:
  listen: ":8080"
```

Until issue #10 lands, `serve` does not open the database: it validates its configuration and
serves the system endpoints (`/-/healthz`, `/-/readyz`, `/-/version`). It does contact the
registries: at startup it asks each one what it supports (see
[capabilities](#registry-capabilities)) and logs the answer at info. A registry that does not
answer within 10 s is logged at error and `serve` starts anyway, so that one backend down does not
keep a replica from starting.

## Reference

### server

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `server.listen` | string, `host:port` | none, required | `LAJSTROM_SERVER_LISTEN` | TCP address to listen on. `":8080"` listens on every interface, `"127.0.0.1:8080"` on loopback only. |
| `server.externalURL` | `https://<host>[:port]` | empty | `LAJSTROM_SERVER_EXTERNAL_URL` | Base of every absolute URL Lajstrom renders: token realm, redirects, simple index links ([http-routing.md](spec/http-routing.md) §1). No path, query or fragment: Lajstrom owns the whole host. `http://` only for the dev stack on loopback (the `server.insecureHTTP` switch and TLS keys come with #50). Example: `https://art.example.com`. |
| `server.shutdownTimeout` | duration (`30s`, `2m`) | `30s` | none | How long in-flight requests get to finish after SIGINT or SIGTERM before their connections are closed. Must be positive. |

### log

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `log.level` | `debug`, `info`, `warn`, `error` | `info` | `LAJSTROM_LOG_LEVEL` | Lowest level that is logged. Flag: `--log-level`. |
| `log.format` | `text`, `json` | `text` | `LAJSTROM_LOG_FORMAT` | `text` for terminals, `json` for log collectors. Flag: `--log-format`. |

The `log` section is an addition to the shape in [layout.md](spec/layout.md) §6. Logs go to
standard error. Every request is logged once with method, path (never the query string),
status, bytes, duration, request id and remote address.

### database

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `database.driver` | `sqlite`, `postgres` | `sqlite` | `LAJSTROM_DATABASE_DRIVER` | Engine of the index database ([index-schema.md](spec/index-schema.md)). |
| `database.dsn` | string | `file:lajstrom.db` | `LAJSTROM_DATABASE_DSN` | Data source name. SQLite: `file:/var/lib/lajstrom/lajstrom.db`. Postgres: `postgres://lajstrom@db:5432/lajstrom?sslmode=require`. A DSN with a password belongs in `LAJSTROM_DATABASE_DSN`, not in the file. |

### registries

A list; each entry is one backend OCI registry. Repos refer to it by name.

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `registries[].name` | name | none, required | none | Unique; lowercase letters and digits separated by single `.`, `_` or `-`, at most 64 characters. Example: `local`, `ecr-prod`. |
| `registries[].type` | `generic`, `ecr` | `generic` | none | `generic` covers distribution, Zot, Harbor and GHCR; `ecr` is Amazon ECR (issue #36). |
| `registries[].url` | absolute `http(s)` URL | none, required | none | Base URL of the registry, e.g. `http://localhost:5000`. Must not contain credentials. |
| `registries[].prefix` | string | empty | none | Instance prefix of every OCI repository Lajstrom creates, e.g. `art/` gives `art/raw/scratch/…`. Must not start with `/` and must end with `/`. Clients never see it. |
| `registries[].auth.username` | string | empty | none | User for the registry. Set together with `passwordEnv`; leave both empty for anonymous access. |
| `registries[].auth.passwordEnv` | environment variable name | empty | none | Variable that holds the password. Startup fails when it is set here but the variable is unset or empty. Example: `ZOT_PASSWORD`. |
| `registries[].timeout` | duration (`30s`, `1m`) | `30s` | none | Bounds one request to the registry up to its response headers: connecting and the TLS handshake (each at most 10 s) and the wait for the headers. A blob upload or download streams for as long as it takes. Must not be negative; `0` means the default. |
| `registries[].retries` | integer | `3` | none | Retries of a failed request: 408, 429, 5xx, timeouts and connection resets, with exponential backoff from 250 ms (or the `Retry-After` of a 429), at most 3 s. Only requests whose body can be sent again are retried; a streamed blob upload is sent once. `0` means the default; a negative value disables retries. |
| `registries[].capabilities.createOnPush` | bool | probed | none | Pushing to a new repository creates it. |
| `registries[].capabilities.referrers` | bool | probed | none | The registry serves the OCI referrers API. `false` makes Lajstrom use the `sha256-<digest>` tag schema without asking. |
| `registries[].capabilities.catalog` | bool | probed | none | The registry lists repositories (`/v2/_catalog`). `false` turns repository listing off without asking, for registries whose catalog is missing, slow or wrong; Lajstrom then uses its own list of repositories. |
| `registries[].capabilities.deleteManifest` | bool | probed | none | Manifests can be deleted. |
| `registries[].capabilities.immutableTags` | bool | probed | none | The registry itself refuses to re-point a tag. |
| `registries[].capabilities.blobRedirect` | bool | probed | none | Blob downloads may be redirected to the registry's storage. `false` makes Lajstrom stream every blob through itself without asking the registry for a redirect. |
| `registries[].capabilities.maxBlobSize` | bytes | `0` | none | Largest blob the registry accepts; `0` means unknown. Must not be negative. |

#### Registry capabilities

At startup Lajstrom asks each registry what it supports, once, and keeps the answer: `GET /v2/`
(the registry answers, and accepts the credentials), `GET /v2/_catalog?n=1` (a catalog: 200 yes;
401, 403, 404, 405 no) and the referrers of a digest no manifest has in the repository
`<prefix>lajstrom-probe` (an OCI image index: the referrers API; a 404: the tag schema). The
`generic` type assumes create-on-push, manifest deletes and blob redirects, and no immutable
tags; a registry that answers a manifest delete with 405 turns `deleteManifest` off from then on.
Each key under `capabilities` replaces what was found, for registries that answer wrongly; a
key that is set is not probed. There is no `catalog` key outside `capabilities`. What was tested
against which registry is in [registries/](registries/README.md).

### auth

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `auth.anonymousRead` | bool | `false` | none | Server-wide switch for anonymous read. When `false`, no repo is readable without credentials, whatever `repos[].anonymousRead` says. Enforced by RBAC (issue #45, [rbac.md](spec/rbac.md)). |
| `auth.tokenSigningKeyFile` | path | empty | none | PEM file with the key that signs OCI bearer tokens (issue #26). Example: `/etc/lajstrom/jwt.key`. |

### keys

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `keys.masterKeyEnv` | environment variable name | empty | none | Variable that holds the master key, which encrypts signing keys at rest (issue #41). Example: `LAJSTROM_MASTER_KEY`. |

### repos

Optional. Repos to create at startup when they do not exist (the config bootstrap of issue #14);
repos created through the API are not listed here.

| Key | Type | Default | Env | Description |
| --- | --- | --- | --- | --- |
| `repos[].name` | name | none, required | none | Repo name, unique within its format; same rule as registry names ([manifest-profile.md](spec/manifest-profile.md) §4.1). Example: `scratch`. |
| `repos[].format` | name | none, required | none | Plugin name: `raw`, `pypi`, `apt`, `maven`. |
| `repos[].registry` | name | none, required | none | One of `registries[].name`. |
| `repos[].anonymousRead` | bool | `false` | none | Readable without credentials, when `auth.anonymousRead` is `true`. |

`(format, name)` must be unique across `repos`.
