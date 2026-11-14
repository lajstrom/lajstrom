<!-- The survey behind #446, made on 2026-11-10 from public sources only, and the decisions taken on it up to 2026-11-12; kept for the record. -->

# Read-after-write lag of OCI registries: what Lajstrom must tolerate

Survey made on 2026-11-10 for [#446](https://github.com/lajstrom/lajstrom/issues/446), from
public sources only: vendor documentation, the issues and commits of tools that push to these
registries, and users' reports. Nothing in the survey was measured live. What Lajstrom measured
since is at the end, with the decisions taken on it.

## Summary

| Registry | Same-region manifest or blob after push | Tag list, catalog | Replication, cache |
| --- | --- | --- | --- |
| Amazon ECR | 0.1 to 5 s, worse under concurrent pushes ([ocync#104](https://github.com/clowdhaus/ocync/pull/104); [docker/github-builder 0396b4ecfb](https://github.com/docker/github-builder/commit/0396b4ecfb); docker/actions-toolkit#1016). AWS documents none | no reports | replication: the majority under 30 min; pull-through cache: 24 h |
| GHCR | rare, under 1 s to a few s | none reported | none |
| Docker Hub | none for the registry; referrers lag seconds, up to about a minute | none reported | none |
| Quay | immediate | tag list up to 120 s, catalog up to 60 s | 60 to 300 s after a delete |
| Azure Container Registry | same replica: nothing documented | per replica | geo-replicas are eventual, no published bound |
| Google Artifact Registry | no reports | none reported | a remote repository caches its tags for 1 h |
| distribution on S3, GCS or Azure Blob | immediate | immediate | only from extras: the in-memory cache, a CDN |

What this means:

- **A publish can be confirmed before a reader can get it.** On ECR the manifest Lajstrom just
  pushed, or a blob a client just uploaded, can answer `404` for a few seconds. A version in the
  index must not point at what the registry does not serve yet (INV-4).
- **A missing read is not a delete.** A tag list or a catalog that lacks a name, a `404` by tag,
  a `5xx`, a `429` or a timeout says nothing about whether the manifest is gone. Only a `404
  MANIFEST_UNKNOWN` to the manifest by its digest does, and only when it lasts (INV-5).
- **Deletes are eventually consistent too.** Quay's caches and ACR's replicas show a deleted
  manifest for minutes. A reconcile inside that lag would index the version again (INV-13).
- **Lists lag on their own.** Quay serves a tag list read before a push for up to 120 s after
  it. A rebuild inside that lag misses the newest manifests (INV-3).

## Numbers for the implementation

- **The visibility barrier.** `HEAD` the manifest by digest; on a registry not classified
  `strong`, also `HEAD manifests/<tag>`, compared by `Docker-Content-Digest`, and each layer by
  digest. Never the tag list or the catalog. Backoff from 100 to 200 ms, doubling, each wait at
  most 5 s, with jitter. Bound: 30 s, 60 s when the referrers must list a referrer too. Past
  it, `503` with `Retry-After: 30`. Read back from the endpoint that took the write.
- **The writer side on ECR.** Send again a manifest `PUT` answered `MANIFEST_BLOB_UNKNOWN`, and
  the first write to an upload session answered `BLOB_UPLOAD_UNKNOWN`, within the same 30 s.
- **Prune.** "Unknown" is only a `404 MANIFEST_UNKNOWN` to a `HEAD` of the manifest by digest,
  at the write endpoint, twice, at least 10 minutes apart. `reconcile.graceAge`: 1 h when
  Lajstrom reads the endpoint it writes to, at least 24 h when reads go through a replica or a
  cache.
- **Deletes.** A deleted version must stay out of the index while a lagging registry still
  lists it, and deletes stay idempotent.

## Decisions

**2026-11-10, the lead: the barrier is unconditional.** Ingest reads back after every manifest
`PUT`, whatever the registry's classification, so that INV-4 holds without a condition and the
measured lag only tunes the bound. The decision named the manifest and each of its layers.

**2026-11-12, the lead: on a `strong` registry the barrier reads the manifest only.** The code
read the manifest by digest on a `strong` registry, and the manifest, its tag and every blob on
any other; the decision text is aligned with the code, not the other way round. The layers are
checked on upload, before the manifest exists:

- a frontend's file streams into an upload session while ingest computes its digest, and the
  session is committed with that digest (ADR 0014): the registry stored exactly those bytes, and
  answered `201` for them;
- a client's blob through `/v2` is checked before the manifest `PUT`: the facade's hook reads
  each layer (`checkLayers`), ingest checks them again (Validate), and the registry refuses a
  manifest that names a blob it does not have;
- on a `strong` registry a blob that answered `201` reads back at once, by the definition of
  `strong`.

Reading the layers again after the `PUT` would cost one to three `HEAD`s per publish and prove
nothing more on such a registry. On any other registry they are read, as before.

**2026-11-12, the lead: a deleted version stays out through the audit log.** A run on a registry
not classified `strong` loads the digests of the repo's `delete` rows, state `ok`, younger than
`reconcile.graceAge`, and does not index a listed manifest the index lacks under one of them; the
report counts it as `deletedPending` (#838). No tombstone table, and no change of the index
schema or of `Store`. The cost: a manifest pushed straight to the registry, by-passing Lajstrom,
under a digest deleted through Lajstrom less than the grace age before, is indexed only once the
grace age has passed. On a `strong` registry the audit log is not read.

**A delete does not read back in the request.** The plan offered a read-back after the delete
(`deletion.awaitGone`), up to the bound, then a `503`. It is not built. The guard above is what
keeps the version out of the index; a read-back would add up to the bound to every `DELETE` on
ECR and Quay, and a `503` after a delete that the registry took would tell a client to retry a
delete that went through. A client that reads the registry right after its `DELETE` may still
see the manifest for the registry's lag; it does not see it in Lajstrom's indexes, which are
rendered from the index.

**Monotonic reads** (an endpoint that picks a replica per request, ACR's global endpoint with
geo-replication) are left to [#564](https://github.com/lajstrom/lajstrom/issues/564).

**Live ECR.** #446 closes on fakes (`providertest.Lag`, the probe's fake registries, moto for the
API). The measurement against a real ECR, and the `untested` markers of INV-4 and INV-5, stay with
[#19](https://github.com/lajstrom/lajstrom/issues/19).

## What was built

| Item | Where |
| --- | --- |
| The consistency model, per registry | `provider.Capabilities.Consistency`, `ConsistencyBound`; `registries[].capabilities.consistency`, `consistencyBound` |
| The visibility barrier | `ingest.Pipeline.awaitVisible`; `503` with `Retry-After: 30` past the bound |
| Read-back retries | Validate, streamed uploads, referrers, the facade's hook |
| The writer side | a manifest `PUT` sent again on `MANIFEST_BLOB_UNKNOWN` (#842); a new session's first write sent again on `BLOB_UPLOAD_UNKNOWN` (#843) |
| Prune | two `MANIFEST_UNKNOWN` answers 10 minutes apart, in `prune_sightings`; `reconcile.graceAge` |
| Deletes | the guard of reindex on the audit log, `deletedPending` (#838) |
| Lagging listings | no code: the next run adds what a listing missed, and prune never removes on a listing; `TestRebuildLaggingListing` (#839) |
| Tests | `providertest.Lag` lags manifests, blobs, tag lists, the catalog, deletes and `StatManifest` (#831) |
| The probe | the row "Read-after-write lag": the lists read before each write, then the lag of each delete (#845) |

## Measured

The probe's row "Read-after-write lag", from [registries/README.md](../registries/README.md#consistency):

| Registry | When | Writes | Deletes |
| --- | --- | --- | --- |
| distribution 3.1.2, filesystem, S3, MinIO, SeaweedFS | 2026-11-11 | none seen | not measured then |
| distribution 3.1.2, filesystem | 2026-11-12 | none seen, the lists read before | none seen |
| Zot v2.1.23 | 2026-11-11, 2026-11-12 | none seen; on 2026-11-12 the lists read before | none seen (2026-11-12) |
| GitLab A and B | 2026-11-11 | none seen | not measured |
| Nexus | 2026-11-11 | none seen | not measured |
| Project Quay 3.18.2 | 2026-11-12, by hand | manifests and blobs at once; a tag list read before a push stale up to 120 s, a catalog page 60 s | not measured |
| JFrog JCR 7.161.26 | 2026-11-13 (#947) | none seen, the tag list read before (no catalog); by curl, the referrers list up to 9 ms behind a referrer's push | none seen; by curl, the referrers list about 10 ms behind a referrer's delete |

Not measured yet: Harbor, Gitea, Forgejo and GHCR with the probe of #845; ECR (#19).
