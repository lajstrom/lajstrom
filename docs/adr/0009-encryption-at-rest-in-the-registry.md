# 0009. Encryption at rest in the registry: OCI encrypted layers, keys held by Lajstrom

- Status: accepted
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
