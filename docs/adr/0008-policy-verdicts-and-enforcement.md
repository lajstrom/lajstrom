# 0008. Policy verdicts: deny lists, quarantine, and where enforcement lives

- Status: accepted; amended by #101 (below, Amendment)
- Date: 2026-11-07
- Deciders: gecube

## Context

Enterprise artifact servers ban things: Artifactory blocks downloads by Xray policy, Nexus
quarantines components with Firewall, Harbor refuses pulls above a vulnerability threshold,
and every proxy product has a deny list of package names and versions. The need is real:
proxy repositories (#67) are the door through which the public ecosystems reach a company,
and the day a malicious release lands on PyPI or npm the operator wants one switch that keeps
it out. Hosted repositories need it less often, for a version that must not be installed any
more or a licence that must not be used.

Lajstrom will have proxy repositories after the MVP, so the PoC does not build any of this. It
must not build anything that makes it hard either. This record names the concepts, decides
where enforcement lives, and lists the seams the PoC keeps open.

The hard question is the OCI side. Every artifact lives in an OCI registry (ADR 0001), and
container images reach Lajstrom through the distribution API (`/v2`, #24, #71). A registry
that refuses `docker pull` of a digest is a registry that breaks a running cluster: kubelets
pin digests, roll-outs and roll-backs pull old images, nodes re-pull after a reboot, and a
denied image is exactly the one someone needs to pull in order to investigate it. The content
is addressed by hash and the decision whether to *run* it belongs to the consumer, who already
has the tools: admission controllers (Kyverno, Gatekeeper, the Sigstore policy controller),
runtime verification in containerd, image scanners on the node. A registry that bans is a
single point of failure for a decision it is not the right place to take.

## Terminology

| Term | Meaning |
| --- | --- |
| registry | the backend, a server of the OCI Distribution API: Zot, distribution, ECR, Harbor, GHCR. Lajstrom never is one of those; it fronts one. |
| OCI repository | `<prefix><format>/<repo>/<package path>` in the registry (manifest-profile.md §4.1) |
| repo | Lajstrom's control-plane entity `<format>/<name>` |
| format `oci` | a repo whose artifacts pass through `/v2` as they are: container images, Helm charts, anything OCI; Lajstrom indexes nothing in it. docker addresses it as `host/oci/<repo>/<name>`, Helm and ORAS as `oci://host/oci/<repo>/<name>`. "Docker registry" is a client's word; the protocol and the content are OCI. |
| artifact | one manifest in the registry, identified by its digest; a container image is one kind of artifact |
| coordinates | `(format, repo, name, version, variant)` as the index stores them; the identity of an artifact that may not exist yet |
| verdict | a decision about one artifact, by digest: `allow`, `deny` or `quarantine`, with a reason, a source and a validity |
| rule | a pattern over coordinates that yields a verdict for artifacts that match it, including ones not fetched yet |
| deny list, block list, ban | the `deny` rules and verdicts in force; the words are synonyms, the document says *deny* |
| allow list | rules with a default of `deny`: only what matches may pass |
| quarantine | `deny` until a condition holds: an age ("no upstream version younger than 72 hours"), a scan result, a manual release |
| enforcement point | the place where a verdict changes an answer: ingest, the proxy fetch, index rendering, a download, a pull |

## Considered options

- **Enforce everywhere, on the server.** Every read of a denied artifact is refused, `/v2`
  included (the Harbor model). Simple to explain, and wrong for the reasons above: the registry
  becomes the point that breaks clusters, and the decision is taken far from the information
  (which cluster, which namespace, which exception).
- **Policy as data only.** Lajstrom records verdicts and publishes them; every consumer
  enforces. Right for OCI, useless for apt, pip, mvn and npm: those clients have no policy
  hook, and a deny list that `apt-get` cannot see is no deny list.
- **Enforce at an edge proxy in front of Lajstrom.** An ingress, a WAF or an Envoy filter
  blocks URLs: `/pypi/*/files/requests/2.32.0/*`, `/v2/<name>/manifests/sha256:…`. It is a
  real kill-switch (minutes, no release, the organisation's own tooling) and the only place
  that can filter Lajstrom's *upstream* fetches in proxy mode (an egress proxy). It is not a
  deny list: the edge cannot take a version out of `Packages` or the simple index, so `apt-get`
  and `pip` fail on the file instead of choosing another version; it does not know which
  digest a tag names until it parses the response; a blob is shared by images that are not
  denied; the client gets no reason, Lajstrom no audit row, and two lists drift apart; and
  for OCI it is, seen from the node, the same registry that refuses a pull.
- **Enforce in the protocol frontends and the proxy, publish for OCI.** Lajstrom is the only
  policy point the protocol clients have, so it enforces there. For OCI it publishes verdicts
  in the forms the consumers' tools read, and refuses pulls only where an operator turns that
  on for a repo. An edge proxy is one more consumer of the published data.

## Decision

The third option.

**Two kinds of policy data.** A *verdict* is a signed in-toto attestation stored in the
registry as a referrer of the artifact it judges (the attachments model of #89), predicate type
`dev.lajstrom.verdict/v1`: `{verdict, reason, source, scope, validUntil}`. A manual ban is a
verdict the server signs with its own key (#41) when an admin asks; a scanner's verdict is one
the scanner job signs; the policy says which keys are trusted. The index keeps a derived table
of verdicts (rebuilt by reindex), the registry stays the source of truth (ADR 0001, 0006), and a
consumer can verify a verdict with nothing but the registry and the public key. A *rule* lives in
the control plane (`policy_rules`: a repo mask as in bindings, a name pattern, a version
constraint, a variant, the effect, a reason, who made it, when it expires), because a proxy must
refuse a version before anything of it exists to attach a verdict to.

**Enforcement, by surface.**

- *Protocol frontends* (apt, pypi, maven, raw, later rpm, npm, terraform) and *proxy fetches*:
  Lajstrom enforces. A denied version is left out of the rendered indexes (`Packages`, the
  simple index, `maven-metadata.xml`, the raw listing), so resolvers choose another one; a
  direct download of a denied artifact is `403` with the reason in the body, not `404`: that an
  artifact is denied is not a secret, and a client must not retry forever. A proxy evaluates the
  rules before it fetches from upstream and refuses the fetch; a quarantine rule holds a new
  upstream version back for the configured time. An upload of a denied coordinate is refused at
  ingest (`403`); the registry is not touched.
- *OCI* (`/v2`, every format): Lajstrom does not refuse pulls. It publishes the verdicts: the
  referrers API (#28, with the tag-schema fallback for backends without it), `GET
  /api/v1/policy/verdicts?digest=…`, a signed deny-list artifact at `<prefix>_meta/policy/denylist`
  that a node or an air-gapped mirror can pull and verify offline, `GET /api/v1/policy/denylist`
  as flat JSON (URL prefixes and digests) for an edge proxy or an OPA policy that wants a
  kill-switch outside Lajstrom's release cycle, and webhooks (#77) when a verdict changes. The Sigstore policy controller, Kyverno and the like verify the verdict
  attestation the way they verify a signature; nothing in the cluster depends on Lajstrom being
  up. A repo may opt in to `policy.enforcePull: true`: then a `GET` of a denied manifest answers
  `403` with the OCI error code `DENIED`, blobs and tag lists are never refused (a layer is
  shared by images that are not denied), and the default is off. Writes to OCI repositories
  follow the frontend rule: a push of a denied coordinate is refused at the manifest hook.

**Audit and RBAC.** Every refusal is an audit event with the rule or verdict that caused it.
Rules are a system-level resource `policy` for the admin and, scoped to a repo mask, `manage`
on the repo (rbac.md §2); verdict attestations need `attach` on the repo (#89).

## What the PoC keeps open

Nothing above is built in the PoC. These are the seams it must not close:

- An artifact's identity is its manifest digest, and its coordinates are the five columns the
  index has. Nothing else identifies an artifact, so a verdict can always find its subject.
- Each plugin has one read path for indexes (`RenderIndex` through `CachedDocument`) and one
  for downloads (the `ServeBlob` helper of #27). A `policy.Decider` is asked in exactly those
  two places, in ingest before the claim, and in the proxy fetcher; it is added with its first
  consumer, not before, so that no dead interface waits in the contract.
- `index_cache` is invalidated when a rule or a verdict changes, for every repo the rule's mask
  matches; the apt index issue (#40) keeps one invalidation path, not a cache key per policy.
- Verdicts reuse the attachments model of #89 as it is: a predicate type, a trusted-key list,
  `attach` as the action. The deny-list artifact is one more attachment kind.
- The proxy design (#67) takes rules and quarantine as inputs from day one: rules before the
  upstream fetch, verdict attestations on what was cached, the same enforcement as hosted.
- `repos.policy` (the JSON column) holds per-repo switches such as `enforcePull` and
  `quarantineNewVersions`; the rules themselves are rows, not JSON in the repo.

## Consequences

- apt, pip, mvn and npm users get a working deny list and quarantine on the only point that
  can enforce it; OCI consumers get verifiable verdicts and keep the decision where the
  information is.
- A registry outage or a Lajstrom outage never changes what a cluster may run: the verdicts are
  in the registry, signed, and cached by the consumer's own tools.
- An operator who wants the Harbor behaviour turns it on per repo and knows what it costs.
- Two kinds of data mean two models to explain: rules (patterns, control plane) and verdicts
  (digests, signed, derived). The UI and the API show both under one word, *policy*.
- The post-MVP issue of this record carries the contract changes: `policy_rules`, the verdict
  predicate, the API, the `policy` resource, the `enforcePull` switch.

## Amendment: the choices the issue left open (#101, 2026-11-15)

Issue #101 builds this record and names its contract changes; [policy.md](../spec/policy.md) is
the spec. These are the choices the issue left open, each with the default the spec takes.

- **A. Version constraints: one grammar for every format.** `==`, `!=`, `<`, `<=`, `>`, `>=`,
  joined with `,`, a bare version meaning `==`, and `*` as a glob in `==` and `!=`. The ranges
  compare in the format's order where the plugin has one (`format.VersionComparer`: npm's semver
  and Maven's `ComparableVersion` today), and a rule with a range on another format is refused.
  Rejected: each ecosystem's own syntax (PEP 440 specifiers, npm ranges, Debian relations, Maven
  ranges), four grammars, and no rule over a mask that spans formats. Consequence: `==` and
  globs work everywhere from the first day; a pypi, apt or rpm range is refused until its plugin
  orders versions.
- **B. A rule's mask matches as a binding's**, by whole segments, and a decision is taken in the
  repo that holds the artifact. A mask that names a group exactly also holds for each of its
  members. Rejected: a group's rules applied only when it merges, which would make a member's
  documents differ by the path they were read through, while `index_cache` keeps one per
  member. Consequence: a rule on `npm/all` holds for `npm/npmjs` read directly too.
- **C. The quarantine's clock is the upstream's publish time** where its document gives one (npm
  `time`, pypi's PEP 700 `upload-time`), which a held version keeps as its `created_at`
  (manifest-profile.md §3.1); else the first time the repo saw the version listed (`policy_seen`,
  a cache), and a held version the time of its fetch. Rejected: first seen always, or the later of the two, which resist an
  upstream that backdates but hold back every version for the whole quarantine the day it is
  turned on. Consequence: the upstream's clock is trusted; a lost `policy_seen` row lengthens a
  quarantine and never shortens it.
- **D. The deny list is an OCI artifact at `lajstrom/policy/denylist`**, not `_meta/policy/denylist`:
  a component of an OCI name cannot begin with `_`, and `lajstrom` is the first segment that no
  format has and snapshots use already (manifest-profile.md §9). One JSON layer and one layer with
  its signature as `cosign sign-blob` writes it, by the server's policy key, so that `oras pull`
  and `cosign verify-blob --key` check it with no other tool. Its tags sort by the policy
  generation and never move; `/v2` answers `latest` as the greatest. Rejected: a moving `latest`
  in the registry (a second exception to immutable tags), and an in-toto envelope, which `cosign
  verify-blob` does not read without a bundle. Consequence: a consumer that pulls from the
  registry directly lists the tags and takes the greatest.
- **E. `DENIED` under `enforcePull`** is `403` for `GET` and `HEAD` of a manifest once its
  reference has resolved to a digest, with the reason; blobs, tag lists and referrers never.
  Rejected: `404`, which hides why and makes a client try elsewhere, and a decision before the
  resolve, which does not know the digest a tag names. Consequence: a denied image index is
  refused, a denied platform manifest is refused when the client asks for it by digest.
- **F. The server signs with one ECDSA P-256 key**, the policy key, a `keys` row of kind `policy`,
  sealed with the master key or held in the KMS of ADR 0020. Rejected: a repo's GPG key (#41),
  which is OpenPGP and not what `cosign` verifies, and the token key (ADR 0010), one key for two
  purposes. Consequence: one public key for every verdict the server makes and for the deny list,
  answered by `GET /api/v1/policy/key`; no rotation in #101.
- **G. Precedence:** a `deny` verdict, then an `allow` rule, then a `deny` rule, then quarantine,
  which an `allow` verdict releases. An `allow` rule is an exception to the rules and to
  quarantine; a signed `deny` verdict wins over every rule, and lifts only by its expiry or the
  deletion of its referrer.
- **H. A verdict's `scope` is a repo mask**, the repo that holds the referrer when absent: a
  trusted signer may ban a digest wherever it is held.
- **I. Who may.** #89 has not decided its `attach` action (option C). Verdicts pushed through
  `/v2` need what every referrer needs on `main`, `write`; they follow option C if the maintainer
  takes it. The deny of the management API, which the server signs, needs `manage`.
- **J. A reason for pip and apt.** A version left out of an index is a version the client does
  not see, so `pip install requests==2.32.0` fails with "no matching distribution" and `apt-get
  install p=1.0` with "version not found", and neither shows the rule's reason: neither protocol
  has a field for it. The reason is in the `403` of a direct download, in the audit row of the
  refusal, and in the API.
- **K. The predicate type is a URI**, `https://lajstrom.dev/verdict/v1`, where this record and the
  issue say `dev.lajstrom.verdict/v1`: `cosign attest --type` and `cosign verify-attestation
  --type` refuse a predicate type that is neither one of their short names nor a URI ("invalid
  predicate type", cosign v3.1.4), and in-toto names predicate types by URI. Consequence: the
  consumers' policies name the URI; the CloudEvents types and the media types of Lajstrom keep
  their reverse domain names.
