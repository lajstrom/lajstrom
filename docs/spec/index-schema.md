# Spec: index database schema

- Status: normative for the MVP (2026-11-06). Owned by `internal/index`. Column changes go
  through a new migration file; renames of derived tables are free, control-plane tables need an
  issue labelled `contract`.
- Related: [ADR 0001](../adr/0001-oci-registry-as-sole-storage.md),
  [ADR 0006](../adr/0006-derived-index-and-control-plane.md).

Two kinds of tables. **Derived** tables are rebuilt by `lajstrom reindex` from the registry and
may be dropped at any time. **Control plane** tables are small, authoritative and backed up. The
**coordination** tables, `tag_claims`, `reindex_runs`, `fetch_claims` and `replicas`, are neither:
their rows live for one publish, one reindex run, one fetch or one `serve` process.
`prune_sightings` is neither too: the operational state of prune, whose rows live from one
reindex run to a later one.
SQLite and Postgres share one logical schema; dialect differences are confined to the migration
files (`internal/index/migrations/{sqlite,postgres}/NNNN_*.sql`).

Types below: `id` = integer primary key (SQLite) / `bigserial` (Postgres); `ts` = UTC timestamp
stored as RFC 3339 text in SQLite, `timestamptz` in Postgres; `json` = `text` in SQLite,
`jsonb` in Postgres. Every `json` column is nullable: NULL is "absent or empty" (`policy`,
`scopes`, `files`, `details`, `attrs`, `metadata`), and the store reads it back as nil.

## Control plane

| Table | Columns | Notes |
| --- | --- | --- |
| `repos` | `id`, `format` text, `name` text, `registry` text (config name), `prefix` text, `kind` text (`hosted`, `proxy`, `group`; [below](#proxy-and-group-repos)), `anonymous_read` bool, `policy` json (`maxFileSize`, `maven.sessionTimeout`, …), `upstream` json null, `group` json null, `created_at` ts, `updated_at` ts | unique `(format, name)` |
| `users` | `id`, `uid` (a UUID v7; `uuid` on Postgres, text on SQLite), `name` text unique, `password_hash` text null, `kind` text (`local`; `oidc` later), `disabled` bool, `created_at` ts, `deleted_at` ts null, `deleted_name` text null | the first admin is created through the bootstrap token (`bootstrap_token`) or by `lajstrom admin user create --admin`. `uid` is unique and never changes (0008). A deleted user keeps its row (rbac.md §1): its `name` becomes `~<uid>`, which no user name can be, so that the name is free for a live user, and the name it had goes to `deleted_name`. (SQLite cannot drop the unique constraint of a column without rebuilding the table, and a rebuild would cascade into `tokens`; this way both dialects keep it.) |
| `tokens` | `id`, `kind` text (`pat`, `deploy`), `user_id` → users null, `repo_id` → repos null, `name` text, `hash` text (sha256 of the secret), `scopes` json (`["read","write"]`), `expires_at` ts null, `last_used_at` ts null, `created_at` ts, `revoked_at` ts null | a PAT acts as its user; a deploy token is a principal of its own bound to one repo |
| `bootstrap_token` | `id` int (always 1), `hash` text (sha256 of the secret), `replica` text (host name and process id of the `serve` that made it), `created_at` ts, `expires_at` ts, `used_at` ts null | at most one row (0015): the token that makes the first admin, the same on every replica ([auth.md](../auth.md#bootstrap-the-first-admin)). `serve` makes or replaces it at start while no enabled user has an admin binding, by its old hash only, so that of replicas that start together one makes it; the exchange sets `used_at` in the transaction that creates the admin, by its hash, unused and before `expires_at`, so that it is used once |
| `bindings` | `id`, `principal` text (`user:<uid>`; `group:<name>` after the MVP), `role` text (`reader`, `writer`, `maintainer`, `admin`), `repo_mask` text (`apt/*`, `*/payments`, `maven/releases`), `created_at` ts | unique `(principal, role, repo_mask)`: the same binding twice is `409`; rbac.md |
| `keys` | `id`, `repo_id` → repos null (null = server key), `kind` text (`gpg`, `jwt`), `public` blob, `private_enc` blob (encrypted with the master key), `fingerprint` text, `created_at` ts, `retired_at` ts null | apt signing keys; JWT signing key. Unique `keys_active` on `(kind, COALESCE(repo_id, 0)) WHERE retired_at IS NULL`: one key that is not retired per kind and owner, so two replicas that make a repo's first key at once leave one (the second insert is a conflict, and that replica reads the first one's key). A `gpg` row: `public` the OpenPGP public key packets, `private_enc` nonce ‖ AES-256-GCM(private key packets) with the fingerprint (uppercase hex) as additional data |
| `sessions` | `id`, `repo_id`, `key` text (`g:a:v`; `blob-upload:<id>` for an upload of the facade), `principal` text (`user:<uid>`, `token:<id>`), `state` text (`open`, `committed`, `aborted`, `expired`), `files` json (a publication session: `{"name", "version", "files": [{"name", "digest", "size", "checksums"}], "committing"}`; an upload: `{"name", "location", "id", "offset", "sha256", "hashed", "recheck"}`, the backend's session, the sha256 state of the bytes that passed so far and their count, and whether a chunk failed since), `created_at` ts, `updated_at` ts, `expires_at` ts | publication sessions (maven; later docker stitching) and the blob uploads of the facade. Unique `sessions_open_key` on `(repo_id, key) WHERE state = 'open'` (0007): two requests that open the session of one version at once end in one, and a change of its files is a transaction that starts with an `UPDATE` of the row, which holds it |
| `audit` | `id`, `at` ts, `principal` text (`user:<uid>`, `token:<id>`, `anonymous`), `principal_name` text null (a user's name at the time of the event, written with the row), `action` text (`push`, `delete`, `repo.create`, `token.create`, …), `repo` text, `subject` text (package/version, or a user's `user:<uid>`), `digest` text null, `ip` text, `details` json, `state` text (`ok`, `pending`, `failed`, `unknown`; [below](#the-audit-row-of-a-registry-write)), `error` text null (why a failed row failed, its first 1024 bytes) | rows are never deleted, and only the row of a registry write changes, its state and details, once (below); retention by age later. Rows written before 0008 carry the principal as it was then, `user:<name>`, and that name as `principal_name`; rows written before 0016 are `ok` |
| `auth_version` | `id` int (always 1), `version` bigint, `updated_at` ts | one row, made by its migration; incremented in the transaction of every insert or delete of `bindings` and every insert, update or delete of `repos`. A replica reads it before it evaluates a request and reloads its snapshot of the bindings and of the repos' `anonymous_read` when it moved (rbac.md §3) |
| `schema_migrations` | `version` int, `applied_at` ts, `requires` int (0014; `0` but for a contract migration) | one row per applied migration; [Upgrades](#upgrades) |

### The audit row of a registry write

No transaction spans the registry and the database (#473; [guarantees.md](guarantees.md)
INV-14). A change of the index alone, or of the control plane, appends its row `ok` in its
transaction. A write to the registry (a publish, a proxied ingest, a delete, a journal entry, a
referrer, a push to an `oci` repo) has its row written twice:

- **Before the write**, in a transaction of its own, `pending`: the principal, the action, the
  repo, the subject, the digest, the client address and the request id. It comes after the
  reads that may refuse the write (the claims, the tag in the backend, the other tags of a
  manifest to delete) and before the first request that changes the registry. When it cannot
  be written, the write is not made and the request fails.
- **After the write**, completed: `ok` in the transaction that records the write in the index
  (the version, the journal entries, the `oci_repos` row; a referrer, which has no index row, in
  a transaction of its own), with the details the write learnt (the referrers a delete took,
  `existed` for a publish that another replica confirmed first); or `failed`, in a transaction
  of its own, with the error, when the registry refused or failed the write or the index
  transaction after it failed. The state is the answer: `ok` was a success, `failed` an error.
- **Unknown**: a row still `pending` whose `at` is older than `ingest.claimTTL` is a writer that
  stopped between the two, or whose database failed both. Every reindex run, the reconcile
  loop's included, sets it to `unknown` when it expires the stale tag claims: the registry may or
  may not have the write, and reindex brings the index to what it has. A writer that ends after
  that still completes its row.

`audit` stays control plane: reindex neither reads it nor needs it, `ResetDerived` leaves it, a
backup keeps it. `GET /api/v1/audit` answers the state of each row and selects by it
(`?state=unknown`). Migration `0016_audit_state.sql` adds `state`, `NOT NULL DEFAULT 'ok'`,
`error`, and the partial index `audit_pending` on `(at) WHERE state = 'pending'` of the sweep. It
only adds: a replica of the release before it, during a rolling update, reads the table by
named columns and appends its rows without a state, which are `ok`, as they were written in the
transaction of their change.

## Proxy and group repos

The tables and columns of the kinds `proxy` and `group` ([ADR 0013](../adr/0013-proxy-and-group-repos.md),
[proxy-and-group.md](proxy-and-group.md)), in all three classes. They come with the migration of
their implementation issue.

| Table | Class | Columns | Notes |
| --- | --- | --- | --- |
| `repos` | control plane | `upstream` json null: a proxy's `{"name", "metadataTTL", "negativeTTL", "offline", "allow", "deny"}`; `group` json null: a group's `{"merge"}` | `kind` is set on insert and never updated. `upstream` only on a proxy, `group` only on a group; a group's `registry` is `''` and its `policy` NULL. The upstream's URL and credentials are in the configuration, never in a row |
| `group_members` | control plane | `group_id` → repos (`ON DELETE CASCADE`), `member_id` → repos (`ON DELETE RESTRICT`), `position` int (from 0, the member order) | primary key `(group_id, position)`, unique `(group_id, member_id)`, index `group_members_member` on `(member_id)` (`ListGroupsOf`). Written in the transaction that updates the group's `repos` row, so `auth_version` moves with it; a repo that is a member is not deleted (`409` naming its groups). Migration `0013_groups.sql`, with the column `repos.group` |
| `fetch_claims` | coordination | `id`, `repo_id` → repos (`ON DELETE CASCADE`), `package` text (the package path), `tag` text, `principal` text, `replica` text (host name and process id), `claimed_at` ts, `heartbeat_at` ts | unique `(repo_id, package, tag)`; written by the proxy fetcher only |
| `proxy_meta` | derived, a cache | `id`, `repo_id` → repos (`ON DELETE CASCADE`), `name` text (the plugin's key of the name), `kind` text (the plugin's document kind, npm `packument`), `state` text (`present`, `absent`), `content` blob null, `content_type` text null, `sha256` text null, `etag` text null, `last_modified` text null (the upstream's validators), `checked_at` ts | unique `(repo_id, name, kind)`; the `proxy_meta (later)` of the Derived table |

A `fetch_claims` row is the lock that keeps two replicas from downloading one artifact: the
fetcher inserts it before the download and deletes it after the ingest, or after a failure, in
transactions of its own. It beats every 30 seconds, and a row whose `heartbeat_at` is older than
2 minutes is stale: a fetch of the artifact takes it over, and `reindex` and the reconcile loop
delete it. It is not a `tag_claims` row: the digest is unknown until the download ends, and the
ingest inside the fetch takes its tag claims as every publish does. `ResetDerived` does not touch
it.

`proxy_meta` is a cache of the upstream, not of the registry: reindex neither reads nor writes
it, `ResetDerived` leaves it, it is not backed up, and dropping it costs upstream requests and
nothing else. A row's expiry is not stored: it is `checked_at` plus the repo's TTL of its
state, read when the row is. A write of a row invalidates the repo's `index_cache` rows of the
name in the same transaction. Expired `absent` rows are deleted once a minute, with the expired
sessions.

A proxy repo has `packages`, `versions`, `files`, `index_cache` and `oci_repos` rows as a hosted
repo has, rebuilt by reindex from its manifests; the `attrs` of a proxied version hold its
`lajstrom.origin.*` annotations too. A group has none of them, nor `proxy_meta`, `fetch_claims`
or `tag_claims` rows: reindex and the reconcile loop pass it by, and its documents are merged
from its members' on every request (proxy-and-group.md §8.5). `group_members` is control plane:
a backup of the database keeps it, and reindex neither reads nor writes it.

## Coordination

Neither control plane nor derived: a row lives for one publish, one reindex run or one `serve`
process, or, in `prune_sightings`, from one run to a later one, so it is neither backed up nor
rebuilt.

| Table | Columns | Notes |
| --- | --- | --- |
| `tag_claims` | `id`, `repo_id` → repos (`ON DELETE CASCADE`), `package` text (the package path, `name_oci`), `tag` text (`''` in the claim on the package path), `digest` text (manifest digest being published; in the claim on the package path, the sha256 digest of the spelling, `lajstrom.name`), `principal` text, `claimed_at` ts | unique `(repo_id, package, tag)`; written by ingest only |
| `reindex_runs` | `id`, `repo_id` → repos null (`ON DELETE CASCADE`; null: a run of every repo), `state` text (`running`, `done`, `failed`, `abandoned`), `source` text (`cli`, `api`, `reconcile`), `principal` text, `replica` text (host name and process id), `options` json (`prune`, `verify`), `started_at` ts, `heartbeat_at` ts, `finished_at` ts null, `report` json null (layout.md, `reindex.Report`), `error` text null | index `(COALESCE(repo_id, 0), started_at)`; written by reindex only |
| `replicas` | `id` text (a UUID v7 made at the start), `host` text (host name), `pid` int, `version` text (the binary's), `started_at` ts, `heartbeat_at` ts | primary key `id`; written by `serve` only. Migration `0018_replicas.sql` (#478) |
| `prune_sightings` | `repo_id` → repos (`ON DELETE CASCADE`), `path` text (the OCI repository, as `oci_repos.path`), `digest` text (the manifest digest of an index row), `first_at` ts (the first answer since the row was cleared), `last_at` ts (the latest answer) | primary key `(repo_id, path, digest)`; written by reindex only |

A `versions` row exists only after the registry accepted the manifest and served it back (the
visibility barrier, [registries/README.md](../registries/README.md#consistency)); the claims in
`tag_claims` are the locks that serialise the writers of one OCI repository across replicas,
and a claim older than `ingest.claimTTL` is stale: ingest takes it over, `reindex` and the
reconcile loop delete it. A claim is keyed by the package path, as the registry keys the
manifest. Names that differ only in ASCII case (maven `Com.Example:lib` and `com.example:lib`,
raw `Docs/Readme.MD` and `docs/readme.md`) have one package path, so one OCI repository and one
set of tags, and a package path has one spelling: the first one published
(`packages (repo_id, name_oci)`, #244).

A publish takes two claims, first in its transaction, before the registry write: the claim on
the package path (tag `''`, digest the sha256 digest of the spelling), then the claim on its
tag (the manifest digest). After them it reads the package of the package path and the version
of the tag, so a concurrent writer of the tag, or of another spelling, waits for the first
one's commit and then sees its outcome. Every publish takes the two in this order, so two
publishes never wait on each other crosswise. A claim with the same digest is shared: on a tag,
two replicas publishing the same bytes; on a package path, the publishes of one spelling. A
fresh claim with another digest is a `409`: on a tag, other bytes; on a package path, another
spelling being published. A package of the package path with another name is a `409` naming
that name. A publish refused by a claim or by the package pushes no manifest. An upload of a
frontend whose tag a fresh claim of another digest holds takes that claim's manifest over when
the registry has it with the same name and file layers: the retry of a publish that the
visibility barrier answered `503`, which keeps its claims, built again with another
`org.opencontainers.image.created`.

The transaction that writes the derived rows releases both claims; from then on the package
row holds the package path. A publish that stops before that releases the claim on its tag and
leaves the claim on the package path to go stale, for another publish of the spelling may share
it. A publish that shares a stale claim on a package path makes it again (deletes the row and
inserts its own, in its transaction), so that the claim is not taken over while the publish
runs. `ResetDerived` does not touch claims. A replica of an earlier version keys its claims by
the spelling and takes no claim on the package path. Its rows are released or go stale as
before and need no migration, but during a rolling upgrade the claims of the two versions do
not exclude each other, and the check of the tag in the backend before the push is the one
guard between them.

A `reindex_runs` row in state `running` is the lock that keeps two runs of one repo apart across
replicas and the CLI. Starting a run takes a transaction lock of its own
(`pg_advisory_xact_lock` with the reindex key on Postgres, the write lock of `BEGIN IMMEDIATE`
on SQLite) and, under it, refuses a run when a running row overlaps it: a run of every repo overlaps every run, a run of one repo overlaps a run of every
repo and a run of the same repo. A running row whose `heartbeat_at` is older than the stale bound
(2 minutes; the run beats every 30 seconds) is closed as `abandoned` by the start that finds it,
and the new run starts. `done` is a run that went to the end, whatever its report lists;
`failed` one that stopped on an error of its own (the registry or the database unreachable, the
process shut down). Finishing a run deletes the finished runs of the same scope beyond the ten
newest.

A `replicas` row is a `serve` process on the database: written at its start, after the
migrations, beaten every 30 seconds and deleted when it stops; a row whose `heartbeat_at` is
older than 2 minutes is of a process that is gone, and the next start deletes it. The CLI's
commands write none. On SQLite, which serves one replica (its referrers and journal locks are
locks of the process), the start is a transaction that holds the write lock of the file, and a
row of another replica with a fresh heartbeat refuses it: `serve` exits with an error that
names that replica (id, host, process, version, start, last heartbeat) and when its row goes
stale. A process that crashed leaves its row, so `serve` on the file is refused until the row
is stale, 2 minutes after its last beat at most; an orchestrator that restarts it on failure
starts it then. A replica that stood still for that long finds its row
deleted at its next beat and writes it again; on SQLite, while another replica's row is fresh,
that is refused, and `/-/readyz` answers `503` until it is not. On Postgres the rows of every
replica coexist. At its start and at every beat a replica also compares its clock with the
database's (`DatabaseTime`; [operations.md](../operations.md#clocks)).

A `prune_sightings` row is the registry's answer `404 MANIFEST_UNKNOWN` to the digest of an index
row's manifest (`Provider.StatManifest`, `provider.ErrManifestUnknown`), which reindex waits on
before it removes the row: it removes it only on another such answer at least 10 minutes after
`first_at` (Invariants and rules; guarantees.md, INV-5). A sweep records an answer
(`Writer.RecordSighting`, which keeps `first_at`) and, once it has looked up every row it did not
see, clears the repo's rows it did not record again (`Writer.ClearSightings`): a manifest that read
back, a lookup that failed, a row that is gone. The table is operational state, not derived:
reindex never rebuilds it and never needs it to rebuild the derived tables, `ResetDerived` leaves
it, and it is not backed up. Losing it only delays a removal by one more answer. The rows are
keyed by the registry's manifest, not by an index row, so that one answer counts for every
replica: runs of one repo never overlap (`reindex_runs`), and the 10 minutes keep two runs of
two replicas' loops that follow each other from counting as two answers. Approved by the lead on
2026-11-11 (#446).

The referrers lock has no table. The `sha256-<hex>` tag-schema index of a subject is a
read-modify-write of one tag, and OCI has no compare-and-swap for manifests: two referrers
attached at once through two replicas can each write an index without the other's. Ingest holds
the referrers lock of (OCI repository, subject) for the push of a referrer when the backend has
no referrers API (`Capabilities.Referrers` false), and everything the push does to the index
happens under it (`Store.WithReferrersLock`): on Postgres `pg_advisory_xact_lock` on a key hashed
from both, in a transaction held for the push; on SQLite, which serves one replica, a lock of
the process. With the referrers API the registry keeps the list, and no lock is taken. Approved
by the maintainer on 2026-11-09 (#28).

The same lock is the **tag lock** of a manifest: the referrers lock of its subject when it has
one, else the lock keyed by its own digest. A delete through ingest holds it while it counts the
tags of the manifest and deletes it (http-routing.md §2, DELETE: a delete by tag of a manifest
that other tags name is refused), and a push by tag to an `oci` repo, which passes through to
the backend, holds it for the push; both whether the backend has the referrers API or not. A tag
pushed during a delete is then either counted by it or pushed after it. One operation takes one
such lock, never two, since `WithReferrersLock` holds a connection of the pool, with the one
exception below. Approved by the maintainer on 2026-11-10 (#284).

A delete writes its rows in the index, and its audit row, before it releases its lock, so that
the writer that takes the lock next reads the index without the version (#72). A conditional
delete of an npm version (`format.Condition`, http-routing.md §4) takes two locks, in one order:
first the package's journal lock (the lock keyed by its OCI repository and the empty blob's
digest, [npm](#npm-the-journal-and-its-state-adr-0012) below), then, inside it, the tag lock of
the manifest. Under the journal lock it reads the package's dist-tags, and a dist-tag move, which
checks under the same lock that the version it names exists, comes either before the delete,
which then refuses the version, or after its rows are gone, and is refused itself. **The order
is the journal lock before a tag lock, never the other way**: nothing waits for a journal lock
while it holds a tag lock. A journal append, the delete of a journal entry and reindex's journal
pass take the journal lock alone; a referrer push, a push by tag and every other delete take a
tag lock alone. So two operations never wait on each other crosswise. On Postgres the delete
holds two connections of the pool for the locks and a third for the transaction of its rows, as
a journal append holds one for its lock and one for its transaction: when the pool is
exhausted, a delete that waits for a connection waits until one is free or its request ends.

The **package row** is the last lock of the order. Every version write and delete
(`UpsertVersion`, `DeleteVersion`) locks its package's `packages` row first, in the transaction
it runs in, until that transaction ends (#570): the version writes of one package follow each
other, and `packages.created_at` sees what the writers before committed. A delete takes it in the
transaction of its rows, inside its journal and tag locks; a publish takes it in the transaction
that confirms the version, under no lock of this section; reindex in the transactions of the
rows it writes. No transaction that holds a package row waits for a journal or a tag lock:
`WithReferrersLock` is never called inside `Store.Tx`. So the order is the journal lock, then a
tag lock, then the package row, and a conditional delete that holds the first two waits for the
row only as long as another writer's transaction of the package runs.

## Derived (rebuilt by reindex)

| Table | Columns | Notes |
| --- | --- | --- |
| `packages` | `id`, `repo_id` → repos, `name` text (format spelling), `name_oci` text (escaped path), `created_at` ts (the earliest `created_at` of its versions) | unique `(repo_id, name)`; unique `(repo_id, name_oci)`: one spelling per OCI repository |
| `versions` | `id`, `package_id` → packages, `version` text, `variant` text (`''` when none), `tag` text, `digest` text (manifest digest), `artifact_type` text, `attrs` json (`lajstrom.<format>.*`), `metadata` json null (metadata layer, may be omitted for size), `created_at` ts (from `org.opencontainers.image.created`), `indexed_at` ts (when ingest or reindex last wrote or saw the row), `degraded` bool (written from the annotations alone, the metadata layer missing or unreadable: manifest-profile.md §2) | unique `(package_id, version, variant)`; unique `(package_id, tag)`. The store keeps `packages.created_at` the earliest `created_at` of the package's versions on every version write and delete. Ingest never writes a degraded row; reindex does, and reads it again on every run |
| `files` | `id`, `version_id` → versions, `name` text, `media_type` text, `digest` text (layer), `size` int, `attrs` json (layer annotations) | index `(version_id, name)`; apt pool and maven lookups go by `(repo, name)` → add index on `name` |
| `index_cache` | `id`, `repo_id`, `scope` text (`jammy/main/amd64`, `simple/requests`, `com.example/lib-core`), `kind` text (`Packages`, `Release`, `InRelease`, `simple`, `maven-metadata`), `content` blob, `sha256` text, `rendered_at` ts, `signed_at` ts null | invalidated by ingest, delete, reindex and a key rotation; rebuilt lazily on GET, and stored only while the repo's generation is the one the render read ([below](#the-generation-of-a-repos-documents)). A signed document (apt `InRelease`, `Release.gpg`) has the `rendered_at` of the document it signs and its own `signed_at` |
| `index_cache_generations` | `repo_id` → repos (primary key, `ON DELETE CASCADE`), `generation` bigint | the generation of the repo's `index_cache`; no row is generation 0. Not rebuilt: `ResetDerived` increments it, as every invalidation does. Migration `0014_index_cache_generations.sql` (#477) |
| `oci_repos` | `id`, `repo_id`, `path` text (full OCI path incl. prefix), `seen_at` ts | repository list for backends without a catalog; reconcile source |
| `proxy_meta` (later) | upstream metadata cache with TTL | after the MVP |

### The generation of a repo's documents

A document is rendered lazily, in a transaction that reads what it renders from and then stores
it; an invalidation is part of the transaction of the change. On Postgres, in read committed,
a render that began before an invalidation could store its document after it, and that stale
document stayed until the next invalidation of the repo. So every invalidation of a repo's
`index_cache` (`InvalidateIndexCache`, of one scope or of all, and `ResetDerived`) first
increments the repo's row of `index_cache_generations`, then deletes the documents. A render
reads the generation before anything it renders from (`IndexCacheGeneration`), and the store
of its documents (`PutIndexCache`) first writes the generation row as it is, which takes the
row's lock, and stores them only when the generation is still the one the render read;
otherwise the render is answered and not stored. The row's lock orders the two: a store waits
for an invalidation that holds it and then reads its generation, and an invalidation that comes
after a store waits for it, so that its `DELETE` sees the stored rows. On SQLite every
transaction holds the write lock from its start, and the two never interleave.

The generation is per repo: an invalidation of one scope also refuses the store of a render of
another scope that it overlapped, which renders again on the next request. During a rolling
update from a release before 0017, the invalidations of the older replicas do not increment it,
and their renders do not check it.

### npm: the journal and its state (ADR 0012)

Two more derived tables hold what changes on an npm package: its dist-tags and the deprecation
of its versions, kept in the registry as a journal of manifests (manifest-profile.md §8).

| Table | Columns | Notes |
| --- | --- | --- |
| `journal` | `id`, `package_id` → packages (`ON DELETE CASCADE`), `seq` int, `tag` text (`_j…`), `digest` text (the entry's manifest digest), `artifact_type` text, `attrs` json (the entry's `lajstrom.npm.*` annotations), `created_at` ts (from `org.opencontainers.image.created`), `indexed_at` ts | unique `(package_id, seq)`; unique `(package_id, tag)`. One row per journal entry in the registry |
| `journal_state` | `id`, `package_id` → packages (`ON DELETE CASCADE`), `kind` text (`dist-tag`, `deprecated`), `key` text (the dist-tag; the version), `value` text (the version; the message), `seq` int (the entry that set it) | unique `(package_id, kind, key)`. The current dist-tags and deprecations: the fold of the package's `journal` rows in `seq` order |

- Both are written by the journal writer of ingest, by reindex and by delete, never by a
  frontend. The writer inserts the rows of its entries, updates the package's `journal_state` and
  invalidates its documents in one transaction, before it releases the package's journal lock:
  the referrers lock (Coordination) keyed by the package's OCI repository and the empty blob's
  digest, which no manifest has. The order of a package's entries is the order of that lock.
  Each entry is claimed in `tag_claims` under its `_j<seq>` tag (the package path, the entry's
  digest) before its push, and the transaction of its row releases the claim. An append that
  finds a claim of the next entry, which the registry does not serve, reads it back and adopts
  it, or answers `503` until the claim is stale; on a strongly consistent registry the entry was
  never pushed and the claim is taken over.
- Reindex marks and sweeps `journal` as it does `versions`: a `_j` tag that names the digest of
  its row is seen without a read, a row whose manifest the registry answers as unknown is
  deleted under the rule of `versions` (Invariants and rules).
  Then it replaces the package's `journal_state` with the replay of its rows, in one transaction.
  The test of #307 drops both tables, runs reindex and compares.
- The state is never a column of `versions`: a version row is its manifest's, the state is the
  journal's. A packument (http-routing.md, npm) reads both; its `_rev` is computed from `journal`
  and `versions` and needs no column.
- A package's rows go with it: both tables refer to `packages` with `ON DELETE CASCADE`, so
  `DeletePackage`, `DeleteEmptyPackages` and `ResetDerived` take them, and reindex rebuilds
  them. When its last version is deleted outside an unpublish of the package, its entries stay
  in the registry as orphans, which reindex reports (`orphanJournal`) and the next append to
  that name adopts. Both tables come with the migration of #307.
- `index_cache` holds two documents per npm package: scope the package name (`@acme/widgets`),
  kinds `packument` and `packument-abbreviated`, rendered together. A publish or a delete of a
  version of the package, each journal append and reindex invalidate them: at least the
  package's scope; the whole repo's, as the delete path does, is correct too. A group's
  packuments are ADR 0013's.

## Migrations

The migrations are `internal/index/migrations/{sqlite,postgres}/NNNN_<name>.sql`, embedded in
the binary; both directories have the same versions (a test asserts it), `0001_init.sql` creates
the control-plane and derived tables, `0002_tag_claims.sql` the coordination table,
`0003_packages_name_oci.sql` the unique index on `packages (repo_id, name_oci)`,
`0004_keys_active.sql` the unique index `keys_active` (one key not retired per kind and owner),
`0005_reindex.sql` the table `reindex_runs`, the column `versions.degraded`, and
`packages.created_at` set to the earliest `created_at` of each package's versions,
`0006_auth_version.sql` the table `auth_version` with its row and the unique index
`bindings_unique` (a binding made twice before it is kept once, the first),
`0014_schema_requires.sql` the column `schema_migrations.requires` ([Upgrades](#upgrades)),
`0017_index_cache_generations.sql` the table `index_cache_generations`, `0018_replicas.sql`
the table `replicas`, and `0019_prune_sightings.sql` the table `prune_sightings`.
The runner,
`index.ApplyMigrations`, applies each pending file in a transaction of its own that also records
the version in `schema_migrations` (created when missing). The transaction first takes the
migration lock, `pg_advisory_xact_lock` on Postgres and `BEGIN IMMEDIATE` on SQLite, and reads
`schema_migrations` under it, so replicas that start at once apply each version exactly once.
`lajstrom migrate` and `lajstrom serve` (on start) run it. When no migration is pending, it
runs no statement that changes the schema. A migration file is never changed once a release
has it: the runner records only its version, so a fix is a new migration.

## Upgrades

During a rolling update the replicas of two releases share one database: release N+1 (its
first replica, or `lajstrom migrate`) migrates the schema while the replicas of release N still
serve on it, and a rollback runs release N on the schema of N+1. So every migration is one of
two kinds.

**Expand** (the default): release N runs on the schema after it. It adds a table, an index, or
a column that is nullable or has a default the database fills; it adds a constraint that every
row release N writes meets; it fills or rewrites rows only where release N reads the result as
it read the rows before. A unique index that release N's inserts may violate, or a backfill of
values release N does not understand (0009 rewrote `user:<name>` to `user:<uid>`), is not
expand.

**Contract**: everything else: a dropped or renamed table or column, a changed type, a column
made `NOT NULL`, a constraint that release N's writes do not meet. A contract migration removes
only what the release before it no longer uses, so it comes at least one release after the
change that stopped using it, and its leading comment has the line

```sql
-- lajstrom:requires NNNN
```

where NNNN is the first migration of the release that stopped using what it removes: every
binary that knows NNNN runs on the schema after it, and no binary that uses it knows NNNN. A
release that stops using a table or column and has no migration of its own adds one that only
records its version (a file with a comment only). A rename is an added column, written beside
the old one for as long as a release that reads the old one may run, and a contract migration
that drops the old one later. The runner records NNNN in
`schema_migrations.requires`, so a binary that does not know the migration knows what it
requires. Both dialects declare the same (a test asserts it).

**What a binary tolerates.** So that release N runs on the schema of N+1, and N+1 on the rows
N writes during the update:

- Statements name their columns: no `SELECT *`, no `INSERT` without a column list. Tables,
  columns and indexes a binary does not know are left alone.
- A value it does not know in a column of a fixed set (`kind`, `state`, `role`, `source`, a
  principal's prefix) fails the request that reads the row, or skips the row, and never stops
  the replica or rewrites the row.
- A field added to a `json` column has a meaning when absent: a replica of release N rewrites
  the column (`repos.policy`, `sessions.files`) without it.
- Release N+1 reads rows that N inserts after the migration: a column N+1 adds is NULL or its
  default in them, a backfill in the migration does not cover them, and N+1 reads them as N
  meant them.

**What a binary does with the schema it finds.** `index.ApplyMigrations` reads
`schema_migrations` first, at the start of `serve` and in `lajstrom migrate`:

| The database | What happens |
| --- | --- |
| no migrations | every migration is applied |
| pending migrations, none requiring more than the schema's newest version | they are applied |
| a pending migration requires more than the schema's newest version | nothing is applied, `ErrBreakingMigration`: the replicas serving on the schema may not know NNNN. `serve` on Postgres and `lajstrom migrate` refuse, exit status 1; `lajstrom migrate --allow-breaking` applies it, once the operator has stopped them; `serve` on SQLite applies it, for it is the only replica |
| current | nothing, and no statement that changes the schema |
| versions the binary does not know, none requiring more than its newest | it runs: `serve` logs a warning, `migrate` says so |
| versions the binary does not know, one requiring more than its newest | nothing is applied, `ErrSchemaTooNew`: `serve` and `migrate` refuse with exit status 1 and name the migration a binary must know |

There are no down migrations: a rollback is the binary of the release before on the newer
schema, which expand migrations allow; to a release older than the one a contract migration
requires, it is a restore of the backup taken before the upgrade. The migrations 0001 to 0014
require nothing; binaries built before 0014 do not check the schema. `serve` migrating on start
is the default, enough for one node; with several replicas, `lajstrom migrate` of the new
release runs once before the rolling update ([operations.md](../operations.md#upgrades)). A release with a
contract migration says so in its notes ([release.md](../release.md#migrations-in-the-notes)).
`TestUpgrade` runs the migrations of four releases against one database on both engines: an
expand migration, the marker, a contract migration, a rollback, and an upgrade that skips a
release.

## Invariants and rules

- Every row in `packages`, `versions`, `files` corresponds to a manifest that exists in the
  registry with a tag; reindex removes rows whose manifest is gone and adds rows for manifests it
  finds. `index_cache` is dropped entirely at the end of a full reindex.
- Writes to derived tables happen only in `ingest`, `reindex`, and `delete`. Frontends read.
  Ingest claims the package path and the tag in `tag_claims` before the registry write, and
  writes the derived rows after the registry accepted the manifest, in one transaction that
  also releases both claims.
- Unique constraints implement the append-only tag rule: an insert that violates
  `(package_id, tag)` with a different digest is a `409`.
- A foreign manifest (no `lajstrom.schema` annotation) under a managed path is recorded in
  `oci_repos` only and reported by `reindex --report`, never indexed. A manifest with `subject`
  is a referrer, not a foreign manifest: reindex counts it and reports it as an orphan when no
  version has its subject's digest; an image index under a `sha256-<hex>` tag is the tag-schema
  referrers index (manifest-profile.md §6). Neither is stored before the attachments of #89.
- Reindex never starts from `ResetDerived`: it marks and sweeps. Every row it writes or sees gets
  the run's time in `indexed_at`; a tag whose manifest has the digest of the index row, in a row
  that is not degraded, is seen without being read again (the same digest is the same bytes,
  checked when it was written), unless the run verifies. At the end, each row of the repo not
  seen since the run started is looked up by its tag in the registry once more: a tag that
  names the row's digest keeps it (written after the listing, possibly by a replica whose clock
  is behind), a tag that names another digest is drift (below), and a tag the registry does not
  resolve asks for the manifest by its digest (`Provider.StatManifest`). Only `404
  MANIFEST_UNKNOWN` counts as unknown, and the row goes only on the second such answer at least
  10 minutes after the first (`prune_sightings`, Coordination) and when its `indexed_at` is at
  least `reconcile.graceAge` old (guarantees.md, INV-5); packages left without versions go with
  it. Any other answer (`NAME_UNKNOWN`, a `404` without the code, a `5xx`, `401`, `403`, a
  timeout) keeps the row and is an error of the report, and a run that cannot list the repo's
  OCI repositories sweeps nothing. An `oci_repos` row goes when no run has seen its repository
  for `reconcile.graceAge`; a sweep that finds a version's manifest records its repository
  again.
- A tag that names another digest than the index row of that tag is drift: the index keeps its
  digest, the report lists both, nothing is re-pointed (the rule of the reconcile loop, #30).
  A drifted row is the one row whose manifest is not under its tag any more.
- A repo of format `oci` (pass-through, http-routing.md §1) has rows in `oci_repos` only: the
  facade records the repository of every manifest pushed to it, and nothing else is indexed.
- The control plane can be exported with `lajstrom admin export` (JSON) and, after the MVP,
  snapshotted into the registry as `<prefix>_meta/config`.
