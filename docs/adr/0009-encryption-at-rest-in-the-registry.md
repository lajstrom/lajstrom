# 0009. Encryption at rest in the registry: OCI encrypted layers, keys held by Lajstrom

- Status: accepted; amended by #114 (below, Amendment; accepted on #114 on 2026-11-16, with a
  recovery recipient required)
- Date: 2026-11-08
- Deciders: gecube

## Context

Every artifact lives in an OCI registry that someone else may run (ADR 0001): a cloud
registry in a shared account, a registry team of another department, a managed service. Most
registries encrypt at rest on their own (ECR with KMS, S3 server-side encryption), and for most
audits that is enough. What it does not give is confidentiality from the registry's operator
and from a leaked registry credential: whoever can read the registry can read every artifact.
Enterprises with customer-managed keys ask for exactly that, and they ask for it after the
first deployment, when the storage model is set. This record decides the shape now, so that
the PoC does not close it, and builds nothing.

OCI already has a convention for this: encrypted layers, as `containerd/ocicrypt` defines them
and `containerd` (imgcrypt), `cri-o` and `skopeo` implement them. The layer's media type gets
the suffix `+encrypted`, the blob in the registry is the ciphertext and its digest is the
layer's digest, and the descriptor's annotations carry the data key wrapped for each
recipient: a JWE for a public key, PGP, PKCS7, or a key provider (a KMS) through the
`keyprovider` protocol. The plaintext digest travels inside the wrapped options, readable
only with the key. There is no reason to invent another format.

## Considered options

- **Rely on the registry's own encryption at rest.** Free, and what most deployments will use.
  It does not protect from the registry's operator or from a leaked credential, which is the
  requirement this record is about; the two are not exclusive.
- **A format of our own**: encrypt blobs before the push, keep the mapping in the index. Simpler
  to write, unreadable by any OCI client, and one more thing to document and audit.
- **OCI encrypted layers (ocicrypt), keys held by Lajstrom.** Lajstrom encrypts on the two write
  paths and decrypts on the read path for the protocol clients, which cannot decrypt
  themselves (`apt`, `pip`, `mvn`); OCI clients that implement ocicrypt may pull the ciphertext
  and decrypt with their own key when the policy wraps the data key for them too. Keys are the
  master key of #41 and the KMS of #78 (envelope encryption: a data key per blob, wrapped by the
  repo's key).

## Decision

The third option, as a per-repo policy after the MVP (`policy.encryption: {mode: none | at-rest,
key: <the repo's key or a KMS reference>}`), off by default. The format is ocicrypt's: the
`+encrypted` media type suffix, the data key wrapped in the descriptor's annotations, the
plaintext digest in the wrapped options. Lajstrom holds the keys and does the work:

- *Writes*: the two write paths encrypt. `ingest.Upload` streams the file through the cipher
  into the provider's `Push`; the facade's blob upload session streams each chunk through a
  streaming AEAD (framed, as age and Tink do, so no chunk is buffered) into the backend's
  session, finishes it with the ciphertext digest, and records the plaintext digest the client
  declared. The manifest the client pushes names plaintext digests; the hook rewrites the
  descriptors to the ciphertext digests and the `+encrypted` media types before ingest stores
  it, and keeps the plaintext digests in the wrapped options.
- *Reads*: the one read helper (`ServeBlob`, #27) decrypts and streams; an encrypted blob is
  never answered with a 307 to the backend, since the client could not read it. A client that
  asks `/v2/<name>/blobs/<plaintext digest>` is served through the mapping below; an OCI
  client with its own key may ask for the ciphertext digest and get the blob as it is.
- *The index*: `files.digest` stays the layer digest, what the registry serves. Plaintext hashes
  are what the metadata layers already carry (`sha256` of a `.deb`, the wheel's hash), so the
  apt and pypi indexes do not change. A derived table maps plaintext digests to layer digests
  per repo; it is rebuilt by reindex from the manifests, which needs the key to open the
  wrapped options, and the key is Lajstrom's.
- *Costs, stated*: no blob redirect on encrypted repos, cipher work on every read and write,
  no deduplication of one file across repos with different keys (the same plaintext gives
  another ciphertext), and a `reindex` that needs the key. Signatures (cosign, notation) sign
  the manifest with the ciphertext digests and keep working; referrers do not change.

## What the PoC keeps open

- Blob bytes go through exactly two write paths (`ingest.Upload`'s blob push and the facade's
  upload session) and one read helper (`ServeBlob`, #27). No third path, in any plugin: a
  frontend that wants a blob asks the helper.
- Nothing assumes that a layer's digest is the file's digest. The deb and pypi metadata carry
  the file's own hashes, and the indexes render those (#40 does); the raw version is the file's
  sha256 and stays so, computed before encryption.
- The redirect decision of #27 is taken per blob, through the provider (`OpenBlob`), and a
  policy can turn it off for a repo.
- The key material of #41 (the master key at rest, per-repo keys, rotation with a grace
  period) and the KMS binding of #78 are the same for data keys: nothing is designed twice.
- The manifest hook (#25) owns the manifest between the client and ingest; descriptor
  rewriting lives there, nowhere else.
- `policy` on a repo is JSON, so `policy.encryption` is a key, not a column; the derived mapping
  table and the `+encrypted` rows of manifest-profile.md are the contract changes of the issue
  that builds this.

## Consequences

- A deployment that needs confidentiality from its registry gets it per repo, with its own
  keys, in a format other OCI tooling reads.
- The cost is paid only by the repos that turn it on; everything else keeps redirects and
  deduplication.
- Losing the keys loses the artifacts: the runbook of #41 (master key backup, rotation) is the
  same one, and `reindex` is key-dependent for such repos.
- Two code paths and one helper carry an invariant from now on: artifact bytes never flow
  anywhere else. Reviews check it.

## Amendment: the cipher, the keys and what clients see (#114, 2026-11-15)

The issue that builds this record had to settle what the record left open. Each point below
gives the options, the default chosen and what it means for operators and users.

### The blob cipher: ocicrypt's, not a framed AEAD

The Decision above asks for a framed streaming AEAD (as age and Tink do) and for ocicrypt's
format, and the issue asks that `skopeo` or `ctr` decrypt what Lajstrom stored. The two do not
meet: ocicrypt reads one blob cipher, `AES_256_CTR_HMAC_SHA256`, and a blob in any other cipher
is one that no OCI client decrypts.

- **A framed AEAD of our own** (AES-256-GCM per 64 KiB frame, a counter nonce, a last-frame flag).
  Every frame is authenticated before its bytes go out. No OCI client reads it, and the
  ciphertext is longer than the plaintext.
- **ocicrypt's cipher** (chosen). A random 32-byte data key and 16-byte nonce per blob,
  AES-256-CTR over the bytes, HMAC-SHA256 with the data key over the ciphertext, the HMAC in the
  public options. CTR is a stream: the ciphertext is as long as the plaintext, the cipher holds
  no buffer, and a reader can start at any offset. The HMAC covers the whole blob, so it is
  known only at the end.

Consequences:

- Memory stays constant on both paths: the cipher keeps the CTR counter and the running hashes,
  never a chunk.
- A whole blob is authenticated when its last byte is read. `ServeBlob` holds the last byte back
  until the HMAC (and the plaintext digest, where it is known) match, as it holds it back for
  the digest of a plain blob (guarantees.md, INV-40): a client never gets all of a blob that was
  changed in the registry, and the response is cut instead. A range is not authenticated, as a
  range of a plain blob is not hashed.
- Sizes do not change: a layer's `size` is the file's size, and `maxFileSize` bounds both.
- A chunked upload of the facade keeps the CTR position (the offset) and the sha256 states in its
  row. `crypto/hmac` does not marshal its state, and the record forbids a cipher primitive of our
  own, so the row keeps the HMAC of the bytes so far after the first chunk with bytes; a session
  that gets bytes in a second request is closed with the ciphertext digest, and its HMAC is
  computed by reading the committed blob back once. Clients send a blob in one request
  (docker, containerd, skopeo, oras, crane by default), so the read back is rare.

### The recipient scheme: JWE with RSA-OAEP-256

ocicrypt wraps the private options (the data key, the nonce and the plaintext digest) for each
recipient in one of four schemes: JWE, PKCS7, OpenPGP, or a key provider over a protocol of its
own.

- **OpenPGP.** The repo keys of #41 are OpenPGP keys, but signing keys without an encryption
  subkey, and in a KMS (ADR 0020) they can only sign; `skopeo` decrypts through a `gpg` binary and
  keyring.
- **PKCS7.** It needs an X.509 certificate per recipient, which nothing in Lajstrom issues.
- **A key provider.** Every client would need a program of ours, and so would every recovery.
- **A wrapper of our own.** No client reads it.
- **JWE** (chosen): `org.opencontainers.image.enc.keys.jwe`, a JWE in JSON serialization with the
  content cipher `A256GCM` and one recipient per key, each `RSA-OAEP-256` (`ECDH-ES+A256KW` for an
  EC key of `recipients`), with `kid` the key's fingerprint. `skopeo copy --decryption-key
  <private key PEM>` and `ctr-enc` read it.

**The repo key** is a key pair of its own, a row of kind `enc` in `keys`: one active key per
repo, made on the repo's first encrypted write.

- *Made by Lajstrom* (the default, `policy.encryption.key` empty): RSA 3072 bits. `public` is its
  SubjectPublicKeyInfo, `private_enc` its PKCS #8 private key sealed with the master key, as the
  signing keys are. Every wrap and unwrap is local.
- *In a KMS that never exports it* (`policy.encryption.key` a key URI of ADR 0020): an RSA key
  for decryption with OAEP and SHA-256, AWS `ENCRYPT_DECRYPT` of `RSA_2048`, `RSA_3072` or
  `RSA_4096`, or a GCP version of `RSA_DECRYPT_OAEP_{2048,3072,4096}_SHA256`. JWE's `RSA-OAEP-256`
  is exactly that algorithm, so Lajstrom wraps with the public key it read once (no KMS call on a
  write) and asks the KMS to decrypt the recipient's `encrypted_key` on a read; the rest of the
  JWE opens locally. `private_enc` seals the key URI, as for a signing key in a KMS.

**Recipients for OCI clients.** No client can hold the repo key: a KMS key never leaves the KMS,
and a key Lajstrom made never leaves Lajstrom. `policy.encryption.recipients` lists public keys
(RSA of 2048 bits or more, or EC P-256, P-384, P-521, as PEM) that every new data key is wrapped
for as well: the key a team gives `skopeo` or `ctr-enc`, and a way to recover the blobs without
Lajstrom or its keys. The maintainer decided on #114 (2026-11-16) that an `at-rest` repo needs at
least one, the recovery recipient, so that losing the master key or the KMS key does not lose the
data: creating a repo with `at-rest` and no recipient, changing one to it, removing the last
recipient of one, and a repo of the configuration with it are refused (`400`, or an error of the
configuration).

### The library: go-jose, and the cipher from the standard library

- **`github.com/containers/ocicrypt` v1.3.2** (Apache-2.0), the reference implementation. It adds
  three modules, `containers/ocicrypt`, `smallstep/pkcs7` (MIT) and `stefanberger/go-pkcs11uri`
  (Apache-2.0), and compiles `sirupsen/logrus` (MIT) into the binary. Its streaming encryptor
  keeps its state in memory, which a chunked upload across replicas cannot use, and its JWE
  unwrap takes private keys only, which a KMS key is not: Lajstrom would still need the cipher
  and the unwrap of its own.
- **`github.com/go-jose/go-jose/v4`** (Apache-2.0; chosen, a maintainer decision since layout.md
  §3 does not list it). It is in the module graph already (sigstore-go and grpc require v4.1.6),
  so it adds no module. It writes and reads the JWE, and opens a recipient through the KMS with
  its `OpaqueKeyDecrypter`. The blob cipher is `crypto/aes`, `crypto/cipher` (CTR) and
  `crypto/hmac` of the standard library, in the layout ocicrypt reads; the test of the issue
  checks it with `skopeo`.

### What is encrypted

Every layer of a manifest of an encrypted repo, the metadata layer included; never the config
(ocicrypt encrypts layers only, and a manifest of a managed repo has the empty config), the
manifest or its annotations, and so never the tags.

- **The metadata layer encrypted** (chosen). It holds what the file holds: a deb's control file
  and the list of its files, a wheel's `METADATA` and its description, a POM. Reindex decrypts
  it; no read of a protocol client does, since the renders read the index.
- **The metadata layer plain.** Reindex without the key would rebuild everything but the
  mapping, and the files would still need the key; the registry's operator would read the
  descriptions and file lists of every package.

The consequence for users: the registry's operator sees the names, versions, variants, sizes,
times and annotations of the versions (what `lajstrom.*` annotations carry), and none of their
bytes.

### What a client gets

- **A protocol client** (`apt`, `pip`, `mvn`, `npm`, `curl` of raw, `dnf`) gets the plaintext,
  decrypted by `ServeBlob`, never a `307`.
- **An OCI client through Lajstrom** gets a manifest in the form it was pushed in: the plaintext
  digests and media types, the client's bytes exactly, under the digest the client computed.
  The facade answers a push with that digest, which `oras` and `docker` check. The stored
  manifest is that manifest with each layer descriptor changed in place (the digest, the media
  type, two annotations added), and the facade reverses the change to answer it; the plaintext
  digests come from the mapping below. A blob asked by its plaintext digest is decrypted. The
  stored manifest asked by its own digest, and a blob asked by its ciphertext digest, are
  answered as the registry has them, for an OCI client with a recipient key. So `oras pull`
  returns the plaintext, and `cosign` signs the digest the client sees; the facade stores a
  referrer under the stored digest of its subject, and lists it under the client's.
- **A client straight at the registry** (the registry's operator, a leaked registry credential)
  gets the stored manifests and the ciphertext. It decrypts nothing unless it holds the private
  key of a recipient.

The alternative, the stored manifest through Lajstrom too, breaks every OCI client that does not
implement ocicrypt: `oras push` refuses the digest of the answer, and `oras pull` writes the
ciphertext to disk.

### Deduplication and mounts

- **Across repos**: none. Each repo has its own key, and each upload a new data key.
- **Within a repo**: a blob that the facade finds in the same OCI repository by its plaintext
  digest (the mapping) is not uploaded again (`HEAD` answers it), and a mount from another path of
  the same repo mounts its ciphertext. An upload through a frontend stores its own ciphertext:
  the same `.deb` in two suites is two blobs.
- **A mount from another repo** into an encrypted repo is answered with a plain session, which
  the distribution spec allows: the client uploads, and the blob is encrypted. A mount from an
  encrypted repo into another names the plaintext digest, which the registry does not have there.

### The mapping

`blob_map` (derived) holds, per repo and OCI repository, the plaintext digest of every encrypted
layer and of every manifest of an encrypted repo, the stored digest it maps to, the key that
wraps its data key and that wrapping, and the public options. Reindex rebuilds it from the
manifests and the keys referrers below. A row of a blob that the facade or a publication session
received and no manifest names yet is kept by reindex for a day, the longest a publication
session lives.

### Reindex without the key

A run that cannot open the repo's keys (no master key, a key whose seal does not open, no client
for the KMS that holds one) skips the repo whole and says why in the repo's report (`skipped`):
its rows stay as they are, `blob_map` included. A KMS that refuses or does not answer during the
run fails the manifests it was asked for, which keep their rows, and the next run tries again. A run with the keys rebuilds everything, decrypting each metadata
layer and unwrapping each layer's data key once (one KMS request per layer for a KMS key).

### Rotation: a keys referrer per manifest

A manifest is immutable and its tag never moves, so a data key cannot be wrapped again in the
manifest. `lajstrom admin key rotate --repo <r> --encryption` makes a new key from
`policy.encryption.key` as it is then, retires the old one, which keeps decrypting, and then, for
each manifest of the repo whose layers are not wrapped for the new key, pushes a **keys
referrer**: `artifactType` `application/vnd.lajstrom.keys.v1`, `subject` the manifest, the empty
config, no layers, and the annotation `lajstrom.keys`, the layers' data keys wrapped for the new
key. Reads and reindex take the newest wrapping. A rotation stopped half way continues where it
stopped when run again. It never touches a blob, and never pushes a manifest of a version.

Consequences: an OCI client keeps decrypting with the recipient keys of the manifest, which the
rotation does not change. A rotation does not protect a blob from an old key that leaked, since
the manifest keeps the old wrapping; only a new upload of the file would. It lets the old key
retire: once every manifest has a keys referrer for the new key, nothing needs the old one.
Re-encrypting the blobs under a new key, which an old key that leaked does not open, is a separate
issue, #1244, later.

### Which repos

`at-rest` on hosted and proxy repos of the managed formats. An `oci` repo (pass-through, its
manifests indexed by nobody) and a group (it stores nothing) refuse it with `400`; a group serves
an encrypted member's files decrypted, with the member's key. A repo that turns `at-rest` on
keeps its plain blobs, and one that turns it off keeps its encrypted ones readable: every layer
says by its media type what it is.

### Costs, documented per repo

No blob redirect for an encrypted layer; AES-CTR and HMAC on every read and write (measured in
operations.md); no deduplication across repos or uploads; a `reindex` that needs the keys and
decrypts every metadata layer; with a KMS key, one KMS request per data key unwrapped on a
replica, cached in memory for 15 minutes (4 096 data keys at most), and per layer during
reindex; a chunked upload in more than one request reads its blob back once.
