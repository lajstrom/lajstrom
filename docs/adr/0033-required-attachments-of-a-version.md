# 0033. Required attachments: a repo serves a version only with the verified attachments it names

- Status: proposed; A decided on #1287 (2026-11-16)
- Date: 2026-11-18
- Deciders: gecube

## Context

PCI DSS v4.0.1 6.3.2: "An inventory of bespoke and custom software, and third-party software
components incorporated into bespoke and custom software is maintained to facilitate
vulnerability and patch management." 6.5.4: "only reviewed and approved changes are deployed".
SOC 2 CC6.8: "The entity implements controls to prevent or detect and act upon the introduction
of unauthorized or malicious software to meet the entity's objectives" (#1287,
[compliance.md](../compliance.md)). What Lajstrom has:

- Attachments (#89, [manifest-profile.md](../spec/manifest-profile.md) §6): the signatures, SBOMs,
  scan reports and provenance of a version, as referrers of its manifest, each labelled
  `verified`, `untrusted`, `unsigned`, `invalid` or `unsupported` against `attachments.trust`
  and `attachments.keyless`, with the name of the key or identity that verified it (`signedBy`);
  `?without=sbom` lists the versions without one. A label never refuses a push.
- Policy (#101, ADR 0008, [policy.md](../spec/policy.md)): rules and verdicts, decided by one
  `policy.Decider` at the rendered indexes, the downloads, ingest, the proxy, and `/v2` under
  `enforcePull`. It refuses what a rule or a verdict denies, not a version that lacks an
  attachment.

Nothing keeps a version from being served before, or without, its SBOM, its provenance or its
signature.

## Considered options

- **A. A repo policy `requireAttachments: [{predicateType, signedBy}]`**, decided by #101's
  Decider at its enforcement points: the rendered indexes, the downloads, `/v2` under
  `enforcePull`.
- **B. Leave it to the consumers**: an admission controller (the Sigstore policy controller,
  Kyverno), `cosign verify-attestation` in deploy jobs. For OCI images only: apt, pip, npm and
  Maven clients have no such hook.
- **C. At promotion only**: a release repo takes a version only with its attachments. It needs a
  promotion between repos, which Lajstrom does not have.

## Decision

A (the maintainer, 2026-11-16, on #1287). policy.md §11 says what holds; these are the defaults
proposed for what the decision left open:

1. **Second in the decision**: after a `deny` verdict, before the `allow` rules, the `deny` rules
   and quarantine. No `allow` rule and no `allow` verdict lets a version without its attachments
   through; an allow list (`allow @acme/*` beside `deny *`) would otherwise exempt all it names.
2. **Every version, whenever it was pushed**: a requirement holds from the commit that sets it.
   No time exempts a version: the time a version carries is the pusher's word.
3. **What meets a requirement**: an attachment of the variant's own manifest, labelled
   `verified`, whose predicate type equals the requirement's or continues it after a `/` (as the
   kinds of manifest-profile.md §6.1), and whose `signedBy` is one of the requirement's names (a
   list; any one). A signature counts as `https://sigstore.dev/cosign/sign/v1`.
4. **The names are the trust's**: the keys of `attachments.trust` and the identities of
   `attachments.keyless.identities`, each trusted for a type the requirement matches, checked
   when the policy is written (`400` otherwise); `lajstrom`, the policy key's, is not one. A name
   that the replica's trust no longer has meets nothing at once, without waiting for a reindex.
5. **Reads only, and not a producer's**: an upload, a push and a referrer leave the requirements
   out, since a version exists before its attachments; so does a read of `/v2` whose token grants
   the repository `push` or `attach`, since `cosign sign`, `cosign attest` and `oras attach` read
   the manifest they attach to. A token asked with `pull` only, a consumer's, is refused under
   `enforcePull`.
6. **Where**: a hosted repo of a format other than `oci`, not encrypted at rest. A proxy would
   never fetch a version that has no attachment yet; an `oci` repo indexes no attachments; an
   encrypted repo's attachments are encrypted and never verify. A group has no policy: its
   members' requirements hold through it.
7. **Not in the deny list**, which stays the rules' and the verdicts': a version waiting for its
   attachments is refused by Lajstrom alone.
8. **Who and how**: `manage` on the repo, as every key of its policy, through `PATCH` of the repo,
   `lajstrom admin repo create --policy` and the configuration's `repos`, audited as
   `repo.update`; a refusal is `policy.refuse` with `requirement`; `GET …/versions?unmet=` lists
   what a requirement would leave out, before it is set and after. Whoever may set a requirement
   may lift it: `manage` on a production repo goes only to those who may turn the requirement
   off, and its `repo.update` rows show each time it was (decided on #1287).

## Consequences

- A version is not served between its push and the commit of its last required attachment's
  rows: CI attaches before consumers pull, and a job that installs what it has just published
  attaches first.
- Turning a requirement on hides at once every version without its attachments. `?unmet=` shows
  which beforehand; attaching to them, or another repo, is the way to keep old versions.
- The label is the index's, decided when the attachment was indexed. A name dropped from the trust
  stops counting at the next start; a key replaced under the same name keeps counting for what
  it verified until the next reindex, so a new key takes a new name.
- An attachment's rows are written best effort: a version whose rows failed waits for the next
  reindex of its repo.
- On a repo with requirements, each attachment written or deleted invalidates the repo's documents,
  and each render reads the repo's verified attachments once.
- Without `enforcePull`, `/v2` still serves the manifest of a version that misses an attachment,
  as it serves a denied one (ADR 0008): set `enforcePull` too where `oras pull` must be refused.
  Under it, a token that grants `push` or `attach` still reads: the requirement keeps unattested
  versions from consumers, not from the producers who sign them. OCI consumers keep verifying the
  attestations themselves.
- Proxy repos, `oci` repos and encrypted repos cannot require attachments.
