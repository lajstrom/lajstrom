# 0036. The master key in a KMS: a data key wrapped by a symmetric KMS key, named by its id

- Status: proposed (#1388); stage B of [ADR 0034](0034-replacing-the-master-key.md)
- Date: 2026-11-19
- Deciders: gecube

## Context

PCI DSS v4.0.1 3.6.1 asks that "Key-encrypting keys used to protect data-encrypting keys are at
least as strong as the data-encrypting keys they protect" and are "stored separately from
data-encrypting keys", with access to keys "restricted to the fewest number of custodians
necessary" and keys "stored in the fewest possible locations" ([compliance.md](../compliance.md)).
Lajstrom's master keys are 32 bytes in a variable, a file or a directory of files (ADR 0034):
whoever reads the Secret or the store holds them, and so does every copy of that store.

ADR 0034 decided A, the held keys and the sealing key in the index, and then B: the master key as
a data key wrapped by a KMS key (envelope encryption), so that its custody is the KMS's. Its
section "What B needs of A" fixes what B is: one more source of held keys, a data key's id the
HKDF of its 32 unwrapped bytes, the sealing row naming an id and never a source, every move a
rewrap, `serve` not ready while it could not unwrap the sealing key, an unwrapped key kept in
memory, and no feature gate. It leaves four points to this contract: where the wrapped key is
stored, which KMS operations, its caching and how a start waits, and how a data key is made and
named in the configuration. This ADR decides them, and what they lead to.

What there is to build on: the KMS clients of `internal/keys/awskms` and `internal/keys/gcpkms`
(ADR 0020; #114, ADR 0009's Amendment), over `net/http`, with SigV4 from AWS's default chain and
the token of GCP's metadata server, two retries with backoff, and errors that wrap
`keys.ErrKMSUnavailable` when asking again may succeed. They call `DescribeKey`, `GetPublicKey`,
`Sign` and an RSA key's `Decrypt` (OAEP with SHA-256) on AWS, and the same four on GCP.

## Considered options

### Which KMS key and which operations

- **A. A symmetric KMS key, `Encrypt` and `Decrypt`.** AWS `SYMMETRIC_DEFAULT` and GCP
  `GOOGLE_SYMMETRIC_ENCRYPTION`, both AES-256 in the KMS, so the key that wraps is as strong as
  the master key it wraps. The KMS wraps: wrapping takes `kms:Encrypt`, which `serve` never needs.
  Both clouds rotate such a key themselves, and the ciphertext names the key material that
  decrypts it, so a rotation needs no new name. Each client gains two operations (about 150 lines
  each with its tests), and a GCP key is named without a version.
- **B. An RSA decryption key, its `Decrypt` only.** What #114's clients already call, with no
  new operation: the CLI wraps with the public key, the KMS unwraps. *Not taken:* RSA of 3072 or
  4096 bits is about 128 bits of strength (NIST SP 800-57) against the 256 of the key it wraps,
  which 3.6.1 asks against; anyone with the public key can wrap a key of their own choosing; and
  neither cloud rotates an asymmetric key in place (AWS does not rotate one, a GCP version is
  another key name), so each rotation is a new KMS key, a wrap and two changes of the
  configuration.
- **C. Vault transit or another KMS.** A new integration. *Not taken* now: it can come later as
  one more scheme of the same source, with nothing stored differently.

### What the configuration names

- **A. Each data key, by its id and its KMS key** (`keys.masterKeyKMS: [{id, kms}]`): a process
  holds a data key only if its configuration names it and the 32 bytes the KMS answers have that
  id. As in A, the configuration lists the keys a process holds, and the index names the one that
  seals.
- **B. The KMS key alone**, every wrapped key under it held. *Not taken:* whoever can write the
  database and may encrypt with the KMS key could add a key every process holds, and name it to
  seal, with no change of the configuration; ADR 0034 keeps that a change of the sources.
- **C. The ids alone**, the KMS key taken from the row. *Not taken:* a row would choose which KMS
  key a process calls with its credentials.

### Where the wrapped key is

- **A. A control-plane table, one row per data key** (`wrapped_master_keys`, migration 0041), in
  every backup and every snapshot.
- **B. The configuration**, the wrapped key in a Secret. *Not taken:* the store's custody again,
  and the snapshots and backups would not carry what opens them.
- **C. A column of `sealing_key`.** *Not taken:* that row names one key, and a move holds two.

### How a restore into an empty database finds it

A sealed snapshot is sealed with a key derived from the sealing key (manifest-profile.md §9), so
the wrapped key cannot be only in the document that it seals.

- **A. An annotation of the snapshot's manifest**, `lajstrom.snapshot.wrappedKey`, with the
  wrapped key that sealed it, beside the layer; and every row in the document.
- **B. A second layer.** *Not taken:* a snapshot has exactly one layer, and every reader checks it.
- **C. No sealed snapshot under a data key.** *Not taken:* the sealed form is the default.

### After the start

- **A. Unwrapped once, kept until the process ends**, as ADR 0034 says.
- **B. Unwrapped again every hour, and forgotten on a refusal**, so that a KMS key disabled cuts
  running replicas off. *Not taken:* a KMS outage would take running replicas out of the edge,
  and ADR 0034 decided against it.

## Decision

A in each. [index-schema.md](../spec/index-schema.md#a-master-key-in-a-kms), A master key in a KMS,
says what holds; these are the defaults proposed for what the decisions left open:

1. **The configuration names each data key** by its id and its KMS key. A process unwraps only
   those, and holds one only when the KMS's answer has that id; another answer is not held, and
   the log names the id.
2. **A symmetric KMS key**: AWS spec `SYMMETRIC_DEFAULT`, usage `ENCRYPT_DECRYPT`, named by its key
   ARN, `awskms:///arn:aws:kms:<region>:<account>:key/<key id>`; GCP purpose `ENCRYPT_DECRYPT`,
   algorithm `GOOGLE_SYMMETRIC_ENCRYPTION`, named by its key without a version,
   `gcpkms://projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>`. Wrapping is `Encrypt`,
   unwrapping `Decrypt`, each with the additional data `lajstrom master key <id>` (an AWS
   encryption context `{"lajstrom": "master key <id>"}`), so that a wrapped key opens as its own
   id only and AWS's log names the key it unwrapped.
3. **A key of a file is never wrapped.** `lajstrom admin key wrap` makes the 32 bytes itself, so
   that a data key has been in clear only in the memory of the processes that hold it. Moving onto
   the KMS is a rewrap to a new data key (ADR 0034).
4. **`serve` is not ready, and does not refuse to start**, while the sealing key is a data key it
   has not unwrapped; it seals nothing meanwhile. It asks the KMS at its start, then again after 1,
   2, 4, 8 and 16 seconds and every 30 seconds after, until the key opens, whether the KMS refused
   or did not answer, so that a key enabled again or a permission granted ends it without a restart.
5. **An unwrapped key is never fetched again**: it stays in memory until the process ends.
   Disabling the KMS key stops the next starts, not the running processes.
6. **A command unwraps every data key it is configured with at its start**, and exits `1` before
   it writes when one does not open: a rewrap with a key missing would count the values under it
   as under none. `lajstrom admin snapshot restore` takes the wrapped key from its snapshot when
   the database has none.
7. **`replicas.master_keys` lists the keys a replica holds**, a data key once it is unwrapped,
   written again at the beat that follows: the rewrap's guard refuses `--to` a data key that a live
   replica has not unwrapped.
8. **`lajstrom admin key wrap --kms <uri>`** makes a data key: it reads the KMS key, makes 32
   random bytes, has the KMS wrap them, has it unwrap them again and compares, then writes the row
   and its `key.wrap` audit row in one transaction, and prints the entry of `keys.masterKeyKMS`.
   `--id <id>` wraps a data key it holds again under its KMS key, for a GCP rotation.
9. **`lajstrom admin key drop --master <id>`** deletes a row, and refuses while the id is the
   sealing key or a live replica holds it, so that the configuration leaves every process first.
10. **The KMS key outlives the backups and snapshots that hold a key wrapped under it**: disabled,
    not deleted, as ADR 0034 archives an old key. Deleting it loses what the data key sealed.
11. **A sealed snapshot under a data key carries its wrapped key in the manifest's annotation**,
    and every snapshot carries the rows of `wrapped_master_keys` in its document.
12. **Permissions**: `serve` and the commands `kms:Decrypt` (GCP
    `cloudkms.cryptoKeyVersions.useToDecrypt`); `lajstrom admin key wrap` also `kms:DescribeKey`
    and `kms:Encrypt` (GCP `cloudkms.cryptoKeys.get` and `useToEncrypt`).
13. **Audit**: `key.wrap` and `key.drop`, of subject `master:<id>`; `config.change` counts
    `keys.masterKeyKMS` by its value, ids and KMS keys, which are no secret. Lajstrom writes no row
    per unwrap: the KMS's log has each, with its caller.
14. **A rolling update to the release with 0041**: the release before refuses `keys.masterKeyKMS`
    as an unknown key, so every process runs the new release before the source is added.

## Consequences

- With `keys.masterKeyKMS` alone, the master key is in clear in no file, no Secret, no database
  and no backup: who may `Decrypt` with the KMS key holds it, and the KMS's log records each
  unwrap. Moving an installation there is ADR 0034's rollout, with a data key as the new key.
- Every start of a process is one `Decrypt` per data key it is configured with. A KMS that does
  not answer leaves running replicas as they are, and the ones that start then are not ready
  while the key they need seals: a rollout or a node drain during a KMS outage takes the edge's
  replicas out one by one. A command run then exits `1`.
- The KMS rotates its own key: AWS keeps every key material it rotated out, and the wrapped key
  opens as before; GCP keeps the older versions until they are disabled, and
  `lajstrom admin key wrap --id <id>` wraps the key again under the primary first. No value is
  sealed again, and the id and the configuration do not change.
- Disabling the KMS key is not a kill switch for running replicas: stop them as well.
- Deleting the KMS key, or every copy of the row, loses the data key and what it sealed; a
  rewrap with `--allow-lost` is then the way out, as for a lost key (ADR 0034, decision 4).
- The clients of #114 gain `Encrypt` and a symmetric `Decrypt`; no module is added.
