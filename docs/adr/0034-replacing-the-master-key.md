# 0034. Replacing the master key: the keys a process holds, the one the index names to seal

- Status: proposed; A, then B, decided on #1288 (2026-11-16); the held keys and the sealing key
  in the index decided in the review of #1371 (2026-11-18); what was left to B's contract in
  [ADR 0036](0036-the-master-key-in-a-kms.md) (#1388)
- Date: 2026-11-18
- Deciders: gecube

## Context

PCI DSS v4.0.1 3.6.1 and 3.7.1 to 3.7.7 apply to "cryptographic keys used to protect stored
account data". Lajstrom's keys protect signatures, bearer tokens, webhook secrets and, with #114,
artifacts, so these requirements reach them only where an organisation brings them into its
key-management procedures. Among them, 3.7.5: keys are retired or replaced when "The integrity of
the key has been weakened, including when personnel with knowledge of a cleartext key component
leaves the company" and when "The key is suspected of or known to be compromised". SOC 2 CC6.1;
its points of focus (March 2020 edition) include "Protects Encryption Keys" (#1288,
[compliance.md](../compliance.md)). What Lajstrom has:

- One master key, 32 bytes in `keys.masterKeyEnv` or `keys.masterKeyFile`, which seals with
  AES-256-GCM every private key of `keys` (apt and rpm signing keys, the bearer-token key, the
  policy key, the keys of encrypted repos, or the key URIs of keys in a KMS), the webhooks'
  secrets, the data keys of open uploads to encrypted repos, and sealed snapshots
  ([index-schema.md](../spec/index-schema.md#the-master-key-and-its-replacement)).
- Each sealed key rotates on its own (`lajstrom admin key rotate`).

No way replaces the master key. operations.md's path for a new one is a rotation of every apt repo
(every client takes a new `key.asc`) and `admin key drop --token`; webhook secrets are made again,
and an encrypted repo's key, sealed with it, cannot be made again without losing its files. A
custodian who leaves means re-keying every client.

In practice the key comes from a secret store through a CSI driver (the Secrets Store CSI
driver with Vault, AWS Secrets Manager and the like), mounted as files and rotated in the store.
A pod starts with what the store holds at that moment, whatever starts it: a rollout, a node
drained, a crash. The order in which the processes see a change of their keys is not the
operator's.

## Considered options

- **A. `lajstrom admin key rewrap`**: open every sealed value and seal it again under a new key,
  while every process holds both. How a process knows which key seals:
  - **A1. From its configuration**: a second key of the configuration beside the master key,
    which only decrypts (the decision on #1288 named it for the previous key, and the first review
    of #1371 for what it does), and three rolling updates so that no request fails: the new key as
    the one that only decrypts, then the new key sealing and the old one only decrypting, then
    without the old key. *Not taken* (the maintainer's review on #1371, 2026-11-18): roles taken
    from the configuration make the safety of a replacement depend on every process getting its
    changes in the right order, and with keys from a CSI driver that order is not under control: a
    pod started for any reason takes the role its files say then.
  - **A2. From the index**: the configuration lists the keys a process holds and gives them no
    role (`keys.masterKeyDir`, a directory with a key per file, beside `keys.masterKeyEnv` and
    `keys.masterKeyFile`); a control-plane row names the key that seals, and the rewrap sets it.
  - **A reload of the key files on `SIGHUP` or `SIGUSR1`, or a watch of them.** *Not taken*:
    `SIGHUP` already drains `serve` (#720, #751); Kubernetes sends no signal when a volume
    changes; and a reload path behind a feature gate is a second path to test. Reading
    `keys.masterKeyDir` again at every beat may come later, with no gate, since it only changes
    the keys a process holds and never which key seals; it is not part of this contract.
- **B. The master key in a KMS (envelope)**: the index keeps data keys wrapped by a KMS key (AWS
  KMS, GCP Cloud KMS, Vault transit), unwrapped at start; rotation, custody and its log are the
  KMS's. A KMS call at every start, and a dependency on it.
- **C. Keep today's path**: new keys for every repo, every client pins again. Not workable for
  encrypted repos.

## Decision

A2, then B as a later step (the maintainer: A then B on #1288, 2026-11-16; A2 in the review of
#1371, 2026-11-18). index-schema.md, The master key and its replacement, says what holds; these
are the defaults proposed for what the decisions left open:

1. **Keys come from the configuration only**, and are named by their ids everywhere else: the
   command takes no key in a flag, an argument or standard input, which would leave it in a
   shell's history and in the list of processes; no log line, error, row or output holds a key or
   a value.
2. **A process that does not hold the key the index names never seals.** `serve` refuses to start
   and a command exits `1`, naming the id; a replica whose beat, every 30 seconds, reads a key it
   does not hold stops sealing and is not ready. At the start it refuses rather than staying up not
   ready, since its keys change only at a start: the orchestrator's restart is what brings a key
   added to the store.
3. **The guard**: a rewrap refuses while the command, or a live replica, does not hold the key it
   names; a replica of a release before 0040 holds none. A replica that starts after the guard
   without that key fails closed at its first beat (2).
4. **A value under no held key stops the run before it writes**, naming the row, never the value.
   `--allow-lost` leaves such values as they are instead, for a key that is lost: without it a
   lost key would leave no way to name a new one.
5. **Every value of `keys.private_enc` and `webhooks.secret_enc`**, retired keys included: a
   retired `enc` key still decrypts the files wrapped for it.
6. **The switch and the `pending` row in one transaction**, by compare-and-set on the id read: no
   switch is made without its record, and the sealing row leads to the audit row of who ran it.
7. **A minute after the switch** (a beat of 30 seconds and as much for one that runs late), the
   batches; then a count again, which finds what a late replica sealed under the old key, and a
   run again seals it.
8. **The first start of a release with 0040**, without a row: a process that holds one key records
   it, after it opened the first key of the index with it, insert-if-absent; one that holds several
   refuses and names `lajstrom admin key rewrap --to <id>`. Which key seals is never taken from the
   order or the names of files.
9. **The old key is archived, not destroyed, as long as a backup or a sealed snapshot made under it
   is kept**: those are not sealed again, and a restore needs the key among the held ones. A
   restored sealing row that names a key no process holds stops every process until the key is
   added, or a rewrap names one they hold.
10. **The sealing row is in a snapshot**, as in a backup: a restore brings back the key that sealed
    the values it brings back, and a restore into an empty database chooses nothing.
11. **`keys.masterKeyDir`**, read at start: the regular files and the symbolic links of a
    Kubernetes Secret or a CSI volume, without the entries whose name starts with a dot (`..data`
    and the volume's timestamped directory); at most 8 keys in all sources.
    `keys.masterKeyEnv` and `keys.masterKeyFile` stay, for an installation with one key, and their
    key joins those of the directory.
12. **A key's id**: 8 bytes of HKDF-SHA256 of the key, in hex, in the start's log, the `replicas`
    rows (the ids of every key a replica holds), the sealing row and the audit row.
13. **Batches of 100 rows**, each a transaction, each value written by compare-and-set; idempotent.
    One transaction for every row, the issue's wording, would hold SQLite's write lock for the
    whole run.
14. **Its row**, `key.rewrap`, written as a registry write's: `pending` with the ids `from` and
    `to` and the values found under each other key, then `ok` or `failed`. A run that finds nothing
    left writes `ok` with zeros: with no upload session open under the old key, the operator's sign
    that the old key can go.
15. **No row says which key sealed it**: a value is tried with the sealing key, then with the
    others, and the sealed format does not change, so that the release before still reads every
    row.
16. **Open upload sessions are not sealed again**; they end, an hour after their last request at
    the latest.
17. **`--dry-run`** shows the sealing key, each live replica's keys, and the values under each key;
    `auth.requireReason` covers the command.
18. **The rollout: two changes of the sources and one command**, the same after a leak: the new key
    added to the store, so that every process holds both; `rewrap --to <new id>`; the old key
    removed once a rewrap run again says nothing is left under it.

## What B needs of A

- **A source of held keys, not a mechanism.** B adds a source beside `keys.masterKeyDir`,
  `keys.masterKeyFile` and `keys.masterKeyEnv`: a KMS key of ADR 0020's key URIs (`awskms:///…`,
  `gcpkms://…`), through #114's KMS clients. Its data keys, 32 bytes that Lajstrom makes and keeps
  in the index wrapped by that KMS key, join the held keys once unwrapped. A data key's id is A's,
  HKDF of its 32 unwrapped bytes, so that ids compare across sources.
- **An id, never a source.** The sealing row names an id. During a move one replica may hold a key
  from a file and another the same key from the KMS; a rewrap's guard compares ids only.
- **Every move is a rewrap.** Onto the KMS: the KMS source added to every process, `rewrap --to`
  the data key's id, the file key removed, as A's rollout. Off the KMS: the same with a file key. A
  new data key under the same KMS key: the same. A rotation of the KMS key itself wraps the data key
  again: its id does not change and no value is sealed again.
- **A KMS that does not answer.** At the start the process holds the keys of its other sources;
  while the sealing key is a data key it could not unwrap, `serve` is not ready, rather than
  refusing to start as with a key no source holds, since the KMS may answer later. Once unwrapped,
  a data key stays in memory: it is not fetched again for each seal or open.
- **Nothing else changes**: what is sealed, the format, the additional data, the sealing row, the
  guard, the rewrap and the rollout. No feature gate: one mechanism serves A and B, and nothing in
  A makes B add a second path for sealing or for the rollout.

Left to B's contract, which starts from these:

- where the wrapped data key is stored: a control-plane row, its table and migration, and whether a
  snapshot carries it;
- which KMS operations: an RSA decryption key's `Decrypt`, which #114's clients already call (OAEP
  with SHA-256: AWS `kms:Decrypt`, GCP `asymmetricDecrypt`), with no new operation, or a symmetric
  key, which adds `Encrypt`;
- the caching and refresh of the unwrapped data key, how a start waits for the KMS, and what
  readiness says meanwhile;
- how a data key is made, and the name of its key of the configuration.

## Consequences

- A custodian who leaves, or a key that may have leaked, means a new key in the store, one command,
  and the old key out of the store; no client pins anything again, and no request fails on the way.
- A pod started at any step, for any reason, holds what the store holds then and seals with the key
  the index names: no order of restarts is the operator's to keep.
- Every replica must hold the key the index names ([guarantees.md](../spec/guarantees.md), one
  database): one that does not refuses to start, or seals nothing and is not ready.
- The old key stays needed for the snapshots and backups made under it: it is archived, apart from
  the store, until they are gone.
- A replica of the release before writes no key ids, and seals with its one key whatever the index
  says: a rewrap refuses to run until every replica runs a release with 0040, and the sources hold
  one key until then.
- A command reads the sealing row once, at its start: one that started before a switch may seal
  under the old key until it ends, which the rewrap run again before the old key goes seals again.
- The Helm chart, which refuses `keys.masterKeyFile` and passes one key as the variable
  `LAJSTROM_MASTER_KEY`, gains with the implementation a way to mount a Secret or a CSI volume as
  `keys.masterKeyDir`, never with `subPath`, and keeps the variable for an installation with one
  key.
