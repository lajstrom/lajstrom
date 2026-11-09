# Operations

What running Lajstrom asks of an operator beyond the [configuration](configuration.md). The
dev stack is in [dev-stack.md](dev-stack.md).

## Users

Users live in the index database, not in the configuration file; [auth.md](auth.md) has the
details. Every `lajstrom admin user` command works on the database of the configuration,
without HTTP, and is audited with the source `cli`.

### The first admin

After `lajstrom migrate` (or the first start of `serve`), on one replica:

```sh
LAJSTROM_ADMIN_PASSWORD='…' lajstrom admin user create --name admin --admin
```

The user gets the `admin` binding on `*/*`. In Kubernetes, run it as a Job (or
`kubectl exec` into a replica) with the variable from a Secret, as for the master key below;
without the variable the password comes from standard input with `--password-stdin`, or from a
prompt on a terminal. Check it with `curl -u admin https://art.example.com/api/v1/whoami`.

### A lost password

```sh
lajstrom admin user set-password alice      # LAJSTROM_ADMIN_PASSWORD, --password-stdin, or the prompt
```

The old password fails at the next request on every replica. A lost admin password is recovered
the same way: the command needs the database, not a password.

### Disabling a user

```sh
lajstrom admin user disable alice
lajstrom admin user enable alice
```

A disabled user's credentials are refused with `401` from the next request on, on every
replica: the user is read from the database on every request, and the cache of verified
passwords never outlives a change. Users are not deleted in this version; `lajstrom admin user
list` shows who is disabled. A disabled user's personal access tokens are refused too.

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
the examples), never in the configuration file, and every replica gets the same value. `serve`
and `lajstrom admin key` read it when they start. A variable that is set but empty, or is not 64
hexadecimal characters, stops them with an error that names the variable and the format, never
the value. Without a master key (`keys.masterKeyEnv` empty, or the variable not set) signing is
off: `serve` logs a warning for every apt repo, `InRelease`, `Release.gpg` and `key.asc` are
`404`, and clients have to mark the source `[trusted=yes]`; bearer tokens are signed with an
ephemeral key ([below](#the-bearer-token-key)). Once the index holds apt signing keys, `serve`
refuses to start without the master key: those keys are useless without it, and the clients
that pinned them would see the repos turn unsigned. The token key does not count: no client
pins it.

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

A client pins the repo's `key.asc` and names it in its source:

```sh
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://art.example.com/apt/ubuntu-internal/key.asc | sudo tee /etc/apt/keyrings/ubuntu-internal.asc > /dev/null
echo "deb [signed-by=/etc/apt/keyrings/ubuntu-internal.asc] https://art.example.com/apt/ubuntu-internal jammy main" |
  sudo tee /etc/apt/sources.list.d/ubuntu-internal.list
sudo apt-get update
```

The general form is
`deb [signed-by=/etc/apt/keyrings/<repo>.asc] https://host/apt/<repo> <suite> <component>`.
The apt of Ubuntu 22.04 and 24.04 and of Debian 12 reads the armored `.asc` in `signed-by` as
it is (the integration tests run all three); for an older apt, `gpg --dearmor` makes the binary
`.gpg` form. `key.asc` needs read on the repo like the rest of it. For configuration management, `lajstrom admin key export --repo apt/<repo>` prints
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

When the grace ends the retired key no longer signs and leaves `key.asc`, and a client that
still has only the old key fails `apt-get update` with `NO_PUBKEY`.

The runbook:

1. `lajstrom admin key rotate --repo apt/<repo>`; note the fingerprint it prints.
2. Distribute the new `key.asc` to every client: fetch it again into `/etc/apt/keyrings/`, or
   roll out `lajstrom admin key export --repo apt/<repo>` with configuration management.
   `gpg --show-keys /etc/apt/keyrings/<repo>.asc` lists the new fingerprint.
3. Finish before `keys.retireGrace` runs out.

Rotate when the algorithm changes (`keys.algorithm` applies to keys made from then on), when a
new master key replaces a lost one, and when a key may have leaked. For a leaked key, set
`keys.retireGrace: 0s` first, so that the retired key stops signing and leaves `key.asc` at
once, and accept that clients fail until they have the new `key.asc`.

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
  replicas reload the keys at most once a second, so for that second a replica may refuse a
  token of the new key once; the client asks for another. A rotation is an audit row
  `key.rotate` without a repo, the new fingerprint as its subject, the retired one in its
  details. Rotate with the master key's rotation, when the algorithm changes, or at will:
  nothing else needs to happen.
- **Drop**: `lajstrom admin key rotate --token --drop` is the same with no grace: the old key
  stops verifying within a second on every replica and every outstanding token dies, for a key
  that may have leaked or when every token must go now (a user who must lose access to `/v2`
  before their token expires). Clients get new tokens with their credentials at their next
  request. Its audit row says `"dropped": true`.
- **Export**: `lajstrom admin key export --token` prints the public key as PEM, for a verifier
  that is not Lajstrom, should one ever be needed.

The keys of the server can be listed with
`sqlite3 lajstrom.db "SELECT id, fingerprint, created_at, retired_at FROM keys WHERE kind = 'jwt'"`
(or the same query on Postgres).

### `lajstrom admin key`

Every subcommand needs the configuration, the master key in the variable `keys.masterKeyEnv`
names, and a migrated database; none goes through HTTP.

| Command | What it does |
| --- | --- |
| `admin key export --repo apt/<repo>` | prints the repo's armored keyring, what `key.asc` serves; makes the key when there is none |
| `admin key rotate --repo apt/<repo>` | gives the repo a new key, retires the old one for `keys.retireGrace`, drops the repo's cached indexes; prints the new fingerprint |
| `admin key export --token` | prints the public bearer-token key as PEM; makes the key when there is none |
| `admin key rotate --token` | a new bearer-token key; the old one verifies for 21 minutes; prints the new fingerprint |
| `admin key rotate --token --drop` | the same, and the old key verifies nothing from then on |
| `admin key drop --token` | retires the bearer-token key without a successor, whether it decrypts or not (the next token a replica issues, or the next start of `serve`, makes one); prints its fingerprint |

`--repo` and `--token` exclude each other, and one of them is required. Without the master key
the token commands fail and say that `serve` signs with an ephemeral key then.

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
  the run did not see is looked up in the registry by its tag once more, and removed only when
  the registry says the tag is unknown; a package left without versions goes with it. A
  registry that answers with an error removes nothing, and a run that cannot list a repo's
  repositories sweeps nothing in that repo.
- At the end the repo's cached indexes are dropped (the next `GET` renders them again), and tag
  claims older than `ingest.claimTTL` are deleted.

A repo whose format has no plugin in this binary (pypi and maven until they land) is listed
with one error and left alone.

### Running it

```sh
lajstrom reindex --config /etc/lajstrom/lajstrom.yaml             # every repo, with prune
lajstrom reindex --repo apt/ubuntu-internal --report              # one repo, the lists too
lajstrom reindex --verify --json > reindex-report.json            # every manifest read again
```

The output is one line of counts per repo (manifests, added, updated, unchanged, removed,
degraded, referrers, foreign, orphan referrers, drift, errors); `--report` adds the lists, at most
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

### On a cloud registry

ECR bills storage and data transfer, not requests; within a region the transfer is free. A run
over known digests reads the manifests' headers only. A full rebuild reads, per manifest, the
manifest, the metadata layer and what the format plugin checks: for apt the head of the `.deb`
up to its control member. The connection is closed early, but the bytes already in flight are
transferred, so count the size of the packages: 10 000 packages of 2 MB are about 20 GB once.
In the region that is free; through a NAT gateway it is the gateway's processing price (about
$0.045 per GB, depending on the region), which the S3 gateway endpoint of the VPC avoids
because ECR serves layers from S3; across regions or to the internet it is the transfer price
(about $0.09 per GB, on both sides between regions). Prices as of November 2026.

### Disaster recovery

- **The derived tables are damaged or missing, the control plane is intact**: `lajstrom
  reindex`. Frontends keep serving while it runs; what it has not reached yet is missing until
  it does.
- **The database is lost and there is a backup**: restore the backup, `lajstrom migrate`, then
  `lajstrom reindex`. Everything published after the backup is in the registry and comes back;
  users, tokens and bindings come back as they were at the backup. The keys decrypt only with
  the master key of the backup ([The master key](#the-master-key)).
- **The database is lost without a backup**: the control plane is gone. Create the repos again
  with `lajstrom admin repo create` under the same format, name, registry and prefix, then
  `lajstrom reindex` brings back every package. Users and tokens are created again; apt repos
  get new keys, and their clients need the new `key.asc`.
- **The registry was restored from its own backup or a replica**: `lajstrom reindex` with prune;
  rows of manifests the restored registry does not have are removed, drift is reported.
