# Signed SBOMs and scan reports

An SBOM or a vulnerability report attached to a version is worth something only when you know who
made it. Lajstrom stores these attachments in the registry next to the version, lists them per
version with their history, and tells you whether a key you trust signed each one
([#89](https://github.com/lajstrom/lajstrom/issues/89)).

Lajstrom does not scan and does not sign. A job of yours makes the SBOM and the scan report, signs
them with its key, and pushes them; Lajstrom checks the signature against the keys you configured
and labels the attachment. A label never blocks a push. Refusing to serve a version without a
verified attachment is the policy layer of
[#101](https://github.com/lajstrom/lajstrom/issues/101), not built yet.

This page uses cosign v3.1.4 with a key pair. Its steps were run on 2026-11-14 against Lajstrom
of the #89 stack on Zot 2.1.23, on a raw file: the attestations, the signature,
`cosign verify-attestation` through Lajstrom, the list, the reindex after a change of the trust
and the delete. What is not there yet is at the end.

## 1. Trust the key that signs

Make a key pair for the job that produces the attachments, and keep `cosign.key` in that job:

```sh
cosign generate-key-pair   # cosign.key, cosign.pub
```

Give Lajstrom the public key, in the configuration of every replica
([configuration.md](../configuration.md#attachments)):

```yaml
attachments:
  trust:
    - name: scanner
      publicKeyFile: /etc/lajstrom/scanner.pub
      predicateTypes:                      # leave out to trust the key for every type
        - https://cyclonedx.org/bom
        - https://cosign.sigstore.dev/attestation/vuln/v1
```

`serve` reads the key at start and refuses one it cannot parse. A key in a KMS works the same:
put its public key here and let the job sign with `cosign attest --key awskms://…`.

## 2. Attach an SBOM to a version

Attachments go through the OCI API of Lajstrom, `/v2`, for a version of any format: a wheel, a
`.deb`, a jar, a raw file. Ask the management API for the version's OCI repository and its
manifest digest:

```sh
curl -s -u "$USER:$TOKEN" "$LAJSTROM/api/v1/repos/pypi/internal/packages/requests/versions/2.32.3" |
  jq -r '"\(.ociRepository)@\(.variants[0].digest)"'
# pypi/internal/requests@sha256:5c11…
```

Then sign and push the SBOM as an in-toto attestation. The credentials are a Lajstrom user or a
deploy token with `write` on the repo, in the job's docker configuration:

```sh
syft scan dir:. -o cyclonedx-json > sbom.json      # or trivy --format cyclonedx
cosign attest --key cosign.key --type cyclonedx --predicate sbom.json \
  --tlog-upload=false --use-signing-config=false -y "$HOST/pypi/internal/requests@sha256:5c11…"
```

`--tlog-upload=false` keeps the signature out of the public Rekor log, and cosign 3.1 also wants
`--use-signing-config=false` for that (both deprecated in favour of a signing configuration without
a transparency log). Lajstrom does not look at a transparency log either way. A vulnerability report goes the same way: `trivy image --format
cosign-vuln` and `cosign attest --type vuln`. A plain SBOM pushed with `oras attach` is stored and
listed as `unsigned`.

To tell which tool and which vulnerability database a result came from, without reading it,
`oras attach --annotation` can set `lajstrom.attachment.tool`, `.toolVersion`, `.database` and
`.input` (the digest of the SBOM a scan was computed from; [manifest-profile.md](../spec/manifest-profile.md)
§6.2). `cosign attest` cannot set manifest annotations.

## 3. See the attachments and their labels

```sh
curl -s -u "$USER:$TOKEN" \
  "$LAJSTROM/api/v1/repos/pypi/internal/packages/requests/versions/2.32.3/attachments" |
  jq -r '.attachments[] | "\(.kind)\t\(.verification)\t\(.signedBy // .reason)\t\(.createdAt // "-")"'
```

```text
sbom       untrusted  no trusted key verifies its signature    2026-11-14T12:48:49Z
scan       untrusted  the key of scanner signed it, and is not trusted for the predicate type "https://cosign.sigstore.dev/attestation/vuln/v1"    2026-11-14T12:48:26Z
signature  verified   scanner    2026-11-14T12:48:26Z
sbom       verified   scanner    2026-11-14T12:48:24Z
```

That run trusted `scanner` for CycloneDX and signatures only: its vulnerability report is
`untrusted` and says why, and the SBOM signed by another key is `untrusted`. `cosign
verify-attestation --key cosign.pub` checks the same attestations through Lajstrom's `/v2`.

[api.md](../api.md#attachments-of-a-version-get-apiv1reposformatnamepackagespackageversionsversionattachments)
has the filters (`?kind=scan`, `?verified=true`, `?predicateType=`) and every field. The list is
newest first, and every result is kept: a scan run again after a database update is one more
entry, so the first `scan` entry is the latest.

| Label | Meaning |
| --- | --- |
| `verified` | a key of the trust, trusted for this predicate type, signed it, and the statement names this version's manifest; `signedBy` is the trust entry |
| `untrusted` | signed, but by no trusted key, or by a key not trusted for this predicate type |
| `unsigned` | no signature: a raw SBOM |
| `invalid` | it does not parse, or a trusted key signed a statement about another artifact |
| `unsupported` | keyless (a Fulcio certificate), notation, or larger than `attachments.maxBlobSize` |

The check of the subject matters: an attestation signed for another image, copied onto this
version, is `invalid` here even when your key signed it.

## 4. When the trust changes

The label is decided when an attachment is indexed: at its push, and by reindex. After you add a
key, remove one or change its predicate types, run a reindex of the repos (or wait for the
reconcile loop, `reconcile.interval`): it reads every attachment labelled under the old trust
again.

```sh
lajstrom reindex --repo pypi/internal --json | jq '.repos[0].attachments'
```

Reindex also adds attachments pushed straight to the registry, past Lajstrom, and drops those
deleted there. It asks the registry for the referrers of each version, one request per version.

## 5. Deleting

`DELETE /v2/<ociRepository>/manifests/<referrer digest>` deletes one attachment, with `delete` on
the repo; deleting a version deletes its attachments first. Nothing prunes old scan reports yet.

## Not there yet

- **Keyless signatures** (cosign with Fulcio and Rekor): listed as `unsupported`. Verifying them
  needs a sigstore library that is not approved yet (#89, option A).
- **An `attach` permission** that lets a scanner push attachments without `write` on versions
  (option C).
- **Finding the versions without a fresh scan** (`?without=` on the versions list, option D),
  and reading the tool and database from the signed predicate when the producer used cosign
  (option B).
- **Keeping the SBOM a kept report names**, and pruning old reports (option E, with #72).
- **A reference scan job** under `deploy/jobs/scan/` (option F).
- **Refusing to serve a version without a verified attachment**: #101.
