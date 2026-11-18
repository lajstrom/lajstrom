# Policy: deny lists, quarantine, verdicts and required attachments

The day a malicious release lands on PyPI or npm, or a version of your own must not be installed
any more, you want one switch that keeps it out. Lajstrom has two kinds of policy data for that
([ADR 0008](adr/0008-policy-verdicts-and-enforcement.md), spec [policy.md](spec/policy.md)):

- a **rule** is a pattern over coordinates (repo, package, version, variant) that denies or
  allows, including versions that do not exist yet: what a proxy must refuse before it fetches;
- a **verdict** is a signed attestation about one artifact, by the digest of its manifest, stored
  in the registry as a referrer of it: `allow`, `deny` or `quarantine`, with a reason.
- a **requirement** of a repo keeps a version out until it has a verified attachment, an SBOM,
  provenance or a signature, signed by a key or a CI identity you name (§7, #1287).

apt, pip, Maven and npm clients have no policy hook, so Lajstrom enforces for them: a refused
version is left out of `Packages`, the simple index, `maven-metadata.xml` and the packument, its
download is `403` with the reason, its upload is refused, and a proxy does not fetch it. For
container images and every other artifact read through `/v2`, Lajstrom publishes the verdicts
for the cluster's own tools (§5) and refuses a pull only on a repo that asks for it (§4).

This page was run on 2026-11-16 against Lajstrom of the #101 stack, on distribution 3.1.2 as the
registry, with pip 21.2, oras 1.3.6 and cosign v3.1.4: the rule and pip (§1), the deny of an image
and `cosign verify-attestation` through Lajstrom (§2), `enforcePull` (§4), the Sigstore policy
controller 0.13.1 in a kind cluster (§5) and the deny list with `oras pull` and `cosign verify-blob`
(§6). The Kyverno policy of §5 was not run. The required attachments of §7 (#1287) were not run with
clients: what §7 says is what the tests of the #1287 stack check.

## 1. A rule

Deny one version of `requests` in the proxy of PyPI, for whoever manages that repo:

```sh
curl -s -u admin -X POST "$LAJSTROM/api/v1/policy/rules" -H 'Content-Type: application/json' \
  -d '{"repoMask": "pypi/proxy-pypi", "name": "requests", "version": "==2.32.0", "reason": "CVE-2026-1234: do not install"}'
# or: lajstrom admin policy rule create --mask pypi/proxy-pypi --name requests --version ==2.32.0 --reason '…'
```

The simple index no longer lists the file, so pip picks another version, and a pin of the denied
one finds nothing:

```console
$ pip install --index-url "$LAJSTROM/pypi/proxy-pypi/simple/" requests==2.32.0
ERROR: Could not find a version that satisfies the requirement requests==2.32.0 (from versions: 2.32.1)
ERROR: No matching distribution found for requests==2.32.0
$ curl -s -u admin "$LAJSTROM/pypi/proxy-pypi/files/requests/2.32.0/requests-2.32.0-py3-none-any.whl"
requests 2.32.0 is denied by policy: CVE-2026-1234: do not install
```

pip and apt show no reason for a version they do not see: neither protocol has a field for one.
The reason is in the `403` of a direct download, in the audit log (`GET
/api/v1/audit?action=policy.refuse`: the rule, the surface, the client), and in the API.

The fields of a rule:

| Field | Value |
| --- | --- |
| `repoMask` | a mask as a binding's: `pypi/proxy-pypi`, `npm/*`, `*/payments`, `*/*`. A mask that names a group also holds for its members |
| `name` | the package name, `*` any run of characters, case ignored: `requests`, `@evil/*`, `com.acme:*` |
| `version` | `*`, or clauses joined with commas: `==2.32.0`, `==2.32.*`, `!=1.0`, `>=4.0,<4.17.21`. A range compares in the format's order: npm's semver, Maven's, PEP 440 for pypi, dpkg's for apt, rpm's for rpm; on raw and `oci` it is refused |
| `variant` | `*` (the default), or a pattern of the variant: an apt architecture, a wheel's tags |
| `effect` | `deny` (the default), or `allow`: an exception to the deny rules and to quarantine |
| `expiresAt` | when it stops holding; none by default |

A mask of one repo needs `manage` on it; any other mask the admin role. `GET
/api/v1/policy/rules?repo=pypi/proxy-pypi` lists the rules that hold in a repo, for its readers.

## 2. A verdict

Deny the versions you hold by their digest, signed by the server's **policy key**, which Lajstrom
makes at its first start with a master key (`keys.masterKeyEnv`), or holds in a KMS
(`policy.signingKey`, [configuration.md](configuration.md#policy)):

```sh
curl -s -u admin -X POST "$LAJSTROM/api/v1/repos/apt/internal/packages/openssl/versions/3.0.2-0ubuntu1/deny" \
  -H 'Content-Type: application/json' -d '{"reason": "CVE-2026-1234"}'
curl -s -u admin -X POST "$LAJSTROM/api/v1/repos/oci/images/packages/app/versions/v1/deny" \
  -H 'Content-Type: application/json' -d '{"reason": "malware found in the image"}'
```

Each variant's manifest gets the verdict twice, in the two forms cosign writes: a sigstore bundle as
a referrer (what `cosign attest` writes, and `cosign verify-attestation` reads, by default) and a
layer of cosign's legacy attestation under the tag `sha256-<hex>.att` (`--new-bundle-format=false`,
what the Sigstore policy controller reads with a key). Anyone verifies it with the public key:

```sh
curl -s "$LAJSTROM/api/v1/policy/key" > lajstrom.pub
cosign verify-attestation --key lajstrom.pub --type https://lajstrom.dev/verdict/v1 \
  --insecure-ignore-tlog=true "$HOST/oci/images/app@sha256:37c2…"
```

`--insecure-ignore-tlog` because the server's verdicts are not in a transparency log; the
signature is checked against the key. `DELETE …/deny` removes the server's verdicts of a version:
it deletes their bundles and writes the legacy attestation again without their layers, keeping
another producer's.
`GET /api/v1/policy/verdicts?repo=oci/images` lists the verdicts, with whether each is in force.

A scanner of yours can make verdicts too: sign them with its key, and trust that key for the
predicate type in `attachments.trust` ([guides/sbom-and-scans.md](guides/sbom-and-scans.md)):

```sh
echo '{"verdict": "deny", "reason": "critical CVE", "source": "trivy"}' > verdict.json
cosign attest --key scanner.key --type https://lajstrom.dev/verdict/v1 --predicate verdict.json \
  --tlog-upload=false "$HOST/npm/npmjs/left-pad@sha256:…"
```

Add `--new-bundle-format=false` for the Sigstore policy controller (§5), which reads a key's
attestations in the legacy form only.

A `deny` verdict wins over every rule; a `quarantine` verdict holds until an `allow` verdict
releases it or its `validUntil` passes; a verdict that no trusted key signed judges nothing.

## 3. Quarantine

Hold back a proxy's versions that are new upstream:

```sh
curl -s -u admin -X PATCH "$LAJSTROM/api/v1/repos/npm/npmjs" -H 'Content-Type: application/json' \
  -d '{"policy": {"quarantineNewVersions": "72h"}}'
```

A version published upstream less than 72 hours ago is neither listed nor fetched; its tarball is
`403 … is quarantined until <time>`. The clock is the upstream's publish time where its document
has one (npm's `time`), else the first time the repo saw the version listed. When a version is
fetched after its quarantine, Lajstrom attaches an `allow` verdict, source `lajstrom:quarantine`.
An `allow` rule (`@acme/*`) or an `allow` verdict releases a version early.

## 4. Refusing pulls on `/v2`

A registry that refuses `docker pull` of a digest breaks the nodes that need it: a roll-back, a
reboot, an investigation. So `/v2` refuses nothing by default; a repo may ask for it:

```sh
curl -s -u admin -X PATCH "$LAJSTROM/api/v1/repos/oci/images" -H 'Content-Type: application/json' \
  -d '{"policy": {"enforcePull": true}}'
```

Then a `GET` or `HEAD` of a denied manifest, by tag or digest, is `403 DENIED` with the reason;
blobs and tag lists stay readable, since a layer is shared by images that are not denied.
A repo that requires attachments (§7) refuses a version without them the same way, but to a
token that may push or attach to the repo: the clients that sign and attach read the version
first.

## 5. Admission controllers

The verdicts are in the registry, signed: a cluster checks them with nothing but the registry and
the public key, whether Lajstrom is up or not.

The [Sigstore policy controller](https://docs.sigstore.dev/policy-controller/overview/), in the
namespaces labelled `policy.sigstore.dev/include=true`:

```yaml
apiVersion: policy.sigstore.dev/v1beta1
kind: ClusterImagePolicy
metadata:
  name: lajstrom-verdicts
spec:
  images:
    - glob: "lajstrom.example.com/oci/**"
  authorities:
    - name: lajstrom
      key:
        data: |
          -----BEGIN PUBLIC KEY-----
          …the answer of GET /api/v1/policy/key…
          -----END PUBLIC KEY-----
      attestations:
        - name: verdict
          predicateType: https://lajstrom.dev/verdict/v1
          policy:
            type: cue
            data: |
              predicateType: "https://lajstrom.dev/verdict/v1"
              predicate: verdict: !="deny"
```

A key authority checks no transparency log, and reads the legacy attestation (`sha256-<hex>.att`),
which Lajstrom writes for every verdict it signs; `signatureFormat: bundle` is for keyless
authorities only. Run with policy controller 0.13.1: a pod of the image denied in §2 is refused
(`predicate.verdict: invalid value "deny"`). Mind what an authority asks: an attestation of its key
that passes its policy. So the policy above refuses every image without a verdict of the server
too. To admit the images the server never judged, give them a verdict of another key, a scanner
that attests `allow` or `deny` for every image, as a second authority of the same form with its key:
an image passes when one authority passes. That also admits an image the scanner allowed and the
server denied: an authority's policy sees only its own key's attestations, so a deny of one key does
not overrule an allow of another. Where the server's ban must win in the cluster, keep it out at the
source as well: the frontends, `enforcePull` (§4), or the deny list at the edge (§6).

[Kyverno](https://kyverno.io), refusing an image whose verdict says `deny` (not run; Kyverno must
read cosign's attestations of a key in one of the two forms Lajstrom writes):

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: lajstrom-verdicts
spec:
  validationFailureAction: Enforce
  webhookTimeoutSeconds: 30
  rules:
    - name: no-denied-images
      match:
        any:
          - resources: {kinds: [Pod]}
      verifyImages:
        - imageReferences: ["lajstrom.example.com/oci/*"]
          mutateDigest: true
          attestations:
            - type: https://lajstrom.dev/verdict/v1
              attestors:
                - entries:
                    - keys:
                        publicKeys: |-
                          -----BEGIN PUBLIC KEY-----
                          …the answer of GET /api/v1/policy/key…
                          -----END PUBLIC KEY-----
                        rekor: {ignoreTlog: true}
                        ctlog: {ignoreSCT: true}
              conditions:
                - all:
                    - key: "{{ verdict }}"
                      operator: NotEquals
                      value: deny
```

## 6. The deny list outside Lajstrom

The rules and verdicts in force that refuse, with the digests and URL paths they refuse, are
written into the registry as a signed artifact, `lajstrom/policy/denylist`, whenever they change
(`policy.denylist.registry`, the first registry by default). A node or an air-gapped mirror pulls
it from the registry and verifies it offline; the greatest tag is the newest:

```sh
tag=$(oras repo tags "$REGISTRY/lajstrom/policy/denylist" | sort | tail -1)
oras pull "$REGISTRY/lajstrom/policy/denylist:$tag"     # denylist.json, denylist.json.sig
cosign verify-blob --key lajstrom.pub --signature denylist.json.sig --insecure-ignore-tlog=true denylist.json
```

`GET /api/v1/policy/denylist` answers the same document as JSON, without a signature, with the
entries of the repos the caller may read: for an edge proxy or an OPA policy that blocks `prefixes`
and `digests` while Lajstrom is down. An edge can block a path and nothing else: it cannot take
a version out of an index, and a range rule has no path.

## 7. Required attachments

A production repo can serve only what your release pipeline built and attested: a repo's
`policy.requireAttachments` lists **requirements**, each a predicate type and the names that may
sign it ([ADR 0033](adr/0033-required-attachments-of-a-version.md), spec
[policy.md](spec/policy.md) §11). A version is served only while its manifest has, for every
requirement, an attachment labelled `verified` ([guides/sbom-and-scans.md](guides/sbom-and-scans.md))
of that predicate type, or of a longer version of it after a `/`, signed by one of the names.
Otherwise it is left out of every index, its download is `403`, and under `enforcePull` (§4) its
manifest is `403 DENIED` to a consumer.

**1. Trust the signers.** The names are those of `attachments.trust` (a key) and
`attachments.keyless.identities` (a CI job's keyless identity), on every replica
([configuration.md](configuration.md#attachments)). Each must be trusted for a predicate type the
requirement matches: no `predicateTypes` (every type), or one that equals it or a longer version.

**2. See what it would hide.** A requirement holds for every version of the repo, those pushed
before it too: turning it on hides at once every version without the attachment. List them per
package first, with the names given once each:

```sh
curl -s -u "$USER:$TOKEN" \
  "$LAJSTROM/api/v1/repos/pypi/prod/packages/app/versions?unmet=https://spdx.dev/Document&signedBy=release" |
  jq -r '.versions[] | .version as $v | .variants[] | "\($v) \(.variant)"'
```

Attach to those you keep (the guide, §8), or serve them from another repo.

**3. Set it**, with `manage` on the repo, as every key of its policy:

```sh
curl -s -u admin -X PATCH "$LAJSTROM/api/v1/repos/pypi/prod" -H 'Content-Type: application/json' -d '{"policy": {
  "requireAttachments": [
    {"predicateType": "https://spdx.dev/Document", "signedBy": ["release"]},
    {"predicateType": "https://slsa.dev/provenance", "signedBy": ["release", "release-2027"]}
  ]}}'
```

`lajstrom admin repo create --policy` and the configuration's `repos` take the same key. The
server refuses, `400` naming the field, a name its trust does not have (a typo would hide every
version), `lajstrom` (the policy key, which signs verdicts only), a name trusted for no predicate
type the requirement matches, a predicate type that is not an absolute URI, more than 16
requirements or 16 names, and a proxy, `oci` or `at-rest` repo, whose versions it could never
serve. A group has no policy: its members' requirements hold through it. `null` removes the key.
Each change is a `repo.update` row with the policy it left: whoever has `manage` on the repo can
turn the requirement off, so grant `manage` on production repos only to those who may, and review
those rows ([compliance.md](compliance.md), 6.3.2).

**What it does.**

- **The order.** Only a `deny` verdict comes before a requirement. No `allow` rule and no `allow`
  verdict lets a version without its attachments through: an allow list (`allow @acme/*` beside
  `deny *`) exempts nothing. A version that is also quarantined shows its quarantine once its
  attachment is there.
- **The reason** names the first requirement not met:
  `app 1.4.0 is denied by policy: requires an attachment https://spdx.dev/Document signed by release`.
  Each refusal is a `policy.refuse` row whose `details.requirement` is the requirement as the
  policy has it; `/v2`'s `403 DENIED` has it in `detail.requirement`. These versions are not in
  the deny list (§6): Lajstrom alone refuses them.
- **Each variant** needs its own attachments: a wheel is not served on another wheel's SBOM.
- **Uploads are never refused for it.** A version is pushed, then its attachments; it is served
  from the moment the rows of its last required attachment are written, which renders the repo's
  indexes again. So CI attaches before consumers pull, and a job that installs what it has just
  published attaches first. An attachment whose rows were not written (logged, and counted in
  `lajstrom_attachments_index_failures_total`) counts after the next reindex of the repo.
- **`/v2` under `enforcePull`** refuses a version without its attachments to a token asked with
  `pull` only. A token that grants the repository `push` or `attach` still reads it, `GET` and
  `HEAD`, because `cosign sign`, `cosign attest` and `oras attach` read the manifest they attach
  to. Without `enforcePull`, `/v2` serves it, as it serves a denied version: OCI consumers verify
  the attestations themselves.
- **The trust.** Only an attachment `verified` counts, never `untrusted` or `unsigned`. A name
  removed from the configuration counts no more from the next start of each replica, without a
  reindex. A key replaced under the same name counts for what the old key verified until the next
  reindex labels it again, so give a new key a new name and list both in `signedBy` while the old
  versions are served. `serve` warns at start of a repo whose requirements name a signer its trust
  does not have.
- **Rolling update.** A replica of a release before this one serves the repo's versions as if it
  had no requirement, and refuses any `PATCH` of its policy (an unknown key). Set a requirement
  once every replica runs a release that has it.

## 8. Not there yet

- Lajstrom's `/v2` does not serve the deny-list artifact; pull it from the registry.
- The server's policy key is not rotated.
- The pypi and maven proxies get the policy of the proxy core when they merge (#67); until then
  the npm proxy has it.
- What a requirement would hide is listed per package (`?unmet=`), not for a whole repo at once.
