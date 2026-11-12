# Spec: guarantees

- Status: normative, v2 (2026-11-11: the lead's answers to the questions of v1). Changes need
  an issue labelled `contract` (below, "How a guarantee changes").
- Owned by: the lead. Every pull request that changes a mechanism or a test named here updates
  the entry.
- Related: [ADR 0001](../adr/0001-oci-registry-as-sole-storage.md) to
  [ADR 0013](../adr/0013-proxy-and-group-repos.md), every spec of this directory,
  [threat-model.md](../security/threat-model.md), [registries/README.md](../registries/README.md)
  (the matrix), [acceptance.md](../acceptance.md), [testing.md](../testing.md).

## The rule

**What this document guarantees is the contract. Anything it does not guarantee, a user has no
right to rely on.** That an observable behaviour holds today does not make it a promise
(Hyrum's law, refused on purpose): it may change in any release, without notice.

- **What is in the contract.** The guarantees below; the normative text of the specs of this
  directory (the wire formats, endpoints, statuses, tables and Go contracts they define); the
  references of the three interfaces the README's promise names: the management API
  ([api.md](../api.md) and its OpenAPI document), the keys of the configuration and what each
  means ([configuration.md](../configuration.md)), and the CLI's commands, flags and exit
  statuses; the names of the metric series of [operations.md](../operations.md#metrics) and its
  rule that no label carries a repo or package name; and the prefixes `ljs_pat_` and `ljs_dep_`
  of token secrets ([auth.md](../auth.md#tokens)), which secret scanners key on. This document
  includes them by reference and does not repeat them. Not in the contract: the default value of
  a configuration key, and the timings of retries ([Not guaranteed](#not-guaranteed)). The ADRs
  record why; what they decided is guaranteed through the spec that carries it. Everything else
  describes and does not promise: architecture.md, operations.md but for its metric names, the
  guides, the README, code comments, log lines, tests. When this document and a spec disagree,
  that is a bug of one of them: open an issue labelled `contract`; until it is decided, the
  narrower reading holds.
- **How a guarantee changes.** Adding, withdrawing or changing the statement, the scope or the
  assumptions of a guarantee is a contract change: an issue labelled `contract`, a pull request
  the lead approves, and a line in the release notes that says what changed and how to adapt.
  While Lajstrom is 0.x, that may happen in any release, as the README's
  [promise to users](../../README.md#our-promise-to-users) says of the API, the configuration and
  the CLI; from 1.0 on, only in a major release. Updating the mechanism, the tests or the status
  of an entry as the code moves is not a contract change, and a pull request that makes a
  guarantee weaker than its status says is a bug.
- **Ids.** `INV-1`, `INV-2`, …, never reused and never renumbered; a withdrawn guarantee keeps
  its id and says so. v1 called them `G-1`, …, too close to acceptance.md's checks `G-01` to
  `G-07`.

## How to read an entry

Each guarantee has:

| Field | What it says |
| --- | --- |
| Statement | one sentence that a test can check |
| Scope | the formats, repo kinds (`hosted`, `proxy`, `group`, and `oci`, the pass-through format) and paths it covers; what is outside it is not guaranteed |
| Assumes | what it needs of the registry and of the database, by the names of the next section; "any" means it needs none of them |
| Mechanism | the code that makes it hold: `package.Function` or `package.Type.Method`, as the threat model names them (`ingest.Pipeline.claimTag` is the method in `internal/ingest`) |
| Tests | the tests that prove it, by name, and the steps of the end-to-end scripts (`test/e2e/<script>`, with the acceptance id they cover) |
| When violated | whether Lajstrom notices, where it says so, and what repairs it |
| Status | `holds`; `holds on strongly consistent registries only`; `partly` (the entry says which part does not hold); `not yet` (with its issue) |

Two markers flag a weakness of the evidence, not of the code: **untested**, a part of the
statement that no test proves; **unchecked**, an assumption that Lajstrom relies on and does not
verify at run time. Every marker has a row in [Gaps](#gaps).

## What the guarantees rest on

### The registry

Lajstrom checks none of these at run time unless an entry says it does. What each registry
does is measured in [registries/README.md](../registries/README.md); a registry that is not there
is not tested.

| Name | The property | What is known |
| --- | --- | --- |
| **read-after-write** | after the registry answers `2xx` to a manifest push or delete, every later read sees it: `HEAD` and `GET` by tag and by digest, the tag list, the catalog | `registry probe` measures the lag (the row "Read-after-write lag", [registries/README.md](../registries/README.md#consistency)): none seen on distribution, Zot and GitLab; ECR and GHCR are eventually consistent by users' reports, `capabilities.consistency: eventual` ([#446](https://github.com/lajstrom/lajstrom/issues/446)) |
| **monotonic reads** | once a read of Lajstrom's sees a write, every later read through the same endpoint sees it too, until it is deleted | assumed everywhere; not measured. A registry behind DNS that picks a replica per request (ACR's global endpoint with geo-replication) can break it |
| **durable 201** | what the registry acknowledged stays until it is deleted through the registry's API | assumed everywhere; never measured |
| **complete listings** | the catalog and the tag list name every repository and tag written before the call, and none deleted before it | the matrix measures the catalog's limits and the tag lists' order, not their lag; Harbor, GHCR and ECR have no usable catalog, and Lajstrom reads `oci_repos` instead ([operations.md](../operations.md#without-a-catalog)) |
| **digest check** | the registry refuses a blob whose bytes are not the digest it is closed with | every registry of the matrix but JFrog JCR 7.161 (it stores any bytes under any digest) |
| **scoped blobs** | a blob is readable only in a repository that holds it | measured on Zot only (threat model P-5) |
| **case-exact tags** | two tags that differ in case are two tags | not on Gitea and Forgejo, which fold tag case ([#426](https://github.com/lajstrom/lajstrom/issues/426)) |
| **GC keeps what is named** | the garbage collector keeps a manifest a tag names and a referrer whose subject exists | GitLab B deletes an untagged referrer 24 h after its push; Nexus's task "Delete unused manifests" deletes one; Gitea deletes the manifest a re-pointed tag named |

**Where the guarantees hold**, by the tiers of the matrix
([Versions tested](../registries/README.md#versions-tested)):

- `CI` (distribution on the filesystem, Zot, at their pinned versions): as each entry's status
  says. Every pull request that touches code checks them.
- `manual` and `hosted`: as measured on its date, at the pinned version, and not checked since:
  GitLab A and B, GHCR and Harbor on 2026-11-07; distribution on S3, MinIO and SeaweedFS on
  2026-11-09; JFrog JCR, Nexus, Gitea and Forgejo on 2026-11-10.
- `untested` (ECR, Quay, Azure Container Registry, Docker Hub), and any version other than the
  pinned one: nothing is promised.

**An eventually consistent registry is not refused.** Until
[#446](https://github.com/lajstrom/lajstrom/issues/446) (priority p1) lands, Lajstrom serves a
registry without **read-after-write**, such as ECR, and warns at start instead: the entries whose
status is `holds on strongly consistent registries only` do not hold there.

### The database

| Name | The property |
| --- | --- |
| **one database** | every replica uses the same database, and the same master key (`keys.masterKeyEnv` or `keys.masterKeyFile`) |
| **Postgres for replicas** | more than one replica needs Postgres. SQLite serves one replica: its referrers and journal locks are locks of the process (index-schema.md, Coordination). A `serve` that finds another live one on the SQLite file refuses to start and names it (acceptance.md H-14); the CLI's commands are not counted |
| **clocks agree** | the clocks of the replicas, and of the CLI, agree within 60 seconds: the skew a bearer token is checked with, and well inside the bounds that compare times written by other replicas (stale claims after `ingest.claimTTL`, 10 minutes; stale fetch claims and reindex runs after 2 minutes). `serve` compares its clock with the database's at its start and every 30 seconds: past 10 seconds it warns, past 60 seconds it is not ready (operations.md, Clocks). `lajstrom reindex` and the `admin` commands that open the database compare their host's clock with it before they run: past 10 seconds they warn, past 60 seconds they refuse and write nothing |

## Storage and the index

### INV-1. Artifact bytes live only in the registry

- **Statement.** The bytes of a package file are stored in the registry and nowhere else: never in
  the database, and on a replica's disk only as a spool of one request, removed when it ends. A
  raw, apt, Maven, PyPI or npm upload is not on the disk at all ([ADR 0014](../adr/0014-uploads-stream-into-the-registry.md)).
- **Scope.** Every format and kind, both write paths (a frontend upload, a `/v2` blob upload) and
  the proxy fetch. The database keeps derived copies of metadata that reindex rebuilds or a cache
  may drop: `versions.metadata` (a metadata layer of at most 4 MiB), rendered documents in
  `index_cache`, an upstream's documents in `proxy_meta`.
- **Assumes.** Any.
- **Mechanism.** `format.Spool` (one temporary file per proxy fetch, mode 0600, unlinked at
  creation where the system allows, removed on `Close`), the proxy's
  `proxy.Core.spool`; `/v2` uploads stream into the backend's session (`facade` uploads, the
  provider's `BlobUploader`) and take no disk, but for a chunk of unknown length that
  `uploads.spoolUnknownLength` spools (one temporary file of the chunk, unlinked when it is
  made). A raw `PUT`, an apt, a twine and an npm upload, and a Maven file (`ingest.Pipeline.Add`), stream into a session too
  (`ingest.Pipeline.UploadStream`, the provider's `BlobWriter`), which is closed with the digest
  Lajstrom computed only once every check passed and cancelled otherwise, so a refused upload
  stores nothing; but a wheel or a `.zip` sdist read back from the registry (ADR 0014 step 5),
  whose blob a refusal leaves to the registry's garbage collection, without a manifest.
- **Tests.** `TestSpool`, `TestSpoolErrors`, `TestSpoolNoTempDir`, `TestUploadDoesNotHoldTheFile`,
  `TestStreamingMemory`, `TestPutWritesNoTempFile` and
  `TestPutRefusedStoresNothing` (raw), `TestUploadWritesNoTempFile` and `TestUploadRefusedStoresNothing`
  (deb), `TestDeployWritesNoTempFile` and `TestDeployRefusedStoresNothing` (maven), `TestSessionAddStreams`
  (ingest), `TestUploadWheelWritesNoTempFile`, `TestUploadWheelRefused` and
  `TestUploadSdistRefusedStoresNothing` (pypi), `TestPublishWritesNoTempFile` and
  `TestPublishRefusedStoresNothing` (npm), `TestUploadStreamRefused` (ingest); `ha.sh` H-05 (a
  replica killed with `SIGKILL` in a 500 MiB upload: nothing left in its `/tmp`).
- **When violated.** A spool that a `SIGKILL` leaves where the system cannot unlink at creation
  stays in `$TMPDIR` (threat model U-2); nothing serves from it.
- **Status.** `holds`.

### INV-2. One write path

- **Statement.** Every manifest Lajstrom writes to a managed repo (every format but `oci`) is
  written by `internal/ingest`: a publish, a referrer, a journal entry, a proxied artifact, a
  delete.
- **Scope.** Managed repos. On an `oci` repo the facade passes a manifest without a subject
  through to the backend itself (`facade.handler.passThrough`, http-routing.md §2); blobs reach
  the registry through the facade's upload sessions and ingest's blob pushes, by design.
- **Assumes.** Any.
- **Mechanism.** Frontends publish through `format.Deps.Ingest` (`format.Publisher`, implemented
  by `ingest.Pipeline`); the manifest hook hands `/v2` pushes to ingest
  (`facade.handler.putManaged`); the proxy fetcher calls `Publisher.Upload`. `make lint`
  (depguard, rule `registry-client`) keeps the registry client out of every package but
  `internal/provider`.
- **Tests.** `TestManifestHook`, `TestReferrerHandOff`, `TestUploadWithOrigin`, `TestFileMiss`
  (proxy), and per format the upload tests that assert the published rows (`TestUpload` of deb
  and pypi, `TestPutGetHead` of raw, `TestDeploy` of maven, `TestPublish` of npm).
- **When violated.** A manifest written past ingest has no claim, no validation and no audit row;
  reindex finds it later as a publish past Lajstrom (INV-3).
- **Status.** `holds`. **Untested:** nothing machine-checks that a plugin does not push:
  `format.Deps.Providers` gives plugins the provider for reads, and its `Repository` has `Push`
  and `PushReference`; lint stops only the import of the registry client.

### INV-3. The index is rebuildable from the registry

- **Statement.** After `lajstrom reindex` over a repo, the derived rows (`packages`, `versions`,
  `files`, `oci_repos`, npm's `journal` and `journal_state`) equal what ingest wrote for the
  manifests in the registry, in every column but the ids and `indexed_at`, and `index_cache` is
  empty.
- **Scope.** Every format and the `hosted` and `proxy` kinds (a proxy's `proxy_meta` is a cache
  and is not rebuilt); the control plane is not derived and comes from a backup
  ([operations.md](../operations.md#backup-and-restore)). On a registry without a catalog, only the
  OCI repositories that `oci_repos` records or `--attach` names. The manifests at OCI names in the
  grammar of [ADR 0017](../adr/0017-the-grammar-of-oci-names.md), which Lajstrom writes, and at
  the earlier forms of those names, which it reads
  ([manifest-profile.md](manifest-profile.md#43-escape-and-escapecomponent) §4.3); on Zot an
  earlier name that ends in `blobs`, `.uploads` or `.sync` is not in the catalog, and only
  `oci_repos` or `--attach` finds it; on Harbor an earlier name with a component `manifests` or
  `referrers`, or `tags/list…`, answers the check of its blobs at random. A package whose
  manifests are at two forms of its name has the rows of one form; those at the other are
  reported as a split, not indexed ([index-schema.md](index-schema.md#invariants-and-rules), #609).
- **Assumes.** Registry: **complete listings** (**unchecked**) of the names in the grammar of
  ADR 0017 (Zot's catalog leaves out others, [quirks.md](../registries/quirks.md)); **durable 201**.
  Database: any.
- **Mechanism.** One mapping from a manifest to its rows: `ingest.Pipeline.Inspect` runs the
  read-only steps of ingest, and `ingest.WriteRows` writes what ingest writes; the
  `reindex.Worker` lists (`scan.list`, `scan.ociRepo`), marks and sweeps; the journal is
  replayed in `seq` order (`reindex.replay.run`). A manifest whose metadata layer is missing is
  indexed from its annotations and flagged `degraded`. A manifest at an earlier form of
  its OCI name is the package of its annotations (`ingest.publish.checkTarget`), which keeps that
  name: a writer looks it up at every form (`index.PackageAt`, `ingest.Pipeline.storedPath`).
  A manifest whose package the index has at another form is not written (`reindex.scan.write`),
  and a run reads the newer forms of a name first (`reindex.newestFirst`), so that a rebuild
  keeps the form ingest wrote.
- **Tests.** `TestRebuild` (I-01 on a raw repo), `TestRebuildFormats` (three more formats) and
  `TestRebuildPackageCreated` of `internal/index/reindex`, `TestIntegration` and
  `TestIntegrationFormats` of `internal/index/reindex` (I-01 against Zot and distribution, on
  SQLite and Postgres), `TestRenderSameSecond` of `internal/format/pypi`, the store conformance
  case `PackageCreatedAtRace` (writers of one package at once keep its earliest time, #584),
  `TestInspectIsIngest`, `TestJournalReindex`, `TestJournalReindexLost`, `TestPushReindex` (npm),
  `TestReindexProxied`, `TestNoCatalog`, `TestAttach`, `TestRebuildTagSchema`, `TestDegraded`,
  `TestKnownDigests`, `TestScale`, `TestRebuildEarlierNames` and `TestEarlierPackagePath` (the
  OCI names of before ADR 0017), `TestIntegrationNames` (the names of ADR 0017 in Zot's
  catalog), `TestRebuildSplit`, `TestSplitIndexedBefore` and `TestIntegrationSplit` (a package at
  two forms, #609); `cosign.sh` (the referrers found by `reindex --repo`), `oras.sh` (raw files at those
  names, on Gitea too).
- **When violated.** A column that is not derivable is lost silently at the first rebuild. A
  listing that lags misses the newest manifests until a later run.
- **Status.** `holds on strongly consistent registries only`
  ([#446](https://github.com/lajstrom/lajstrom/issues/446): a listing that lags is not yet
  tolerated). **Untested:** acceptance.md I-01 is not a step of the end-to-end suite.

### INV-4. A version in the index is readable from the registry

- **Statement.** Every `versions` row names a manifest that the registry serves under the row's
  tag, with the row's digest, and whose blobs it serves.
- **Scope.** Managed repos, every kind that has rows (`hosted`, `proxy`); a proxy answers a miss
  from the registry right after its ingest, so the same holds for it.
- **Assumes.** Registry: **monotonic reads** (**unchecked**), **durable 201** (**unchecked**),
  **GC keeps what is named**. Not **read-after-write**: the barrier below waits for the lag.
  Database: any.
- **Mechanism.** A row is written only after the registry accepted the manifest and served it
  back: `publish.pushManifest`, then the visibility barrier `ingest.Pipeline.awaitVisible`, then
  `ingest.Pipeline.confirm`. The barrier reads the manifest by digest; on a registry not
  classified `strong` (`provider.Capabilities.Consistency`) also the tag, compared by digest,
  and every blob, and reads again with backoff what it did not see, for
  `ingest.visibilityTimeout` (30 s) or the registry's `consistencyBound` when longer. Past it
  the publish is `503` with `Retry-After: 30`, the claims stay, and the same publish again
  completes it. A delete deletes in the registry first (INV-13); the reconcile loop removes the
  rows of a tag the registry no longer knows (INV-5).
- **Tests.** `TestIngestPushFails`, `TestIngestIndexFailsAfterPush`, `TestBypassAndDelete`,
  `TestDeleteThenReindex`; `TestVisibilityBarrier`, `TestVisibilityTimeout`,
  `TestVisibilityTimeoutIngest` (a registry that misses the first reads of a manifest and its
  tag, or every read), `TestNotVisibleNames`, `TestReadBack`, `TestManifestHookNotVisible`
  and `TestReferrerNotServed` (`503 UNAVAILABLE` with `Retry-After`, naming what is not served).
- **When violated.** A client gets `404` (`BLOB_UNKNOWN`, `MANIFEST_UNKNOWN`, the format's
  `404`) for a version the index lists: only where a registry stops serving what it served
  once. A manifest the registry lost goes from the index at the next reconcile, which counts it
  among `removed` and says nothing more; a tag re-pointed in the backend is drift (INV-7), and on
  Gitea the manifest it named is then deleted. A publish that the registry did not serve in
  time is a `503` and a warning `the registry took a push and does not serve it yet`; on
  `/v2` the `503` names what is not served (the manifest, the tag, the blobs), so that a
  client tells a lag from a loss.
- **Status.** `holds`. **Untested:** no test runs against a registry that lags for real (ECR,
  [#19](https://github.com/lajstrom/lajstrom/issues/19)).

### INV-5. Prune never removes what the registry still holds

- **Statement.** Reindex and the reconcile loop delete a derived row only when the registry
  answered `404 MANIFEST_UNKNOWN` to the row's manifest by its digest twice, at least 10 minutes
  apart with no other answer between, and the row is at least `reconcile.graceAge` old (ingest
  wrote it, or a run last saw its manifest, that long ago). A tag that does not resolve, a
  listing that misses the row or failed, `NAME_UNKNOWN`, a `404` without the code, a `5xx`,
  `401`, `403` and a timeout remove nothing.
- **Scope.** `reindex` with `--prune` (the default for every repo), the API's reindex, the
  reconcile loop; `versions` (with their `files`, and the packages left empty) and the npm
  journal's rows. An `oci_repos` row goes when no run has seen its repository for
  `reconcile.graceAge`. A delete through the API is not prune (INV-13).
- **Assumes.** Registry: a manifest it holds reads back by its digest, at the endpoint Lajstrom
  reads, within `reconcile.graceAge` of its write, and is not answered `MANIFEST_UNKNOWN` twice
  10 minutes apart afterwards (**unchecked**; [operations.md](../operations.md#on-an-eventually-consistent-registry)
  says how to set the grace age). Database: **one database**, **clocks agree**: the 10 minutes and
  the grace age compare times other replicas wrote.
- **Mechanism.** `reindex.scan.sweep` and `reindex.replay.sweep` resolve each row the run did
  not see by its tag; a tag that does not resolve goes to `reindex.scan.lookup`, which asks
  `Provider.StatManifest` and counts only `provider.ErrManifestUnknown`. The first answer is a
  `prune_sightings` row (`Writer.RecordSighting`), shared by every replica; the sweep clears the
  rows it did not record again (`Writer.ClearSightings`). `scan.list` sweeps nothing when the
  listing failed, and runs of one repo never overlap (`reindex_runs`, INV-26). A `404` without
  `MANIFEST_UNKNOWN` in an OCI repository whose tag list is a `404` (`reindex.scan.repoUnknown`)
  is reported, not counted: `RepositoryUnknown` (#567).
- **Tests.** `TestPrune` (`MANIFEST_UNKNOWN` once, twice 9 and 10 minutes apart, a version
  younger than the grace age, a grace age of 24 h, the manifest read back in between, a failure
  in between, `NAME_UNKNOWN`, a `404` without a code, a `5xx`, a `403`, a timeout),
  `TestPruneTwoReplicas`, `TestStatManifest`, the store conformance cases `Sightings`,
  `SightingRace` and `Sweep`, `TestSweepKeepsWhatItCannotCheck`, `TestBypassAndDelete`,
  `TestDrift`, `TestNoCatalog`, `TestNoCatalogRepositoryGone`, `TestNoCatalogRecordsAgain`, `TestJournalReindexLost`,
  `TestRepositoryUnknown`, `TestRepositoryUnknownAskedOnce`, `TestReconcile`; `TestIntegration` of
  `internal/provider/generic` and of `internal/index/reindex`, and `TestIntegrationRepositoryUnknown`,
  against Zot and distribution.
- **When violated.** A registry that lags longer than the grace age and answers
  `MANIFEST_UNKNOWN` twice for a manifest it holds loses that version's rows until the next run
  lists it again and adds it back; the run's audit row `reconcile` counts the removal; nothing
  alerts. The other way round, a manifest the registry lost stays listed, its downloads `404`,
  until both answers and the grace age: the report counts it as `pending`. A version whose whole
  OCI repository the registry no longer knows (Zot answers `NAME_UNKNOWN`) stays until it is
  deleted through the API: every run lists it under `repositoryUnknown`, with that request, and
  warns.
- **Status.** `holds`. **Unchecked:** the registry's lag against the grace age. **Untested:** no
  run against a registry that lags for real (ECR, [#19](https://github.com/lajstrom/lajstrom/issues/19)).

## Publishing and tags

### INV-6. Among concurrent publishes of one tag, one wins

- **Statement.** When publishes of one tag of one package path run at once, on one replica or
  several, at most one manifest is pushed under the tag: the others share it (the same bytes,
  `201`, one version) or get `409` naming the winner's digest.
- **Scope.** Managed repos, every write path (frontends, `/v2`, the proxy fetch).
- **Assumes.** Registry: any. Database: **one database**, **Postgres for replicas**, **clocks
  agree** (a claim older than `ingest.claimTTL` is taken over).
- **Mechanism.** `ingest.Pipeline.claimTag`: in one transaction, the claim on the package path
  (`claimPath`), then on the tag (`Writer.ClaimTag`, unique `(repo_id, package, tag)` in
  `tag_claims`), then the version of the tag is read; `UpsertVersion` resolves a race with
  `INSERT … ON CONFLICT DO NOTHING` and a second read.
- **Tests.** `TestIngestConcurrent`, `TestIngestConflict`, `TestIngestStaleClaim`,
  `TestUploadRepeatRace`, `TestIngestSpellingRace`, the store conformance cases `TagClaims`,
  `TagClaimRace`, `TagClaimsLifecycle`, `VersionTagRace`; `ha.sh` H-02 (one 100 MiB file through
  both replicas: one version) and H-03 (twenty rounds of one tag through both: one `201`, one
  `409 TAG_INVALID` naming the winner, whose digest the backend and the index hold).
- **When violated.** Not observed. A claim left by a dead publish holds its tag until it is
  stale; reindex and the reconcile loop delete stale claims.
- **Status.** `holds`. During a rolling upgrade from a version before #244, the claims of the two
  versions do not exclude each other (index-schema.md, Coordination).

### INV-7. A tag is never re-pointed through Lajstrom

- **Statement.** Once a tag of a managed repo names a manifest in the registry, no request to
  Lajstrom makes it name another: a publish of other bytes under it is `409`, and nothing in
  Lajstrom re-points a tag on conflict, on reindex or on drift.
- **Scope.** Managed repos: version tags and the `_j<seq>` tags of npm's journal. Not `oci`
  repos, whose tags pass through and follow the backend's rules. Not writes straight to the
  backend: Lajstrom reports them (drift) and leaves them.
- **Assumes.** Registry: **monotonic reads** (**unchecked**) and a lag shorter than
  `ingest.claimTTL` for the backstop below; **case-exact tags** (on Gitea and Forgejo two
  versions that differ only in case are one tag, and the second is a `409`:
  [#426](https://github.com/lajstrom/lajstrom/issues/426)). Database: as INV-6.
- **Mechanism.** The index and the claims (INV-6) decide; then `ingest.Pipeline.checkTag` resolves
  the tag in the backend before the push and refuses another digest there, which catches a tag
  the index lacks (a write past Lajstrom, an index not yet rebuilt, a confirm that failed after
  the push); a frontend upload of the same files as the manifest there takes that manifest over
  instead and pushes its digest again, so the tag keeps it (INV-8); a backend with immutable
  tags refuses the push on its own. On a lagging registry
  that read cannot miss Lajstrom's own pushes: a confirm runs only after the visibility barrier
  saw the tag (INV-4), and a push the registry did not serve in time keeps its claim for
  `ingest.claimTTL`, which refuses another digest meanwhile. `Writer.UpsertJournal` never
  rewrites an entry; each entry is claimed in `tag_claims` before its push, the journal adopts a
  stray entry instead of overwriting it (`ingest.journal.adopt`), and an entry an earlier append
  claimed and the registry does not serve yet is read back, then waited for (`503`) until its
  claim is stale (`ingest.journal.claimed`); on a `strong` registry a claimed entry it does not
  serve was never pushed, and is taken over. Reindex reports drift (`scan.drift`) and keeps the
  index's digest.
- **Tests.** `TestIngestConflict`, `TestIngestTagInBackend`, `TestIngestIndexFailsAfterPush`,
  `TestUploadRetryUnconfirmed`, `TestManifestHook`, `TestDrift`, `TestJournalAdopts`, the store
  conformance case `Journal`;
  `TestVisibilityTimeout`, `TestVisibilityRetryRebuilt`, `TestJournalVisibility`,
  `TestJournalClaimNotPushed`;
  `oras.sh` (another manifest under the tag: `409`), the conflicts of `raw.sh`, `pypi.sh`,
  `apt.sh`, `maven.sh` (acceptance.md E-01).
- **When violated.** Reindex lists the tag as drift (`reindex --report`, `GET …/reindex`) with
  both digests; nothing repairs it, by design: whoever re-pointed it decides.
- **Status.** `holds`. A lag longer than `ingest.claimTTL` (10 min) would let a stale claim's tag
  be pushed again; none is reported for a registry's own region
  ([registries/README.md](../registries/README.md#consistency)).

### INV-8. The same bytes again are an idempotent repeat

- **Statement.** A publish of a manifest a tag already holds, and a frontend upload of the same
  files under a tag that holds them, answer success with the existing digest and write nothing.
- **Scope.** Managed repos. Through `/v2` the manifest decides (its digest); through a frontend
  the file layers do, whatever `org.opencontainers.image.created` the new build carries
  (manifest-profile.md §4.2). A raw `PUT` of bytes an older version holds does not make it the
  newest (http-routing.md, raw).
- **Assumes.** Any.
- **Mechanism.** `ingest.Pipeline.repeat` and `sameFiles` before the blobs are pushed, again under
  the claims in `claimTag`; `confirm` reports `Existed`. An upload whose tag a fresh claim of
  another digest holds, or the backend holds with a manifest the index lacks (`checkTag`), takes
  that manifest over when the registry has it with the same name and files
  (`ingest.Pipeline.adopt`), and confirms it: the retry of a publish answered `503` by the
  visibility barrier (INV-4), or whose confirm failed after its push (a database that did not
  answer, #641; a client gone, #709), a publish of the same files still on its way, or one that
  stopped after its push and whose claim went stale.
- **Tests.** `TestUploadRepeat`, `TestUploadRepeatRace`, `TestIngest`, `TestSessionRepeat`,
  `TestVisibilityRetryRebuilt`, `TestUploadRetryUnconfirmed`, `TestSessionCommitUnavailable`,
  `TestIndexUnavailableAtConfirm` (raw), `TestUploadIndexUnavailableAtConfirm` (pypi); R-02 in
  `raw.sh`, the repeats of `pypi.sh`, `apt.sh`, `maven.sh` and `oras.sh` (acceptance.md E-01).
- **When violated.** Not observed.
- **Status.** `holds`.

### INV-9. A version is published whole, or not at all

- **Statement.** A version appears in the index with all its files or not at all, and a publish
  that is refused (`4xx`) pushes no manifest.
- **Scope.** Managed repos. A maven version is one commit of its publication session; npm, pypi,
  apt and raw one request; `/v2` one manifest. Blobs uploaded before a refusal stay in the
  registry for its garbage collector.
- **Assumes.** Any.
- **Mechanism.** One manifest per (package, version, variant) (ADR 0005); validation, the profile
  and the claims run before the manifest push (`ingest.Pipeline.validate`, `claimTag`);
  `ingest.Pipeline.Commit` publishes a session's files at once, and an expired session publishes
  nothing.
- **Tests.** `TestIngestInvalid`, `TestIngestSpelling`, `TestManifestLayersBound`,
  `TestSessionCommit`, `TestSessionCommitOnce`, `TestSessionExpiry`, `TestDeployInterrupted`,
  `TestDeployExpiry`; `maven.sh` (M-01: one manifest with four file layers).
- **When violated.** Not observed.
- **Status.** `holds`.

### INV-10. A case variant never squats another spelling

- **Statement.** Names that differ only in ASCII case share one package path, the first spelling
  published holds it, and a publish of another spelling is `409` naming the first, refused before
  its manifest or its files are stored.
- **Scope.** Managed repos and every write path; raw paths, maven coordinates, and the names of
  every format whose package path folds case.
- **Assumes.** Database: as INV-6.
- **Mechanism.** `ingest.Pipeline.claimPath` (the claim on the package path with the digest of
  the spelling) and `ingest.packageOf` under the claims; earlier, before a file is stored,
  `raw.checkSpelling` and maven's `deployer.checkSpelling`.
- **Tests.** `TestIngestSpelling`, `TestIngestSpellingRace`, `TestIngestPathClaim`,
  `TestManifestHookSpelling`, `TestManifestHookSpellingMaven`, `TestDeploySpelling`,
  `TestFirstSpellingWins`, `TestIngestSpellings` (pypi); R-09 in `raw.sh`.
- **When violated.** Not observed. Threat model F-5.
- **Status.** `holds`, with INV-6's note on rolling upgrades.

### INV-11. A blob is stored only under its own digest

- **Statement.** No blob is completed in the registry through Lajstrom under a digest that its
  bytes do not have, whether the registry checks digests or not.
- **Scope.** `/v2` uploads (monolithic, chunked, a `POST ?digest=`), across replicas; the blobs
  ingest pushes for a frontend upload, a proxy fetch and a metadata layer. Not a mount and not a
  blob the registry already has: nothing streams, and they rely on the registry (**digest check**,
  **scoped blobs**). What Lajstrom streams to a client is hashed on the way out (INV-40).
- **Assumes.** Registry: none for what streams through Lajstrom. Database: **one database** for a
  chunked upload across replicas.
- **Mechanism.** `facade.verifier` hashes a `/v2` upload as it streams and holds its last byte
  back until the hash is known; between chunks the sha256 state is in the session row
  (`uploadState`), and `facade.closingHash` asks the backend's offset after a failed chunk.
  Ingest's own pushes and reads go through `ingest.verifier` (`target.pushBlob`,
  `publish.readMetadata`, `publish.metadataUnreadable`, a whole read of `fetchedBlob`).
- **Tests.** `TestUploadVerifiesDigest`, `TestUploadVerifiedAcrossReplicas`, `TestUploadBrokenBody`,
  `TestVerifierShortReads`, `TestUploadDigestInvalid`, `TestIngestFetcher`, `TestFetchedBlob`,
  `TestUploadErrors` (ingest).
- **When violated.** The upload is `400 DIGEST_INVALID` and its session ends; ingest's is
  `ErrInvalid`. A range read of a plugin's `Validate` (`fetchedBlob.ReadAt`: a zip's directory,
  a `.deb`'s control member) cannot be hashed and trusts the registry.
- **Status.** `holds` ([#368](https://github.com/lajstrom/lajstrom/issues/368)); the residue of
  threat model P-9 (two requests of one session at once, on a backend that checks neither
  offsets nor digests) is accepted.

### INV-12. A managed repo holds only manifests of the profile

- **Statement.** Every manifest that Lajstrom indexes in a managed repo satisfies
  manifest-profile.md (§5's checks and the plugin's `Validate`), and only the proxy fetcher
  writes the `lajstrom.origin.*` annotations.
- **Scope.** Managed repos, every write path, and reindex for what it finds in the registry
  (a manifest that fails is an error of the report, never indexed). Referrers are passed through
  as they came (§6), and foreign manifests are listed, never indexed.
- **Assumes.** Registry: **scoped blobs** for the layer checks. Database: any.
- **Mechanism.** `profile.Validate`; `facade.handler.putManaged` validates the profile before it
  asks the registry (`checkLayers`); `ingest.Pipeline.validate` (target, sizes, blobs, the
  plugin's `Validate`), `publish.checkOrigin`; reindex goes through `Inspect`.
- **Tests.** `TestValidate`, `TestValidateReportsAll`, `FuzzValidate`, `TestManifestHookInvalid`,
  `TestIngestInvalid`, `TestInspectInvalid`, `TestOriginRefused`, `TestForeignAndReferrers`, and
  each format's `TestBuildValidate`, `TestValidateRefuses`, `TestBuildRefuses`.
- **When violated.** `400 MANIFEST_INVALID` naming the rule (http-routing.md §2), or the
  format's `400`.
- **Status.** `holds`.

## Delete

### INV-13. Each version is deleted from the index, the caches and the audit log at once

- **Statement.** A delete removes a version's manifest and its referrers from the registry first,
  then, in one transaction, the version's rows (its package's when it was the last), the repo's
  rendered documents, and writes the audit row; a backend that cannot delete changes nothing. A
  delete of several versions is not all-or-nothing: they are deleted one by one, those deleted
  before a failure stay deleted, and the answer names them.
- **Scope.** `DELETE` on `/v2`, the raw frontend, `/api/v1` versions and their batch, npm's
  unpublish. Several versions in one request: a raw path, an npm package, a version with every
  variant (http-routing.md §4); a batch answers what became of each of its versions.
- **Assumes.** Registry: **read-after-write** (**unchecked**): a reconcile that lists a just-deleted
  tag adds its rows back until the next run. Database: any.
- **Mechanism.** `ingest.Pipeline.Delete`: `deletion.find`, then under the tag lock
  (`deletion.locked`) `deletion.begin` (the other tags counted, the audit row written pending),
  then `deletion.finish` on a context detached from the request and bounded by
  `ingest.claimTTL`, so that a client that goes away does not stop it half-way (#710):
  `deletion.deleteInBackend` (the first request a plain delete so that a refusal changes
  nothing) and `deletion.record` (the rows by the manifest's digest, `InvalidateIndexCache` and
  the audit row completed in one `Store.Tx`, before the lock is released).
- **Tests.** `TestDeleteVersion`, `TestDeleteRefused`, `TestDeleteNotFound`, `TestDeleteRowsFail`,
  `TestDeleteClientGone`, `TestDeleteReferrerClientGone`, `TestIntegrationDeleteReferrerClientGone`,
  `TestDeleteThenReindex`, `TestDeleteKeepsPackage`, `TestDeleteReferrer`, `TestDeleteManifests`,
  `TestDeleteTagged`, `TestTagPushAndDelete`, `TestDeleteInvalidatesIndexes` (deb),
  `TestDeleteGoneAndAuditWriter` (raw), `TestDeletePartial` (raw), `TestDeleteVersionPartial`,
  `TestUnpublishPartial` (npm), `TestPartial`, `TestIntegrationDelete`,
  `TestIntegrationDeleteUnsupported`, `TestIntegrationDeleteRefused`; R-07 in `raw.sh`, the deletes
  of `oras.sh` and the unpublishes of `npm.sh`.
- **When violated.** No transaction spans the registry: when the transaction fails after the
  registry deleted the manifest, the client gets `500` saying so, the rows and the cached
  documents stay until reindex removes them, and the delete's audit row is `failed` with the
  error (INV-14).
- **Status.** `holds`: the answer to a delete of several versions that fails names those deleted
  before the failure, a `5xx` too (`format.PartialError`, #476).

### INV-14. Every write has an audit row

- **Statement.** Every write through Lajstrom (a push, a delete, a journal entry, a proxied
  ingest) and every change of the control plane has one audit row, with the principal, the
  action, the repo, the subject, the digest, the client address and the request id. A change of
  the index or of the control plane writes its row in its transaction. A write to the registry,
  which no transaction spans, has its row before the write, and the row says how the write
  ended: `ok`, `failed` with the error, or `unknown` when its writer stopped in between.
- **Scope.** As rbac.md §5 and §7; reads and the bearer tokens of `/v2/token` are not audited. A
  reindex writes one row when it starts (`repo.reindex`), the reconcile loop one row per repo it
  changed (`reconcile`), not one per version. A write refused before the registry is asked to
  change anything (a denial, an invalid upload, a conflict found in the index or by a read of
  the registry) has no row; one the registry refuses or fails has a `failed` row.
- **Assumes.** Database: any. The wiring of `serve`, which sets no `audit.Writer` (a test seam
  that writes after the commit and only logs a failure). A write takes less than
  `ingest.claimTTL` from its row to its index transaction; one that takes longer is `unknown`
  for a while, then completes its row all the same.
- **Mechanism.** Before the registry write, after the reads that may refuse it, a row
  `pending` in a transaction of its own (`ingest.Pipeline.intend`,
  `facade.handler.intendPassThrough`); when it cannot be written, the registry is not asked.
  After the write the row is `ok` in the transaction that records it in the index
  (`ingest.Pipeline.writeRows` (publish), `deletion.record`, `journal.record`,
  `facade.handler.recordPassThrough` for an `oci` repo), in one of its own for a referrer
  (`ingest.Pipeline.auditReferrer`); or `failed` with the error, in one of its own
  (`ingest.Pipeline.fail`), when the write or that transaction failed. A row still `pending`
  after `ingest.claimTTL` is `unknown` (`index.Writer.ExpirePendingAudit`, at the start of every
  reindex run, the reconcile loop's included). The `auth.Users`, `auth.Tokens` and
  `auth.Bindings` changes and the `api` repo changes append theirs `ok` in their transaction;
  the address and the request id through `ingest.withOrigin`. `GET /api/v1/audit` answers the
  state and selects by it (index-schema.md, The audit row of a registry write).
- **Tests.** `TestAuditIntent` (SQLite) and `TestIntegrationAuditIntent` (Postgres): a publish
  killed between the registry and the index, or whose database goes away then, leaves a
  `pending` row that a reindex after `ingest.claimTTL` marks `unknown`; an index transaction
  that fails after a publish or a delete (#474) leaves a `failed` row with the error; a delete
  whose database goes away after the registry delete leaves `pending`, then `unknown`; the happy
  path one `ok` row each. The store conformance case `AuditState`; `TestIngestPushFails`,
  `TestIngestConcurrent`, `TestDeleteRowsFail`, `TestDeleteRefused`, `TestIngestAuditWriter`,
  `TestPushAuditOrigin`, `TestOCIPushAudit`, `TestOCIPushAuditAlone`, `TestRequestIDInAudit`,
  `TestClientAddrInAudit`, `TestPrincipalInAudit`, `TestTokensAPIAudit`, `TestTokensAuditWriter`,
  `TestTokensCreatedBy`, `TestUsersAuditWriter`, `TestDeleteGoneAndAuditWriter`, `TestAuditAPI`,
  `TestReconcile`; the audit rows checked by `oras.sh` (#245) and `npm.sh` (`proxy.fetch`).
- **When violated.** A row that cannot be completed is logged at error and stays `pending`
  until it is `unknown`: a write whose outcome only the registry knows, which reindex brings to
  the index. Writes straight to the registry have no row by construction; the reconcile row
  counts what they changed in the index.
- **Status.** `holds`. **Untested:** acceptance.md S-09 (one row per write of the whole suite)
  is not automated.

## Access

### INV-15. A denial is decided before the input is read

- **Statement.** A principal that may not do a request gets the denial of rbac.md §3 whatever the
  input: before the body is read, before a digest, a tag or a name is parsed, before the repo is
  looked up where the rule says so.
- **Scope.** `/v2` (with a bearer and without), the management API (creating a repo, creating a
  token, the system resources), the frontends.
- **Assumes.** Any.
- **Mechanism.** `facade.handler.authorize` first in every handler, `facade.namedByToken` before
  `resolve`; `api` checks the admin role and `auth.MayCreatePAT` before `decodeBody`; the
  frontends call `format.Authorize` at the start of each handler (`raw.handlers.start`,
  `pypi.handlers.start`, npm's `publish`, maven's deployer, deb's upload).
- **Tests.** `TestDenialBeforeInput`, `TestBearerExistence`, `TestTokensAPINoMint`,
  `TestAuthorization` (facade, api, raw), `TestUploadAuthorize` (deb), `TestPublishDenied` (npm),
  `TestResolveAuthorize` (maven); `rbac.sh`.
- **When violated.** A denial that depends on the input tells whether a repo exists (threat model
  R-2, fixed in #260).
- **Status.** `holds`. **Untested:** on the frontends, no test sends a malformed body or path to
  a repo the principal may not read.

### INV-16. A denial does not tell whether a repo exists

- **Statement.** `anonymous` gets `401` on a repo it may not read and on a missing one alike; an
  authenticated principal without `read` gets `404`, the answer for a missing repo; `403` only
  to a principal that can read the repo; lists name only readable repos.
- **Scope.** Every route of rbac.md §3: frontends, `/v2` (with and without a bearer, `_catalog`,
  the token endpoint's scopes), `/api/v1`. Not the time an answer takes ([Not
  guaranteed](#not-guaranteed)).
- **Assumes.** Any.
- **Mechanism.** `format.Authorize` (the credential's limit, then the bindings; `404` when the
  credential refuses and the user may not read), the repo middleware, `facade.repoUnknown`,
  `facade.unknownScopes`, `facade.handler.catalog`, `token.Reduce`.
- **Tests.** `TestAuthorizeRBAC`, `TestAuthorizeScopeOfAUser`,
  `TestRepoMiddlewareMissingRepoByPrincipal`, `TestBearerExistence`, `TestBearerCatalog`, `TestCatalog`, `TestListFiltered`, `TestAuthorization`
  (facade, raw, api), `TestReferrersAccess`; `rbac.sh` (S-02, S-08, X-05).
- **When violated.** Threat model R-1 to R-4.
- **Status.** `holds`.

### INV-17. An authorization change counts from the next request on every replica

- **Statement.** A binding created or deleted, a repo created or deleted, a repo's
  `anonymousRead` changed, a user disabled or a token revoked counts from the next request on
  every replica; a bearer token already issued keeps its access until it expires (20 minutes),
  except on a deleted repo, which is unknown at once.
- **Scope.** Frontends and `/api/v1` at once; `/v2` at the client's next `/v2/token`.
- **Assumes.** Database: **one database**. When `auth_version` cannot be read, the request is an
  error, never a decision.
- **Mechanism.** Every such change moves `auth_version` in its transaction; `auth.RBAC.current`
  reads it before an evaluation and reloads the snapshot when it moved (`RBAC.fresh`,
  `RBAC.load`); tokens are looked up on every request (`auth.Authenticator`).
- **Tests.** `TestRBACNextRequest`, `TestRBACIndexDown`, the store conformance case `AuthVersion`,
  `TestTokensRevoke`; `rbac.sh` (S-07), the control-plane step of `ha.sh`, S-05 in `tokens.sh`.
- **When violated.** Not observed.
- **Status.** `holds`.

### INV-18. A credential never grants more than it says

- **Statement.** A deploy token acts on its one repo with its `read` and `write` scopes and never
  `delete` or `manage`; a personal access token's scopes only narrow its user's rights; a token
  never creates a personal access token; a bearer token never carries an action its credential
  lacks.
- **Scope.** Every protocol, the bearer of `/v2` included.
- **Assumes.** Any.
- **Mechanism.** `auth.Credential.Limit`, `token.ParseScopes`, `token.Reduce` (at most 100
  repositories a request), `auth.MayCreatePAT` (asked by `api.createToken` and again by
  `auth.Tokens.CreatePAT`).
- **Tests.** `TestCredentialLimit`, `TestReduce`, `TestReduceCredential`, `TestParseScopes`,
  `TestTokenScopesBound`, `TestTokensAPINoMint`, `TestTokensAPIAuthorization`,
  `TestTokensCreatedBy`, `TestAuthorizeCredential`; `tokens.sh`, `rbac.sh` (S-04), acceptance.md
  X-14.
- **When violated.** Threat model T-1, T-6, T-8, T-9.
- **Status.** `holds`.

### INV-19. A bearer token verifies on every replica, and only under a live key

- **Statement.** A token issued by one replica verifies on every replica while its key is active
  or retired within the token lifetime plus 60 seconds of skew; it verifies nowhere once its key
  is dropped, within a second; the verifier accepts only the algorithm of the key the `kid`
  names.
- **Scope.** `/v2/token` and the bearer of `/v2`, with `server.externalURL` set, which the
  configuration requires but for the dev stack (`server.insecureHTTP`), where the audience is
  the request's `Host` (threat model T-2, [#248](https://github.com/lajstrom/lajstrom/issues/248)).
- **Assumes.** Database: **one database** with one master key on every replica (without a master
  key each replica signs with an ephemeral key of its own, and says so at start); **clocks
  agree** (checked: a replica more than 60 s off the database's clock is not ready).
- **Mechanism.** ADR 0010: the key is a `keys` row of kind `jwt` under the master key;
  `keys.Tokens.TokenVerifiers`; `token.Verifier.Verify` (`iss`, `aud`, `exp`, `nbf`, `iat`, the
  `kid`, the algorithm pinned per key, a reload at most once a second for an unknown `kid`).
- **Tests.** `TestTokenAcrossReplicas`, `TestIntegrationTokenAcrossReplicas`, `TestVerifierReload`,
  `TestVerifierFlood`, `TestVerifyRefusals`, `TestVerifyTimes`, `TestRotationGrace`,
  `TestRotateToken`, `TestDropToken`, `TestTokenUndecryptable`, `TestEphemeralTokens`,
  `TestExternalURL`; `oci-token.sh`, `ha.sh` H-12.
- **When violated.** `401` and a new token from the client's credentials.
- **Status.** `holds`.

### INV-20. Credentials go only where they belong

- **Statement.** The registry's credentials are sent only to the registry's origin and to a token
  realm it is allowed to name, never over a downgrade to `http`; an upstream's credentials only
  to the upstream's origin; a client's credentials never reach a registry or an upstream.
- **Scope.** The generic provider (manifests, blobs and their redirects, upload sessions, the
  bearer challenge) and the proxy's upstream client.
- **Assumes.** The registries of the matrix: the realm and redirect checks were not probed on
  ECR, GHCR, Harbor and the rest (threat model §6). oras-go's `auth.Client` drops `Authorization`
  on a cross-origin redirect (**unchecked** by a test of ours for that client).
- **Mechanism.** `generic.Provider.location`, `generic.Provider.blobRequest`,
  `generic.realmTransport` with `refuseRealm` and `realmHosts` (`registries[].tokenRealmHosts`);
  `proxy.Upstream.authorize`, `checkRedirect`, `checkTarget`; the fetcher's requests are its own
  (`User-Agent: lajstrom/<version>`), and a forwarded request carries only what the plugin names
  (`Proxy.Forward`).
- **Tests.** `TestBlobRedirectDowngrade`, `TestBlobRedirectLeavesOrigin`, `TestTokenRealm`,
  `TestRefuseRealm`, `TestUploadDowngrade`, `TestSameOrigin`, `TestCredentials`, `TestRedirects`,
  `TestForward`, `TestProxyRequestHost`, `TestLoadNestedPrefixes`.
- **When violated.** Threat model P-2, P-3, P-4.
- **Status.** `holds`.

### INV-21. No secret is logged

- **Statement.** No password, token secret, bearer token, `Authorization` or `Cookie` value,
  master key, registry or upstream credential, database DSN or presigned URL appears in a log
  line, an error message or a metric label.
- **Scope.** Every log of `serve` and the CLI, at every level.
- **Assumes.** Any.
- **Mechanism.** `server.Server.accessLog` (method, path, status, principal, addresses; never the
  query, a header or a body); the redacting `String` of `token.Token`, `keys.TokenKey`,
  `config.RegistryAuth`; the provider's `logTransport` and `withoutURL`; `postgres.Open`; the
  proxy logs no body and no redirect target; `auth.Authenticator.refuse` and `refuseToken` log
  a user by its uid, and neither they nor `server.Server.authenticate` log a user name as typed;
  `token.Verifier.refuse` logs the first 16 bytes of the `kid` and its length (threat model L-1
  to L-4).
- **Tests.** `TestTokensNeverLogged`, `TestRedactSecrets`, `TestTokenRedacted`,
  `TestTokenKeyRedacted`, `TestEntityRedacted`, `TestMasterRedacted`, `TestOpenDoesNotLeakTheDSN`,
  `TestAccessLog`, `TestBearerAccessLog`, `TestRetryLog`, `TestRefusalsLogNoSecret` (a token
  pasted as the user name, a password typed there and a 4 KiB `kid`, at `debug`),
  `TestAuthenticateFailure`.
- **When violated.** Not detected by Lajstrom.
- **Status.** `holds`. **Untested:** acceptance.md C-07 (grep the logs of both replicas for every
  secret the suite used) is not automated.

### INV-22. The client address comes only from a trusted proxy

- **Statement.** `X-Forwarded-For` is read only when the peer is in `server.trustedProxies`,
  right to left, the first untrusted hop being the client; the result is the address of the
  access log, of `audit.ip` and of the per-address throttles, and no other code reads a
  forwarded header.
- **Scope.** Every request. `audit.ip` is exactly as reliable as the chain in front (ADR 0011).
- **Assumes.** Any.
- **Mechanism.** `server.resolveClient`, `server.ClientAddr`.
- **Tests.** `TestResolveClient`, `TestClientAddress`, `TestClientAddrInAudit`,
  `TestAddressThrottle`; the client-address step of `ha.sh`.
- **When violated.** Threat model N-1.
- **Status.** `holds`.

## Bounds

### INV-23. Every request is bounded in size

- **Statement.** No request makes a replica hold more than the documented bounds: a manifest of
  `server.maxManifestSize` (4 MiB) with at most 1 000 layers, a management body of 64 KiB, 100
  repositories a token request, a frontend file or a `/v2` blob of the repo's
  `policy.maxFileSize` (by default its format's, npm 512 MiB to `oci` 16 GiB) or the registry's
  `capabilities.maxBlobSize` when smaller, the npm bounds of http-routing.md (a publish body
  besides its tarball 4 MiB, an audit body 16 MiB), an upstream document of
  `upstreams[].maxMetadataSize`, `server.maxConcurrentUploads` uploads at once (32), a body that
  stops moving for `server.stallTimeout`. A principal holds at most `server.uploadSessions` live
  `/v2` upload sessions on a replica (256 in a repo, 1024 in all), and a Maven deploy stores at
  most the repo's `policy.maven.maxSessionFiles` files (256) (#246).
- **Scope.** Every route.
- **Assumes.** Any.
- **Mechanism.** `facade.readManifest`, `api` body limits, `token.Reduce`, `format.MaxFileSize`
  and `format.LimitBody`, `facade.limitBlob` and `facade.blobLimit`, `facade.liveUploads`,
  `ingest`'s `sessionState.room`, `server.Server.limitUploads`, `server.Server.stall`, npm's
  `readBody`, `proxy.readDocument`.
- **Tests.** `TestManifestHookSize`, `TestManifestLayersBound`, `TestOversizedBody`,
  `TestTokenScopesBound`, `TestIngestMaxFileSize`, `TestPutMaxFileSize`, `TestUploadMaxFileSize`
  (deb, facade), `TestManifestHookMaxFileSize`, `TestMaxFileSize`, `TestLimitBody`,
  `TestReadBodyBounded`, `TestDocumentLimits`, `TestForwardBounds`, `TestMaxUploads`,
  `TestStallRequestBody`, `TestStallResponse`, `TestUploadMaxBlobSize`,
  `TestUploadDefaultMaxFileSize`, `TestUploadLiveSessions`, `TestLiveUploads`,
  `TestDeploySessionFiles`, `TestDeploySessionFilesCounted`, `TestMaxConcurrentUploads`.
- **When violated.** `413`, `400` naming the bound, `429` with `Retry-After` naming the bound and
  its key, or `503` with `Retry-After`.
- **Status.** `partly`: the bytes of `$TMPDIR` a replica may hold have no budget
  ([#427](https://github.com/lajstrom/lajstrom/issues/427)); the bounds of #246 are per replica
  and have their defaults (threat model U-3 to U-6).

### INV-24. A parser allocates in proportion to its input, within a fixed cap

- **Statement.** No package a client uploads, and no document an upstream serves, makes a parser
  allocate or work beyond the caps of threat model F-4: archive bombs, entry floods, entity
  expansion and oversized metadata are refused within seconds.
- **Scope.** The parsers of deb, pypi, maven and npm, the profile's `Validate`, the proxy's
  documents.
- **Assumes.** Any.
- **Mechanism.** deb `capReader` and `xzReader.startBlock`, `pypi.checkZipEntries`,
  `pypi.readTarGzFile`, `npm.ReadTarball`, the size caps of POM, `maven-metadata.xml`, wheel
  `METADATA` and checksum files; `encoding/xml` expands no entities.
- **Tests.** `TestXZBlocks`, `TestSdistBombs`, `TestZipBombs`, `TestTarballBombs`,
  `TestParsePOMExpansionBounded`, `TestParseControlLimit`, `TestMetadataTooLarge`, and the fuzz
  targets of testing.md (`FuzzZipEntries`, `FuzzPackageJSON`, `FuzzParse`, `FuzzPOM`, …), which
  check that allocation grows with the input (`testutil.CheckAllocated`).
- **When violated.** `400` or `413` naming the bound.
- **Status.** `holds`.

### INV-25. A replica's memory does not grow with the size of a file

- **Statement.** Uploads and downloads stream; only bounded metadata is read into memory, so a
  replica's heap does not grow with the size of a blob or the number of bytes in flight.
- **Scope.** Every download (`format.ServeBlob`), `/v2` uploads, frontend uploads (raw, apt, maven, pypi and npm hold one
  chunk of 8 MiB, apt the head of a `.deb` too, pypi the end of a zip, npm the `package.json` of its tarball), the proxy fetch.
- **Assumes.** Any.
- **Mechanism.** `format.ServeBlob` through a fixed buffer, `facade` request bodies passed to the
  provider's session, the provider's `BlobWriter` (one chunk), `format.Spool`; the bounded reads
  of operations.md, Performance.
- **Tests.** `TestStreamingMemory` (1 GiB up and down through `/v2`, heap under 64 MiB),
  `TestPutStreams1GiB` (1 GiB through a raw `PUT`, heap under 64 MiB), `TestUploadStreams1GiB` (deb, the
  same through an apt upload), `TestDeployStreams1GiB` (maven, a jar of 1 GiB), `TestUploadWheelStreams` and
  `TestUploadSdistStreams` (pypi, 256 MiB), `TestPublishStreams` (npm, 160 MiB),
  `TestUploadDoesNotHoldTheFile`, `TestStream1GiB`; the load test (`make load`, acceptance.md
  N-01), nightly and not required.
- **When violated.** Not observed.
- **Status.** `holds`.

## Replicas and caches

### INV-26. Replicas coordinate through the database

- **Statement.** Any replica continues what another began (a `/v2` upload session, a maven
  publication session), and the claims and locks of index-schema.md exclude across replicas: one
  publish per tag (INV-6), one reindex run per scope, one proxy fetch per artifact, one referrers
  index or journal append at a time per subject or package, one open session per key, one
  active key per kind and owner.
- **Scope.** Every replica of one deployment.
- **Assumes.** Database: **one database**, **Postgres for replicas** (checked when `serve`
  starts: H-14), **clocks agree** (checked: by `serve` at its start and every 30 seconds, by
  `reindex` and `admin` before they run), and the
  transaction of `Store.WithReferrersLock` lasts until its function returns (**unchecked**; as
  in INV-35, a lock lost earlier is `index.ErrUnavailable`, not a success).
- **Mechanism.** `tag_claims` (`Writer.ClaimTag`), `reindex_runs` (`Writer.StartReindexRun` under
  `pg_advisory_xact_lock` or `BEGIN IMMEDIATE`), `fetch_claims` (`Writer.ClaimFetch`, a heartbeat
  every 30 s, stale after 2 minutes), `Store.WithReferrersLock` (the referrers, tag and journal
  locks), the unique indexes `sessions_open_key` and `keys_active`, the `sessions` rows of
  uploads (`facade` `uploadState`); `replicas` (`Writer.StartReplica`, which on SQLite refuses a
  second live replica, and the heartbeat of `serve`, which compares the clocks); on SQLite a
  start deletes a refusing row first when its process was of the same host, boot and PID
  namespace and the kernel says it is gone (`processGone`: no process has its PID, or one that
  started at another time; #720); the CLI's `checkClock`, before `reindex` and `admin` open the index (#563).
- **Tests.** The store conformance cases `TagClaimRace`, `ReindexRunRace`, `FetchClaimRace`,
  `KeyRace`, `SessionsConcurrent`, `JournalRace`, `PackageNameOCIRace`, `PackageRace`,
  `ReferrersLock`, `Replicas`; `TestFileReplicas`, `TestReplicasCoexist`,
  `TestServeTwoReplicasOnSQLite`, `TestServeAfterCrash`, `TestServeHangup`, `TestServeKilled`,
  `TestProcessGone`, `TestReplicaBeat`, `TestReplicaBeatGone`, `TestReplicaClock`,
  `TestCheckClock`, `TestOpenMigratedClock`, the conformance case `DatabaseTime`;
  `TestUploadAcrossReplicas`, `TestMonolithicUploadAbandoned`,
  `TestIntegrationUploadAcrossReplicas`, `TestSessionConcurrent`, `TestSessionCommitOnce`, `TestKeyForRace`, `TestTokenSignerRace`,
  `TestRuns`, `TestTakenOver`, `TestStaleFetchClaims`, `TestFileTwoReplicas`, `TestFileClaims`,
  `TestIntegrationReferrersConcurrent`, `TestIntegrationJournalReplicas`, `TestBootstrapReplicas`;
  `ha.sh` H-02, H-03, H-04, H-05, H-06, H-07, H-08, H-09, H-11, H-12, H-13 and the step of #28.
- **When violated.** A replica whose clock is more than 60 s off the database's is not ready,
  so the edge sends it no new requests, but its work in flight and its reconcile loop go on and
  can take over a live claim; the log and `lajstrom_clock_skew_seconds` say so from 10 s on.
  `reindex` and `admin` on a host more than 60 s off refuse to run, naming the skew; a clock
  that moves away during a long run is not compared again until the next command. A second replica on one
  SQLite file is refused when it starts, unless the kernel says its process is gone; a row the
  kernel cannot say that of (another host, a container restarted in a new PID namespace) refuses
  starts until it is stale, 2 minutes after its last beat. A replica that stood still for 2
  minutes while another started is not ready from its next heartbeat on, and both run until the
  operator stops one.
- **Status.** `holds`.

### INV-27. A cached document is invalidated by every change it depends on

- **Statement.** After a publish, a delete, a journal append, a reindex, a key rotation or a
  write of an upstream's document commits, no replica serves a rendered document of the repo
  that predates it. Once the grace of a retired apt key ends, no replica serves a signature
  that key made.
- **Scope.** `index_cache` (apt, pypi, maven, raw listings, npm packuments, a proxy's documents);
  the snapshot of the bindings (INV-17) and the cache of token keys (INV-19) have their own entries.
  A group caches nothing.
- **Assumes.** Database: any. For the end of the grace, **clocks agree**.
- **Mechanism.** Each change invalidates in its own transaction (`Writer.InvalidateIndexCache` in
  `ingest.Pipeline.writeRows`, `deletion.deleteRows`, `journal.record`, `reindex.Worker.repo`, the
  `proxy_meta` writes), and every invalidation first increments the repo's generation
  (`index_cache_generations`; index-schema.md, The generation of a repo's documents).
  `format.CachedDocument`, `proxy.Core.render` and the apt signatures
  (`deb.handlers.storeSigned`) read the generation before they render, and
  `Writer.PutIndexCache` stores a render only while it is current, under the lock of the
  generation row: a render that an invalidation overtook on Postgres (read committed) is
  answered and not stored. The end of a grace is a time, not a write, so the apt routes check a
  cached `InRelease` or `Release.gpg` on every read (`keys.Service.Current`): when a key that
  signed it no longer signs, they sign it again (`deb.handlers.signed`, #577).
- **Tests.** The store conformance cases `IndexCache` and `IndexCacheRace` (an invalidation
  committed during a render, and one not committed when the render stores);
  `TestCachedDocument`, `TestCachedDocumentErrors`, `TestRoutesCache`,
  `TestRoutesNewRender`, `TestRoutesSignedAfterPublish`, `TestRoutesGraceEnds`, `TestCurrent`,
  `TestPackumentCache`, `TestDeleteInvalidatesIndexes`, `TestIndexCache` (formattest),
  `TestDocumentCache`; `ha.sh` H-13, H-11 (A-09: a `.deb` uploaded through one replica, listed by
  `apt-get update` through the other within a second).
- **When violated.** Not observed. A replica of a release before migration 0017 neither
  increments nor checks the generation, so while one serves during a rolling update the window
  is open on Postgres: a stale document stays until the next invalidation of the repo.
- **Status.** `holds`.

### INV-28. A migration is applied exactly once

- **Statement.** Each schema migration is applied once, in a transaction that records it, also
  when several replicas and the CLI start at once.
- **Scope.** `lajstrom migrate` and the start of `serve`, both engines. Which binary runs on which
  schema during an upgrade is INV-41.
- **Assumes.** Database: **one database**.
- **Mechanism.** `index.ApplyMigrations` under the migration lock (`pg_advisory_xact_lock`,
  `BEGIN IMMEDIATE`).
- **Tests.** `TestConcurrentMigrate`, `TestFileMigrate`, `TestMigrationVersions`,
  `TestMigrationVersion`, `TestMigrate`.
- **When violated.** Not observed.
- **Status.** `holds`.

## npm

### INV-29. The journal has one order, and its replay is the state

- **Statement.** A package's dist-tag and deprecation changes are journal entries numbered from 1
  without gaps in the order of the package's journal lock, never rewritten, and the state the
  packument serves is the fold of the entries in `seq` order, which reindex replays to the same
  state.
- **Scope.** Hosted npm repos. A proxy journals nothing: its dist-tags are the upstream's.
- **Assumes.** Registry: **monotonic reads** (**unchecked**), and a lag shorter than
  `ingest.claimTTL`: an entry a writer claimed and the registry does not serve after the claim
  is stale counts as never pushed. Database: **Postgres for replicas** (on SQLite the journal
  lock is a lock of the process).
- **Mechanism.** `ingest.Pipeline.Journal`: under `Store.WithReferrersLock` (the package's OCI
  repository and the empty blob's digest), `journal.append` reads the last `seq`, `journal.adopt`
  takes the entries the registry has after it and `journal.claimed` the ones an earlier append
  claimed in `tag_claims` that the registry does not serve yet (read back, else `503` until the
  claim is stale), `journal.push` claims and pushes each entry, the visibility barrier reads them
  back (INV-4), `journal.record` (rows, the folded state, the invalidation, the audit rows and the
  claims released in one transaction); `profile.JournalState.Apply` folds; `reindex.replay`
  replays and reports what breaks the rules.
- **Tests.** `TestJournal`, `TestJournalConcurrent`, `TestJournalAdopts`, `TestJournalRefused`,
  `TestJournalDelete`, `TestIntegrationJournalReplicas`, the store conformance cases `Journal`,
  `JournalState`, `JournalCascades`, `JournalRace`, `TestJournalState` (profile),
  `TestJournalReindex`, `TestJournalReindexLost`, `TestJournalReindexOrphans`,
  `TestJournalVisibility`, `TestJournalClaimNotPushed`, `TestDistTags`,
  `TestDeprecate`, `TestDeleteJournalEntry`; the dist-tag and deprecate steps of `npm.sh`.
- **When violated.** Reindex reports an entry whose annotations, tag or name break the rules, a
  missing `seq`, and orphans (`orphanJournal`); an entry that breaks the rules stops the next
  append until the operator removes it.
- **Status.** `holds`. An append whose entries the registry does not serve in time is `503`; its
  client's retry adopts them and appends its ops again, which folds to the same state.

### INV-30. The packument never says other than what the tarball installs

- **Statement.** A version's document in the packument agrees with its tarball's `package.json`,
  after npm's three normalisations, on `name`, `version`, every kind of dependency, `bin` and
  the install scripts; `hasInstallScript` and `_hasShrinkwrap` are read from the tarball.
- **Scope.** Versions published to a hosted npm repo by `npm publish` (and pnpm and yarn, which
  send npm's body) or by `oras push`. Not a proxy's packument, which is the upstream's document.
- **Assumes.** Any.
- **Mechanism.** npm's `checkInstall`, `Tarball.normalise`, `facts.check`, the plugin's `Validate`.
- **Tests.** `TestPublishRefuses`, `TestNormalise`, `TestNormaliseCaptured`, `TestNormaliseBinDir`,
  `TestBuildValidate`, `TestValidateRefuses`, `TestReadTarball`, `FuzzPackageJSON`; the publishes
  of `npm.sh`.
- **When violated.** `400` naming the key. A normalisation that a later npm adds is a `400` for an
  honest package until ADR 0012 adds it.
- **Status.** `holds`.

## Proxy and group repos

### INV-31. A proxy downloads an artifact once, and checks it before it keeps it

- **Statement.** Concurrent misses of one artifact download it once, on one replica or across
  replicas; nothing is pushed unless the strongest checksum the upstream's document gives
  matches the downloaded bytes, and a file without a checksum is refused; one replica asks the
  upstream for one name's document once at a time.
- **Scope.** Proxy repos (npm in M4), directly and through a group.
- **Assumes.** Registry: **monotonic reads** (**unchecked**) for the waiters, which look the
  version up in the index and are then served from the registry: the index has it only once the
  registry served it (INV-4). Database: **Postgres for replicas**, **clocks agree**. The upstream's document and file come over the same channel: the
  checksum proves the transfer, not the upstream's honesty.
- **Mechanism.** `proxy.flights.join` on a replica; `proxy.Core.claim` (`fetch_claims`),
  `Core.heartbeat`; `Core.download` with `strongest`; `Core.reject` writes `proxy.reject`.
- **Tests.** `TestFileOneFetch`, `TestFileTwoReplicas`, `TestFileClaims`, `TestFileChecksum`,
  `TestFileRefused`, `TestFileMiss`, the store conformance case `FetchClaimRace`; `npm.sh` (each
  tarball fetched once, `proxy.fetch` in the audit log).
- **When violated.** A checksum mismatch is `502`, an audit row `proxy.reject` and a warning
  naming both values.
- **Status.** `partly`: files hold; concurrent misses of one document on one replica each ask the
  upstream ([#383](https://github.com/lajstrom/lajstrom/issues/383)).

### INV-32. A proxy reaches only what the operator configured

- **Statement.** The URL and the credentials of an upstream come only from the configuration; a
  URL on another host, from a document or a redirect, is followed only over `https` and only to
  public unicast addresses; a redirect from `https` to `http` is refused.
- **Scope.** Proxy repos and the requests forwarded through them (npm audit).
- **Assumes.** Any; with an egress proxy, Lajstrom refuses an address literal of a private range
  and the egress proxy enforces the rest.
- **Mechanism.** `config` (`upstreams[]`), `proxy.ParseSettings` (a repo names an upstream, never
  a URL), `proxy.Upstream.checkRedirect`, `checkTarget`, `dialFunc` and `public`.
- **Tests.** `TestPublic`, `TestRedirects`, `TestEgressProxy`, `TestLoadUpstreams`,
  `TestLoadUpstreamErrors`, `TestParseSettingsErrors`, `TestPatchUpstreamInvalid`,
  `TestCreateProxyInvalid`, `TestForwardErrors`.
- **When violated.** `502` (http-routing.md §6); threat model S-1.
- **Status.** `holds`.

### INV-33. A proxied artifact is an ordinary artifact

- **Statement.** What a proxy fetched is a manifest of the profile in the registry, with its
  origin in annotations, so it survives the loss of the database and of the upstream, and
  reindex rebuilds it; `proxy_meta` is a cache whose loss costs upstream requests only.
- **Scope.** Proxy repos.
- **Assumes.** As INV-3.
- **Mechanism.** The fetcher publishes through `Publisher.Upload` with an `Origin`
  (`ingest.setOrigin`); reindex neither reads nor writes `proxy_meta`, and `ResetDerived` leaves
  it.
- **Tests.** `TestReindexProxied`, `TestUploadWithOrigin`, `TestFileUpstreamDown`,
  `TestDocumentUpstreamDown`, `TestDocumentCache`; the proxy steps of `npm.sh`.
- **When violated.** As INV-3.
- **Status.** `holds`.

### INV-34. In a group, a name the hosted member holds hides every proxy member's versions

- **Statement.** When a group's hosted member has a package of a name's package path, no proxy
  member's version or file of that name is served through the group and no upstream request is
  made for it, unless the name matches `group.merge`.
- **Scope.** Group repos (proxy-and-group.md §8), of the formats whose plugin has them
  (`format.Grouper`: npm). The rule holds only while the hosted member has the name; `deny` on
  the proxy protects names that are not published yet.
- **Assumes.** Database: any.
- **Mechanism.** `proxy.grouped.hides` (the hosted member's `index.PackageAt` of the name's
  package path, unless `proxy.Merged` matches it), asked before any proxy member by
  `proxy.Core.groupDocument` and `proxy.Core.groupFile` (§8.2).
- **Tests.** `TestGroupDocument`, `TestGroupFile` (`internal/proxy`), `TestGroupDependencyConfusion`
  (npm); J-08 in `npm.sh`.
- **When violated.** A proxy member's version of a hosted name reaches the group's clients: the
  dependency confusion of ADR 0013. Nothing reports it.
- **Status.** `holds`.

## Referrers

### INV-35. Referrers are kept as they came, and none is lost to a race

- **Statement.** A referrer pushed through Lajstrom is stored unchanged and listed by
  `GET /v2/<name>/referrers/<digest>`; two referrers of one subject pushed at once, through one
  replica or two, are both listed, also on a registry without the referrers API.
- **Scope.** Managed and `oci` repos; the tag schema `sha256-<hex>` where the registry has no
  referrers API.
- **Assumes.** Registry: **GC keeps what is named** (**unchecked**): GitLab B deletes an untagged
  referrer 24 hours after its push, and Nexus's task "Delete unused manifests" deletes one.
  Database: **Postgres for replicas** (on SQLite the referrers lock is a lock of the process),
  and the lock's transaction lasts until its push returns (**unchecked**): no timeout that the
  client cannot lift ends it, so PgBouncer's `idle_transaction_timeout` and Postgres's
  `transaction_timeout` are 0 for Lajstrom's users (index-schema.md, Coordination).
- **Mechanism.** `ingest.Pipeline.PutReferrer` (`checkReferrer`, `pushReferrer` under
  `Store.WithReferrersLock` without the API), the provider's tag-schema fallback
  (`Repository.Referrers`). The lock's transaction lifts `idle_in_transaction_session_timeout`
  for itself, and its `COMMIT` is checked: a lock lost while the push ran is
  `index.ErrUnavailable`, `503` with `Retry-After`, never a success (#615). The delete of one
  referrer without the API (`deletion.deleteReferrer`) pushes its subject's index again without
  it right after the index's delete and before the referrer's; when the registry does not take
  that push, the index goes back as it was and the referrer stays (`503` with `Retry-After`).
  An index that lacks referrers the audit log has as pushed and the registry has, or that is
  missing, is pushed with them first (`deletion.subjectIndex`), so the retry of such a delete
  lists them again, also after a delete whose referrer went before its index came back
  (`deletion.findReferrer`; #711).
- **Tests.** `TestPutReferrer`, `TestPutReferrerRefused`, `TestPutReferrerOCI`, `TestReferrers`,
  `TestReferrerPush`, `TestIntegrationReferrers`, `TestIntegrationReferrersConcurrent`,
  `TestRebuildTagSchema`, `TestReferrerIndexDeleteRefused`, `TestReferrersLockLost`,
  `TestPutReferrerLockLost`, `TestDeleteReferrerIndexNotPushed`, `TestDeleteReferrerRepairsIndex`,
  `TestDeleteReferrerGone`, `TestIntegrationDeleteReferrerIndexRepair`; `cosign.sh` (on Zot and on
  distribution), the step of #28 in `ha.sh`.
- **When violated.** A referrer the registry collected is missing from the list; nothing reports
  it. A lock whose transaction ended while its push ran (a pooler's timeout, a terminated
  backend, a failover) is a `503` for that push, whose retry lists its referrer; a referrer
  that another writer pushed meanwhile, with success, can be missing from the list, and nothing
  reports it.
- **Status.** `partly`: no race loses one while the lock's transaction lasts; a lock lost to the
  database can, and a registry's garbage collector can.
  [#326](https://github.com/lajstrom/lajstrom/issues/326) writes the tag schema as well on a
  registry with the API, as insurance.

## Operations

### INV-36. A replica that stops drains first

- **Statement.** On `SIGTERM` or `SIGINT` a replica answers `/-/readyz` with `503`, refuses new
  connections, lets the requests in flight finish within `server.shutdownTimeout`, and what an
  abort leaves behind another replica can handle: an upload session in the index and the
  registry, a publish not yet indexed for the next reconcile, a claim that goes stale.
- **Scope.** `serve`.
- **Assumes.** The orchestrator's grace period is longer than `server.shutdownTimeout`
  (operations.md, Shutdown).
- **Mechanism.** `server.Server.Run`, `server.Server.track`, `waitHandlers`.
- **Tests.** `TestDrainCompletesUpload`, `TestDrainTimeoutAborts`, `TestDrainReadyz`,
  `TestRunGracefulShutdown`, `TestRunShutdownTimeout`; `ha.sh` H-05 (a replica killed, not
  stopped, in an upload: the client's retry through the edge succeeds on the other), H-10.
- **When violated.** The client of an aborted request retries.
- **Status.** `holds`.

### INV-37. Readiness says whether a replica can serve

- **Statement.** `/-/readyz` is `200` only while the index database answers and every configured
  registry answered `GET /v2/` with Lajstrom's credentials within the last minute, and `503`
  while draining; `/-/healthz` is `200` while the process runs.
- **Scope.** `serve`, `lajstrom healthcheck`.
- **Assumes.** Any.
- **Mechanism.** `server.Server.readyz`, the provider's `Pinger` every 20 s, the circuit breaker.
- **Tests.** `TestReady`, `TestReadyFollowsTheRegistries`, `TestSystemEndpoints`, `TestPing`,
  `TestBreakerOpensAndCloses`, `TestHealthcheck`.
- **When violated.** Not observed.
- **Status.** `holds`.

## Decided, not built

### INV-38. A denied version is kept out of the protocol clients' indexes

- **Statement.** A version that a policy rule or verdict denies is left out of every rendered
  index, its download is `403` with the reason, its upload is refused at ingest, and a proxy
  does not fetch it (ADR 0008); OCI pulls are refused only on a repo with `enforcePull`.
- **Scope.** Every frontend, the proxy fetch, `/v2` as ADR 0008 says.
- **Assumes.** As INV-27 for the invalidation of rendered indexes when a rule changes.
- **Mechanism.** None yet: the seams of ADR 0008 (one read path per plugin, `index_cache`
  invalidation, the proxy's step 3).
- **Tests.** None.
- **When violated.** —
- **Status.** `not yet` ([#101](https://github.com/lajstrom/lajstrom/issues/101)).

### INV-39. A repo encrypted at rest is unreadable without Lajstrom's key

- **Statement.** On a repo with `policy.encryption.mode: at-rest`, every blob Lajstrom writes is
  an ocicrypt ciphertext under a key the registry never sees, and is decrypted only on
  Lajstrom's read path (ADR 0009).
- **Scope.** Repos that turn it on.
- **Assumes.** The master key, or the KMS, outlives the artifacts.
- **Mechanism.** None yet: the two write paths and the one read helper that ADR 0009 keeps open.
- **Tests.** None.
- **When violated.** —
- **Status.** `not yet` ([#114](https://github.com/lajstrom/lajstrom/issues/114)).

### INV-40. A download that streams through Lajstrom has its digest

- **Statement.** A blob that Lajstrom streams to a client is hashed as it passes, and its last
  byte is held back until the hash is known: when the bytes do not have the blob's digest, the
  connection is aborted, so that no client receives the whole of a blob whose bytes are not its
  digest. As on upload (INV-11).
- **Scope.** Every download through `format.ServeBlob`: the frontends and the blob `GET` of
  `/v2`; and the manifest `GET` of `/v2`, whose bytes stream from the backend the same way.
  Not a `307` redirect to the backend's storage, whose bytes do not pass through Lajstrom
  ([Not guaranteed](#not-guaranteed)), and not a `206` answer to a `Range` request, which is
  not the whole blob.
- **Assumes.** Any.
- **Mechanism.** `format.blobAnswer.copy` through `format.digestWriter`, and
  `facade.handler.streamManifest` through `facade.verifier`. The abort is a panic of an error
  that wraps `http.ErrAbortHandler`: net/http closes the connection (HTTP/1, without the rest of
  the body or the last chunk) or resets the stream (HTTP/2), and `server.Server.recovery` logs
  the cause. A stream from the registry that breaks halfway is aborted the same way when the
  answer has no `Content-Length` (chunked, or HTTP/2 without `content-length`), since the
  client could not tell its end from the blob's (#568); with `Content-Length`, net/http closes
  the connection and the client has the length to see the short body by.
- **Tests.** `TestServeBlobDigest` (with `Content-Length`, chunked, HTTP/2; a range not
  hashed), `TestServeBlobBrokenStream` (the registry drops the stream: chunked and HTTP/2
  aborted, with `Content-Length` returned), `TestDownloadVerifiesDigest` (a 256 MiB blob and a manifest through the server, the
  heap under 64 MiB), `TestRecoveryAbortCause`, `TestStreamingMemory`.
- **When violated.** Lajstrom notices: the client gets a short body and a broken connection,
  `lajstrom_download_digest_mismatches_total{registry}` counts it, and an `error` line
  `response aborted` names the repository, the digest and the registry. The registry holds the
  only copy (INV-1): what repairs it is the registry's, or a new publish.
- **Status.** `holds`.

## Upgrades

### INV-41. A binary runs only on a schema it can run on

- **Statement.** `serve` and `lajstrom migrate` refuse a schema with a migration they do not know
  that requires a version above the newest they know, and apply no migration that requires a
  version above the schema's newest unless told to (`migrate --allow-breaking`; `serve` on
  SQLite), applying nothing when they refuse; a binary of the release before runs on the schema
  after an expand migration (index-schema.md, Upgrades).
- **Scope.** Binaries from migration 0014 on, both engines.
- **Assumes.** Database: **one database**. A migration that is not expand carries its
  `-- lajstrom:requires` line: review checks it, no code does.
- **Mechanism.** `index.ApplyMigrations`, `schema_migrations.requires`.
- **Tests.** `TestUpgrade`, `TestMigrationRequires`; from v0.1.0 on, the job `upgrade` of the
  Upgrade workflow (`test/upgrade/run.sh`): on every pull request that changes a migration, the
  previous release's end-to-end suite, with its binary, on a Postgres schema that the pull
  request's binary migrated.
- **When violated.** A contract migration without its line: the replicas of the release before
  keep serving and fail on the statements it broke; a rollback starts and fails the same way.
  From v0.1.0 on, the job `upgrade` fails on the pull request that adds it.
- **Status.** `partly` until a release from v0.1.0 on exists, then it holds: until then the job
  `upgrade` has no release to run and is skipped, whether a migration is expand is reviewed and
  not tested, and the binary of the previous release on the schema of the next one is
  **untested** ([#485](https://github.com/lajstrom/lajstrom/issues/485)).

## Retention

### INV-42. A conditional delete never removes a version that changed after the client read it

- **Statement.** A delete with conditions removes a manifest only while, under the locks the
  delete holds, the version still has the digests the client read, was published no later than
  the client's cut-off, and no pointer names it unless the delete is forced; when a condition
  fails nothing is deleted, no audit row is written (the refusal comes before the pending row,
  as a refused delete by tag does), and the answer says which (`412`, or `409` naming the
  pointers). A variant published while the delete runs is not among the digests the client
  sent and stays.
- **Scope.** `DELETE /api/v1/…/versions/<version>` with `If-Match` or `If-Unmodified-Since`, and
  every item of `POST /api/v1/…/versions:delete` (http-routing.md §4); the pointers are npm's
  dist-tags. Not `DELETE` on `/v2`, the raw frontend or npm's unpublish, which take no
  condition. The publish time is the manifest's `org.opencontainers.image.created`: the
  frontends write it from the server's clock, a client pushing on `/v2` writes its own, which
  the facade refuses when it is more than `server.maxCreatedAhead` (5 minutes) after the
  server's clock or more than `server.maxCreatedBehind` (an hour) before it: a writer moves the
  age of its version by that much at most.
- **Assumes.** Registry: a delete by digest removes that manifest only (any registry of the
  matrix). Database: the referrers lock excludes across replicas (Postgres; SQLite serves one
  replica, INV-26).
- **Mechanism.** `ingest.Pipeline.Delete` with `format.Deletion.Condition`: `deletion.locked`
  takes the package's journal lock, then the tag lock (index-schema.md, Coordination: one order),
  `deletion.check` reads the version again by its digest, its `created_at` and the package's
  `journal_state` under them, and `deletion.record` deletes the rows by digest before the locks
  are released; a dist-tag move checks under the journal lock that its version exists
  (`journal.checkVersions`). The API checks the same conditions for every variant before the
  first delete (`api.conditions.check`), so that a refusal finds nothing deleted. The bound of a
  `/v2` push's time is `facade.checkCreated`, before the facade asks the registry; the repeat of
  a manifest the repo has passes, as it writes nothing.
- **Tests.** `TestConditionalDelete`, `TestConditionalDeletePointers` (the lock order),
  `TestConditionalDeleteRepublishRace`, `TestConditionalDeleteDistTagRace` (goroutines, `-race`),
  `TestDeleteVersionConditions`, `TestBatchDelete`, `TestAdminRetention`,
  `TestManifestHookCreated`, `TestManifestHookCreatedRepeat`; `retention.sh`, and O-03 in
  `oras.sh` (a manifest created a year back).
- **When violated.** A delete that removed a version re-published or dist-tagged after the read
  leaves the newer version's rows without a manifest, which reindex removes, or a dist-tag
  naming a version that is gone, which the packument leaves out; nothing reports it.
- **Status.** `holds`.

## Audit

## Summary

| Id | Guarantee | Status | Markers |
| --- | --- | --- | --- |
| INV-1 | artifact bytes only in the registry | holds | |
| INV-2 | one write path | holds | untested (plugins could push) |
| INV-3 | the index rebuildable from the registry | strongly consistent only | unchecked (listings); untested (I-01 e2e) |
| INV-4 | a version in the index is readable | holds | unchecked; untested |
| INV-5 | prune never removes what the registry holds | holds | unchecked (lag within the grace age); untested (ECR) |
| INV-6 | one winner among concurrent publishes | holds | |
| INV-7 | a tag is never re-pointed | holds | unchecked |
| INV-8 | the idempotent repeat | holds | |
| INV-9 | a version whole or not at all | holds | |
| INV-10 | case variants never squat | holds | |
| INV-11 | a blob stored only under its digest | holds | |
| INV-12 | only manifests of the profile | holds | |
| INV-13 | each version deleted at once from index, caches, audit | holds | unchecked |
| INV-14 | every change of the index has its audit row | holds | untested (S-09) |
| INV-15 | denial before input | holds | untested (frontends) |
| INV-16 | a denial hides existence | holds | |
| INV-17 | authorization changes count at once | holds | |
| INV-18 | a credential never grants more | holds | |
| INV-19 | bearer tokens across replicas, live keys only | holds | |
| INV-20 | credentials go only where they belong | holds | unchecked (oras-go redirects) |
| INV-21 | no secret logged | holds | untested (C-07) |
| INV-22 | the client address from trusted proxies only | holds | |
| INV-23 | every request bounded in size | partly | |
| INV-24 | parsers bounded | holds | |
| INV-25 | memory independent of file size | holds | |
| INV-26 | replicas coordinate through the database | holds | unchecked (the referrers lock's transaction) |
| INV-27 | cached documents invalidated | holds | |
| INV-28 | a migration applied once | holds | |
| INV-29 | the npm journal's order and replay | holds | unchecked |
| INV-30 | packument agrees with the tarball | holds | |
| INV-31 | proxy fetch once, checked | partly | unchecked |
| INV-32 | a proxy reaches only what is configured | holds | |
| INV-33 | a proxied artifact is ordinary | holds | |
| INV-34 | hosted names hide the upstream in a group | holds | |
| INV-35 | referrers kept, none lost to a race | partly | unchecked (GC, the lock's transaction) |
| INV-36 | drain on stop | holds | |
| INV-37 | readiness | holds | |
| INV-38 | denied versions kept out | not yet (#101) | |
| INV-39 | encryption at rest | not yet (#114) | |
| INV-40 | a streamed download has its digest | holds | |
| INV-41 | a binary only on a schema it can run on | partly | untested until v0.1.0 (the previous release's binary) |
| INV-42 | a conditional delete never removes a changed version | holds | |

## Not guaranteed

Behaviours people are likely to depend on, and must not. Each line says why; where a spec
promises part of it, the line says which part stays a promise.

- **The order of items in a list**, unless the contract states it. It states these: the raw
  listing (by path, then `created`), apt's `Packages` (by name and version), npm search (by
  name), a group's members (in member order), and the lists of api.md (repos by format and
  name, users by name, bindings by id, tokens by creation, the audit log newest first). Not
  stated, so not guaranteed: the order of `tags/list` and `_catalog` beyond what `n`, `last` and
  `Link` need to continue a page, the keys of a packument's `versions` and `time`, the
  manifests of a referrers index, the suites, components and architectures of a `Release`.
- **The wording of a message.** Statuses, the OCI error codes and the seven codes of `/api/v1`
  (http-routing.md §4) are the contract, and so are the structured fields a spec names
  (`detail.existing` of a `409 TAG_INVALID`). Where a spec says that a message names something (a
  field, a digest, a bound, a backend), it does; the sentence around it may change in any
  release. Parse the codes and the structured fields, never the text.
- **Timing and latency.** Response times, throughput, the measurements of operations.md and the
  targets of acceptance.md §7, how soon within its interval the reconcile loop runs, and whether
  a denial takes as long as the answer for a missing repo (threat model R-5). What is
  guaranteed are the bounds the specs set (a token's 20 minutes and 60 seconds of skew, the
  second of `admin key rotate --token --drop`) and the TTLs and timeouts a configuration sets;
  their default values are not (below).
- **The internal OCI layout.** What manifest-profile.md makes normative is guaranteed: the media
  types, the annotations, the tags, the repository paths, the journal. Not guaranteed: the
  bytes of a manifest Lajstrom builds (field order, whitespace), so its digest for the same file
  differs between builds and may differ between releases; keys of a metadata layer beyond those
  §2 lists; whether a blob is uploaded whole or in chunks, and whether it is mounted; the blobs
  that refused or aborted uploads leave in the registry, which are its garbage collector's.
- **Tags of `oci` repos.** They pass through, and are as mutable or immutable as the backend
  makes them.
- **The bytes of a redirected download are their digest.** A `307` to the backend's storage
  sends the client to bytes that do not pass through Lajstrom: the registry is trusted to serve
  what it stored, and the clients check (OCI clients the descriptor, apt the `Packages` hashes,
  pip `#sha256=`, npm `integrity`, Maven the checksum files). A download that streams through
  Lajstrom is hashed on its way to the client (INV-40); a `206` range, which is not the whole
  blob, is not.
- **The bytes of a rendered document across releases.** Its content, as the format's protocol
  defines it, is guaranteed; its whitespace and field order, and so its `ETag` and its
  `by-hash` name, may change in a release, and a reindex renders it again.
- **The database as an interface.** SQL against any table: the derived and coordination tables
  may be renamed freely (index-schema.md), and the queries operations.md shows are an operator's
  aid. Read through `/api/v1`, the CLI, and `lajstrom admin export`.
- **Opaque values.** Upload ids, a token secret after its prefix, the claims of a bearer
  token beyond those rbac.md §4 names, generated request ids, run ids, an `ETag` whose form no
  spec states. What a spec defines stays defined: a blob's `ETag` is its digest, an npm
  document's its sha256, npm's `_rev` is `<seq>-<hex>`.
- **Which replica answers.** Any replica may answer any request, and every answer is the same
  but for what is per replica by design: the throttles of failed logins and of `/v2/token`
  count per replica (auth.md; a shared one is [#282](https://github.com/lajstrom/lajstrom/issues/282)).
- **Default values, and the timings of retries.** configuration.md documents the default of
  each key, how many times a request is retried by default (`registries[].retries`,
  `upstreams[].retries`) and the waits between retries (doubling from 250 ms to at most 3 s,
  with jitter); a release may change any of them: set a key to rely on its value. What a key
  means stays a promise: for the retries, which failures are retried and that only idempotent
  requests are sent again. The circuit breaker's thresholds and periods (operations.md, Limits)
  are not guaranteed either.
- **Logs, and metrics beyond their names.** No log message, field or level of a line is
  stable: `log.format` chooses text or JSON lines, and that is all the configuration promises.
  Of the metrics, the names of the series and the rule that no label carries a repo or package
  name are in the contract; their other labels, a histogram's buckets and the help texts are
  not. What is guaranteed about logs is INV-21.
- **Registries outside the `CI` tier.** What a guarantee promises on the other tiers is what
  [Where the guarantees hold](#the-registry) says: as measured on a date for `manual` and
  `hosted`, nothing for `untested` and for versions other than the pinned ones.
- **Anything marked preview or not committed.** The roadmap's "After v0.1.0 (preview, not
  committed)", the work of draft pull requests, the plugin hooks of proxy-and-group.md §10
  ("not a contract yet"), and what an entry with status `not yet` promises beyond the part its
  status says holds today.
- **A blob redirect or a stream.** Whether a download is a `307` to the backend's storage or a
  stream is the backend's and the repo's policy's (http-routing.md §2): a client must follow
  both. How long a presigned URL lives is the backend's.
- **A raw file's sniffed `Content-Type`.** Without one in the metadata layer, the type is
  sniffed, and the sniffing may change.
- **Deduplication.** Whether one file in two repos is one blob is the registry's.
- **The CLI's human output.** Tables, progress lines and their wording; `reindex --json` is the
  `reindex.Report` of layout.md.
- **The `details` of an audit row** beyond the keys rbac.md names (`createdBy`, the `proxy.fetch`
  and `proxy.reject` keys, `via`).
- **Writes straight to the registry.** Lajstrom indexes a profile manifest it finds there at the
  next run and reports what it cannot index; until then it promises nothing about it.
- **An upstream's cache.** A proxy's `proxy_meta` rows may be dropped at any time, and what a
  proxy lists follows the upstream while it answers.

## Gaps

Every **untested** and **unchecked** marker above, and every `partly` and `not yet`, with what it
costs and what would close it. "new issue" means that no issue tracks it yet.

| Gap | Impact | Proposed fix | Issue |
| --- | --- | --- | --- |
| Read-after-write is assumed, never measured or tolerated (INV-3, INV-13); INV-4, INV-7, INV-29 and INV-31 have the visibility barrier and the claims, INV-5 the grace age | on ECR: a deleted version re-added for an interval | a consistency model per registry, a visibility barrier on publish, read-back retries, a grace age and two observations before a removal; the probe measures the lag | [#446](https://github.com/lajstrom/lajstrom/issues/446), [#339](https://github.com/lajstrom/lajstrom/issues/339) |
| No test with a registry that lags (INV-3); INV-4's tests have `providertest.Lag` | the gap above cannot regress visibly | a fake registry with configurable lag, as #446's criteria say | [#446](https://github.com/lajstrom/lajstrom/issues/446) |
| **Monotonic reads** are assumed (INV-4, INV-7, INV-29, INV-31) | behind an endpoint that picks a replica per request (ACR's global endpoint with geo-replication), a version the barrier saw on one replica can be `404` on the next | point `registries[].url` at one regional endpoint; measure it in the probe with reads from fresh connections | new issue |
| A durable `201` is assumed (INV-4) | a manifest the registry lost leaves the index silently at the next reconcile, counted among `removed` | report the removal of a row ingest confirmed as a finding of its own, with a metric | new issue |
| I-01 is not in the end-to-end suite (INV-3); the equality tests (`TestRebuild`, `TestRebuildFormats`, npm's) publish through the frontends in-process, not with the real clients | a column that only a real client's upload fills, and that annotations cannot rebuild, would be lost at the first reindex, unnoticed | I-01 as an end-to-end step: publish with every client, dump the derived rows, reindex, compare | new issue |
| Nothing machine-checks the one write path in plugins (INV-2) | a plugin could push a manifest past ingest: no claim, validation or audit row | a read-only provider view in `format.Deps`, or a lint rule against `Push`, `PushReference` and `Delete` outside `internal/ingest`, `internal/facade` and `internal/provider` (a `contract` change of `format`) | new issue |
| A timeout the client cannot lift, a terminated backend or a failover can end the referrers lock's transaction while its push runs (INV-26, INV-35) | that push is `503` and its retry lists its referrer; a referrer that another writer pushed meanwhile can be missing from the `sha256-<hex>` index, and nothing reports it | keep PgBouncer's `idle_transaction_timeout` and Postgres's `transaction_timeout` 0 (index-schema.md, Coordination); reindex `--verify` could add back a referrer that the audit log has as pushed and the index lacks | new issue |
| The garbage collectors of GitLab B and Nexus delete untagged referrers (INV-35) | signatures and SBOMs vanish | the tag schema as well on registries with the API (#326); for GitLab B and Nexus, a vendor-table warning or a tag per referrer | [#326](https://github.com/lajstrom/lajstrom/issues/326), new issue |
| No frontend test of a denial with malformed input (INV-15) | a regression of the R-2 kind on a frontend goes unseen | a table test per frontend, as `TestDenialBeforeInput` does for `/v2` | new issue |
| Acceptance C-07 and S-09 are not automated (INV-14, INV-21) | a redaction or an audit regression goes unseen | after the suite, grep both replicas' logs for every secret used, and count the audit rows of its writes | new issue |
| No budget for the bytes of `$TMPDIR` (INV-23) | frontend uploads stream (ADR 0014); what still spools has no byte budget: the proxy fetches of a replica, which `server.maxConcurrentUploads` does not count either, and the `/v2` chunks of `uploads.spoolUnknownLength` | a spool budget, or a proxy fetch that streams too (ADR 0014) | [#427](https://github.com/lajstrom/lajstrom/issues/427) |
| One upstream request per document per replica (INV-31) | concurrent misses of a packument each ask the upstream | share the request on a replica (pull request #399) | [#383](https://github.com/lajstrom/lajstrom/issues/383) |
| Registries that fold tag case (INV-7) | two versions that differ in case: the second is a `409` with an unclear message | `Capabilities.TagsFoldCase` | [#426](https://github.com/lajstrom/lajstrom/issues/426) |
| Whether a migration is expand is reviewed, not tested; the job `upgrade`, which runs the previous release's binary on the next schema, is skipped until there is a release; pre-#244 claims (INV-6, INV-41) | a contract migration without its `requires` line breaks the replicas of the release before during a rolling update; a mixed fleet may not exclude publishes | tag v0.1.0: from then on the job `upgrade` (`test/upgrade/run.sh`) runs on every pull request that changes a migration | [#485](https://github.com/lajstrom/lajstrom/issues/485) |
| Policy (INV-38), encryption at rest (INV-39) | not built | as the issues say | [#101](https://github.com/lajstrom/lajstrom/issues/101), [#114](https://github.com/lajstrom/lajstrom/issues/114) |
