<!-- The design note that #573 asked for, written on 2026-11-13. Nothing in it is built; it is built only if users ask, and then after a contract issue. -->

# One maven version from several CI jobs: an opt-in shared staging session

Design note of 2026-11-13 for [#573](https://github.com/lajstrom/lajstrom/issues/573). Native
builds deploy their per-platform classifiers from separate runners into one
`groupId:artifactId:version`, and today the first `maven-metadata.xml` commits the version. The
lead decided on 2026-11-13:

1. document the pattern in which one last job deploys the whole version once
   ([quickstart/maven.md](../quickstart/maven.md#one-version-from-several-ci-jobs));
2. write this note on an opt-in shared staging session with an explicit commit, to be built only
   if users ask for it;
3. never one manifest per job, a union of manifests under one version, which cannot be undone.

## Summary

- **Opt-in, per repo.** A maven repo whose policy has `maven.commit: explicit` keeps a version's
  session open after `maven-metadata.xml`. Every writer of the repo adds files to the one open
  session of a `g:a:v`, and a last CI step commits it through the management API. Without the
  key a repo behaves as today.
- **One manifest per version stays.** The commit hands the session's files to ingest's upload,
  as the `PUT` of `maven-metadata.xml` does today. The profile, resolve, `maven-metadata.xml` and
  reindex do not change, and INV-9 holds with the scope it has: a maven version is one commit
  of its publication session.
- **The cost is in the session's rules.** Today a session belongs to one principal and lives
  `maven.sessionTimeout` after its last request, 30 minutes by default. A shared one has several
  writers, lives longer, and lets a writer add a file to a release that another principal
  commits (U-5 of the threat model).
- **About 3 PRs, 3 to 4 days** by the estimate on #573, after a contract issue: http-routing.md
  §3, api.md, the `files` column of `sessions` in index-schema.md, the audit details in rbac.md,
  and `format.Sessions`.

## Today

From `internal/format/maven/deploy.go` and `internal/ingest/sessions.go`, pinned by
`TestDeployTwoJobsOneVersion`:

- A session is keyed by the repo and `g:a:v`, and one principal holds it. Another principal's
  file is `409` while it is open (`Pipeline.Open`); the unique index of open sessions per key
  (migration 0007) keeps it to one.
- With one principal, several jobs' files land in one session, and the first
  `maven-metadata.xml` commits whatever has arrived. A later file opens a new session, whose
  commit is `400` without a POM and `409` with one, since the version exists with other files.
- A file that arrives while the commit runs is `409` (`TestSessionCommitOnce`).
- The version cannot be completed afterwards: its tag is immutable.

## 1. Opening, joining and committing

**The policy.** `maven.commit` takes `metadata` (the default, today's behaviour) or `explicit`.
It is read by ingest, as `maven.sessionTimeout` and `maven.maxSessionFiles` are
(`timeoutOf`, `maxFilesOf`). A policy rather than a header that each deploy sends: the repo's
manager decides once how its versions are published, and a job that forgets a header would
commit a part of a release.

**Opening.** The first file of a `g:a:v` opens the version's session, as today. The row's
`principal` is the one who opened it.

**Joining.** Under `explicit`, `Open` gives any principal with `write` on the repo the open
session of the key instead of `409`. Each file records the principal that added it (`by` in the
`files` column). A file name that the session holds already:

- with the same digest, from anyone: `201`, and nothing changes;
- with another digest, from the principal that added it: replaces it, as a retry does today;
- with another digest, from another principal: `409` naming the file and who added it. Two
  runners that build the main jar or the POM with other bytes must not overwrite each other
  silently.

Two jobs on one shared token are one principal, so the third rule cannot tell them apart; their
files replace each other as today.

**`maven-metadata.xml`.** Under `explicit` its `PUT` answers `201` and commits nothing; it
touches the session, which moves its expiry on. mvn and Gradle take any `2xx` as success.

**Committing.** A call of the management API, since Maven has no verb for it (the Central
Portal's "publish" is an API call too):

```http
POST /api/v1/repos/maven/<repo>/sessions/<groupId>:<artifactId>:<version>/commit
{"expect": ["lib-core-1.2.0-linux-x86_64.jar", "lib-core-1.2.0-osx-aarch_64.jar"]}
```

- `expect`, optional, names files that the session must hold: one missing is `409` naming it,
  and the session stays open. With it, a CI step that runs before the last leg's deploy has
  finished does not publish a part of the release.
- The answers are those of today's commit: `201` with the digest, `200` for the idempotent
  repeat, `409` for other files under the version (the session is aborted), `400` for a refused
  file or a session without a POM, and `404` when there is neither an open session nor the
  version.
- `GET` on the same path lists the files with who added each, and `DELETE` aborts.
- A CI step runs it with `curl` and the deploy token. A `lajstrom admin` command reads the
  database directly, so it suits an operator, not a CI job.

## 2. What an uncommitted session holds, and for how long

**What it holds.** The row: the opener, the state, the expiry, and in `files` each file's name,
digest, size, checksums and `by`, with the refused marks. The bytes are blobs in the OCI
repository of the package, which no manifest references. No manifest, no tag, and no row of
`versions` or `files`: a `GET` of a file of the version is `404`, and `maven-metadata.xml` does
not list it. `maven.maxSessionFiles` bounds the session as a whole, the files of every job
together; 256 holds the largest version on Maven Central, 76 files.

**How long.** Every request of any writer moves the expiry on by `maven.sessionTimeout`
(`1m` to `24h`). A matrix whose legs end an hour apart needs more than the default 30 minutes.
Whether `explicit` should bring a longer default is open (question 5).

**The registry's garbage collection.** A blob that no manifest references is collected after the
registry's delay: Zot's `gcDelay`, one hour by default and on the dev stack
([zot.md](../registries/zot.md#garbage-collection)). A session open longer than that can lose its
files before the commit, which then fails when Build reads the POM back or a layer is missing.
Today this needs a `sessionTimeout` above an hour; a shared session makes long sessions the
normal case. The commit should check the session's blobs before it marks itself committing, and
answer `409` naming the files to upload again (question 6).

**When it expires.** Nothing is published, as today. There is no seal that publishes what
arrived (#53). The blobs are left to the garbage collection.

## 3. RBAC and audit

- **Join:** `write` on the repo, as a file `PUT` today.
- **Commit:** `write`, so that the deploy token of a CI job can run it.
- **Abort:** whether it takes `write` from any writer, or from the opener and the principals that
  added files only (question 4).

The cost is U-5 of the threat model. Today a writer who opens a version's session first can only
hold it. Under `explicit` a writer can add a file to a release that another principal commits,
such as a stray classifier from a branch build or a planted one. This takes nothing that a writer
lacks, since a writer may publish the version first anyway. Still, the file goes out under
someone else's commit. Two things limit it: `expect` names what the committer wants, and `GET`
shows who added what. U-5 must be revisited with the contract.

**Audit.** File `PUT`s are not audited today, and joining is no different. The commit writes the
`push` row of ingest, as every publish does, its principal the committer; `details.contributors`
lists the principals that added files. The manifest's `lajstrom.uploaded-by` holds one principal
(manifest-profile.md §3). Under `explicit` that is the committer, the one who published the
version, while today it is the session's only principal. `DELETE` writes a row `session.abort`.

## 4. The ingest pipeline and the one write path

Everything stays in `internal/ingest`. The API's commit finds the session (`Store.GetSession`)
and calls `Sessions.Commit`, which hands the files to `Pipeline.Upload`: validate, claim the tag,
push, read back, confirm, as every publish does. Neither the API nor the frontend pushes
anything. What changes:

- `Pipeline.Open` reads `maven.commit` and lets another writer join.
- `Pipeline.Add` records `by`. The principal is in the context (`auth.PrincipalFrom`), or `Add`
  takes it as a parameter. The interface's doc says that one principal holds a session, so
  `format.Sessions` changes either way: a contract change.
- `Commit` takes the committer for `uploaded-by`, and checks `expect` and the session's blobs
  before it marks the session committing.
- The maven frontend's `metadata` reads the policy and does not call `Commit` under `explicit`.

What does not change: the lock of `changeSession`, which already serializes Gradle's parallel
uploads, serializes the Adds of several jobs; the committing mark refuses a late file as today;
one manifest per version under `Escape(version)`, with no variant.

## 5. What reindex sees

Nothing of an uncommitted session. Reindex lists the manifests, and a session has none; its blobs
are referenced by nothing. `sessions` is a control-plane table, which reindex neither reads nor
writes and `ResetDerived` leaves alone. Once committed, the version is one manifest like any
other and is rebuilt from its annotations. The contributors are in the audit row only, which is
not derived. A control plane restored from a backup older than a session loses the session; its
jobs deploy again, and a blob the repository holds already is not stored twice.

## 6. Why the union is ruled out

The other option of #573 had each job commit its own manifest under `<version>__<variant>`, as
pypi does with wheels, and served the version as the union of them.

- **It cannot be undone.** Every variant tag written stays in the registries that hold it, since
  tags are append-only. Every reader would have to merge variants forever: resolve,
  `maven-metadata.xml`, reindex, and tools that read the registry without Lajstrom.
- **A release would change under its readers.** Between the first variant and the last, a
  consumer resolves a version that lacks classifiers, and later the same version has them.
  Maven takes a release as fixed, and caches a classifier that was missing.
- **It changes the contract for one CI pattern:** manifest-profile.md, where maven has no
  variant, and ADR 0005. It also needs conflict rules that do not exist: the same POM in two
  variants, one file name with two contents.
- **"Complete" would have no meaning.** No manifest says that the set is whole, while signatures
  and referrers need a subject that holds the release.

A shared session reaches the same end, files from several jobs in one version, and writes one
manifest once.

## 7. Open questions

1. A per-deploy header (`<httpHeaders>` in `settings.xml`), besides or instead of the policy?
2. Is `expect` optional, or required under `explicit`? Should it also take a file count?
3. Should `maven-metadata.xml` under `explicit` answer `202` rather than `201`?
4. Who may abort: any writer, or the opener and the contributors only? Should the commit take its
   own action instead of `write`?
5. The default `maven.sessionTimeout` under `explicit`, and whether 24 hours stays the bound.
6. The registry's GC delay: check the blobs at commit only, or also refuse a `sessionTimeout`
   above a delay that the registry declares (a new capability)?
7. Should the same session collect the platforms of a docker image for server-side stitching
   (architecture.md §7)? The shape is close: a key, several writers, and a commit that writes one
   object.
8. The contract issue would cover http-routing.md §3, api.md (the policy key and the
   endpoints), index-schema.md (`by` in `sessions.files`), rbac.md (`session.abort`,
   `details.contributors`), the threat model's U-5, and `format.Sessions`.
