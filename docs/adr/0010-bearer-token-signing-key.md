# 0010. Where the bearer-token signing key lives: the database, under the master key

- Status: proposed
- Date: 2026-11-07
- Deciders: gecube

## Context

The OCI facade issues bearer tokens (`docs/spec/rbac.md` §4, issue #26): `GET /v2/token` with
Basic credentials answers a JWT signed with ES256, `exp` 20 minutes, which every `/v2` request
then carries and every replica must verify. One asymmetric key pair signs and verifies; only
Lajstrom ever verifies, so there is no JWKS to publish and no third party to agree with.

Three texts say three things about that key. Issue #26, written for M1, says "key file from
config, generated on first start if missing with a warning", and `auth.tokenSigningKeyFile`
sits in the configuration since #7 as its placeholder. The index schema
([index-schema.md](../spec/index-schema.md), `keys`) has a `kind` of `jwt` and `repo_id` null
for "the server key", next to the apt signing keys of #41, encrypted with the master key. The
acceptance criteria (H-12) require a token issued by replica A to be accepted by replica B
"because the signing key is in the database, not in a replica's memory or local file". The
lead asked for the trade-offs of every place the key could live, written down, before #26 is
briefed.

Whatever the place, the rest is the same. ES256 on P-256, asymmetric so that the public key
can one day be handed to a verifier that is not Lajstrom (the edge proxy of ADR 0008) without
sharing a secret; the verifier accepts `ES256` and nothing else (no `none`, no HMAC: the
algorithm is pinned, not read from the token), and a token must name its key with a `kid`.
The pin is to the algorithm of the key, not to a constant: each `keys` row says
its algorithm (as the apt keys do with `keys.algorithm`), the verifier accepts for a `kid`
exactly the algorithm of that key, and a change of algorithm is a rotation, a new key with
another algorithm beside the old one for the token lifetime, with no change of format. ES256
today; EdDSA is a configuration value away; a post-quantum signature (ML-DSA, whose JOSE
registration is a draft and whose Go implementation is arriving) is the same rotation once
both exist, and until then the exposure is bounded by the key's lifetime, since a signature
key protects nothing at rest: an attacker has to break the key while it signs. A periodic
rotation (the runbook suggests one with the master key's) keeps that lifetime short.
Tokens of 20 minutes with 60 s of clock skew, so the clocks of the replicas must agree to
that; the key is loaded or made at startup, not at the first token, so that a server that
cannot load its key fails at start and not under a client, and is cached in memory, with a
reload when a token names a `kid` the cache lacks, at most once a second, so that a flood of
tokens with made-up `kid`s costs one query a second and not one a request. Rotation means: a
new key signs from now on, the old key verifies until every token it signed has expired (20
minutes plus the skew), then it is gone; a compromised key is dropped at once and every
outstanding token dies with it, which is the point of short tokens.

## Considered options

### A. An environment variable

`LAJSTROM_TOKEN_KEY` holds the PEM of the private key in one line (base64), with
`LAJSTROM_TOKEN_KEY_PREVIOUS` for the key being retired; the operator generates the key
(`openssl ecparam -genkey -name prime256v1`), keeps it in the secret store and injects it into
every replica, as the master key is injected today.

- Provisioning: one more secret beside the master key, generated and formatted by the
  operator; a multi-line PEM in an environment variable is base64 and a decode step.
- Consistency between replicas: by deployment discipline; nothing checks that two replicas
  carry the same key until a token from one is refused by the other.
- In Kubernetes: a variable from a `Secret` is read when the pod starts and never again, so
  every rotation is a rolling restart of every replica.
- Rotation: two rollouts, not one. First every replica must learn to *verify* the new key
  (`PREVIOUS` is the old, `KEY` still the old, a third variable or a restart with the new key
  as `PREVIOUS`), then every replica switches to *sign* with it; a single rolling restart with
  `KEY=new, PREVIOUS=old` leaves the replicas not yet restarted refusing tokens the restarted
  ones signed, for up to 20 minutes. Afterwards someone removes `PREVIOUS` by hand.
- Dependencies: none. Tokens work without the database being reachable for verification and
  without a master key (apt signing off, bearer tokens on).
- Exposure: the process environment (`/proc/<pid>/environ`, `docker inspect`, a core dump); the
  master key has the same exposure already.
- Backups: the key is not in the database backup, which is harmless (tokens live 20 minutes).
- The dev stack: `make run` without the variable needs an ephemeral in-memory key, or the
  variable in `.env`; a second replica of the `ha` stack needs the same value.

### B. A file

`auth.tokenSigningKeyFile`, a PEM on the file system, mounted as a secret (a Kubernetes
`Secret` volume, a file from the secret store), with a previous-key file for rotation; or, as
#26 said, generated by the server on first start when missing.

- Provisioning: natural where secrets arrive as files; the same operator steps as A otherwise.
- Consistency between replicas: a file generated on first start is a *different* key on every
  replica unless the directory is shared, so the auto-generation of #26 works on one node
  only and is a trap in HA (H-12 fails silently until a client hits the other replica). A
  file provisioned from the secret store has A's discipline problem.
- Rotation: replace the file and signal the process (a reload on `SIGHUP`, or a restart); the
  two-phase problem of A, plus the reload code.
- In Kubernetes: a `Secret` mounted as a whole volume is updated in place (with the kubelet's
  delay, a minute or so), so the reload path can work; a file mounted with `subPath`, the
  common way to put one file into a directory the image owns, is never updated, and rotation
  is a restart again.
- Dependencies: none, as A.
- Exposure: file permissions on the node; not in the process environment; present in node
  backups and images if someone bakes it in.
- The dev stack: a key file in the checkout (ignored by git) or generated on first start; the
  `ha` stack needs the same file for both replicas.

### C. The database, encrypted with the master key

A row of `keys` with `kind` `jwt` and no repo, made at startup when there is none, the
private key encrypted with the master key as the apt signing keys are (AES-256-GCM, #41). Every
replica reads it from the database, decrypts it with the master key from its own environment,
caches it, and reloads when a token names a `kid` it does not know. `auth.tokenSigningKeyFile`
goes away.

- Provisioning: nothing beyond the master key, which apt signing requires anyway; the key is
  made by the server and never leaves the database in clear.
- Consistency between replicas: by construction, one row; two replicas that make the first
  key at once leave one (the `keys_active` index).
- Rotation: `lajstrom admin key rotate --token` (the apt command with another kind): the new
  key signs at once on every replica (the next reload, or the first token with its `kid`),
  the retired key verifies for the token lifetime plus the skew and is deleted after. One
  command, no restart, nothing to remember. A compromised key: the same with no grace.
- Dependencies: the master key is required for bearer tokens, and the database is on the
  verification path once per key, not per request (the cache). The database is already on the
  path of every request for the index, so the failure mode is not new. Without a master key
  there are no bearer tokens: the dev stack then signs with an ephemeral in-memory key and
  logs at startup that tokens do not survive a restart and are not shared between replicas
  (one process, which is what `make run` is); a production start without a master key is
  the same warning, and the operator who wants tokens to survive sets the key.
- Exposure: encrypted at rest; a database backup carries the encrypted key, useless without
  the master key; the clear key exists in process memory only, as in A and B.
- A row that does not decrypt (a backup restored under another master key, a typo in the
  variable) stops the server at startup naming the key, as an apt key does: the operator sets
  the right master key, or drops the row (`admin key drop --token`) and the server makes a
  new one; nothing is silently regenerated over a key that exists.
- In Kubernetes: nothing to mount or restart; the master key is the one `Secret`, and it
  changes only when the master key itself is rotated.
- Master key rotation re-encrypts this row with the apt keys, whichever procedure #41's
  follow-up chooses; nothing specific to the token key.

### The three side by side

| | A. environment | B. file | C. database |
| --- | --- | --- | --- |
| Secrets the operator manages | master key + token key | master key + token key file | master key |
| Same key on every replica | by discipline | by discipline, or broken by auto-generation | by construction |
| Rotation | two rollouts and a manual cleanup | file replace + reload, two phases | one command, no restart |
| Rotation in Kubernetes | rolling restart (env is read at pod start) | in-place update without `subPath`, restart with it | nothing to do |
| Needs the database to verify | no | no | once per key (cached) |
| Needs the master key | no | no | yes (ephemeral key without it) |
| Key at rest | secret store, process env | secret store, node disk | database, encrypted |
| Code | PEM parsing, two variables | PEM parsing, reload, generation | the `keys` service with one more kind |
| Fits the schema and H-12 | no | no | yes |

## Decision

C: the token signing key is a row of `keys` with `kind` `jwt`, encrypted with the master key,
made at startup when missing and rotated with `lajstrom admin key rotate --token`; the retired key
verifies for the token lifetime plus the clock skew. `auth.tokenSigningKeyFile` is removed
from the configuration before any release reads it. Without a master key the server signs with
an ephemeral in-memory key and says so at startup. The `kid` of the JWT header is the key's
fingerprint.

The argument is the one that decided #41: the operator holds one secret, the master key, and
everything derived from it lives in the database, consistent across replicas and rotated
online. A and B are stateless and simpler in the single-node case, and lose exactly where the
product promises replicas (H-12) and operations without restarts.

## Consequences

- #26 (and #43 before it) builds on `internal/keys` rather than on a key file: one more
  `kind`, the server key with no repo, the rotate command's flag, the grace from the token
  lifetime rather than `keys.retireGrace`.
- The `keys` row carries its algorithm; `keys.tokenAlgorithm` (ES256, later EdDSA and what
  JOSE registers next) chooses the algorithm of keys made from now on, as `keys.algorithm`
  does for apt.
- The configuration loses `auth.tokenSigningKeyFile` (`docs/configuration.md`,
  `lajstrom.example.yaml`); `keys.masterKeyEnv` gains a second reader, named in its description.
- The ephemeral key without a master key keeps the dev stack and the quick starts working with
  no new variable; the startup line names the consequence.
- A token is trusted for its lifetime, as the distribution token flow expects: a user
  disabled or a binding removed takes effect at once on the frontends and at the next
  `/v2/token`, and within 20 minutes on `/v2` for a token already issued (a deleted repo is
  the exception: its name is unknown at once, G-04). An operator who needs every outstanding
  token gone now rotates the key with no grace; #26 says so in `docs/auth.md`, and whether the
  facade should instead re-evaluate the bindings on every `/v2` request is #45's call, with
  the cost of one authorizer lookup per request.
- If a deployment ever needs stateless verification (a token checked by something that is not
  Lajstrom and has no database), that is a new requirement, and A is the answer then: the key
  would be exported from the database, not generated elsewhere.
