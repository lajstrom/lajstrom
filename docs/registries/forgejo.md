# Forgejo container registry

What the container registry built into Forgejo answered, measured with `curl`, `oras`,
`lajstrom registry probe`, the integration tests and the end-to-end scenarios. It was first
measured on 2026-11-10 beside Gitea (#120), with the same setup. On 2026-11-12 it got a page
and a column of its own (#816), and the probe and the tests ran against it again. On
2026-11-15 the probe ran once more, for its rows on consistency
([The probe again](#the-probe-again-2026-11-15-850)), and on 2026-11-20 on GitHub Actions
runners ([On a runner](#on-a-runner-2026-11-20-850)).

Forgejo is a hard fork of Gitea, made in 2024 (Codeberg e.V., GPLv3+). Both implement the
distribution API themselves, in their package registry. The registry is `/v2/` on Forgejo's
own port, a repository is an image of an owner (a user or an organisation), and every tag is a
package version in Forgejo's database. Most of what [gitea.md](gitea.md) measured holds here
too. The two codebases now differ where Lajstrom feels it: Forgejo serves ranges, and its
upload status is one byte ahead ([Where it differs from Gitea](#where-it-differs-from-gitea)).
The [summary column](README.md#capabilities) is in the README; this page has the details.

## Version tested

- Forgejo **17.0.1** (released 2026-10-26), the rootless image
  `codeberg.org/forgejo/forgejo:17.0.1-rootless`, index
  `sha256:5ef02fbcf22beef56b8c844952ffad58cfe177b6f98e46e6abb5df90b88dc8d3` (linux/amd64,
  arm, arm64). The binary says `forgejo version 17.0.1+gitea-1.22.0 (release name 17.0.1)
  built with … go1.26.10`; `/api/v1/version` and `/api/forgejo/v1/version` say
  `17.0.1+gitea-1.22.0`. SQLite, packages on local storage (`/var/lib/gitea/packages`).
- The same index is published as `data.forgejo.org/forgejo/forgejo`, Forgejo's own registry.
  On 2026-11-12 Codeberg answered the pull `403 Codeberg is under load`, here and on CI's
  runners, so the image was pulled from there by its digest; the bytes are the same. PR #826
  moves the pin of compose.yaml to `data.forgejo.org` for that reason.
- On macOS with OrbStack, arm64; the tests with Go 1.27.3, at a7aa4d3e (main) and at 5d9e5328
  (the branch of PR #814).
- On 2026-11-15 the probe ran again on the same index, pulled from `data.forgejo.org` (the pin
  of compose.yaml since #826), at a8762730
  ([The probe again](#the-probe-again-2026-11-15-850)).
- On 2026-11-20 the probe ran on the pin on GitHub Actions runners, ubuntu-26.04, linux/amd64,
  Go 1.27.4 ([On a runner](#on-a-runner-2026-11-20-850)).

## Run it

The compose file and the set-up script are Gitea's,
[deploy/compose/gitea/compose.yaml](../../deploy/compose/gitea/compose.yaml) (its profile
`forgejo`) and [gitea.sh](../../deploy/compose/gitea/gitea.sh). They set Forgejo up as they set
up Gitea, with its CLI and REST API, never the UI ([gitea.md, Run it](gitea.md#run-it)):

```sh
deploy/compose/gitea/gitea.sh up forgejo    # Forgejo on http://localhost:5014, set up
deploy/compose/gitea/gitea.sh down          # Gitea and Forgejo, with their volumes and token files
```

`gitea.sh up forgejo` creates the site admin `lajstrom-admin`, the user `lajstrom`, the
private organisation `lajstrom-it` with a team `packages` that may write its packages, and a
token of the user with the scope `write:package`, in `deploy/compose/gitea/.work/forgejo.token`.
`LAJSTROM_FORGEJO_PORT` moves the port; `ROOT_URL`, from which Forgejo builds its Bearer realm,
moves with it. For Lajstrom: `prefix: lajstrom-it/`, `auth.username: lajstrom`, the token in the
variable `passwordEnv` names, and `uploads.spoolUnknownLength: true` for clients that stream
more than one chunk ([item 3](#what-the-provider-must-do-differently)).

## The row

| Registry | Version | Auth | Create on push | Referrers | Catalog | Delete manifest | Immutable tags | Blob redirect | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Forgejo | 17.0.1, local storage, `prefix: lajstrom-it/` (an organisation) | Bearer from `/v2/token` with the token, `scope="*"`, 24 h; Basic straight on `/v2/` works too | yes, below an existing owner the user may write, at any depth | **no**: `/referrers/` is a 404 with an empty body; the `sha256-<digest>` tag schema | yes, `n`/`last`/`Link`, lexical; the user's view | yes, by digest, with every tag and **the blobs only it used**; by tag the tag, and the manifest too when it was pushed by that tag only | **none**; tags are **case-insensitive and lower-cased** | none on local storage | pass but for **the upload status, one byte ahead** (PR #814 corrects for it) and a streamed `PATCH` after the first chunk (passes with `uploads.spoolUnknownLength`); ranges work |

## `lajstrom registry probe`

On 2026-11-12, at a7aa4d3e, with `--registry-prefix lajstrom-it/ --max-blob-size 512MiB
--max-manifest-size 32MiB -v` ([Commands](#commands)). The provider's own probe said
`vendor=gitea createOnPush=true referrers=false catalog=true deleteManifest=true
immutableTags=false blobRedirect=true maxBlobSize=0`, and `serve`'s hint about
`spoolUnknownLength`. Nothing was left behind. It ran again on 2026-11-15, with the rows added
since ([The probe again](#the-probe-again-2026-11-15-850)).

| Row | Forgejo 17.0.1 |
| --- | --- |
| Create on push | yes, with the first manifest |
| Where a repository may be created | below `lajstrom-it/`; outside it not tried; at any depth, inside another repository too; no length limit met: 256 characters, a component of 256 bytes |
| Routing words in a name | ok: 20 blob `HEAD`s in each name, all 200; `GET` 200 |
| Referrers API, `OCI-Subject` | no: an empty 404, the tag schema; no |
| Catalog | yes, `n`/`last`/`Link`, lexical, no largest `n` |
| Tag list | **lower-cased, case-insensitive**; lexical; `n`/`last`/`Link` |
| Read-after-write lag | none: each read saw each of 3 writes at once |
| Delete by digest | yes, 202, with its tags **and the blobs only it used**; its referrers stay |
| Delete by tag | the tag; **the manifest too**, when it was pushed by that tag only |
| Delete a repository | no `/v2` API; it leaves `_catalog` with its last manifest |
| Immutable tags | none: a tag re-points with 201, and **the manifest it named is deleted** |
| Blob redirect, `Range` | none: the bytes; 206, 416 past the end; `Accept-Ranges: bytes` on `GET` only, not on `HEAD` |
| Largest blob, chunk size | 512 MiB in one `PUT` and in one `PATCH`; no limit met |
| Manifest size | 8 MiB taken; 16 MiB is 413 `MANIFEST_INVALID` "Manifest exceeds maximum size" |
| Profile | accepted, stored as pushed |
| Uploads | 416 `BLOB_UPLOAD_INVALID` out of order; the session's `GET` 204 with `Range`: **one byte too many** (`0-3` after `0-2`); the cancel 204, then 404 `BLOB_UPLOAD_UNKNOWN`; a mount 201; a wrong digest 400 `DIGEST_INVALID`, the session stays; the empty blob stored; a session continued by another provider works |
| What the probe finds, overrides | `referrers=false catalog=true`; none |

The probe's row for a session continued by another replica says "works" because it streams one
chunk and closes the session, and never reads the status. Lajstrom's facade does read it
([Our tests against it](#our-tests-against-it)).

The deleted 512 MiB blobs stayed on Forgejo's disk until its cron task `cleanup_packages`
ran (as on Gitea, [gitea.md, Cleanup](gitea.md#cleanup)). With
`LAJSTROM_GITEA_CLEANUP_OLDER_THAN=1s` and
`POST /api/v1/admin/cron/cleanup_packages`, `/var/lib/gitea/packages` went from 1.1 GB to
716 KB.

## The probe again, 2026-11-15 (#850)

Since the run of 2026-11-12 the probe reads the lists before each write and measures the
deletes in its row "Read-after-write lag" (#845), and it has a row "Monotonic reads" (#564).
Neither had been measured on Forgejo. Lajstrom at a8762730, against Forgejo 17.0.1, the pin,
index `sha256:5ef02fbcf22beef56b8c844952ffad58cfe177b6f98e46e6abb5df90b88dc8d3`, pulled from
`data.forgejo.org`. Forgejo came from a copy of compose.yaml with another compose project name,
next to an unchanged copy of gitea.sh, because other runs shared the machine.

```sh
LAJSTROM_FORGEJO_PORT=57214 deploy/compose/gitea/gitea.sh up forgejo
bin/lajstrom registry probe --url http://localhost:57214 --registry-prefix lajstrom-it/ \
  --username lajstrom --password-file deploy/compose/gitea/.work/forgejo.token --max-manifest-size 32MiB -v
```

The provider's own probe said `vendor=forgejo createOnPush=true referrers=false
referrersAlwaysTag=false catalog=true deleteManifest=true immutableTags=false blobRedirect=true
maxBlobSize=0 consistency=unknown maxNameDepth=0 tagsFoldCase=true buildRoutes=false`, with
`serve`'s hint about `spoolUnknownLength`. It names Forgejo since #829 (item 6), and
`tagsFoldCase` comes from the vendor table (item 2). The probe ran three times. Each run took
about 1.3 s, the three tables were the same, and nothing was left behind (`_catalog` was
empty). `--max-blob-size` stayed at its default of 8 MiB because the shared disk had about
6 GiB free; the 512 MiB of 2026-11-12 still stands. The rows that are new or that changed:

| Row | Forgejo 17.0.1 |
| --- | --- |
| Read-after-write lag | none: each read saw each of 3 writes at once (manifest by digest, manifest by tag, blob, tag list, catalog); each read saw each of 3 deletes at once (manifest by digest, manifest by tag, tag list) |
| Monotonic reads | none: no read saw a change undone; 3 writes, a tag move, 3 deletes, each read 5 times more over new connections after the first that saw it, from 1 address |
| Delete a repository | no `/v2` API; it leaves `_catalog` with its last manifest, its tag list 200 |
| Largest blob, chunk size | 8 MiB in one `PUT` and in one `PATCH`; no limit met |
| What the probe finds, overrides | `referrers=false catalog=true`; none |

Every other cell was the same as in the [table of 2026-11-12](#lajstrom-registry-probe), the
routing words and the upload status one byte ahead included.

What was read, from the `-v` log:

- **The lists, primed.** In the repository `lag`, before each of the three manifests, the probe
  read the tag list and the catalog once. Before the first, the tag list answered 200 with no
  tags, since the repository held the blob just uploaded, and the catalog did not list `lag`.
  Right after each `201`, `HEAD` by digest was 200, `HEAD` by tag answered the new digest, the
  tag list had the new tag and the catalog listed `lag`, each at the first ask.
- **The deletes.** Each manifest deleted by digest was 202. Then `HEAD` by digest and by tag
  were 404, and the tag list no longer had the tag, at the first ask.
- **Monotonic reads.** In the repository `mono`, the blob's `HEAD` and each manifest's `HEAD`
  by digest and by tag saw each write at once, and then 5 more times, each over a new
  connection. The tag `mono-0` was then moved to another manifest (201; Forgejo deletes the
  manifest it named, as the probe's row "Immutable tags" [above](#lajstrom-registry-probe)
  says), and each of 6 reads by tag answered the new digest. Then each of the three manifests
  was deleted by digest, and each read of it, by digest and by tag, stayed 404.

What is new against the [column](README.md#capabilities):

- **Read-after-write lag, monotonic reads.** These are measured on Forgejo for the first time
  with the lists read before each write, with the deletes, and over new connections. The lists
  did not go stale, a deleted manifest was gone from every read at once, and no read over a new
  connection undid a change. As on Gitea, the `none` is a measurement of three writes and three
  deletes on one Forgejo with SQLite and local storage, three times over, not a promise of
  Forgejo's. The vendor table's entry for Forgejo leaves its consistency unknown, so a publish
  reads back its tag and blobs too, and that is right.
- **Delete a repository.** An emptied repository leaves `_catalog`, as before, and its tag
  list answers 200 with `"tags":[]`, not 404, as on Gitea.
- **What the probe finds.** The provider now names the vendor `forgejo`. It said `gitea` on
  2026-11-12, before #829.

The `-v` log still shows a 401 before nearly every request: 587 of the 1,262 requests of each
run, with 85 token requests. These are the probe's own requests. The probe has an auth client of
its own (`newClient`, internal/provider/probe/client.go), not the provider's, so the alias of
#1042 ([gitea.md](gitea.md#the-probe-again-2026-11-13-850)) is not in it, and this run does not
measure that fix.

## On a runner, 2026-11-20 (#850)

The probe ran again on Forgejo 17.0.1, the pin, this time on GitHub Actions runners. The
workflow Probe (`.github/workflows/probe.yml`, #1440) pulls Forgejo from `data.forgejo.org`
with the four tries of the job `e2e (forgejo)`, brings it up with `gitea.sh up forgejo`, and
runs the probe as [above](#the-probe-again-2026-11-15-850) (`--registry-prefix lajstrom-it/
--max-manifest-size 32MiB -v`). Lajstrom was built from main at 7f93d1a3 with Go 1.27.4, on
ubuntu-26.04, linux/amd64. There were two runs on 2026-11-20,
[44656305997](https://github.com/lajstrom/lajstrom/actions/runs/44656305997) and
[44657661519](https://github.com/lajstrom/lajstrom/actions/runs/44657661519). Each job's
summary has the table, and its artifact `probe-forgejo` has the `-v` output.

The provider's own probe said what it said on 2026-11-15, `vendor=forgejo … consistency=unknown
maxNameDepth=0 tagsFoldCase=true buildRoutes=false`, with `serve`'s hint about
`spoolUnknownLength`. Both tables were the same, and nothing was left behind. Every cell was the
one of 2026-11-15 and, for the rows that run did not list, of the [table of
2026-11-12](#lajstrom-registry-probe), with the blobs at the default 8 MiB. The consistency
rows:

| Row | Forgejo 17.0.1, two runs on runners |
| --- | --- |
| Read-after-write lag | none: each read saw each of 3 writes at once (manifest by digest, manifest by tag, blob, tag list, catalog); each read saw each of 3 deletes at once (manifest by digest, manifest by tag, tag list) |
| Monotonic reads | none: no read saw a change undone; 3 writes, a tag move, 3 deletes, each read 5 times more over new connections after the first that saw it, from 1 address |

So a second machine, a Linux runner with Forgejo on amd64, finds what the Mac found. It is
still a measurement of three writes and three deletes on one Forgejo, and the vendor table's
entry stays without a consistency.

The 401 before nearly every request is gone: 85 of the 760 requests of each run were 401s, one
before each token request. Since #1147 the probe's own auth client adds the alias of #1042
after a challenge too (`aliasClient`, internal/provider/probe/client.go).

## What the provider must do differently

1. **Read the upload status one byte back.** `GET` of a session answers one byte past the bytes
   the session holds: `Range: 0-5` after a `PATCH` of `0-4` that was answered `Range: 0-4`
   ([Uploads](#uploads)). A session that another replica continues from that status sends its
   next chunk one byte late and gets 416, and the facade tells its client the wrong offset.
   Gitea answers right, so the vendor entry `gitea`, which Forgejo is detected as, cannot carry
   it. **PR #814 (#804)** measures it instead: at the first status that disagrees with the
   offset Lajstrom counted, the provider opens a scratch session, sends 8 bytes, and reads its
   status. If the status is one byte ahead, every later status of that provider is read one
   byte back. Run against this Forgejo on 2026-11-12, it fixes the facade's
   `TestIntegrationUploadAcrossReplicas` and the provider's `TestIntegrationUploads`
   ([the comment on #814](https://github.com/lajstrom/lajstrom/pull/814#issuecomment-6827103087)).
   PR #814 merged on 2026-11-13.
2. **Know that tags are case-insensitive.** As on Gitea ([gitea.md, item 2](gitea.md#what-the-provider-must-do-differently)),
   a tag is stored lower-cased and resolved in any case. That was read in the code until now.
   It has now run: reindex cannot rebuild a Maven version with capitals from the registry.
   `TestIntegrationFormats/sqlite/maven` publishes `com.example:lib-core:1.4.0-M1`. The publish
   works, but a rebuild from the registry reported `the tag of lajstrom.version and
   lajstrom.variant does not match the tag pushed to: "1.4.0-M1", want "1.4.0-m1"` and left
   the version out.
   - **Done in #844.** Reindex now matches the listed tag to the annotations' tag without case,
     when the registry resolves the annotations' tag to the same manifest. It takes the version
     and the tag from the annotations. This is the same as on Gitea ([gitea.md, item 2](gitea.md#what-the-provider-must-do-differently)).
     It is tested on a fake that folds case. The integration tests have not been run against
     Forgejo again: the job `e2e (forgejo)` runs the scenarios and the provider's tests, not
     reindex's.
   - **Two versions that differ only in case** behave as on Gitea. The vendor table says that
     Forgejo folds case (`capabilities.tagsFoldCase: true`, #426). Through Lajstrom the second
     version is a 409 that names the first, and two such publishes at once are one claim, so the
     second pushes nothing. Reindex indexes the one manifest the tag holds, and reports a version
     whose tag was re-pointed past Lajstrom as drift. Versions with capitals are not refused.
3. **Do not stream a chunk of unknown size into a session that already holds bytes.** As on
   Gitea, it is 400 `BLOB_UPLOAD_INVALID` "Stream uploads after first write are not allowed".
   `registries[].uploads.spoolUnknownLength: true` spools such a chunk (#425). `serve` hints at
   it at start, since Forgejo is `vendor=gitea` (#449).
4. **Write names in the 1.0 grammar.** `a--b`, `a---b` and `a__b` are 400 `NAME_INVALID`, and so
   are `a..b`, `a._b` and capitals (curl, 2026-11-12). `a-b`, `a_b` and `a.b` are taken. Names
   written since ADR 0017 fit: `h0_h…`, `_2d_2d`. A name in its earlier form cannot be stored:
   `TestIntegrationSplit` pushes those forms on purpose and fails at `a--b.txt` and
   `.gitignore` (`h0__…`). quirks.md said this from the code ([Per registry](quirks.md#per-registry)).
   It is now measured on Forgejo, and on Gitea since 2026-11-13; Gitea's main takes these names
   since go-gitea/gitea#40363 ([gitea.md](gitea.md#the-name-grammar-2026-11-15)).
5. **Expect a digest delete to take the blobs only that manifest used**, as Gitea's does. A
   push of the same manifest right after the delete, with blobs uploaded before it, is
   404 `BLOB_UNKNOWN`. The facade's `TestIntegrationDeleteInterleaved` pushes a manifest while a
   delete of the same digest holds the lock. Eight of its subtests fail so, on main and with
   #814: `PUT v2 after the delete: status 404, want 201; … BLOB_UNKNOWN`. The client has to
   upload the blobs again. Nothing in the provider can keep them, since the registry deletes
   them itself. The test's expectations are distribution's.
6. **Name the vendor.** `GET /v2/` challenges `Bearer
   realm="<ROOT_URL>v2/token",service="container_registry",scope="*"`, as Gitea does, and its
   200 carries only `Docker-Distribution-Api-Version`. So Forgejo is `vendor=gitea`. Forgejo
   alone answers `GET /api/forgejo/v1/version` (200, `{"version":"17.0.1+gitea-1.22.0"}`), and
   its `/api/v1/version` has `+gitea-` in the version. Telling the two apart lets a quirk
   apply to one only. PR #829 asks that endpoint when `GET /v2/` says Gitea, and names Forgejo
   `vendor=forgejo`, with Gitea's entry; it logged so against this Forgejo.

Nothing else: the token service, the tag schema, the catalog, tags, deletes, mounts, the
monolithic `POST`, the cancel, the digest check and ranges work as the provider expects.

## Where it differs from Gitea

Every command of [gitea.md](gitea.md) was run against Forgejo on 2026-11-10. These answers
differ:

- **Ranges work.** `Range: bytes=0-4` is 206 with `Content-Range: bytes 0-4/32`, and a range
  past the end is 416 `bytes */32` with the body `invalid range: failed to overlap`.
  `Accept-Ranges: bytes` comes on a `GET`, not on a `HEAD`. Gitea answers 200 with the whole
  blob ([gitea.md, item 3](gitea.md#what-the-provider-must-do-differently)).
- **The upload status is one byte ahead** ([Uploads](#uploads)); Gitea's is right.
- `POST` of a session answers `Range: 0-0`; Gitea sends no `Range`. The referrers 404 has an
  empty body; Gitea's says `404 page not found`.
- `/api/forgejo/v1/version` exists ([item 6](#what-the-provider-must-do-differently)).

Everything else measured the same on 2026-11-10: the token and its claims, owners,
case-folded tags, the deletes, re-pointing, media types, the catalog, the manifest size limit
and the streamed `PATCH`. The probe agreed on 2026-11-12. The 1.0 name grammar
([item 4](#what-the-provider-must-do-differently)) was measured on Forgejo only.

## Uploads

curl, 2026-11-12 (`T` the token, `L` the session's `Location`):

```text
POST   /v2/lajstrom-it/curl/status/blobs/uploads/       202  Range: 0-0, Location, Docker-Upload-Uuid
PATCH  Content-Range: 0-4   (5 bytes)                   202  Range: 0-4
GET    the session                                      204  Range: 0-5     one byte ahead, no Location
PATCH  Content-Range: 5-9                               202  Range: 0-9
GET    the session                                      204  Range: 0-10    one byte ahead, no Location
PATCH  Content-Range: 11-12 (where the status points)   416  BLOB_UPLOAD_INVALID
PATCH  Content-Range: 10-11 (the right offset)          202  Range: 0-11
DELETE the session                                      204
```

The rest is Gitea's ([gitea.md, Uploads](gitea.md#uploads)). A chunk out of order is 416. A
streamed `PATCH` works as the first write and is 400 after any write. A wrong digest is 400
`DIGEST_INVALID` and the session stays. The empty blob is stored, and the cancel is 204.

## Our tests against it

`LAJSTROM_TEST_REGISTRY=localhost:5014`, `LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom`, the token in
`LAJSTROM_TEST_REGISTRY_PASSWORD` and `LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/`, with
`go test -race -count=1 -tags integration ./...` on 2026-11-12. There was no
`LAJSTROM_TEST_POSTGRES_DSN`, so the Postgres cells were skipped. The provider's uploads ran
again with `LAJSTROM_TEST_REGISTRY_SPOOL_UNKNOWN_LENGTH=1`, and the provider and facade
packages on the branch of PR #814.

| Test | main, a7aa4d3e | PR #814, 5d9e5328 |
| --- | --- | --- |
| provider `TestIntegration`, `TestIntegrationNameLength` | pass | pass |
| provider `TestIntegrationUploads` | **fail** at "session across providers": the chunk of unknown size, 400 (item 3); with the spool flag, `BlobUploadStatus = {… Offset:21 …}` for 20 bytes (item 1) | pass with the spool flag, all 12 steps |
| probe `TestIntegrationRun` | **fail**: `sessionStatus = "204 with Range: one byte too many (0-3 after 0-2)"`, where the test wants every registry's status right | the same: the probe measures the registry |
| facade `TestIntegrationUploadAcrossReplicas` | **fail**: `GET on B: 204, Range "0-38"` for 38 bytes | pass |
| facade `TestIntegrationDeleteInterleaved` | **fail**: 8 subtests, 404 `BLOB_UNKNOWN` (item 5) | the same |
| facade: delete, delete referrer, delete unsupported, catalog after delete, pass-through, referrers, raw push, tokens across replicas, monolithic abandoned, blob reads, proxy repo push, client body breaker | pass | pass |
| reindex `TestIntegrationFormats` | **fail**: maven, the case-folded tag (item 2; reindex fixed in #844, not run again); apt, `oci_repos` lacks `goodbye`, the image whose only version the test deleted, which left `_catalog` with it | not run |
| reindex `TestIntegrationSplit` | **fail**: `a--b.txt` and `.gitignore` (item 4); `k8s/manifests/app.yaml` passes | not run |
| reindex `TestIntegrationNoCatalog` | **fail** in the cell `probe`: the test's front is on `127.0.0.1`, the realm's host is `localhost`, so the provider sends no credentials there (#354); the cell `config` passes | not run |
| ingest, raw, deb (`TestAptGet`, `TestAptGetInstall/registry`, `TestAptGetSigned` in `ubuntu:22.04`, `ubuntu:24.04`, `debian:12`), every other package | pass | not run |

**The end-to-end scenarios**, at a7aa4d3e:
`test/e2e/run.sh oras auth rbac oci-token cosign tokens raw apt`, with the variables of
[testing.md](../testing.md#a-registry-with-credentials-and-a-prefix) for Forgejo. All eight
pass, `raw.sh` too: its range step got 206 here, where Gitea fails it. `pypi`, `maven` and
`npm` were not run (the machine's disk was nearly full). `ha.sh` was skipped.

**In CI**, PR #826 adds the job `e2e (forgejo)` beside `e2e (gitea)`, not required. It runs
these scenarios but apt, and the provider's integration tests with the spool flag. Since #814
merged, they include the two steps of `TestIntegrationUploads` that read the status.

The failures, by owner:

- **Forgejo's:** the status one byte ahead (item 1), the case-folded tags (item 2), the 1.0
  name grammar (item 4), the streamed `PATCH` after the first write (item 3). The deleted
  blobs (item 5) are Forgejo's choice, not a deviation.
- **Ours, the provider:** the status, corrected by PR #814.
- **Ours, the tests:** the probe's integration test wants `sessionStatus` right on every
  registry. Reindex's apt cell wants an emptied image to stay in `_catalog`, as on
  distribution's filesystem. `TestIntegrationNoCatalog/probe` gives its front no
  `tokenRealmHosts`, so it fails on any registry with a token realm. The interleaved delete
  test wants the blobs to outlive their manifest.

## Upstream

Filed on 2026-11-13, each with curl lines run against Forgejo 17.0.1 and, for Gitea, against
1.27.3 and 28.3.0:

- the upload status one byte ahead, against distribution-spec 1.1.1 ("The `<end-of-range>`
  value is the position of the last uploaded byte"); the same `GET` has no `Location`, which
  that response must have: [forgejo#15258](https://codeberg.org/forgejo/forgejo/issues/15258). Gitea fixed both in go-gitea/gitea#34725
  and #34795; Forgejo's ports of them, forgejo#8198 and #8259, left them out;
- tags lower-cased and resolved without case, against the tag grammar, which keeps case:
  [forgejo#15259](https://codeberg.org/forgejo/forgejo/issues/15259), [go-gitea/gitea#40359](https://github.com/go-gitea/gitea/issues/40359);
- the 1.0 name grammar, where 1.1.1 allows `__` and runs of `-` in a component:
  [forgejo#15260](https://codeberg.org/forgejo/forgejo/issues/15260), [go-gitea/gitea#40360](https://github.com/go-gitea/gitea/issues/40360).
  Gitea fixed it on main in go-gitea/gitea#40363, in no release yet
  ([gitea.md](gitea.md#the-name-grammar-2026-11-15)).

The [deviations](README.md#deviations-from-the-specification) table has them with the spec's
words.

## Commands

The stack as in [Run it](#run-it). With the token in a variable (`T=$(cat
deploy/compose/gitea/.work/forgejo.token)`):

```sh
curl -si localhost:5014/v2/
curl -s localhost:5014/api/forgejo/v1/version
TOKEN=$(curl -s -u "lajstrom:$T" 'localhost:5014/v2/token?service=container_registry&scope=repository:lajstrom-it/curl/status:pull,push' | jq -r .token)
# the upload status: POST …/blobs/uploads/, PATCH with Content-Range: 0-4, then GET the Location
lajstrom registry probe --url http://localhost:5014 --registry-prefix lajstrom-it/ \
  --username lajstrom --password-file deploy/compose/gitea/.work/forgejo.token -v
```

The tests and the scenarios:

```sh
LAJSTROM_TEST_REGISTRY=localhost:5014 LAJSTROM_TEST_REGISTRY_USERNAME=lajstrom LAJSTROM_TEST_REGISTRY_PASSWORD="$T" \
  LAJSTROM_TEST_REGISTRY_PREFIX=lajstrom-it/ go test -race -count=1 -tags integration ./...
LAJSTROM_E2E_REGISTRY=localhost:5014 LAJSTROM_E2E_REGISTRY_USER=lajstrom \
  LAJSTROM_E2E_REGISTRY_PASSWORD_FILE=deploy/compose/gitea/.work/forgejo.token \
  LAJSTROM_E2E_REGISTRY_PREFIX=lajstrom-it/ test/e2e/run.sh oras auth rbac oci-token cosign tokens raw
```

`LAJSTROM_TEST_REGISTRY_SPOOL_UNKNOWN_LENGTH=1` turns `uploads.spoolUnknownLength` on in the
provider's integration tests ([item 3](#what-the-provider-must-do-differently)).
