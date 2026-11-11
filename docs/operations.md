# Operations

What running Lajstrom asks of an operator beyond the [configuration](configuration.md). The
dev stack is in [dev-stack.md](dev-stack.md).

## Deployment

Lajstrom runs behind an edge that terminates TLS: [reverse-proxy.md](reverse-proxy.md) has what
the edge must do (`server.externalURL`, uploads, forwarded headers, timeouts) and the rate limits
that belong there, with an nginx and a Caddy configuration.

### What Lajstrom answers browsers

Lajstrom's clients are package managers, but a person may open a URL of it in a browser, and a
browser keeps the HTTP Basic credentials it was given. So every answer of the main listener
carries, whatever its route or status (#63):

| Header | Value | Why |
| --- | --- | --- |
| `X-Content-Type-Options` | `nosniff` | a browser takes the type sent, and never guesses HTML in a file |
| `Content-Security-Policy` | `default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'` | a raw file uploaded as `text/html` or `image/svg+xml`, or a POM served as XML, runs no script, loads nothing, submits no form and is framed by no page, on the registry's own origin |
| `Referrer-Policy` | `no-referrer` | a link followed from a PyPI simple page does not send the registry's URL elsewhere |

and no `Server` header. A request that changes something (not `GET`, `HEAD` or `OPTIONS`) and
that a browser sends from another site, by its `Sec-Fetch-Site` or `Origin`, is refused with
`403` before its credentials are checked: a page an admin visits cannot submit a form to
`/api/v1` with the admin's cached credentials. Package clients send neither header and pass.
`Strict-Transport-Security` is the edge's to set, with the certificate. The threat model is
[security/threat-model.md](security/threat-model.md).

## Users

Users live in the index database, not in the configuration file; [auth.md](auth.md) has the
details. Every `lajstrom admin user` command works on the database of the configuration,
without HTTP, and is audited with the source `cli`.

### The first admin

No admin password goes in the environment or the configuration ([auth.md](auth.md#bootstrap-the-first-admin)).
The first admin is made with a one-time bootstrap token that you make; `serve` never makes or
logs one:

1. **The token.** `openssl rand -hex 32`, in a secret mounted as a file (a Kubernetes Secret, a
   Compose secret), named in `auth.bootstrap.tokenFile` (`LAJSTROM_BOOTSTRAP_TOKEN_FILE`). While
   no enabled user has an admin binding, `serve` makes it valid at start on every replica, for
   `auth.bootstrap.ttl` (24 hours). Without the file, a warning at start says what to do.
2. **The exchange**, from anywhere that reaches the server:

   ```sh
   lajstrom admin bootstrap --server https://art.example.com --name admin
   ```

   It prompts for the token and the password, or takes `--token-file` and `--password-stdin`.
3. **After it**, the token is dead and `POST /api/v1/bootstrap` is `404`. Remove the secret: a
   start warns while its file exists. Check the admin with `curl -u admin
   https://art.example.com/api/v1/whoami`.

With the database at hand no token is needed: `lajstrom admin user create --name admin --admin`,
with the server's configuration, makes the admin straight in the database, before or after the
first start of `serve`. The password comes from standard input with `--password-stdin` or from a
prompt on a terminal.

### A lost password

The database is the authority: whoever can run `lajstrom admin` with the server's configuration
can recover any account, with no password and no running server.

```sh
lajstrom admin user set-password alice --config lajstrom.yaml       # a prompt, or --password-stdin
lajstrom admin user create --name rescue --admin --config lajstrom.yaml
```

`set-password` works on a disabled user too; the old password fails at the next request on every
replica. `create --admin` makes a new admin when every admin is lost, disabled, or forgotten
together with its password. Each writes an audit row (`user.password`, `user.create`) by
`anonymous` with the source `cli` and the host name of the machine it ran on, so that a recovery
shows in `GET /api/v1/audit` afterwards. In Kubernetes, run the command in a pod with the
configuration and the database DSN of the Deployment: the image has no shell, and `kubectl exec
-i deploy/lajstrom -- /lajstrom --config /etc/lajstrom/lajstrom.yaml admin user set-password
admin --password-stdin` runs the binary itself.

The bootstrap token is not a way back in: it is valid only while no enabled user has an admin
binding. This is the Gitea, GitLab and Grafana way (`gitea admin user change-password`,
`gitlab-rake "gitlab:password:reset"`, `grafana cli admin reset-admin-password`, each against
the database), not Vault's quorum of key holders: [auth.md](auth.md#how-others-do-it) compares
them.

### Disabling a user

```sh
lajstrom admin user disable alice
lajstrom admin user enable alice
```

A disabled user's credentials are refused with `401` from the next request on, on every
replica: the user is read from the database on every request, and the cache of verified
passwords never outlives a change. `lajstrom admin user list` shows who is disabled. A disabled
user's personal access tokens are refused too. To remove a user for good, `lajstrom admin user
delete alice` (or `DELETE /api/v1/users/alice`) revokes its tokens, deletes its bindings and
frees its name; it is not undone, and the audit rows keep the name.

### A credential for CI

A CI job gets a deploy token: a principal of its own, bound to one repo, with `write` (and
`read` when the job also reads, as an OCI push through `/v2` does), never `delete` or
`manage` ([auth.md](auth.md#tokens)):

```sh
lajstrom admin token create --repo raw/assets --name "ci $(date -u +%Y-%m)" --scopes read,write --expires 90d
```

or `POST /api/v1/repos/raw/assets/deploy-tokens` with `{"name": "ci 2026-11", "scopes":
["read", "write"], "expiresIn": "90d"}`. Put the `secret` of the answer into the CI system's
secret store at once: it is shown this once. The job sends it as Basic credentials,
`deploy:<secret>` (`curl -u`, `oras login -u deploy --password-stdin`, `auth.conf` with
`login deploy`); a client that insists on another user name may send any.

To rotate it, create the next token, switch the CI secret to it, check its `LAST USED` in
`lajstrom admin token list --repo raw/assets`, and revoke the old one by its id:

```sh
lajstrom admin token revoke 41
```

A revoked token is refused from the next request on, on every replica. The names of live
tokens are unique per repo, so a date in the name keeps the old and the new apart.

### Failed logins and the edge

Lajstrom throttles what only it can see ([auth.md](auth.md#failed-logins)): failed password
logins per account (10 a minute) and per client address (30 a minute), and requests to
`/v2/token` per client address (100 a second); beyond, `429` with `Retry-After`. The per-address
throttles need `server.trustedProxies`, so that the address is the client's and not the edge's;
with an empty list they are off, and the startup line says so. Without an edge, with
`server.tls`, the peer is the client and they are on with an empty list too (#248).

The counts live in the memory of each replica and are not shared: N replicas are N budgets, and
a restart starts over. That makes guessing a password slow and keeps argon2id from eating a
replica; it is not a bound across replicas, and it is no volume limit. Both belong to the edge:
a request limit per client address there, on every route, is the bound across replicas, and the
one that holds when the per-address throttle is off. Each throttled request is a `throttled`
line at info with the throttle, the account's uid when it is an account's, and the client
address, and a count in `lajstrom_auth_throttled_total`: an account that keeps showing up is
being guessed, or has a stale password in a job somewhere.

## Keys and apt signing

Every apt repo is signed with a key of its own: `InRelease` is the `Release` clearsigned,
`Release.gpg` its detached signature, and `GET /apt/<repo>/key.asc` serves the public key that
clients pin with `signed-by=` ([http-routing.md](spec/http-routing.md) §3). One more key signs
the bearer tokens of `/v2` ([below](#the-bearer-token-key)). The keys are made and kept by
Lajstrom, encrypted with one master key that only the operator holds.

### The master key

The master key is 32 random bytes written as 64 hexadecimal characters:

```sh
openssl rand -hex 32
```

It lives in the environment variable that `keys.masterKeyEnv` names (`LAJSTROM_MASTER_KEY` in
the examples), or in a file of its own that `keys.masterKeyFile` (`LAJSTROM_MASTER_KEY_FILE`)
names, never in the configuration file, and every replica gets the same value. Setting the file
and the variable both is a configuration error. `serve` and `lajstrom admin key` read it when
they start. A variable or a file that is empty, or is not 64 hexadecimal characters (spaces and
line breaks around them are ignored), stops them with an error that names the variable or the
file and the format, never the value; so does a file that is missing or cannot be read. A file
that the group or others may read or write is used, with a warning: make it `0400` or `0600`.
Without a master key (neither is set, or the variable is not) signing is
off: `serve` logs a warning for every apt repo, `InRelease`, `Release.gpg` and `key.asc` are
`404`, and clients have to mark the source `[trusted=yes]`; bearer tokens are signed with an
ephemeral key ([below](#the-bearer-token-key)). Once the index holds apt signing keys, `serve`
refuses to start without the master key: those keys are useless without it, and the clients
that pinned them would see the repos turn unsigned. The token key does not count: no client
pins it.

With Docker Compose the master key is a Compose secret, the file
`/run/secrets/lajstrom-master-key` of `keys.masterKeyFile`; the file on the host must belong to
the image's user 65532 ([deploy/compose.md](deploy/compose.md#1-secrets)).

In Kubernetes the master key is a Secret, passed to the container as the variable:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: lajstrom-master-key
stringData:
  LAJSTROM_MASTER_KEY: "<the output of openssl rand -hex 32>"
---
# in the Deployment's container
env:
  - name: LAJSTROM_MASTER_KEY
    valueFrom:
      secretKeyRef:
        name: lajstrom-master-key
        key: LAJSTROM_MASTER_KEY
```

A Secret mounted as a volume would be a file, but the kubelet makes its files owned by root:
with `defaultMode: 0400` the user 65532 cannot read it, and with `fsGroup: 65532` it becomes
`0440` (tried on kind, Kubernetes 1.37), which Lajstrom warns about. The variable is the cleaner
fit there.

Keep a copy of the master key apart from the database backups: a backup of the index holds the
private keys only as ciphertext (`keys.private_enc`, AES-256-GCM), and the master key alone
holds nothing. **Losing the master key loses every repo key**: no signature can be made with
them any more. Recover with a new master key and a rotation of every apt repo (below), then
distribute the new `key.asc` to every client at once: the retired keys cannot sign beside the
new ones, so clients that pinned only the old key fail until they have the new one. The token
key is lost with it too: `serve` refuses to start with a token key it cannot decrypt, and
`lajstrom admin key drop --token` under the new master key lets the next start make a new one.

### Repo keys

A repo gets its key on first use: the first `GET` of its `InRelease`, `Release.gpg` or
`key.asc`, or `lajstrom admin key export`. The key is an OpenPGP key of `keys.algorithm`:
`ed25519` by default (EdDSA, which apt verifies since 2.0: Debian 11, Ubuntu 20.04 and later),
or `rsa4096` for older clients; the user id is `Lajstrom apt/<repo>`, it does not expire, and
it signs with SHA-256. The private key is stored encrypted with the master key, its fingerprint
as the additional data; the public key and the fingerprint are stored as they are
([index-schema.md](spec/index-schema.md), `keys`). Replicas that make a repo's first key at the
same moment end up with one: the index keeps one key that is not retired per repo, and the
replica whose insert loses reads the other's key.

### Clients

A client pins the repo's `key.asc` and names it in its source. Nothing is open by default, so
the client reads with a deploy token of the repo with `read`, `APT_TOKEN` here (`lajstrom admin
token create --repo apt/ubuntu-internal --name "apt clients" --scopes read`): apt takes it from
`/etc/apt/auth.conf.d`, as the password of the login `deploy`, and `curl` with `-u`. Without it
`curl -f` fails with `401` and leaves an empty keyring behind, and `apt-get update` fails with
`401`. A repo with anonymous read needs neither.

```sh
printf 'machine art.example.com/apt/ubuntu-internal\nlogin deploy\npassword %s\n' "$APT_TOKEN" |
  sudo tee /etc/apt/auth.conf.d/ubuntu-internal.conf > /dev/null
sudo chmod 600 /etc/apt/auth.conf.d/ubuntu-internal.conf
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL -u "deploy:$APT_TOKEN" https://art.example.com/apt/ubuntu-internal/key.asc | sudo tee /etc/apt/keyrings/ubuntu-internal.asc > /dev/null
echo "deb [signed-by=/etc/apt/keyrings/ubuntu-internal.asc] https://art.example.com/apt/ubuntu-internal jammy main" |
  sudo tee /etc/apt/sources.list.d/ubuntu-internal.list
sudo apt-get update
```

The general form is
`deb [signed-by=/etc/apt/keyrings/<repo>.asc] https://host/apt/<repo> <suite> <component>`.
A `machine` without a scheme is used for `https` only; over plain `http` apt sends the
credentials only to a `machine http://…` ([quickstart/apt.md](quickstart/apt.md#4-install-it-with-apt-get-a-04)
runs this on `http://localhost:8080`).
The apt of Ubuntu 22.04 and 24.04 and of Debian 12 reads the armored `.asc` in `signed-by` as
it is (the integration tests run all three); for an older apt, `gpg --dearmor` makes the binary
`.gpg` form. For configuration management, `lajstrom admin key export --repo apt/<repo>` prints
the same keyring.

### Rotation

```sh
lajstrom admin key rotate --repo apt/ubuntu-internal
```

prints the fingerprint of the new key. In one transaction the repo's key is retired, the new
one becomes active, the repo's cached indexes are dropped, so that the next `GET` renders and
signs again, and an audit row `key.rotate` names the new fingerprint (the retired one in its
details). For `keys.retireGrace` (default `720h`, 30 days) after a rotation the retired key
still signs `InRelease` and `Release.gpg` beside the new one and stays in `key.asc`:

- a client that pinned `key.asc` before the rotation verifies the new `InRelease` through the
  retired key's signature;
- a client that fetches `key.asc` after it gets both keys, and verifies through either;
- an `InRelease` signed before the rotation, from a mirror or a cache, verifies against the new
  `key.asc`.

When the grace ends the retired key leaves `key.asc` and stops signing. The signed `InRelease`
and `Release.gpg` are cached, so every replica checks a cached one before it serves it: the next
request after the end of the grace, on any replica, signs them again with the keys that still
sign. Nothing else needs to happen: no publish, no reindex, no reconcile loop. The `Release` and
the `Packages` stay as they are; the new `InRelease` has a newer `Last-Modified`, so `apt-get
update` fetches it. From then on a client that still has only the old key fails `apt-get update`
with `NO_PUBKEY`.

The runbook:

1. `lajstrom admin key rotate --repo apt/<repo>`; note the fingerprint it prints.
2. Distribute the new `key.asc` to every client: fetch it again into `/etc/apt/keyrings/`, or
   roll out `lajstrom admin key export --repo apt/<repo>` with configuration management.
   `gpg --show-keys /etc/apt/keyrings/<repo>.asc` lists the new fingerprint.
3. Finish before `keys.retireGrace` runs out.

Rotate when the algorithm changes (`keys.algorithm` applies to keys made from then on), when a
new master key replaces a lost one, and when a key may have leaked. For a leaked key, set
`keys.retireGrace: 0s` first and restart the replicas, which read it at start, then rotate: the
retired key stops signing and leaves `key.asc` at once, and clients fail until they have the new
`key.asc`. When the rotation came first, the restart is enough: the grace of the retired key is
over, and the next request for an `InRelease` or a `Release.gpg` signs it again without that
key.

### The bearer-token key

The bearer tokens of `/v2` ([auth.md](auth.md#bearer-tokens-on-v2)) are signed with one server
key ([ADR 0010](adr/0010-bearer-token-signing-key.md)): a row of `keys` with the kind `jwt` and
no repo, its private key encrypted with the master key as the repo keys are, the lowercase
hexadecimal sha256 of its public key as its fingerprint and as the `kid` of every token. It is
the same on every replica because it lives in the database: a token one replica issues verifies
on the others, and nothing is mounted, generated per node or restarted to rotate it.

- **Made at startup**: `serve` loads it before it listens, or makes it when there is none (two
  replicas that start at once end up with one, as for a repo key), and logs its fingerprint and
  algorithm at info. `keys.tokenAlgorithm` (`ES256`, the default, or `EdDSA`) is the algorithm
  of the keys made from then on; a key keeps its own, so a change of algorithm is a rotation.
- **Without a master key**: `serve` signs with an ephemeral key in memory and warns that tokens
  do not survive a restart and are not shared between replicas. That is fine for one process;
  set `keys.masterKeyEnv` for anything with replicas or restarts that clients should not notice.
- **A key that does not decrypt** (a database restored under another master key) stops `serve`
  with an error that names it. Start with the right master key, or retire it with
  `lajstrom admin key drop --token` and start again: a new key is made.
- **Rotation**: `lajstrom admin key rotate --token` makes a new key, which signs from then on on
  every replica, and retires the old one, which still verifies for 21 minutes, the token
  lifetime and the clock skew, so that no token issued before the rotation is refused before it
  expires. Retired rows stay in the table, ignored once retired longer than that ago. The
  replicas reload the keys at most once a second, so for up to a second every replica, the one
  that made the key included, may refuse a token of the new key; a client that asks for
  another after that gets one that passes. A rotation is an audit row
  `key.rotate` without a repo, the new fingerprint as its subject, the retired one in its
  details. Rotate with the master key's rotation, when the algorithm changes, or at will:
  nothing else needs to happen.
- **Drop**: `lajstrom admin key rotate --token --drop` is the same with no grace: the old key,
  and a key that a rotation retired less than 21 minutes before, stop verifying within a second
  on every replica and every outstanding token dies, for a key that may have leaked or when
  every token must go now (a user who must lose access to `/v2` before their token expires).
  Clients get new tokens with their credentials at their next request. Its audit row says
  `"dropped": true`.
- **Export**: `lajstrom admin key export --token` prints the public key as PEM, for a verifier
  that is not Lajstrom, should one ever be needed.

The keys of the server can be listed with
`sqlite3 lajstrom.db "SELECT id, fingerprint, created_at, retired_at FROM keys WHERE kind = 'jwt'"`
(or the same query on Postgres). The active key has no `retired_at`; a dropped key's is 21
minutes before the drop, so that it verifies nothing.

### `lajstrom admin key`

Every subcommand needs the configuration, the master key (in the variable `keys.masterKeyEnv`
names, or in the file of `keys.masterKeyFile`), and a migrated database; none goes through HTTP.

| Command | What it does |
| --- | --- |
| `admin key export --repo apt/<repo>` | prints the repo's armored keyring, what `key.asc` serves; makes the key when there is none |
| `admin key rotate --repo apt/<repo>` | gives the repo a new key, retires the old one for `keys.retireGrace`, drops the repo's cached indexes; prints the new fingerprint |
| `admin key export --token` | prints the public bearer-token key as PEM; makes the key when there is none |
| `admin key rotate --token` | a new bearer-token key; the old one verifies for 21 minutes; prints the new fingerprint |
| `admin key rotate --token --drop` | the same, and no old key verifies anything from then on, one still in its 21 minutes included |
| `admin key drop --token` | retires the bearer-token key without a successor, whether it decrypts or not (the next token a replica issues, or the next start of `serve`, makes one); prints its fingerprint |

`--repo` and `--token` exclude each other, and one of them is required. Without the master key
the token commands fail and say that `serve` signs with an ephemeral key then.

## Readiness

`/-/readyz` answers `200 ok` while the index database answers a ping, no other replica serves on
it when it is SQLite, the replica's clock is within 60 seconds of the database's
([Clocks](#clocks)), and every registry answered `GET /v2/`, with Lajstrom's credentials, within
the last minute; otherwise `503` with a line that names no host, user or registry (`index
database is unreachable`, `another replica serves on this SQLite database`, `the clock of this
replica is 1m15s ahead of the database's`, `a registry has not answered for a minute`); the log
has the cause, a registry's once per outage. `serve` pings every registry at start and every 20
seconds after, each ping bounded by 10 seconds, so a registry
that stops answering takes the replicas out of the edge after about a minute, and they come
back at the first answer. `/-/healthz` is only "the process is up": point the liveness probe
there and the readiness probe at `/-/readyz`, so that an outage of the registry or the database
does not restart the pods.

While the database does not answer (Postgres stopped, restarting or refusing connections;
SQLite locked past its busy timeout), a request that needs the index, its authentication with
credentials included, is answered `503` with `Retry-After: 5` (on `/v2` and the API with the
code `UNAVAILABLE`, which docker retries), and logged with the cause. The replicas serve again at
the database's first answer, without a restart.

Where the check must run inside the container, as a Docker or Compose `healthcheck:` (the image
has no shell and no HTTP client), `lajstrom healthcheck` is the client: one `GET` of
`/-/readyz`, or of `/-/healthz` with `--live`, exit status 0 on `200` and 1 otherwise, one line
either way, 3 seconds at most (`--timeout`). `--url http://127.0.0.1:8080` names the server and
nothing else is read; an `https` URL is checked against the system's roots. Without it the
server is `server.listen` of the configuration on loopback, and only that key,
`LAJSTROM_SERVER_LISTEN` and `server.tls` are read, never a secret.

With `server.tls` set, serve speaks TLS only, and the check speaks `https` to it. The
certificate is pinned: the first certificate of `server.tls.certFile`, the file serve loaded,
is the only root the check trusts, and the server must present it, byte for byte. The check
goes to loopback while the certificate names the public host, so it verifies the name against
one that the certificate carries (its first DNS name, else its first address); the dates count
as for any client, so an expired certificate fails the check. The check reads that file and
never the key. A certificate renewed on disk fails the check until serve is restarted, which is
what loads it (`server.tls.certFile` in [configuration.md](configuration.md#server)), so the
orchestrator's restart picks it up. [deploy/compose.md](deploy/compose.md#the-health-check)
uses the check.

## Clocks

Claims on tags and fetches, reindex runs and bearer tokens compare times that other replicas
wrote: a claim is stale after `ingest.claimTTL`, a fetch claim or a run 2 minutes after its last
heartbeat, and a token is checked with 60 seconds of skew. So the clocks of the replicas must
agree with each other and with the database's within 60 seconds
([guarantees.md](spec/guarantees.md), clocks agree), and should agree within a second.

Run NTP on every host that runs `serve`, the database and the CLI: chrony (`chronyc tracking`
shows the offset, `chronyc sources` the servers), or systemd-timesyncd (`timedatectl
timesync-status`). A container has the clock of its host, so on Kubernetes and Compose it is the
nodes' that must be synchronised, and a managed database keeps its own.

`serve` compares its clock with the database's when it starts and at every heartbeat of its
row in `replicas`, every 30 seconds ([index-schema.md](spec/index-schema.md#coordination)).
Past 10 seconds it logs a warning at every comparison, past 60 seconds an error, and
[`/-/readyz`](#readiness) answers `503 the clock of this replica is 1m15s ahead of the
database's` (or `behind`) until the next comparison within the bound: the edge takes the
replica out, while its requests in flight and its background work (the reconcile loop) go on.
The comparison is the replica's time at the middle of a `SELECT clock_timestamp()` minus the
time it returns, so it is exact to half the round trip of the query.

The metric `lajstrom_clock_skew_seconds` is the last comparison: this replica's clock minus the
database's, in seconds, positive when the replica is ahead. Two replicas disagree by the
difference of their values. An alert that fires before readiness does:

```yaml
- alert: LajstromClockSkew
  expr: abs(lajstrom_clock_skew_seconds) > 10
  for: 5m
```

On SQLite the database runs in the process, its clock is the host's, and the skew is 0.

## Shutdown

On `SIGTERM` or `SIGINT`, `serve` drains: `/-/readyz` answers `503 shutting down`, the listener
closes, so new connections are refused, and idle keep-alive connections are closed. Requests in
flight, uploads included, get `server.shutdownTimeout` (default `30s`) to finish and are answered
as usual, each on a connection that closes after its response; the process exits `0` once the
last one is done. At the deadline, the requests still running are aborted: their contexts are
cancelled, which stops their requests to the registry, and their connections closed; the
process waits up to 5 s for their handlers to return, closes the index and exits with an error
that says so. An upload aborted that way leaves nothing behind that another replica cannot
handle: an OCI upload session lives in the index and in the registry, so the client resumes it
or starts over on another replica; a publish cut off before its manifest is in the registry
published nothing, and one cut off after it is indexed by the next reconcile; a claim on a tag
expires after `ingest.claimTTL`. A reindex run in the background is cancelled at the
signal and finishes as failed. A second signal stops the process at once, without the drain.

Set `server.shutdownTimeout` from the longest upload a replica should finish, and the grace
period of the orchestrator above it, or it kills the process before the drain ends:
`terminationGracePeriodSeconds` on Kubernetes (30 s by default), `stop_grace_period` in Compose
(10 s by default). On Kubernetes the endpoint of a terminating pod is removed while the pod
gets `SIGTERM`, not before: a `preStop` hook that sleeps a few seconds keeps the listener open
until the edge has stopped sending new requests to it. `deploy/kubernetes` sleeps 5 seconds
(the `sleep` action, Kubernetes 1.30 and later; the image has no shell), which a rolling update
on a cluster needed ([deploy/kubernetes.md](deploy/kubernetes.md#upgrade)); the grace period
counts the sleep too.

## Upgrades

A release may migrate the index database. Its migrations are expand or contract
([index-schema.md](spec/index-schema.md#upgrades)): after an expand migration the release
before still runs on the schema, so a rolling update and a rollback work; a contract migration
removes what the release before it stopped using, and its release notes say so
([release.md](release.md#migrations-in-the-notes)). Before every upgrade:

- **Back up the database** ([Backing up](#backing-up)). Past a contract migration, the backup is
  the only way back.
- **Read the notes of every release you skip.** Skipping releases is fine while their
  migrations are expand. A contract migration requires a release before it: from an older one,
  `serve` and `lajstrom migrate` refuse with `a pending migration would break the replicas
  serving on the schema`, apply nothing, and the old replicas keep serving. Upgrade through
  the release it requires, or stop every replica and run `lajstrom migrate --allow-breaking`.

### Several replicas

Run `lajstrom migrate` of the new release once, then roll the replicas. The replicas of the
release before keep serving on the migrated schema, and the new ones find nothing to apply.
`serve` still migrates on start, under the same lock, so a replica that starts before the
migration ran does it itself; a separate run is better for three reasons: a failing migration
stops the upgrade before any replica changes, a long one is not killed by the startup probe
(`deploy/kubernetes`: 30 tries, 2 s apart), and its output is in one place. On Kubernetes, the
Job [`deploy/kubernetes/migrate-job.yaml`](../deploy/kubernetes/migrate-job.yaml) is that run,
with the ConfigMap and the Secret of the Deployment. A Job's pod template cannot change, so each
upgrade deletes the one before:

```sh
kubectl delete job lajstrom-migrate --ignore-not-found
kubectl set image --local -f deploy/kubernetes/migrate-job.yaml \
  migrate=ghcr.io/lajstrom/lajstrom:v0.2.0 -o yaml | kubectl apply -f -
kubectl wait --for=condition=complete --timeout=30m job/lajstrom-migrate
kubectl logs job/lajstrom-migrate            # applied 0015
kubectl set image deployment/lajstrom lajstrom=ghcr.io/lajstrom/lajstrom:v0.2.0
kubectl rollout status deployment/lajstrom
```

A Job that fails (`kubectl wait --for=condition=failed`) applied nothing of the migration that
failed, and the replicas keep their image; its log says why. The run on a cluster, with the
outputs, is in [deploy/kubernetes.md](deploy/kubernetes.md#upgrade).

A Helm `pre-upgrade` hook or an Argo CD `PreSync` hook runs the same Job. Without either, an
init container that runs `lajstrom migrate` before `serve` does the same in each new pod: the
first applies the migrations, the others find nothing.

During the update both releases serve. A request answered by the old release behaves as
before; where the two differ in a way that matters (a lock one of them does not take, as the
claims of #244 in [index-schema.md](spec/index-schema.md#coordination)), the notes say so.
Finish one rolling update before the next.

A rollback (`kubectl rollout undo`) runs the release before on the newer schema. After expand
migrations it starts and logs `the index schema is newer than this binary, and compatible with
it`; so does a rollback to the release a contract migration requires. To an older release it
refuses to start: `the schema is newer than this binary: schema version 0016 needs a binary
that knows migration 0015, and this one knows up to 0014`. Its pods crash-loop, the rollout
stalls, and the replicas of the newer release keep serving (`maxUnavailable: 0`). Roll forward,
or restore the backup of before the upgrade ([Restoring](#restoring)), which
loses the changes to users, tokens and bindings made since.

### One node

With SQLite there is one replica: stop it, back up the file, start the new release. `serve`
applies every migration on start, contract ones included, since nothing else serves on the
file; `lajstrom migrate` before it shows what is applied. Going back to the release before
follows the rollback above. The same holds for one replica on Postgres, such as the Compose
stack, except that `serve` on Postgres does not apply a contract migration that requires more
than the schema has: run `lajstrom migrate --allow-breaking` once it is stopped.

### A database user for serve

On a current schema `serve` changes nothing in it, so on Postgres it can run as a user with data
rights only, and `lajstrom migrate` as the owner of the schema: no replica can then drop or
alter a table, and a replica of a new release whose migrations nobody ran refuses to start
instead of migrating. As an administrator, make the user; then, as the owner (the user of the
migration's `database.dsn`, here `lajstrom`, which owns the database), grant it the data:

```sql
-- as an administrator
CREATE ROLE lajstrom_serve LOGIN PASSWORD '<a password>';
-- as the owner, connected to the database
GRANT USAGE ON SCHEMA public TO lajstrom_serve;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO lajstrom_serve;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO lajstrom_serve;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO lajstrom_serve;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO lajstrom_serve;
```

The first three cover the tables the migrations made so far, the last two the ones the owner's
later migrations make. Before Postgres 15 every user may create tables in `public`: also
`REVOKE CREATE ON SCHEMA public FROM PUBLIC`. Give `serve` (and `lajstrom admin`, `reindex`)
a `database.dsn` with `lajstrom_serve`, and the migration's run the owner's. With a migration
pending, `serve` exits with `permission denied for schema public (SQLSTATE 42501); this database
user may not change the schema: run lajstrom migrate as its owner first`.

## Limits

What bounds the work of one request, towards the registries and from the clients.

### Requests to the registries

Each request to a registry waits at most `registries[].timeout` (default `30s`) for the response
headers, connecting and the TLS handshake included (each at most 10 s); a blob then streams for
as long as its size needs.

A request that fails in a way another attempt may fix is sent again, at most
`registries[].retries` times (default `3`): a `429`, a `5xx` other than `501` and `505`, a
timeout, a connection refused or reset. Never another `4xx`. Only idempotent requests are sent
again: `GET`, `HEAD`, `DELETE` and the `PUT` of a manifest, when their body can be sent again.
A `POST` that opens an upload session or mounts a blob, a `PATCH` that appends to one and the
`PUT` that closes it are sent once, and the error goes back to the client, which retries the
step. A blob that Lajstrom uploads itself (a file a frontend received, a metadata layer) is
uploaded again from the start, in a new session, when the upload failed that way: a blob is
content addressed, so uploading it twice is uploading it once. The wait before a retry doubles
from 250 ms, with up to half of it random, and is at most 3 s; a `Retry-After` of a `429` or a
`503` replaces it, and a registry that asks for more than 3 s gets no retry. A client that goes
away ends the wait. Each retry is a line at `debug` level, `registry request retried`, with the
method, the path, the attempt and the cause, never a header or the query; each attempt is its own
sample in `lajstrom_provider_request_duration_seconds`.

Each registry has a circuit breaker. Five requests in a row that the registry answers with a
`5xx` other than `501`, or does not answer, each after its retries, open it: for 30 s every
request to that registry fails at once instead of waiting for timeouts and retries. The client
gets `503` with `Retry-After`, the seconds until the trial below (on `/v2` with the code
`UNAVAILABLE`, which docker retries), and the log has `registry fails: circuit open` once, with
the registry's name. Then the next `GET` or `HEAD` goes through as a trial, normally the readiness ping of `serve`; an
answer closes the breaker (`registry answers again: circuit closed`), a failure opens it for
another 30 s. A request with a body is never the trial, and fails at once until the breaker is
closed. A `4xx` is an answer; a `429`, and a request whose client went away, count neither way.
`lajstrom_provider_circuit_open{registry}` is `1` while the breaker is open. The ping that it
refuses counts as unanswered, so a breaker that stays open for a minute turns
[`/-/readyz`](#readiness) to `503`, as a registry that does not answer does.

A registry whose host name does not resolve (its container stopped on a compose network, a
DNS outage) is unavailable from the first request on: the client gets `503` without
`Retry-After` (on `/v2` with the code `UNAVAILABLE`), and each such request counts as unanswered
for the breaker, which opens after five as above.

### An eventually consistent registry

Some registries answer `201` to a push before every read sees it: ECR, by the reports of tool
maintainers for 100 ms to several seconds, and GHCR now and then for a fraction of a second
([registries/README.md](registries/README.md#consistency)). Lajstrom does not index a version
until the registry serves it. After the manifest's `PUT` every publish reads the manifest back
by digest; on a registry not classified `strong`, its tag and its blobs too. What it does not
see it reads again, after 100 to 200 ms, then twice as long each time, at most 5 s a wait, for
[`ingest.visibilityTimeout`](configuration.md#ingest) (30 s), or the registry's
`capabilities.consistencyBound` when that is longer. On a registry that serves at once this
costs one `HEAD` per publish on a `strong` one, and a `HEAD` per blob and the tag more on the
others.

What an operator of ECR sees:

- At startup, `registry capabilities … consistency=eventual`, and a warning that no bound of the
  lag is known until `registries[].capabilities.consistencyBound` is set. `lajstrom registry
  probe` measures the lag of a registry of type `generic`; for ECR, #19 does.
- A publish the registry served after more than a second: `ingest: the registry served a push
  late` at info, with the time it took.
- A publish the registry did not serve in time: the client gets `503` with `Retry-After: 30`
  (on `/v2` the code `UNAVAILABLE`, which docker and oras retry), and the log has `ingest: the
  registry took a push and does not serve it yet` at warning, naming what was missing. The
  manifest stays in the registry and the index has no version. The claim on the tag stays as
  well, for `ingest.claimTTL`: another digest under the tag is `409`, and the same publish
  again completes it, also from a frontend that builds the manifest again with another
  `created` time: an upload of the same files takes the first manifest over. A client that
  never retries leaves the manifest for reindex to index.
- A referrer (a signature, an SBOM) waits twice as long, until the registry lists it among its
  subject's referrers too: Docker Hub's referrers list was seen a minute behind.
- An npm dist-tag or deprecation the registry did not serve in time is `503` the same way. Its
  journal entry stays claimed in the index; the next change of that package adopts it once the
  registry serves it, and answers `503` until then, or until the claim is stale.

### Requests from clients

| Bound | Value | What happens |
| --- | --- | --- |
| request headers | 10 s, fixed | a client that has not sent its headers in 10 s is disconnected |
| header size | 1 MiB, fixed | `431 Request Header Fields Too Large` |
| idle keep-alive connection | 120 s, fixed | the connection is closed |
| a body that stops moving | `server.stallTimeout`, `1m` | each read of a request body and each write of a response must move within it, or the connection is closed; the handler gets a read or write error |
| uploads at once | `server.maxConcurrentUploads`, 32 | `503` with `Retry-After: 5`, before the body is read |
| a manifest | `server.maxManifestSize`, 4 MiB | `413` |
| the layers of a manifest | 1 000, fixed | `400 MANIFEST_INVALID` naming the bound, on every repo, before Lajstrom asks the registry about any layer |
| a body of the management API | 64 KiB, fixed | `413` |
| a file a frontend receives | the repo's `policy.maxFileSize`; by default npm 512 MiB, pypi and maven 4 GiB, apt and raw 8 GiB | `413` |
| a blob of `/v2` | the repo's `policy.maxFileSize`, on an `oci` repo too (by default as for a file, 16 GiB for `oci`), or the registry's `capabilities.maxBlobSize` when it is smaller (GHCR's 10 GB from the vendor table); [below](#large-blobs-ml-models) | `413 SIZE_INVALID` naming the bound, before the blob streams when its size is declared (`Content-Length`, `Content-Range`), else where the bytes of the session pass the bound, counted across its `PATCH`es and the closing `PUT`; the session ends |
| live `/v2` upload sessions of a principal | `server.uploadSessions.perRepo`, 256 in one repo, and `server.uploadSessions.perPrincipal`, 1024 in all repos, on each replica | `429 TOOMANYREQUESTS` with `Retry-After` for a new session, naming the bound and its key. A session counts while a request on it runs and for 5 minutes after the last one; an idle one stays resumable until it expires, an hour after its last request. Its `DELETE`, its closing `PUT` and an error that ends it free the slot at once |
| the files of a Maven deploy | the repo's `policy.maven.maxSessionFiles`, 256 by default, in one publication session | `400` naming the bound and the key, before the file is stored; a file of the session sent again replaces it. Signatures and `.sigstore.json` count, checksum files and `maven-metadata.xml` do not; a deploy of a version that exists starts its own session |

There is no bound on the whole length of a request or a response: a blob streams for as long as
its size needs, which a fixed read or write timeout would cut off. `server.stallTimeout` bounds
what a slow or stuck client can hold instead (a connection, a goroutine, an upload slot, a
connection to the registry): only time in which no byte moves counts, and the time the server
waits for the registry or the index between two reads does not. A client behind a slow link
keeps sending and passes; one that stops sending, or opens a download and stops reading, is cut
off. The limit on uploads at once is per replica, 32 by default (#246): set it from what a replica
can hold at once, the disk for the spooled files of the frontends and the connections to the
registry; `0` lifts it. A request refused for it is in the access log with status `503`. How many requests a
client may send is the edge's to limit.

The bounds on live upload sessions count per replica too: a replica counts the sessions it has
answered a request of, so behind N replicas a principal may hold up to N times as many, and one
replica may refuse a session while another has room. Raise `server.uploadSessions.perRepo` when
more CI runners than about 50 push new layers into one repo with one credential at once; a
refused session is in the access log with status `429`.

### Large blobs: ML models

The default bound of a file or a blob is its format's: npm 512 MiB, pypi and maven 4 GiB, apt and
raw 8 GiB, `oci` 16 GiB, above the largest of each that the package size survey of #439 found (an
image layer of 10.78 GiB, rocm/pytorch). Machine-learning models are larger: one blob of 131 GiB
for a 70B model in fp16 (Ollama, KitOps), shards of up to about 50 GB (Docker Model Runner). An
`oci` repo for models raises its own bound, 64 to 256 GiB, a mirror of Ollama's models
terabytes:

```sh
curl -s -X PATCH -u admin "$LAJSTROM/api/v1/repos/oci/models" \
  -H 'Content-Type: application/json' -d '{"policy": {"maxFileSize": 274877906944}}'   # 256 GiB
```

The registry's `capabilities.maxBlobSize`, when it is smaller, still wins (GHCR's 10 GB a layer
cannot be raised), and every hop must take such a blob too:

- **distribution and Zot on S3**: S3 takes at most 10 000 parts, so the storage driver's
  `chunksize` must be at least the blob's size / 10 000 (13.1 MiB for 131 GiB; 26 MiB for 256 GiB),
  and `multipartcopychunksize` large enough for the copy that commits the upload;
- **Harbor**: `BEEGO_MAX_UPLOAD_SIZE_BYTES` of its core;
- **Nexus** on S3: `nexus.s3.multipartupload.chunksize`;
- **the edge and any proxy** between the client, Lajstrom and the registry: no body limit
  (nginx `client_max_body_size 0`), no request buffering (`proxy_request_buffering off`), and
  timeouts that cut a stalled transfer, never a long one (Traefik's `readTimeout=0`; an edge
  whose timeout bounds the whole request cuts a blob of 100 GiB).
  [reverse-proxy.md](reverse-proxy.md) has the nginx, Caddy and Traefik settings.

Lajstrom itself streams a `/v2` blob and spools nothing of it, so its disk needs no room for one;
`server.stallTimeout` only cuts a transfer that stops moving.

## Performance

Lajstrom streams bodies and holds none in memory (#62), so its memory does not grow with the
size of a file or the number of transfers, and a transfer is as fast as the registry and the
network allow:

| Path | What happens to the body |
| --- | --- |
| a download: `GET /v2/…/blobs/<digest>`, `/raw/`, the apt pool, PyPI and Maven files | `format.ServeBlob`: one request to the registry, its body copied to the client through a fixed buffer, or a `307` to the registry's storage |
| a `/v2` blob upload: `POST`, `PATCH`, `PUT` of a session | the client's body is the body of the request to the registry's session (`facade.requestBody`, the provider's `repository.send`); with [`registries[].uploads.spoolUnknownLength`](configuration.md#uploads) on, a chunk of unknown length into a session that holds bytes is spooled first to one temporary file of the chunk's size in `$TMPDIR`, unlinked as soon as it is made and closed when the request ends |
| a raw `PUT` | streamed into an upload session of the registry as it arrives (`ingest.Pipeline.UploadStream`, [ADR 0014](adr/0014-uploads-stream-into-the-registry.md)): in chunks of 8 MiB, each a `PATCH`, one held in memory per upload, while sha256 is computed; the session is closed with that digest only once every check passed, and cancelled otherwise. No disk |
| a file another frontend receives: apt `PUT …/upload`, a twine upload, a Maven `PUT` | spooled to **one temporary file of the body's size** in `$TMPDIR` for the length of the request (`format.Spool`, Maven's `ingest.spoolHashed`), so that the plugin can read it before ingest pushes it; removed when the request ends, and unlinked as soon as it is made where the system allows, so a crash leaves nothing. A twine upload takes a second spool of the file while it is checked (`pypi.readerAt`) |
| what is read into memory | metadata only, each with a bound: a manifest (`server.maxManifestSize`, 4 MiB), a metadata layer (4 MiB), a POM (4 MiB), a deb's control archive (4 MiB unpacked), `maven-metadata.xml` (1 MiB), a wheel's `METADATA` (4 MiB), and the blocks of a zip's directory that ingest reads at offsets (64 KiB each, at most 32 kept) |

So the disk of `$TMPDIR` must hold the apt, npm, PyPI and Maven uploads that run at once: the
sum of their sizes, bounded by `server.maxConcurrentUploads` and the repos' `policy.maxFileSize`:
by default 32 uploads of at most 8 GiB (apt; 4 GiB for pypi and maven, 512 MiB for npm), 256 GiB
at the worst, and twice the file for a twine upload, which spools twice. Lower either bound to
fit the disk. Raw uploads take none; the other formats move to streaming one by one (ADR 0014).
`/v2` uploads take no disk of Lajstrom's, but for the spooled chunks of a registry with
`uploads.spoolUnknownLength` on.

The streaming audit of #62 read every `io.ReadAll`, `bytes.Buffer` and temporary file of
`internal/` and `cmd/`: none is on a blob's path; each reads one of the bounded documents above,
renders an index, or is a request spool. `TestStreamingMemory` (`internal/facade`) checks the
same in `make test`: 1 GiB up and down through `/v2` against an in-process registry with the
heap in use under 64 MiB.

### The load test

`make load` ([test/load/run.sh](../test/load/run.sh), [testing.md](testing.md#load)) serves
`bin/lajstrom` with a SQLite index against the dev stack's Zot, pushes one object of 1 GiB of
random bytes through a raw repo (`curl -T`) and through `/v2` (`oras push`), downloads it 8
times at once each way (`curl`, and `oras blob fetch --output -`), checks every sha256, and
every 0.2 s samples the server's resident set (`ps`), its Go heap in use
(`go_memstats_heap_inuse_bytes` of the metrics listener) and the temporary files it holds open
(`lsof`). It exits 1 when the resident set of a phase passes `LAJSTROM_LOAD_MAX_RSS_MIB`, by
default 256 MiB, the bound of #62, and 2 when a transfer fails or a digest differs. It checks
the free disk before it starts and deletes what it pushed when it ends.

```sh
make dev-up
make load                                   # 1 upload at a time, 8 downloads at once
LAJSTROM_LOAD_UPLOADS=8 make load           # 8 uploads at once too: 11 GiB of disk
LAJSTROM_LOAD_SUMMARY=load.md make load     # the table as Markdown in load.md, too
```

`LAJSTROM_LOAD_UPLOADS` is how many uploads run at once in each upload phase, 1 by default;
each goes to a file or image of its own (`big-<i>.bin`, `oci/<run>/big-<i>:v1`), a repository
of its own in the registry, so that no upload finds its blob there already and skips sending
it. The disk it needs at the peak, when
Docker's disk is on the same filesystem as `$TMPDIR`: during the raw `PUT`, the object and one
upload in Zot's upload area per upload, 1 + U times the object; during the `/v2` push, the
object, Zot's one copy (it deduplicates the same bytes) and U uploads, 2 + U times; plus 1 GiB.
At 1 GiB that is 4 GiB for one upload and 11 GiB for 8. Before raw streamed (ADR 0014), the raw
`PUT` also took one spool per upload, 1 + 2 × U times, 18 GiB for 8.

The [Load workflow](../.github/workflows/load.yml) runs it on a GitHub Linux runner every night,
on demand, and on a pull request that changes the script or the workflow, at 1 GiB with 8
uploads and 8 downloads at once, and puts the table in the job summary. It is not a required
check: a failed run is a regression to look into, not a gate on other pull requests.

#### On Linux, 8 uploads and 8 downloads at once

On 2026-11-10, the Load workflow on its own pull request (#287,
[run 42778572674](https://github.com/lajstrom/lajstrom/actions/runs/42778572674), three
attempts), Lajstrom built from `f45dffb`, the merge of #287 into #286 (the code of main at
`cd2391d`, the script of #286), on a GitHub `ubuntu-latest` runner (image
`ubuntu-26.04` 20261101.186.1): Ubuntu 26.04.1 LTS, kernel 7.0.0-1019-azure, 4 vCPUs of an AMD
EPYC (7763, 9V45 and 9V74 in the three attempts), 16 GiB, Docker 29.4.2, Zot v2.1.23 of the dev
stack, 86 GiB free on the root filesystem (no SDK had to go), client and server on the same
runner. The first attempt:

```text
$ LAJSTROM_LOAD_UPLOADS=8 make load
lajstrom f45dffb (f45dffb, 2026-11-10T07:54:49Z), Linux x86_64, 4 CPUs; object 1024 MiB, 8 uploads and 8 downloads at once; registry localhost:5000
phase           MiB        s    MiB/s  RSS MiB heap MiB  tmp MiB samples
idle              0     0.00        0       65       27        0     28
raw-put        8192    57.65      142       66       39     8192    221
raw-get        8192     6.83     1199       32        5        0     20
v2-push        8192    19.89      412       33        6        0     72
v2-get         8192     8.75      936       33        5        0     25
load: every digest matched; the peak resident set stayed within 256 MiB
```

The three attempts:

| Phase | Bytes | Time | Throughput | Peak resident set | Peak heap in use | Temporary files |
| --- | --- | --- | --- | --- | --- | --- |
| idle, before the first transfer | | | | 65 MiB | 26–27 MiB | |
| raw `PUT`, 8 at once | 8 GiB | 57.7–89.6 s | 91–142 MiB/s | 65–66 MiB | 39 MiB | 8 GiB, 8 whole bodies |
| raw `GET`, 8 at once | 8 GiB | 4.7–6.8 s | 1199–1747 MiB/s | 31–32 MiB | 4–5 MiB | none |
| `oras push` through `/v2`, 8 at once | 8 GiB | 19.9–33.2 s | 247–412 MiB/s | 33 MiB | 5–6 MiB | none |
| `oras blob fetch` through `/v2`, 8 at once | 8 GiB | 6.1–8.8 s | 936–1341 MiB/s | 32–33 MiB | 5 MiB | none |

So the bound of #62 holds with 8 transfers of 1 GiB at once in each direction, raw and `/v2`:
the peak resident set was 66 MiB, during the 8 raw `PUT`s, one MiB above the idle process,
and the heap in use peaked at 39 MiB there, with 8 GiB of bodies in flight. The raw `PUT`s are
the slowest phase: each body is written twice on the runner's one disk, to Lajstrom's spool and
then to Zot's upload area, where a `/v2` push writes it once, and the 4 vCPUs are shared by the
8 clients, Lajstrom and Zot. The times vary from one attempt to the next with the runner's host.

#### On macOS, 1 upload at a time

On 2026-11-09, Lajstrom at `129dc47`, an Apple M4 Pro with 14 cores and 24 GiB, macOS 27.0.1,
Zot v2.1.23 of the dev stack in OrbStack, client and server on the same machine:

```text
$ make load
lajstrom v0.0.1-180-g129dc47-dirty (129dc47, 2026-11-09T21:23:45Z), Darwin arm64, 14 CPUs; object 1024 MiB, 8 downloads at once; registry localhost:5000
phase           MiB        s    MiB/s  RSS MiB heap MiB  tmp MiB samples
idle              0     0.00        0       64       25        0     26
raw-put        1024     4.36      235       31       26     1024      5
raw-get        8192     6.00     1365       35       30        0      6
v2-push        1024     2.62      391       37       32        0      5
v2-get         8192     5.00     1638       33       35        0      7
load: every digest matched; the peak resident set stayed under 256 MiB
```

Four runs that day, the others with the same script while other jobs used the machine:

| Phase | Bytes | Time | Throughput | Peak resident set | Peak heap in use | Temporary file |
| --- | --- | --- | --- | --- | --- | --- |
| idle, before the first transfer | | | | 64–65 MiB | 23–26 MiB | |
| raw `PUT`, one | 1 GiB | 4.0–5.3 s | 192–256 MiB/s | 31–65 MiB | 26–29 MiB | 1 GiB, the whole body |
| raw `GET`, 8 at once | 8 GiB | 2.5–6.0 s | 1365–3287 MiB/s | 30–36 MiB | 28–32 MiB | none |
| `oras push` through `/v2`, one | 1 GiB | 1.9–3.3 s | 312–551 MiB/s | 32–38 MiB | 30–34 MiB | none |
| `oras blob fetch` through `/v2`, 8 at once | 8 GiB | 2.8–5.0 s | 1638–2895 MiB/s | 29–41 MiB | 32–37 MiB | none |

The resident set of a transfer never passed what the idle process held after its start, and
the heap grew by 10 MiB over 18 GiB moved: nothing scales with the size of a body or the number
of downloads. A raw `PUT` takes longer than a `/v2` push of the same bytes because it writes
the spool first (about 1 GiB/s here) and then streams it to the registry. The download
throughput is the sum of 8 clients that each hash what they get, on the machine that also runs
Lajstrom and Zot: it is the throughput of the whole setup, not a bound of Lajstrom's.

The uploads ran one at a time there: 8 at once need 18 GiB of free disk and the machine had 6.

Not measured: a registry behind object storage, where a download is a `307` and does not pass
through Lajstrom at all; a registry on another machine, where the network and not the disk
bounds a transfer.

## Metrics

Prometheus scrapes `GET /metrics` on the metrics listener, `server.metricsListen` (default
`127.0.0.1:9464`), a listener of its own that the edge never routes to; the main listener answers
`404` there. `server.pprof: true` adds Go's `/debug/pprof/` to it. Every series is named
`lajstrom_*` (plus the Go runtime's `go_*` and `process_*`), and no label carries a repo or
package name: the labels are route patterns, methods, statuses, registry names and operations,
so the number of series is fixed by the code and the configuration.

| Series | Labels | What |
| --- | --- | --- |
| `lajstrom_http_request_duration_seconds` | `method`, `route`, `status` | time to answer a request, by the route pattern it matched (`unmatched` for none) |
| `lajstrom_http_request_size_bytes`, `lajstrom_http_response_size_bytes` | the same | the declared request body, the bytes written back |
| `lajstrom_provider_request_duration_seconds` | `registry`, `operation`, `method`, `status` | a request to a backend registry, each retry its own, to its response headers; `operation` is `ping`, `catalog`, `manifest`, `blob`, `upload`, `tags`, `referrers` or `other` (a token realm, a redirect to the storage); `status` is `error` without an answer |
| `lajstrom_provider_circuit_open` | `registry` | `1` while the [circuit breaker](#requests-to-the-registries) of the registry is open, its requests failing at once; `0` otherwise |
| `lajstrom_db_query_duration_seconds` | `method`, `result` | a statement of the index database, by the store method that ran it (`GetRepo`, `UpsertVersion`, …); a missing row is `ok`, a refused or failed statement `error` |
| `lajstrom_ingest_total` | `format`, `result` | publishes through the ingest pipeline: `published`, `existed` (an idempotent repeat), `invalid`, `conflict`, `error` |
| `lajstrom_proxy_upstream_request_duration_seconds` | `upstream`, `operation`, `status` | requests of proxy repos to their upstreams ([proxy-and-group.md](spec/proxy-and-group.md)), to the response headers: `document`, `file` or `forward` (a client's request sent on: npm audit), the status or `error` |
| `lajstrom_proxy_documents_total` | `upstream`, `result` | reads of upstream documents: `fresh` (the cache), `revalidated` (`304`), `refreshed` (`200`), `absent` (`404`, `410`), `stale` (the upstream failed, the cache or the held versions served), `offline`, `shared` (a miss that waited for the request another miss on this replica sent) |
| `lajstrom_proxy_fetches_total` | `upstream`, `result` | file requests of proxy repos: `held` (a hit), `fetched` (a miss downloaded and published), `shared` (a miss another request or replica fetched), `missing`, `rejected` (checksum, size, `Validate`), `error` |
| `lajstrom_clock_skew_seconds` | | this replica's clock minus the index database's at the last comparison, at the start and every 30 seconds; positive when the replica is ahead ([Clocks](#clocks)) |
| `lajstrom_auth_throttled_total` | `throttle` | requests answered `429` by a throttle ([auth.md](auth.md#failed-logins)): `account` and `address` (failed logins), `token` (requests to `/v2/token`) |
| `lajstrom_download_digest_mismatches_total` | `registry` | downloads aborted because the bytes the registry sent do not have their digest ([guarantees.md](spec/guarantees.md), INV-40): a blob streamed to a frontend's or `/v2`'s client, a manifest of `/v2`; the log line `response aborted` names it. Anything but 0 is a registry that serves other bytes than it stored |
| `lajstrom_build_info` | `version`, `commit`, `goversion` | always 1 |

```yaml
# prometheus.yml
scrape_configs:
  - job_name: lajstrom
    static_configs:
      - targets: ["lajstrom-a:9464", "lajstrom-b:9464"]
```

On Kubernetes, with `server.metricsListen: "0.0.0.0:9464"`, a port `metrics` on the pod that no
Ingress names, and the Prometheus operator, the shape the Helm chart (#82) will render:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: lajstrom
spec:
  selector:
    matchLabels: {app.kubernetes.io/name: lajstrom}
  endpoints:
    - port: metrics
      path: /metrics
      interval: 30s
```

## Audit

Every write and every change of the control plane is one row of the `audit` table of the index
([rbac.md](spec/rbac.md) §5):

| Actions | Written by |
| --- | --- |
| `push`, `delete` | the frontends and the facade, through the ingest pipeline and the delete paths; the facade itself for a push to an `oci` repo |
| `npm.dist-tag`, `npm.deprecate` | an entry of an npm package's journal: `npm dist-tag`, `npm deprecate`, the dist-tag of a publish ([ADR 0012](adr/0012-npm-profile-routes-and-dist-tags-journal.md)) |
| `proxy.fetch` | a proxy repo that published a file it fetched from its upstream, as the principal whose request missed ([proxy-and-group.md](spec/proxy-and-group.md) §5) |
| `proxy.reject` | a proxy repo that refused a file of its upstream for its bytes: a checksum, `Validate`, a bound ([rbac.md](spec/rbac.md) §7) |
| `repo.create`, `repo.update`, `repo.delete`, `repo.reindex`, `repo.cache.purge` | the API, the CLI and the repos of the configuration; `repo.cache.purge` from `DELETE …/upstream-cache` |
| `user.create`, `user.rename`, `user.disable`, `user.enable`, `user.password`, `user.delete` | the API and `lajstrom admin user` |
| `binding.create`, `binding.delete` | the API and `lajstrom admin binding` |
| `token.create`, `token.revoke` | personal access tokens and deploy tokens, from the API and the CLI |
| `key.rotate`, `key.drop` | `lajstrom admin key` |
| `reconcile` | the [reconcile](#reconcile) loop of `serve`, one row per repo that a run changed |

A row has the time, the principal (`user:<uid>`, `token:<id>`, `anonymous`) and, for a user,
its name at the time (`principalName`, which stays when the user is renamed or deleted), the
action, the repo, the subject (a package and version, the user or binding acted upon), the
manifest digest, the client address and details: the change, the source of a change of the
control plane (`api`, `cli`, `config`; the rows of `lajstrom admin key` have none) and the
request id of a change made over HTTP; a write or a fetch that came through a group is its
member's row, with the group in `details.via` (`"via": "npm/all"`). The client address is the one of the access log
([ADR 0011](adr/0011-client-address-behind-a-proxy.md)): behind a proxy of
`server.trustedProxies`, the client that `X-Forwarded-For` names, not the proxy. The rows of the
ingest pipeline, a `push` through `/v2` or a frontend and a `delete`, read the address and the
request id from the request that made them.

**The row is written in the transaction of its change.** There is no change of the index
without its row and no row without its change; when the database is down, the change fails with
it, and nothing is buffered or dropped. #46 asked for an asynchronous queue that drops events
when the database is down; with the row in the change's transaction there is nothing left for
such a queue to save, so there is none.

**A write to the registry has its row before it.** No transaction spans the registry, so a
push, a delete, a journal entry or a proxied ingest writes its row `pending` before it asks the
registry, and no write is made without it. The row then says how the write ended, its `state`:
`ok`, written in the transaction that records the write in the index; `failed`, with the
`error` the client was answered (the registry refused, or the index failed after the registry
had the write, which reindex repairs); `unknown`, when the row is still pending after
`ingest.claimTTL`: the process stopped, or the database failed, between the registry and the
index, and the registry may or may not have the write. Reindex and the reconcile loop mark those
rows `unknown` and bring the index to what the registry has
([index-schema.md](spec/index-schema.md#the-audit-row-of-a-registry-write),
[guarantees.md](spec/guarantees.md) INV-14). Every other row is `ok`.

**Not audited:** reads (`pull`, `GET`), and the bearer tokens of `/v2/token`, which a docker
or oras client gets for every pull and push: a row each would be noise, and what a token then
allows is audited itself. Every request, with its principal, is in the access log.

**Reading it:** `GET /api/v1/audit` as admin, newest first, by repo, principal, action, state
and time, a page at a time ([api.md](api.md#audit)), here with `LAJSTROM` the server's URL:

```sh
curl -fsS -u admin "$LAJSTROM/api/v1/audit?repo=raw/scratch&since=2026-11-09T00:00:00Z&limit=50" | jq -c '.events[]'
# the writes whose outcome only the registry knows
curl -fsS -u admin "$LAJSTROM/api/v1/audit?state=unknown" | jq -c '.events[] | {at, action, repo, subject, digest}'
```

or SQL on the index. The table only grows in this version: no retention, no export; it is part
of the backup of the index.

## Retention

Lajstrom keeps every version until someone deletes it: no rule inside the server deletes
versions (decided on [#72](https://github.com/lajstrom/lajstrom/issues/72)). A retention policy is
a job that the operator's scheduler runs, a Kubernetes CronJob, a CI schedule or cron, with
`lajstrom admin retention`, a client of the management API: it lists the packages and versions
of a repo (`GET …/packages`, `GET …/packages/<package>/versions`), decides, and deletes through
`POST …/versions:delete` ([api.md](api.md#delete-versions-in-a-batch-post-apiv1reposformatnameversionsdelete)).
Every deletion has an audit row with the job's id and its token (`details.job`,
`details.token`).

```sh
export LAJSTROM_URL=https://art.example.com LAJSTROM_TOKEN_FILE=/run/secrets/retention-token
lajstrom admin retention --repo npm/internal --keep-last 10 --max-age 90d            # a dry run
lajstrom admin retention --repo npm/internal --keep-last 10 --max-age 90d --apply    # deletes
lajstrom admin retention --repo raw/builds --package 'nightly/*' --max-age 14d --output json
```

**The rules.** A version is kept when any rule keeps it: it is one of the `--keep-last N` newest of
its package, by the publish time of its first variant; it is younger than `--max-age` (`90d`,
`36h`), by its newest variant; a pointer names it, an npm dist-tag, `latest` included; or it is
the newest version of its package, unless `--delete-newest`. At least one of `--keep-last` and
`--max-age` is required. `--package` keeps the packages whose name matches the glob
(`path.Match`, where `*` does not match `/`: `@acme/*`, `nightly/*`), again for more; without it
every package of the repo. The time is the server's, from the `Date` of its first answer, and
the publish time that of the manifest (`org.opencontainers.image.created`), which the frontends
write from the server's clock and a client pushing on `/v2` from its own, within
`server.maxCreatedAhead` and `server.maxCreatedBehind` of the server's
([configuration.md](configuration.md)).

**Dry run first.** Without `--apply` the job prints the versions it would delete and why, the
rule, the age and the rank, and changes nothing; `--output json` prints the same as one JSON
document. With `--apply` it sends them in batches of 100, one job, with the digests it read and
the cut-off it decided on (now less `--max-age`, else now), and prints what the server did with
each: `deleted`, or `changed` (published again, or a variant added, since the list), `protected`
(a dist-tag moved onto it), `young`, `gone`, `error`. The server checks each version under the
lock that a publish of its tag and a move of a dist-tag take, so a version that changed after
the list is reported, never deleted ([guarantees.md](spec/guarantees.md), INV-42). A run that
races a publish deletes fewer versions, never more; the next run picks up the rest.

| Exit status | Meaning |
| --- | --- |
| 0 | nothing to delete, or `--apply` deleted or reported every version |
| 2 | a dry run found versions to delete |
| 1 | an error: the server unreachable, a credential refused, an item the server failed to delete |

**The credential.** A user for the job with the `maintainer` role on the repos it cleans, and a
personal access token of that user without scopes: a token with scopes never deletes, and a
deploy token never does ([auth.md](auth.md#what-a-token-may-do)). The token is read from
`--token-file` or `LAJSTROM_TOKEN_FILE`, else from `LAJSTROM_TOKEN`; the server from `--url` or
`LAJSTROM_URL`. The job reads no configuration and needs no database.

```sh
lajstrom admin user create --name retention
lajstrom admin binding create --principal user:retention --role maintainer --mask npm/internal
lajstrom admin token create --user retention --name "retention $(date -u +%Y-%m)" --expires 365d
```

**A CronJob.** The image is Lajstrom's own; the token is a Secret mounted as a file.

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: lajstrom-retention-npm-internal
spec:
  schedule: "17 3 * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      backoffLimit: 0
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: retention
              image: ghcr.io/lajstrom/lajstrom:v0.1.0
              args: [admin, retention, --repo, npm/internal, --keep-last, "10", --max-age, 90d, --apply]
              env:
                - {name: LAJSTROM_URL, value: "http://lajstrom.lajstrom.svc:8080"}
                - {name: LAJSTROM_TOKEN_FILE, value: /run/secrets/retention/token}
              volumeMounts:
                - {name: token, mountPath: /run/secrets/retention, readOnly: true}
          volumes:
            - name: token
              secret: {secretName: lajstrom-retention-token}
```

Run it once without `--apply` and read the list before you schedule it. The blobs of what it
deletes go when the registry's garbage collection runs, as for every delete.

**The registry's own rules.** A registry's lifecycle rules (ECR lifecycle policies, Harbor tag
retention, GitLab cleanup policies) delete under Lajstrom, by tag and age, and know nothing of
versions, dist-tags or signatures. Prefer `lajstrom admin retention`. Where a registry rule is
needed anyway:

- exclude the npm journal tags `_j<n>` of every package: they are the history of its dist-tags
  and deprecations (manifest-profile.md §8), and a journal with a gap stops the package's next
  dist-tag change;
- exclude cosign's tags `sha256-<hex>`, `sha256-<hex>.sig`, `.att` and `.sbom` and keep untagged
  manifests that have a subject: they are the signatures, attestations and SBOMs of versions
  that stay (manifest-profile.md §6);
- the index follows at the next reconcile, which removes the rows of a manifest the registry
  answers as unknown; until then a client sees a version that is gone (`404` on its download),
  and the delete has no audit row of Lajstrom's. The grace period of
  [#446](https://github.com/lajstrom/lajstrom/issues/446) will keep a version a registry
  briefly forgets.

## Reindex

The index database holds two kinds of tables ([index-schema.md](spec/index-schema.md)): the
control plane (repos, users, tokens, bindings, keys, audit), which only the database has and
which is backed up, and the derived tables (packages, versions, files, index caches, OCI
repositories), which `lajstrom reindex` rebuilds from the manifests in the registries. Reindex
is how a lost or damaged index comes back, how an existing registry prefix is attached, and how
writes that went past Lajstrom (an `oras push` straight to the backend, a manifest deleted
there) reach the index.

### What a run does

For each repo, or the one of `--repo`: the OCI repositories under the repo's path (the
registry's catalog; the `oci_repos` rows of a backend without one), their tags, and per tag the
digest of its manifest.

- A tag whose manifest has the digest of its row in the index is only marked seen: the same
  digest is the same bytes, checked when they were written. A second run therefore costs one
  `HEAD` per tag. `--verify` reads and checks every manifest again, for instance after a fix in
  a format plugin.
- A new digest is read once and sorted. A profile manifest goes through the checks of a publish
  (the profile, the format plugin, the blobs) and its rows are written. A manifest whose
  metadata layer is missing or unreadable is indexed from its annotations and flagged
  `degraded`; the next run reads it again. A manifest with a `subject`, or under a
  `sha256-<hex>` tag (the referrers tag schema, cosign's `.sig`, `.att`, `.sbom`), is a referrer:
  counted, and listed as an orphan when no version has its subject's digest. Anything else under
  a repo's path, a Helm chart for instance, is foreign: recorded in `oci_repos`, listed, never
  indexed, never served.
- A tag that names another digest than its row is drift: the index keeps its digest and the
  report lists both. Lajstrom never re-points a tag; whoever re-pointed it in the backend
  decides which one is right.
- With `--prune` (the default for a run of every repo, off with `--repo` unless given), every row
  the run did not see is looked up in the registry by its tag once more, and, when the tag does
  not resolve, by its manifest's digest. A row goes only when the registry answered `404
  MANIFEST_UNKNOWN` for that digest twice, at least 10 minutes apart, and the row is
  `reconcile.graceAge` old ([On an eventually consistent
  registry](#on-an-eventually-consistent-registry)); until then it is counted as `pending`. A
  package left without versions goes with it. Any other answer removes nothing, and a run that
  cannot list a repo's repositories sweeps nothing in that repo.
- At the end the repo's cached indexes are dropped (the next `GET` renders them again), and tag
  claims older than `ingest.claimTTL` are deleted.

A repo whose format has no plugin in this binary is listed with one error and left alone; a
binary built from this repository has all five (raw, apt, pypi, maven, npm). The manifests of an `oci` repo, which has
no plugin by design, are listed as foreign and recorded in `oci_repos`.

### Running it

```sh
lajstrom reindex --config /etc/lajstrom/lajstrom.yaml             # every repo, with prune
lajstrom reindex --repo apt/ubuntu-internal --report              # one repo, the lists too
lajstrom reindex --verify --json > reindex-report.json            # every manifest read again
lajstrom reindex --repo raw/assets --attach raw-assets.txt        # a registry without a catalog (below)
```

The output is one line of counts per repo (manifests, added, updated, unchanged, removed,
pending, degraded, referrers, foreign, orphan referrers, drift, errors); `--report` adds the lists, at most
1 000 entries each, and `--json` prints the whole report. The exit status is not zero when the
run itself failed (the database or the context) or another run holds the lock; manifests that
could not be read or checked are errors in the report and do not change it. `--concurrency`
(default 8) is how many manifests are read at once per registry. Progress is logged every 1 000
manifests with the rate.

How long it takes, measured with `TestScale` (`LAJSTROM_TEST_REINDEX_MANIFESTS=10000 make
test-integration`) on the Zot of the dev stack, SQLite as the index, an Apple M4 Pro, 2026-11-09:
10 000 raw manifests in 100 packages rebuild from an empty index in 6.1 s (about 1 650 a second),
and a second run over the same digests takes 1.4 s (about 7 000 a second). A registry across a
network is slower by its round trips: a rebuild costs a `HEAD`, a `GET` of the manifest and the
blob reads of the format per manifest, a run over known digests one `HEAD`. Postgres has not
been measured yet.

A run takes the lock of its scope in the table `reindex_runs`: a second run of the same repo, or
any run while every repo is being reindexed, is refused on every replica and in the CLI, with
the replica and the time of the run that holds it. A run beats every 30 seconds; one whose
heartbeat is two minutes old (a killed process) is closed as `abandoned` by the next start. The
ten newest finished runs of each scope are kept, with their reports:
`sqlite3 lajstrom.db "SELECT id, repo_id, state, started_at, finished_at, error FROM reindex_runs ORDER BY id DESC LIMIT 10"`.
Every start is an audit row `repo.reindex` (`anonymous` with the source `cli` from the CLI).

### Reconcile

`serve` runs a reindex of every repo with prune every `reconcile.interval` (default `1h`, `0`
turns it off), so that a push straight to the backend or a delete in the registry's console
reaches the index without an operator. Each replica has the loop; the first run comes after a
random part of the interval, and a tick while another run holds the lock (another replica's
loop, the CLI, the API) is skipped, so with several replicas one of them runs about once an
interval. The next tick is counted from the end of a run. Its runs are `reindex_runs` rows with
the source `reconcile` and show in `GET …/reindex`; they write no audit row when they change
nothing and one row `reconcile` per repo they changed (its details have the counts). A run over
known digests costs one `HEAD` per tag ([On a cloud registry](#on-a-cloud-registry)), so the
default interval is cheap; drift is reported, never repaired by re-pointing a tag. A manifest
deleted in the registry leaves the index at the first run that comes both 10 minutes after the
first run that found it gone and `reconcile.graceAge` after the version was last seen: with the
defaults, one to two hours after the delete.

### On an eventually consistent registry

Some registries answer a write before every read sees it: ECR can answer `404` for an image
shortly after its push, Quay's tag lists lag up to two minutes, ACR's geo-replicas and ECR's
replication and pull-through caches lag minutes to a day
([#446](https://github.com/lajstrom/lajstrom/issues/446)). A run inside that lag would see a
version just published as gone. Prune therefore removes a row only when all of these hold:

- the registry answered `404` with the error code `MANIFEST_UNKNOWN` to the manifest of the row
  by its digest (a `HEAD`, then a `GET` for the code). A tag that does not resolve, a tag list or
  a catalog without it, `NAME_UNKNOWN`, a `404` without a code, a `5xx`, `401`, `403` or a timeout
  never count;
- it answered so twice, at least 10 minutes apart, with no run in between that found the
  manifest or could not ask. The first answer is kept in the table `prune_sightings`, so the
  runs of every replica count, and two replicas' runs a minute apart are one answer, not two;
- the row is at least `reconcile.graceAge` old: ingest wrote it, or a run last saw its manifest,
  that long ago (`versions.indexed_at`).

What an operator sees: a version deleted in the registry's console, or by its lifecycle rules,
stays listed for one to two hours with the defaults, its downloads `404`, and the reports count
it as `pending`. Any other answer keeps it too, as an error of the report (`look up the manifest
before removing its rows`). To remove a version at once, delete it through Lajstrom's API, which
deletes the rows in the same request. For a whole OCI repository deleted in the registry, Zot
answers `NAME_UNKNOWN`: its rows stay and every run reports them, so delete them through the
API. distribution answers `MANIFEST_UNKNOWN` there, and its rows go as above (both measured by
`TestIntegration` of `internal/provider/generic`).

Set `reconcile.graceAge` ([configuration](configuration.md#reconcile)) to more than the
registry's lag at the endpoint Lajstrom reads: the default `1h` when it reads the endpoint it
writes to, `24h` or more when reads go through a replica or a cache.

### Without a catalog

Some registries do not list their repositories to Lajstrom: Harbor's `_catalog` is for system
administrators with Basic auth, GHCR's lists all of GHCR, ECR has none
([registries](registries/README.md)). The probe or the vendor table then sets `catalog` false, as
`capabilities.catalog: false` in the registry's configuration does. A run reads the OCI
repositories that the index records in `oci_repos` instead: ingest records each one it writes
to, and a run each one it reads. Each is checked by its tag list: one the registry no longer
knows loses its row once no run has seen it for `reconcile.graceAge`, and its versions go only
as every prune removes them ([On an eventually consistent
registry](#on-an-eventually-consistent-registry)); one whose tag list fails keeps its row and is
an error of the report. A run that finds a version by its tag records its repository again.

What the index does not record, a run does not see: a push straight to the backend into a new
OCI repository, and every repository once the derived tables are lost (a database lost without
a backup; what was pushed after the backup of one that was restored). Name those once with
`--attach` and `--repo`: a file of OCI repositories of the repo, one per line, as `oci_repos`
holds them, under the repo's `<format>/<name>/` and without the registry's `prefix`. Blank lines
and lines that start with `#` are skipped, and a name outside the repo refuses the run. A
repository the registry does not know is skipped; the others are recorded, so that the next
runs and the reconcile loop find them without the file.

```sh
cat > raw-assets.txt <<'EOF'
# raw/assets, from the registry's own listing (Harbor: GET /api/v2.0/projects/<project>/repositories)
raw/assets/docs/readme.md
raw/assets/bin/tool
EOF
lajstrom reindex --repo raw/assets --attach raw-assets.txt --report
```

### On a cloud registry

ECR bills storage and data transfer, not requests; within a region the transfer is free. A run
over known digests reads the manifests' headers only. A full rebuild reads, per manifest, the
manifest, the metadata layer and what the format plugin checks: for apt the head of the `.deb`
up to its control member (for pypi, the zip's directory and its `METADATA` with range requests:
a few kilobytes per wheel; an sdist until its `PKG-INFO`). The connection of a `.deb` is closed
early, but the bytes already in flight are transferred, so for apt count the size of the packages: 10 000 packages of 2 MB are about 20 GB once.
In the region that is free; through a NAT gateway it is the gateway's processing price (about
$0.045 per GB, depending on the region), which the S3 gateway endpoint of the VPC avoids
because ECR serves layers from S3; across regions or to the internet it is the transfer price
(about $0.09 per GB, on both sides between regions). Prices as of November 2026.

### Disaster recovery

- **The derived tables are damaged or missing, the control plane is intact**: `lajstrom
  reindex`. Frontends keep serving while it runs; what it has not reached yet is missing until
  it does.
- **The database is lost and there is a backup**: restore the backup, `lajstrom migrate`, then
  `lajstrom reindex` ([Backup and restore](#backup-and-restore) has the steps and a rehearsal).
  Everything published after the backup is in the registry and comes back;
  users, tokens and bindings come back as they were at the backup. The keys decrypt only with
  the master key of the backup ([The master key](#the-master-key)).
- **The database is lost without a backup**: the control plane is gone. Create the repos again
  with `lajstrom admin repo create` (or the `repos` of the configuration) under the same format,
  name and registry, with the registry's `prefix` unchanged, then
  `lajstrom reindex` brings back every package. Users and tokens are created again; apt repos
  get new keys, and their clients need the new `key.asc`.
- **The registry was restored from its own backup or a replica**: `lajstrom reindex` with prune,
  and once more after 10 minutes; rows of manifests the restored registry does not have are
  removed once they are `reconcile.graceAge` old, drift is reported.

On a registry without a catalog a run finds only the OCI repositories that `oci_repos` holds:
in the first three cases, name the others per repo with `--attach` ([Without a
catalog](#without-a-catalog)).

## Backup and restore

Lajstrom keeps no artifact: the registry has every byte and every manifest, and its own backup,
or none if its storage is replicated. What Lajstrom adds is the index database and the master
key, and only part of the database can be rebuilt from the registry.

### What is where

| Data | Lives in | In a database backup | Rebuilt by `reindex` |
| --- | --- | --- | --- |
| artifacts: blobs and manifests | the registry | no | they are the source |
| repos | `repos` | yes | no: `reindex` walks the repos the database has; the `repos` of the configuration are created again at the next start |
| users, bindings, personal access tokens, deploy tokens | `users`, `bindings`, `tokens` | yes | no |
| apt signing keys, the bearer-token key | `keys`, the private keys encrypted with the master key | yes, as ciphertext | no |
| audit log | `audit` | yes | no |
| open maven publications, `/v2` upload sessions | `sessions` | yes, as they were | no: clients start them again |
| packages, versions, files, cached indexes, OCI repositories | the derived tables | yes | yes |
| tag claims, reindex runs | the coordination tables | yes | not needed: they live for one publish or one run |
| the master key | the environment of `serve` and `admin key`, or the file of `keys.masterKeyFile`, from your secret store | **no** | no: without it the keys in the database are useless |
| the configuration file, the registry passwords, the DSN | your configuration management | no | no |

`reindex` writes the derived rows from manifest annotations and metadata layers (through
`ingest.WriteRows`), `oci_repos`, its own `reindex_runs` row and one audit row; it reads the
repos from the database and nothing else of the control plane. The manifests of apt, pypi and
maven carry `lajstrom.uploaded-by`, the principal that published (`user:<uid>`), as information;
no user, token, binding or key is in the registry ([manifest-profile.md](spec/manifest-profile.md)).

### Backing up

- **Postgres**: `pg_dump --format=custom`, which reads one consistent snapshot while the
  replicas keep serving; or the snapshots and point-in-time recovery of a managed Postgres.
  The control plane changes rarely: daily, and before every upgrade, is plenty.
- **SQLite**: the online backup of the `sqlite3` shell, consistent while `serve` writes:
  `sqlite3 /var/lib/lajstrom/lajstrom.db ".backup /backup/lajstrom.db"`. A `cp` of the file
  while `serve` runs misses what is still in `lajstrom.db-wal`.
- **The master key**: once, when it is made, kept apart from the database backups (a password
  manager, a vault, a printed copy in a safe). Without it the keys of a restored database do
  not decrypt, and `serve` refuses to start until they are replaced
  ([The master key](#the-master-key)).

What a restore loses is what changed in the control plane after the backup: users, tokens,
bindings and repos created since are gone, and **what was revoked, disabled or deleted since
comes back**, since the dump has the rows as they were: revoke those tokens and disable those
users again ([below](#a-rehearsal), a revoked deploy token works again after the restore). A key rotation since is
undone: rotate again and distribute `key.asc` again. Artifacts are not lost: `reindex` adds what
was published after the backup and removes what was deleted.

### Restoring

1. Stop every replica (`kubectl scale --replicas=0`, `docker compose stop lajstrom`), so that
   none writes into the database while it is restored.
2. Restore into an empty database: `pg_restore --no-owner --exit-on-error -d lajstrom
   lajstrom.dump`; for SQLite, copy the backup in place of `lajstrom.db` with no
   `lajstrom.db-wal` or `lajstrom.db-shm` beside it.
3. Give the processes the master key of the backup, through the same `keys.masterKeyEnv` or
   `keys.masterKeyFile`.
4. `lajstrom migrate`: the binary may be newer than the backup.
5. `lajstrom reindex`: every repo, with prune.
6. Start the replicas, and check: `whoami` of an admin, a package published before and one
   after the backup, the fingerprint of an apt repo's `key.asc`, the bearer-token key's
   fingerprint in the start log.

A database restored on **another server**, a staging copy of production with the same master
key, has production's bearer-token key: a token one of them issues verifies on the other
wherever the service, `server.externalURL`'s host, is the same, with the access of the issuer's
bindings. Before it serves, give it a key of its own there, ending every token of the old one
([The bearer-token key](#the-bearer-token-key)):

```sh
lajstrom admin key rotate --token --drop
```

### A rehearsal

Run on 2026-11-09 against a Postgres 17.11 that was then thrown away, in the Compose stack of
#58 (Lajstrom of the release snapshot, distribution 3.1.2), with `lj` for
`docker compose exec -T lajstrom /lajstrom --config /etc/lajstrom/lajstrom.yaml`. Before the
backup: a user `alice` with a `reader` binding on `raw/*`, a deploy token of `raw/assets`, the
apt repo `apt/debs` with its key, and the files `before.txt` and `gone.txt`:

```sh
docker compose exec -T postgres pg_dump -U lajstrom -d lajstrom --format=custom > lajstrom.dump
```

After it: `after.txt` published, `gone.txt` deleted, a user `bob` created. Then the database
was lost (the container and its volume removed), and restored into a new one:

```sh
docker compose stop lajstrom
docker compose rm -f -s postgres && docker volume rm lajstrom-docs-compose_postgres-data
docker compose up -d --wait postgres
docker compose exec -T postgres pg_restore -U lajstrom -d lajstrom --no-owner --exit-on-error < lajstrom.dump
docker compose run --rm -T lajstrom migrate --config /etc/lajstrom/lajstrom.yaml
docker compose run --rm -T lajstrom reindex --config /etc/lajstrom/lajstrom.yaml --report
docker compose up -d --wait lajstrom
```

```text
schema is current at version 0009
REPO        MANIFESTS  ADDED  UPDATED  UNCHANGED  REMOVED  DEGRADED  REFERRERS  FOREIGN  ORPHANS  DRIFT  ERRORS
apt/debs    0          0      0        0          0        0         0          0        0        0      0
oci/images  1          0      0        0          0        0         0          1        0        0      0
raw/assets  3          1      0        2          1        0         0          0        0        0      0
oci/images: foreign hello:1 sha256:9054279e24be9b8e39ede5221cfe7a7a6e9b8f7d5fac4af0c98c43d5e2acabb2 application/vnd.oci.image.manifest.v1+json
```

`after.txt` was added and `gone.txt` removed. The checks:

| Check | Result |
| --- | --- |
| `admin`, `alice` (her binding), the deploy token | `200` |
| `bob`, created after the backup | `401`: gone |
| `before.txt`, `after.txt` | served |
| `gone.txt` | `404` |
| sha256 of `lj admin key export --repo apt/debs` | `9a63088a41d7e9aa…` before and after: the same key |
| bearer-token key in the start log | `4e28d5adaf672aa2…`, the fingerprint made at the first start: no new key |

A second run on the same stack revoked a deploy token after the backup: `401` before the
restore, `200` again after it.

The same on SQLite 3.54.0 of macOS, with `bin/lajstrom` and a registry of its own: a `.backup`
while `serve` ran, a file published after it, the database removed, the backup copied in its
place, `migrate` and `reindex` (`raw/assets  2  1  0  1  0 …`: one added, one unchanged), and
both files served.
