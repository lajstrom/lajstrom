# 0020. Repo signing keys in a cloud KMS: the key stays there, the database seals its name

- Status: proposed
- Date: 2026-11-14
- Deciders: gecube

## Context

Every apt and rpm repo signs its indexes with an OpenPGP key of its own: `InRelease` and
`Release.gpg`, and `repomd.xml.asc` (#41, #577). Lajstrom makes the key on the repo's first use
and keeps its private key in `keys.private_enc`, sealed with the master key
([index-schema.md](../spec/index-schema.md), `keys`). Some operators may not keep a signing key
outside a key management service: their policy wants the key made in an HSM, never exported,
every use of it in the cloud's audit log, and access to it granted and revoked by IAM (#78).

AWS KMS and GCP Cloud KMS both hold asymmetric signing keys. Each hands out the public key and
signs a digest it is sent, and neither ever releases the private key. What apt and rpm verify is
an OpenPGP signature packet, and no KMS writes one. So Lajstrom has to wrap a KMS key in an
OpenPGP key: a v4 public key packet around the KMS's public key, a user id with its
self-signature, and signature packets whose digest the KMS signs. Some choices are fixed by the
tools:

- **The OpenPGP library.** ProtonMail/go-crypto, already approved, signs RSA and ECDSA through a
  `crypto.Signer` (`packet.Signature.Sign`). It signs EdDSA only with its own key type, so an
  Ed25519 key in a KMS cannot sign here.
- **The algorithms the clients take.** apt 2.7 and later run `gpgv --assert-pubkey-algo` with RSA
  of 2048 bits or more, Ed25519, Ed448 and the NIST curves. dnf/librepo and zypper verify
  `repomd.xml.asc` with gpg or with rpm-sequoia. RHEL 9 refuses SHA-1.
- **One hash for all the keys of a repo.** During a rotation every key of the repo signs the same
  document, and they all sign with SHA-256.

## Considered options

### Where the reference to the KMS key lives

- **A. In `private_enc`, sealed with the master key like a private key, with the fingerprint as
  additional data.** No migration. The seal binds the KMS key to the fingerprint: someone who can
  write the database but has no master key cannot point a repo at a KMS key of their own, just as
  today they cannot plant a private key. The master key is still needed, as it is for every
  other key.
- **B. A new column `keys.kms` in plain text.** The reference can be read with SQL, and a
  KMS-only server would not need the master key. But it takes a migration, and a database write
  alone could then change what signs a repo; only a check at every signature against the stored
  fingerprint would catch that.

### The creation time of the OpenPGP key

The fingerprint of a v4 key hashes its creation time.

- **A. The KMS key's own creation date** (AWS `DescribeKey`, GCP `cryptoKeyVersions.get`), to the
  second. The fingerprint depends on the KMS key alone, so the same KMS key gives the same
  fingerprint in two repos, in two replicas that build it at once, and when a repo rotates back
  to it. Reading it needs one more permission, and only when the key is made.
- **B. The time Lajstrom first builds it.** No extra permission, but every repo, and every
  rotation back to the key, gets another fingerprint for the same KMS key.

### The KMS clients

- **A. Our own clients over `net/http`, with no new module.** AWS takes three calls of the KMS
  JSON 1.1 API (`DescribeKey`, `GetPublicKey`, `Sign`), signed with SigV4 from the core module of
  `aws-sdk-go-v2` with credentials from its `config`, both already approved for ECR. GCP takes
  three REST calls of Cloud KMS with the metadata-server token that `auth.gcp` of oci upstreams
  already fetches. About 300 lines of our own, behind one interface of `internal/keys`.
- **B. `github.com/aws/aws-sdk-go-v2/service/kms`.** A generated client with the endpoints of
  every partition, FIPS and dual-stack switches and the SDK's retryer. It is one more module of
  an approved SDK, under the same license, with no new transitive module, but layout.md §3 names
  only `ecr` and `config`.
- **C. `cloud.google.com/go/kms`.** Every credential source of Application Default Credentials
  (key files, workload identity federation), but it brings gRPC, the Google auth libraries and
  about twenty modules.

## Decision

**A key URI names a KMS key**, in cosign's form without an endpoint:

- `awskms:///<key id | key ARN | alias/<name> | alias ARN>`;
- `gcpkms://projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>/cryptoKeyVersions/<n>`. A GCP
  key must name a version.

**`keys.kms`** is the key URI of the repo keys made from now on: a repo's first key, and a
rotation that names no key. **`lajstrom admin key rotate --repo <r> --kms <uri>`** moves one repo
onto a KMS key, and **`--local`** moves it to a key Lajstrom makes. Every such move is a rotation:
the old key keeps signing beside the new one, and stays in `key.asc`, for `keys.retireGrace`.
Key management stays in `lajstrom admin key`, so no HTTP route or RBAC rule changes.

**Making a KMS key.**

- Lajstrom reads the key's public key, creation date and spec.
- An alias is resolved to the key ARN, so a later move of the alias does not change what signs
  an existing row.
- It accepts AWS `SIGN_VERIFY` keys of `RSA_2048`, `RSA_3072`, `RSA_4096` and `ECC_NIST_P256`, and
  GCP versions in state `ENABLED` of `RSA_SIGN_PKCS1_{2048,3072,4096}_SHA256` and
  `EC_SIGN_P256_SHA256`. Any other key is refused with an error that names its spec.
- It builds the v4 public key with the KMS key's creation date (option A), the user id
  `Lajstrom <format>/<repo>`, no expiry, and a self-signature that the KMS makes. That key goes
  into `public` and `fingerprint`.
- The canonical key URI goes into `private_enc`, sealed (option A). An OpenPGP packet never
  starts with an ASCII letter, so the plaintext tells a KMS key from a private key.

**Signing.** Each signature sends the SHA-256 digest to the KMS (AWS `MessageType: DIGEST` with
`RSASSA_PKCS1_V1_5_SHA_256` or `ECDSA_SHA_256`; GCP `asymmetricSign`). The answer is checked with
the stored public key before it goes into a packet. A signature that does not verify is an
error, and so is any failure of the signer: go-crypto's RSA branch drops the error of the
signer it calls, so keys watches the signer instead. Nothing else changes: the signed documents
are made once per render and kept in `index_cache`, and `Current` and `PublicKeys` read only the
database.

**Failure is visible, never unsigned.**

- A KMS that does not answer, times out, answers 5xx or throttles is retried twice with backoff.
  Then it is `format.ErrUnavailable`: the signed routes answer **503 with `Retry-After: 5`**,
  store nothing, and serve nothing unsigned.
- A refusal (access denied, a disabled or deleted key, the wrong usage) is a 500.
- A retired key whose KMS refuses is left out with a warning, as a retired key that does not
  decrypt is today. A retired key whose KMS is only unavailable fails the document instead.

**The clients** are option A. AWS credentials come from the SDK's default chain. The region comes
from the key ARN, else from the SDK. `AWS_ENDPOINT_URL_KMS`, then `AWS_ENDPOINT_URL`, sets the
endpoint, else it is `kms.<region>.` with the partition's domain (`aws`, `aws-cn`, `aws-us-gov`).
GCP's token comes from the metadata server alone. Its client moves out of `internal/proxy` into
`internal/gcpauth`, which the oci upstreams and the GCP KMS client share.

## Consequences

- An operator can keep the signing keys of apt and rpm repos in a KMS. Lajstrom then holds the
  public key and a sealed name, and every signature is a KMS request in the cloud's audit log.
- A KMS outage stops new signatures only. A repo whose indexes did not change keeps serving the
  signed documents it has. A repo that published during the outage answers 503 on its signed
  routes until the KMS is back, and apt and dnf fail visibly; they are never handed an unsigned
  index.
- **Permissions.** AWS needs `kms:Sign` and `kms:GetPublicKey`, plus `kms:DescribeKey` where a
  key is made: in `admin key rotate`, and in `serve` for a repo's first key under `keys.kms`. GCP
  needs `roles/cloudkms.signerVerifier`, plus `cloudkms.cryptoKeyVersions.get` in the same places.
- **Not supported:**
  - an Ed25519 key in a KMS (go-crypto), and P-384 or P-521 keys (they would need a hash of their
    own);
  - GCP credentials other than the metadata server, which would need option C;
  - partitions other than `aws`, `aws-cn` and `aws-us-gov` without `AWS_ENDPOINT_URL_KMS`;
  - a server that signs with KMS keys and has no master key.

  Each can come later without changing what is stored.
- If the maintainers prefer option B for AWS, it is a swap behind the same interface, with
  layout.md §3 extended.
