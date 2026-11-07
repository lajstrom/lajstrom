# Operations

What running Lajstrom asks of an operator beyond the [configuration](configuration.md). The
dev stack is in [dev-stack.md](dev-stack.md).

## Keys and apt signing

Every apt repo is signed with a key of its own: `InRelease` is the `Release` clearsigned,
`Release.gpg` its detached signature, and `GET /apt/<repo>/key.asc` serves the public key that
clients pin with `signed-by=` ([http-routing.md](spec/http-routing.md) §3). The keys are made
and kept by Lajstrom, encrypted with one master key that only the operator holds.

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
`404`, and clients have to mark the source `[trusted=yes]`. Once the index holds signing keys,
`serve` refuses to start without the master key: those keys are useless without it, and the
clients that pinned them would see the repos turn unsigned.

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
new ones, so clients that pinned only the old key fail until they have the new one.

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
