# Spec: policy, its rules, verdicts and enforcement

- Status: normative (2026-11-15), issue #101; the required attachments of a repo (§11), after
  the MVP (#1287). Owned by `internal/policy`. Changes need an issue labelled `contract`.
- Related: [ADR 0008](../adr/0008-policy-verdicts-and-enforcement.md) and its amendment (the
  decisions this spec writes down), [ADR 0033](../adr/0033-required-attachments-of-a-version.md)
  (§11), [manifest-profile.md](manifest-profile.md) §3.1, §6.3 (the labels of attachments), §6.4
  (the verdict) and §10 (the deny list), [index-schema.md](index-schema.md#policy) (the tables),
  [http-routing.md](http-routing.md) §2, §3, §4, §6 (the answers and routes),
  [rbac.md](rbac.md) §2, §3, §5 (who may, the audit rows),
  [proxy-and-group.md](proxy-and-group.md) §5, §10, [webhooks.md](webhooks.md) §1,
  [guarantees.md](guarantees.md) INV-38.

Policy says what a repo serves. A **rule** refuses or admits coordinates, including those of
artifacts that do not exist yet; a **verdict** judges one artifact, by the digest of its
manifest, and is a signed attestation in the registry. The protocol frontends, ingest and the
proxy enforce both; `/v2` publishes the verdicts and refuses a pull only on a repo that asks for
it (ADR 0008). A repo may also **require attachments** (§11): it serves a version only once the
version has a verified attachment of each predicate type the repo names, signed by a key or an
identity it names.

## 1. Rules

A rule is a row of `policy_rules`, control plane (index-schema.md). It never changes: a rule is
replaced by deleting it and creating another.

| Field | Value |
| --- | --- |
| `id` | assigned by the index |
| `repoMask` | a repo mask of rbac.md §3: `pypi/proxy-pypi`, `npm/*`, `*/payments`, `*/*` |
| `name` | a pattern of the package name (§1.1) |
| `version` | a version constraint (§1.2); `*` matches every version |
| `variant` | a pattern of the variant, as `name`; `*` (the default) matches every variant, `""` the empty variant only |
| `effect` | `deny` or `allow` |
| `reason` | 1 to 512 bytes of UTF-8 without control characters, shown to clients |
| `createdBy` | the principal that created it (`user:<uid>`, `token:<id>`) |
| `createdAt` | when |
| `expiresAt` | when it stops holding; null: never |

A rule **holds** for a subject (§3) when it has not expired, its mask matches the subject's repo,
and its name, version and variant match the subject's.

### 1.1 Repos and names

- **The mask** matches a repo as a binding's mask does, by whole segments. A mask that names a
  group exactly also matches each member of the group: a decision is taken in the repo that holds
  the artifact (§3), so a rule on a group holds for its members, read through the group or
  directly. A mask with a wildcard matches groups and members as it matches any repo.
- **The name** is matched against the package's name as `lajstrom.name` spells it (pypi the PEP
  503 name, npm `@scope/name`, maven `groupId:artifactId`, apt and rpm the package, raw the path,
  `oci` the path below the repo), ignoring ASCII case. `*` matches any run of characters, `/` and
  `:` included, and every other character matches itself, as the lists of a proxy match
  (proxy-and-group.md §3): `requests`, `@evil/*`, `com.acme:*`.

### 1.2 Version constraints

```
constraint = "*" / clause *( "," clause )
clause     = [ op ] version
op         = "==" / "!=" / "<" / "<=" / ">" / ">="
```

Spaces around a clause and an operator are ignored. A clause without an operator is `==`; a
constraint holds when every clause does. `==` and `!=` compare a version as its bytes, and a `*`
in theirs matches any run of characters (`==2.32.*`). Where the format orders versions
(`format.VersionComparer`: npm by semver 2.0, maven by Maven's `ComparableVersion`, pypi by PEP
440, apt by dpkg's order, rpm by rpmvercmp on `[epoch:]version[-release]`), `==` and `!=` without
a `*` compare in that order (`==1.0` holds for maven `1.0.0` and pypi `1.0.0`), and `<`, `<=`, `>`
and `>=` compare in it (pypi `<2.0` holds for `2.0rc1`, apt `>=1.0-1` not for `1.0~rc1-1`). A
version that the order does not parse (a pypi version that is not PEP 440, a Debian version
that dpkg refuses) matches no range, and `==` and `!=` compare its bytes.

A range needs the order: a rule with one whose mask's format segment is `*`, or names a format
without an order (raw, `oci`), is refused (§1.3).

### 1.3 Validation

A rule is refused with `400` naming the field when its `repoMask` is not a mask of rbac.md §3; its
`name` or `variant` is longer than 256 bytes, or `name` is empty; its `version` is not the grammar
above, or has a range it cannot have (§1.2); its `effect` is neither `deny` nor `allow`; its
`reason` is empty, longer than 512 bytes or has a control character; or its `expiresAt` is not in
the future. The repos of the mask need not exist: a rule may come before its repo, and outlives
it, as a binding does.

## 2. Verdicts

A verdict is an in-toto statement of the predicate type `https://lajstrom.dev/verdict/v1` about one
artifact, signed, and stored in the registry as a referrer of the artifact's manifest
(manifest-profile.md §6.4). Its predicate is `{verdict, reason, source, scope, validUntil}`:

| Field | Value |
| --- | --- |
| `verdict` | `allow`, `deny` or `quarantine` |
| `reason` | text, shown to clients as a rule's |
| `source` | who decided: `lajstrom:manual` (the deny of the management API, §7), `lajstrom:quarantine` (a quarantine over, §4), or the producer's own name (`trivy`, a scanner job) |
| `scope` | a repo mask of rbac.md §3: the repos in which the verdict holds for every artifact with its subject's digest. Absent or `""`: the repo whose OCI repository holds the referrer |
| `validUntil` | RFC 3339; absent: no end |

A verdict is **in force** in a repo when it verifies (it is signed by the server's policy key,
§5, or by a key or a keyless identity that `attachments.trust` or `attachments.keyless` trusts
for its predicate type, and names the subject's digest: manifest-profile.md §6.3), its
`validUntil` has not passed, and its scope matches the repo. A verdict that does not verify is an
attachment like any other (manifest-profile.md §6.1), labelled with its verification, and judges
nothing. A trusted signer may widen the scope: a verdict with scope `*/*` on the digest of one
tarball holds in every repo that holds that tarball's version.

The index keeps the verdicts that verify in the derived table `verdicts` (index-schema.md), which
reindex rebuilds from the referrers. The registry is the truth: a verdict is made by pushing a
referrer and removed by deleting it.

## 3. The decision

A **subject** is what is decided: the repo that holds or would hold the artifact, the package
name, the version, the variant, the manifest's digest (none for a coordinate the repo does not
hold yet), and the quarantine clock of the version (§4). `policy.Decider.Decide` answers a
**decision**: `allow`, `deny` or `quarantine`, with the reason, the rule or the verdict that
decided, and until when it holds by the clock alone. In this order, the first that applies:

1. a verdict `deny` in force: `deny`, with its reason;
2. a requirement of the repo that the subject's manifest does not meet (§11), but in the decision
   of an upload or a push (§6.3) and of a producer's read of `/v2` (§6.5), which leave the
   requirements out: `deny`, with the reason `requires an attachment <predicate type> signed by
   <name>`, the names joined by ` or `, of the first requirement not met in the order of the list;
3. a rule `allow` that holds: `allow`;
4. a rule `deny` that holds: `deny`, with its reason (the rule with the lowest `id` when several
   hold);
5. a verdict `quarantine` in force, or a quarantine of the repo not over (§4), unless a verdict
   `allow` is in force: `quarantine`, with the reason `quarantined until <RFC 3339>` and the
   verdict's reason after it, if any;
6. otherwise `allow`.

So an `allow` rule makes an exception to the `deny` rules and to quarantine, and not to a
requirement; an `allow` verdict releases a quarantine and nothing else, and only the expiry or the
deletion of its referrer lifts a `deny` verdict; only the attachments it lacks lift a requirement's
`deny`. `quarantine` refuses as `deny` does: it differs in its reason and in that it lifts. A
decision **holds until** the earliest time at which, by the clock alone, it could change: the end
of a quarantine, or the `expiresAt` or `validUntil` of a rule or verdict that it read, whichever
comes first; none when nothing it read expires. A requirement adds no time to it: what lifts its
`deny` is the write of an attachment's rows, which invalidates the documents of the repo (§11).

## 4. Quarantine

`policy.quarantineNewVersions` on a proxy repo is a duration (Go syntax, `72h`; at least `1m`, at
most `8760h`; absent or `0s`: none). A version of the repo is quarantined from its **clock** until
the clock plus the duration. The clock is the earliest of:

1. for a version the repo holds, its `created_at`: the manifest's `org.opencontainers.image.created`,
   which is the upstream's publish time where the upstream's document gives one, and the time of
   the fetch otherwise (manifest-profile.md §3.1);
2. the upstream's publish time, where the upstream's document gives one
   (`format.UpstreamEntry.Published`: npm `time.<version>`; pypi the earliest PEP 700
   `upload-time` of the version's files; maven none);
3. the first time the repo saw the version in its upstream's document: a `policy_seen` row,
   written by the proxy core when it parses a document of a repo with a quarantine, for each
   version without a publish time.

Without any of them the clock is now. The earliest, so that a version fetched once its
quarantine is over, whose `created_at` is then the time of that fetch, is not quarantined again.
A publish time is the upstream's word: an upstream that backdates a version shortens its
quarantine. A `policy_seen` row that is lost (index-schema.md: a cache) starts the clock of its
version again, which lengthens the quarantine and never shortens it.

A quarantined version is left out of the repo's documents, its file is `403`, and the proxy does
not fetch it (§6). The fetch of a version of a repo with a quarantine after its quarantine is over
attaches an `allow` verdict to its manifest, signed with the server's policy key (§5), source
`lajstrom:quarantine`, reason `quarantine of <duration> over`, scope the repo, once its ingest
confirmed it; best effort: a failure is logged, and the version is served all the same. Without
the policy key no verdict is attached. A version held before its quarantine applied (the
quarantine was turned on later) is left out while it lasts, and gets no verdict when it ends.

## 5. The server's policy key

The verdicts the server makes (§4, §7) and the deny list (§8) are signed with one ECDSA P-256 key
of the server, the **policy key**: a `keys` row of kind `policy` without a repo (index-schema.md),
made at the start of `serve` when there is none, by one replica (`keys_active`). Its private key
is sealed with the master key (`keys.masterKeyEnv`) as a GPG key's is, or held in a KMS when
`policy.signingKey` is a key URI of ADR 0020 (`awskms:///…`, `gcpkms://…`): then the row holds
the KMS key's public key and the sealed URI, and the private key never leaves the KMS. Its
fingerprint is the hex sha256 of its public key's PKIX DER. `GET /api/v1/policy/key` answers the
public key as a PEM `PUBLIC KEY` to anyone: it is what `cosign verify-attestation --key` and
`cosign verify-blob --key` take, and what an admission controller's policy names.

The policy key verifies its own verdicts: ingest and reindex trust it for
`https://lajstrom.dev/verdict/v1`, with the name `lajstrom`, beside `attachments.trust` (its `trust` hash
includes it, manifest-profile.md §6.3). Without a master key and without a KMS key there is no
policy key: rules and the verdicts of trusted producers hold, and what the server would sign is
`501` (§7, §8). This issue does not rotate the key.

## 6. Enforcement

### 6.1 Rendered indexes

`format.CachedDocument` renders a scope through a **policy view** of the index (`policy.View`):
the reader that `RenderIndex` gets leaves out of `ListVersions`, `ListVersionPage`, `GetVersion`,
`GetVersionByTag`, `GetVersionByDigest` and `FindFiles` every version whose decision is not
`allow`, so that no renderer filters on its own. A package whose versions are all left out keeps
its name and has no versions where the format lists a package without versions (the pypi project
list, an empty project page), and has no document where it does not (npm's packument: `404`). npm: a dist-tag that names a version left out is left out, and
`latest` then names the greatest version left that is not a prerelease, or is absent. The view
reads the rules once and the repo's verdicts once per render, and, on a repo with requirements
(§11), the verified attachments of the repo's versions once (`ListAttested`, index-schema.md,
Attachments).

Every document of a render is stored with the earliest time its decisions hold until (§3),
`index_cache.expires_at`; a document read after that time is rendered again. The creation and
the deletion of a rule invalidate the `index_cache` of every repo its mask matches (§1.1, a
group's members included), and the write or deletion of a verdict's rows that of every repo its
scope matches, in the transaction of the change (index-schema.md, The generation of a repo's
documents); on a repo with requirements, so do the write and the deletion of a version's
attachment rows, and a change of the requirements (§11).

A proxy's document (proxy-and-group.md §7) is rendered from the upstream's document, as
`proxy_meta` keeps it, and from the versions the repo holds: the core passes the held versions
through the view, and the kept document through the plugin's `format.Trimmer`, which leaves out
the versions whose decision is not `allow`. A plugin without a `Trimmer` renders the upstream's
versions as they are, and their files are `403` (§6.2).

### 6.2 Downloads

`format.ServeBlob` decides first when it is given the subject (`ServeBlobOptions.Subject`), as
every frontend gives it for a file of a version: a decision other than `allow` is `403` before any
request to the registry, with the reason, in the format's error answer (http-routing.md §3). The
facade's blobs are not decided: a blob is shared by artifacts that are not denied (§6.5).

### 6.3 Uploads

Ingest decides after `Validate` and before the tag claim, on the coordinates and the digest of the
manifest: a decision other than `allow` is `ingest.ErrDenied` (frontends `403`, `/v2` `403
DENIED`), and nothing is claimed or written to the registry; the session of an upload that
streams (`Publisher.UploadStream`) is cancelled. The decision of an upload leaves the repo's
requirements out (§3, step 2): a version has no attachment before it exists, and its attachments
come after it. Referrers, journal entries, `Inspect` (reindex) and `MirrorManifest` are not
decided. A push to an `oci` repo, which does not go through ingest
but for a manifest with a subject, is decided by the facade before it passes through, with the
path below the repo as the name, the tag as the version (`""` for a push by digest) and the
manifest's digest.

### 6.4 The proxy

The proxy decides at step 3 of a fetch on miss (proxy-and-group.md §5), for a version it holds and
for one it would fetch: a decision other than `allow` is `403`, and nothing is asked of the
upstream. The subject of a version it does not hold has no digest, and its clock is the entry's
(§4). A prefetch reports a version so refused as `failed` with `403` and the reason, as a
request of its file would have had (proxy-and-group.md §5.1), and fetches the rest.
An `oci` proxy repo is a `/v2` surface: it refuses only under `enforcePull` (§6.5), once the
manifest's digest is known.

### 6.5 `/v2`

On a repo with `policy.enforcePull: true` (any format, `oci` included), `GET` and `HEAD
…/manifests/<ref>` decide once the reference has resolved to a digest, on the version that digest
is on a managed repo, else on the path below the repo, the tag of the reference (`""` for a
digest) and the digest: a decision other than `allow` is `403 DENIED` with the reason in the
message and `detail` `{"reason", "rule"}`, `{"reason", "verdict"}` (the referrer's digest) or
`{"reason", "requirement"}` (§11). A read whose bearer token grants the repository `push` or
`attach` (rbac.md §4), a producer's, is decided without the repo's requirements: the clients that
sign and attach read the version's manifest first, `cosign sign` and `cosign attest` of a tag
with a `GET` (cosign v3.1.4), `oras attach` with a `HEAD`, and ask their token with `pull,push`,
while a client that only pulls asks for `pull`. A manifest that is no version (a referrer, a
manifest under cosign's legacy tags) is not decided on a requirement. Blobs, tag lists, referrers
and the catalog are never refused. Without `enforcePull` no read of `/v2` is refused; a push of a
denied coordinate is refused on every repo (§6.3).

### 6.6 Answers and audit

The message of a refusal is `<name> <version> is denied by policy: <reason>`, or
`<name> <version> is quarantined until <RFC 3339>` with the verdict's reason after it, if any. A
requirement's reads:
`app 1.4.0 is denied by policy: requires an attachment https://spdx.dev/Document signed by release`.
Every refused request, but a render's leaving a version out, appends an audit row `policy.refuse`
in a transaction of its own (rbac.md §5): the repo, the subject `<name> <version>[ <variant>]`
(the path and the reference on `/v2`), the digest when known, and `details` `{"surface", "effect",
"reason"}` with `rule` (its id) or `verdict` (the referrer's digest) and `source`, or with
`requirement`, the requirement not met as the repo's policy has it (`{"predicateType",
"signedBy"}`). `surface` is `download`, `upload`, `fetch`, `prefetch`, `pull` or `push`.

## 7. The management API

The routes are http-routing.md §4's, their shapes api.md's.

- `GET /api/v1/policy/rules` lists every rule, for the admin role; with `?repo=<format>/<repo>`,
  the rules whose mask matches the repo, for whoever may read it.
- `POST /api/v1/policy/rules` creates a rule (`201`). A mask that names one repo needs `manage`
  on it; any other mask the `policy` resource (the admin role), decided once the body is read.
  Audit `policy.rule.create`, event `policy.rule.created`.
- `DELETE /api/v1/policy/rules/<id>` deletes one (`204`), with the permission its creation
  needed; `404` for an id the caller may not delete. Audit `policy.rule.delete`, event
  `policy.rule.deleted`.
- `GET /api/v1/policy/verdicts?digest=&repo=` lists the indexed verdicts of a subject digest, of a
  repo, or both (one of the two is required), of the repos the caller may read, each with whether
  it is in force now.
- `POST /api/v1/repos/<format>/<repo>/packages/<name>/versions/<version>/deny[?variant=]` with
  `{"reason", "validUntil"}` (`validUntil` optional) signs a verdict `deny`, source
  `lajstrom:manual`, scope the repo, for the manifest of each variant of the version (or of the one
  `variant` names), and pushes each through `ingest.PutReferrer` as the request's principal, as a
  bundle referrer and as a layer of cosign's legacy attestation (manifest-profile.md §6.4); `201`
  with the verdicts. On an `oci` repo `<name>` is the path below the repo and
  `<version>` a tag or a digest. `manage` on the repo. `501` without the policy key.
- `DELETE …/versions/<version>/deny[?variant=]` removes the verdicts of source `lajstrom:manual`
  of those manifests: their bundle referrers are deleted through `ingest.Delete`, and the legacy
  attestation is written again without their layers, keeping another producer's, or deleted when
  none is left; `204`, `404` when there is none.
  `manage` on the repo.
- `GET /api/v1/policy/denylist` answers the deny list of §8 as one flat JSON document, with the
  entries of the repos the caller may read.
- `GET /api/v1/policy/key` answers the policy key's public key (§5), to anyone.

A verdict's rows written or deleted by ingest (a referrer of a verdict pushed or deleted through
any route) append an audit row `policy.verdict.create` or `policy.verdict.delete` in the
transaction of the rows, and write the event `policy.verdict.created` or `policy.verdict.deleted`
(webhooks.md §1). Rows that reindex writes or sweeps write neither.

## 8. The deny list

The deny list is the rules in force and the verdicts in force that refuse (`deny`,
`quarantine`), in one signed document, for consumers outside Lajstrom: a node or an air-gapped
mirror that verifies it offline, an edge proxy or an OPA policy that wants a kill switch that does
not wait for a release. Each entry has its expiry (`expiresAt`, `validUntil`): a consumer drops an
entry whose time has passed, and the list is not written again for an expiry alone.

- **The artifact** (manifest-profile.md §10) is written to the OCI repository
  `lajstrom/policy/denylist` of the registry `policy.denylist.registry` (the first of
  `registries` by default) by every replica that has the policy key: it reads
  `policy_state.generation` every 10 seconds, which every change of a rule or of a verdict
  increments in its transaction, and writes the list when the greatest tag of the repository
  carries another generation. Its tag sorts by the database's time of the read and then by the
  generation, so that the greatest tag is the newest list whatever the order in which two
  replicas push, after a restore of the database too. A consumer pulls it from the registry with
  the registry's own credentials (the operator gives nodes and mirrors read access to
  `lajstrom/policy/denylist`) and takes the greatest tag; Lajstrom's `/v2` does not serve it.
- **The flat JSON** of `GET /api/v1/policy/denylist` is the same document, read from the index at
  the request, without a signature, with the entries of the repos the caller may read: a deploy
  token over `*/*` with `read` gets every entry.

The document:

| Key | Value |
| --- | --- |
| `version` | `1` |
| `generation` | the policy generation it was read at |
| `createdAt` | the database's time of the read, RFC 3339 UTC |
| `rules` | the rules in force with effect `deny`: `{"id", "repoMask", "name", "version", "variant", "reason", "expiresAt"}` |
| `verdicts` | the verdicts in force that refuse: `{"repo", "ociRepository", "subject", "verdict", "reason", "source", "scope", "validUntil", "attestation"}`, `attestation` the referrer's digest |
| `digests` | the manifest digests of the versions the rules and verdicts refuse that the index holds, and the digests of their file layers, deduplicated |
| `prefixes` | URL paths below the external URL that serve only what is refused: `/v2/<ociRepository>/manifests/<digest>` for each refused manifest, and the format's own paths for a rule of one exact version (`format.Locator`: pypi `/pypi/<repo>/files/<name>/<version>/`, maven `/maven/<repo>/<group path>/<artifactId>/<version>/`, npm the tarball's path) and for each file of a refused version the repo holds |

An edge can block a path or a digest, and nothing else: it cannot take a version out of an index,
and a range rule has no prefix (ADR 0008).

A version that a requirement refuses (§11) is in neither: the list is of rules and verdicts, and a
version waiting for its attachments is refused by Lajstrom alone, until the rows of its last
required attachment commit. A requirement, and a change of one, does not move
`policy_state.generation`.

## 9. Repo settings

Three keys of a repo's `policy` (api.md, the policy keys), read on every request:

| Key | Value |
| --- | --- |
| `enforcePull` | bool, default `false`: `/v2` refuses the manifests that the decision refuses (§6.5) |
| `quarantineNewVersions` | a duration, proxy repos only (`400` on another kind): §4 |
| `requireAttachments` | a list of `{"predicateType", "signedBy"}`, absent by default; a hosted repo of a format other than `oci`, not encrypted at rest, only (`400` on another): §11 |

## 10. Reindex

`verdicts` is derived. Reindex rebuilds a managed repo's rows from its attachments: an attachment
of predicate type `https://lajstrom.dev/verdict/v1` that verifies (manifest-profile.md §6.3) has a
`verdicts` row, written and swept with it. An `oci` repo has no attachments: reindex lists the
referrers of each manifest that one of its tags names, one request per distinct manifest (one
`Resolve` per tag costs already), and writes the rows of the verdicts among them; a verdict of a
manifest that no tag names is not found again after the database is lost, and the last deny list
names it. `policy_rules` and `policy_state` are control plane, `policy_seen` a cache: reindex
neither reads nor writes them, and `ResetDerived` leaves them.

## 11. Required attachments (after the MVP, #1287)

`policy.requireAttachments` of a repo (§9) is a list of **requirements**, each
`{"predicateType", "signedBy"}`, decided by the maintainer on #1287
([ADR 0033](../adr/0033-required-attachments-of-a-version.md)). A version's manifest **meets** a
requirement when it has an attachment (manifest-profile.md §6.1) labelled `verified`
(manifest-profile.md §6.3) whose predicate type the requirement matches and whose `signedBy` is one
of the requirement's names. A version is served only while its manifest meets every requirement
of its repo; otherwise it is `deny` (§3, step 2), with the reason of the first requirement it does
not meet, in the order of the list. Each variant is decided on its own manifest: a version's other
variants, and the attachments of another digest, count for nothing.

```json
{"policy": {"requireAttachments": [
  {"predicateType": "https://spdx.dev/Document", "signedBy": ["release"]},
  {"predicateType": "https://slsa.dev/provenance/v1", "signedBy": ["release", "release-2027"]}
]}}
```

- **The predicate type** of a requirement matches an attachment's when the two are equal, or when
  the attachment's continues it after a `/`: `https://cyclonedx.org/bom` matches
  `https://cyclonedx.org/bom/v1.6`, and `https://slsa.dev/provenance` matches
  `https://slsa.dev/provenance/v1`, as the kinds of manifest-profile.md §6.1 recognise them. An
  attachment of kind `signature` counts as `https://sigstore.dev/cosign/sign/v1`, as it does for
  the trust (manifest-profile.md §6.3): a requirement of that type is a signature.
- **The names** are those of `attachments.trust[].name` and `attachments.keyless.identities[].name`
  ([configuration.md](../configuration.md#attachments)), the `signedBy` of what each verifies; any
  one of a requirement's names meets it. The label read is the index's (`verification`, `signed_by`
  of `attachments`), decided when the attachment was indexed (index-schema.md, Attachments), with
  one check more at the decision: a name that the replica's trust does not have meets nothing, so
  that a key or an identity dropped from the configuration stops counting at the start that drops
  it, before any reindex. A key replaced under the same name counts for what the old key verified
  until the next reindex of the repo labels it again: a new key takes a new name. `lajstrom`, the
  name of the policy key, which verifies verdicts only, is no name of a requirement.
- **Who sets it, and how.** It is a key of the repo's policy: `PATCH
  /api/v1/repos/<format>/<repo>` with `manage` on the repo, as every key of a repo's policy;
  `lajstrom admin repo create --policy`; the configuration's `repos`. No route or command of its
  own. Its change is the repo's audit row, `repo.update` (`repo.create` for a new repo), whose
  details name what changed, `changed.policy` the policy as the change left it (rbac.md §5).
  Whoever may set a requirement may lift it, so `manage` on a production repo is the right to turn
  its requirement off: grant it to those who may, and review its `repo.update` rows (decided on
  #1287; the admins only, not taken).
- **Validation.** The policy is refused, `400` with the key path first in the message (api.md, the
  policy keys), when `requireAttachments` is not a list of 1 to 16 requirements (absent or `null`:
  none); a requirement has a key other than the two, or is the same as another; a `predicateType`
  is not an absolute URI (RFC 3986, with a scheme) of at most 256 bytes, or is the verdict's,
  `https://lajstrom.dev/verdict/v1` (a verdict judges by its value, §2, and a `deny` would meet
  it); `signedBy` is not a list of 1 to 16 names, or a name is not a key's or an identity's of the
  replica's `attachments`, or is trusted for no predicate type that the requirement matches (its
  `predicateTypes` empty, every type, or one that equals or continues the requirement's); or the
  repo is a proxy (a version it would fetch has no attachment, so it would never fetch one), an
  `oci` repo (it indexes no attachment: its consumers verify, ADR 0008), or keeps its files
  encrypted (`encryption.mode: at-rest`: its attachments are encrypted, and labelled
  `unsupported`). A group has no policy: a member's requirements hold for its versions read
  through the group, since a decision is taken in the repo that holds the artifact (§1.1). The
  `repos` of the configuration are checked at start, as errors of the configuration. `serve` warns
  at start, once per repo, of a requirement that names a signer its trust does not have: that
  requirement refuses every version.
- **Every version, whenever it was pushed.** A requirement holds for every version of the repo from
  the commit of the change that sets it: the versions pushed before, which nobody attached to, are
  left out of the indexes and refused from then on. No time exempts a version, since the time a
  version has, `org.opencontainers.image.created`, is the pusher's word.
  `GET …/versions?unmet=<predicateType>&signedBy=<name>` lists the variants that a requirement
  would refuse, before it is set as well as after (http-routing.md §4).
- **A version before its attachments.** An upload is never refused for a requirement (§6.3): a
  version is pushed, then its attachments, and it is served from the commit of the rows of the
  attachment that meets its last requirement. Ingest writes those rows once the registry serves the
  referrer back (`PutReferrer`), best effort: a version whose rows were not written waits for the
  next reindex of its repo (index-schema.md, Attachments). On a repo with requirements, every
  write and deletion of a version's attachment rows, by ingest, a delete or reindex, and every
  change of the repo's requirements, invalidates the repo's `index_cache` (a group's with it) in
  its transaction, as a verdict's rows do (§6.1): the next render lists a version whose
  attachment came, and leaves out again one whose attachment went.
- **Where it is decided.** At the enforcement points of §6 on the repo: the rendered indexes
  (§6.1), the downloads (§6.2), and the manifest reads of `/v2` under `enforcePull` but with a
  token that may push, a producer's (§6.5). Never at an upload or a push, a referrer, a journal
  entry, reindex or a delete.
- **Rules, verdicts and quarantine.** Only a `deny` verdict comes before a requirement (§3): no
  `allow` rule and no `allow` verdict lets a version without its attachments through, and an allow
  list (an `allow` rule `@acme/*` beside a `deny` rule `*`) does not exempt what it names. A version
  that lacks an attachment and is also quarantined, by a verdict, is refused for the attachment,
  and its quarantine shows once the attachment is there. A requirement adds nothing to the deny
  list (§8).
- **Reads.** The view of a render reads the repo's verified attachments once (`ListAttested`,
  index-schema.md, Attachments); a download and a pull read those of their version.
