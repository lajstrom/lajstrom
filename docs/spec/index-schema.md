# Spec: index database schema

- Status: normative for the MVP (2026-11-06). Owned by `internal/index`. Column changes go
  through a new migration file; renames of derived tables are free, control-plane tables need an
  issue labelled `contract`.
- Related: [ADR 0001](../adr/0001-oci-registry-as-sole-storage.md),
  [ADR 0006](../adr/0006-derived-index-and-control-plane.md).

Two kinds of tables. **Derived** tables are rebuilt by `lajstrom reindex` from the registry and
may be dropped at any time. **Control plane** tables are small, authoritative and backed up. Two
**coordination** tables, `tag_claims` and `reindex_runs`, are neither: their rows live for one
publish or one reindex run.
SQLite and Postgres share one logical schema; dialect differences are confined to the migration
files (`internal/index/migrations/{sqlite,postgres}/NNNN_*.sql`).

Types below: `id` = integer primary key (SQLite) / `bigserial` (Postgres); `ts` = UTC timestamp
stored as RFC 3339 text in SQLite, `timestamptz` in Postgres; `json` = `text` in SQLite,
`jsonb` in Postgres. Every `json` column is nullable: NULL is "absent or empty" (`policy`,
`scopes`, `files`, `details`, `attrs`, `metadata`), and the store reads it back as nil.

## Control plane

| Table | Columns | Notes |
| --- | --- | --- |
| `repos` | `id`, `format` text, `name` text, `registry` text (config name), `prefix` text, `kind` text (`hosted`; `proxy`/`group` later), `anonymous_read` bool, `policy` json (`maxFileSize`, `maven.sessionTimeout`, …), `created_at` ts, `updated_at` ts | unique `(format, name)` |
| `users` | `id`, `uid` (a UUID v7; `uuid` on Postgres, text on SQLite), `name` text unique, `password_hash` text null, `kind` text (`local`; `oidc` later), `disabled` bool, `created_at` ts, `deleted_at` ts null, `deleted_name` text null | the first user is created by `lajstrom admin user create`. `uid` is unique and never changes (0008). A deleted user keeps its row (rbac.md §1): its `name` becomes `~<uid>`, which no user name can be, so that the name is free for a live user, and the name it had goes to `deleted_name`. (SQLite cannot drop the unique constraint of a column without rebuilding the table, and a rebuild would cascade into `tokens`; this way both dialects keep it.) |
| `tokens` | `id`, `kind` text (`pat`, `deploy`), `user_id` → users null, `repo_id` → repos null, `name` text, `hash` text (sha256 of the secret), `scopes` json (`["read","write"]`), `expires_at` ts null, `last_used_at` ts null, `created_at` ts, `revoked_at` ts null | a PAT acts as its user; a deploy token is a principal of its own bound to one repo |
| `bindings` | `id`, `principal` text (`user:<uid>`; `group:<name>` after the MVP), `role` text (`reader`, `writer`, `maintainer`, `admin`), `repo_mask` text (`apt/*`, `*/payments`, `maven/releases`), `created_at` ts | unique `(principal, role, repo_mask)`: the same binding twice is `409`; rbac.md |
| `keys` | `id`, `repo_id` → repos null (null = server key), `kind` text (`gpg`, `jwt`), `public` blob, `private_enc` blob (encrypted with the master key), `fingerprint` text, `created_at` ts, `retired_at` ts null | apt signing keys; JWT signing key. Unique `keys_active` on `(kind, COALESCE(repo_id, 0)) WHERE retired_at IS NULL`: one key that is not retired per kind and owner, so two replicas that make a repo's first key at once leave one (the second insert is a conflict, and that replica reads the first one's key). A `gpg` row: `public` the OpenPGP public key packets, `private_enc` nonce ‖ AES-256-GCM(private key packets) with the fingerprint (uppercase hex) as additional data |
| `sessions` | `id`, `repo_id`, `key` text (`g:a:v`; `blob-upload:<id>` for an upload of the facade), `principal` text (`user:<uid>`, `token:<id>`), `state` text (`open`, `committed`, `aborted`, `expired`), `files` json (a publication session: `{"name", "version", "files": [{"name", "digest", "size", "checksums"}], "committing"}`; an upload: the backend's session), `created_at` ts, `updated_at` ts, `expires_at` ts | publication sessions (maven; later docker stitching) and the blob uploads of the facade. Unique `sessions_open_key` on `(repo_id, key) WHERE state = 'open'` (0007): two requests that open the session of one version at once end in one, and a change of its files is a transaction that starts with an `UPDATE` of the row, which holds it |
| `audit` | `id`, `at` ts, `principal` text (`user:<uid>`, `token:<id>`, `anonymous`), `principal_name` text null (a user's name at the time of the event, written with the row), `action` text (`push`, `delete`, `repo.create`, `token.create`, …), `repo` text, `subject` text (package/version, or a user's `user:<uid>`), `digest` text null, `ip` text, `details` json | append-only; retention by age later. Rows written before 0008 carry the principal as it was then, `user:<name>`, and that name as `principal_name` |
| `auth_version` | `id` int (always 1), `version` bigint, `updated_at` ts | one row, made by its migration; incremented in the transaction of every insert or delete of `bindings` and every insert, update or delete of `repos`. A replica reads it before it evaluates a request and reloads its snapshot of the bindings and of the repos' `anonymous_read` when it moved (rbac.md §3) |
| `schema_migrations` | `version` int, `applied_at` ts | |

## Coordination

Neither control plane nor derived: a row lives for one publish or one reindex run, so it is
neither backed up nor rebuilt.

| Table | Columns | Notes |
| --- | --- | --- |
| `tag_claims` | `id`, `repo_id` → repos (`ON DELETE CASCADE`), `package` text (the format's spelling, `lajstrom.name`), `tag` text, `digest` text (manifest digest being published), `principal` text, `claimed_at` ts | unique `(repo_id, package, tag)`; written by ingest only |
| `reindex_runs` | `id`, `repo_id` → repos null (`ON DELETE CASCADE`; null: a run of every repo), `state` text (`running`, `done`, `failed`, `abandoned`), `source` text (`cli`, `api`, `reconcile`), `principal` text, `replica` text (host name and process id), `options` json (`prune`, `verify`), `started_at` ts, `heartbeat_at` ts, `finished_at` ts null, `report` json null (layout.md, `reindex.Report`), `error` text null | index `(COALESCE(repo_id, 0), started_at)`; written by reindex only |

A `versions` row exists only after the registry accepted the manifest; the claim in
`tag_claims` is the lock that serialises writers of one tag across replicas, and a claim older
than `ingest.claimTTL` is stale: ingest takes it over, `reindex` and the reconcile loop delete
it. Ingest inserts the claim first in its transaction and reads the version of the tag after,
so a concurrent writer of the tag waits for the first one's commit and then sees its outcome.
A claim with the same digest is shared (two replicas publishing the same bytes); a fresh claim
with another digest is a `409`. `ResetDerived` does not touch claims.

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

## Derived (rebuilt by reindex)

| Table | Columns | Notes |
| --- | --- | --- |
| `packages` | `id`, `repo_id` → repos, `name` text (format spelling), `name_oci` text (escaped path), `created_at` ts (the earliest `created_at` of its versions) | unique `(repo_id, name)`; unique `(repo_id, name_oci)`: one spelling per OCI repository |
| `versions` | `id`, `package_id` → packages, `version` text, `variant` text (`''` when none), `tag` text, `digest` text (manifest digest), `artifact_type` text, `attrs` json (`lajstrom.<format>.*`), `metadata` json null (metadata layer, may be omitted for size), `created_at` ts (from `org.opencontainers.image.created`), `indexed_at` ts (when ingest or reindex last wrote or saw the row), `degraded` bool (written from the annotations alone, the metadata layer missing or unreadable: manifest-profile.md §2) | unique `(package_id, version, variant)`; unique `(package_id, tag)`. The store keeps `packages.created_at` the earliest `created_at` of the package's versions on every version write and delete. Ingest never writes a degraded row; reindex does, and reads it again on every run |
| `files` | `id`, `version_id` → versions, `name` text, `media_type` text, `digest` text (layer), `size` int, `attrs` json (layer annotations) | index `(version_id, name)`; apt pool and maven lookups go by `(repo, name)` → add index on `name` |
| `index_cache` | `id`, `repo_id`, `scope` text (`jammy/main/amd64`, `simple/requests`, `com.example/lib-core`), `kind` text (`Packages`, `Release`, `InRelease`, `simple`, `maven-metadata`), `content` blob, `sha256` text, `rendered_at` ts, `signed_at` ts null | invalidated by ingest, delete, reindex and a key rotation; rebuilt lazily on GET. A signed document (apt `InRelease`, `Release.gpg`) has the `rendered_at` of the document it signs and its own `signed_at` |
| `oci_repos` | `id`, `repo_id`, `path` text (full OCI path incl. prefix), `seen_at` ts | repository list for backends without a catalog; reconcile source |
| `proxy_meta` (later) | upstream metadata cache with TTL | after the MVP |

## Migrations

The migrations are `internal/index/migrations/{sqlite,postgres}/NNNN_<name>.sql`, embedded in
the binary; both directories have the same versions (a test asserts it), `0001_init.sql` creates
the control-plane and derived tables, `0002_tag_claims.sql` the coordination table,
`0003_packages_name_oci.sql` the unique index on `packages (repo_id, name_oci)`,
`0004_keys_active.sql` the unique index `keys_active` (one key not retired per kind and owner),
`0005_reindex.sql` the table `reindex_runs`, the column `versions.degraded`, and
`packages.created_at` set to the earliest `created_at` of each package's versions,
`0006_auth_version.sql` the table `auth_version` with its row and the unique index
`bindings_unique` (a binding made twice before it is kept once, the first).
The runner,
`index.ApplyMigrations`, applies each pending file in a transaction of its own that also records
the version in `schema_migrations` (created when missing). The transaction first takes the
migration lock, `pg_advisory_xact_lock` on Postgres and `BEGIN IMMEDIATE` on SQLite, and reads
`schema_migrations` under it, so replicas that start at once apply each version exactly once.
`lajstrom migrate` and `lajstrom serve` (on start) run it.

## Invariants and rules

- Every row in `packages`, `versions`, `files` corresponds to a manifest that exists in the
  registry with a tag; reindex removes rows whose manifest is gone and adds rows for manifests it
  finds. `index_cache` is dropped entirely at the end of a full reindex.
- Writes to derived tables happen only in `ingest`, `reindex`, and `delete`. Frontends read.
  Ingest claims the tag in `tag_claims` before the registry write, and writes the derived rows
  after the registry accepted the manifest, in one transaction that also releases the claim.
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
  is behind), a tag that names another digest is drift (below), and only a tag the registry
  answers as unknown deletes the row; packages left without versions go with it. Any other
  answer (an error, a timeout) keeps the row and is an error of the report, and a run that cannot
  list the repo's OCI repositories sweeps nothing.
- A tag that names another digest than the index row of that tag is drift: the index keeps its
  digest, the report lists both, nothing is re-pointed (the rule of the reconcile loop, #30).
  A drifted row is the one row whose manifest is not under its tag any more.
- A repo of format `oci` (pass-through, http-routing.md §1) has rows in `oci_repos` only: the
  facade records the repository of every manifest pushed to it, and nothing else is indexed.
- The control plane can be exported with `lajstrom admin export` (JSON) and, after the MVP,
  snapshotted into the registry as `<prefix>_meta/config`.
