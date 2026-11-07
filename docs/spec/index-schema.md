# Spec: index database schema

- Status: normative for the MVP (2026-11-06). Owned by `internal/index`. Column changes go
  through a new migration file; renames of derived tables are free, control-plane tables need an
  issue labelled `contract`.
- Related: [ADR 0001](../adr/0001-oci-registry-as-sole-storage.md),
  [ADR 0006](../adr/0006-derived-index-and-control-plane.md).

Two kinds of tables. **Derived** tables are rebuilt by `lajstrom reindex` from the registry and
may be dropped at any time. **Control plane** tables are small, authoritative and backed up. One
**coordination** table, `tag_claims`, is neither: its rows live for one publish.
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
| `users` | `id`, `name` text unique, `password_hash` text null, `kind` text (`local`; `oidc` later), `disabled` bool, `created_at` ts | the first user is created by `lajstrom admin user create` |
| `tokens` | `id`, `kind` text (`pat`, `deploy`), `user_id` → users null, `repo_id` → repos null, `name` text, `hash` text (sha256 of the secret), `scopes` json (`["read","write"]`), `expires_at` ts null, `last_used_at` ts null, `created_at` ts, `revoked_at` ts null | a PAT acts as its user; a deploy token is a principal of its own bound to one repo |
| `bindings` | `id`, `principal` text (`user:<name>`, `group:<name>`, `token:<id>`, `anonymous`), `role` text (`reader`, `writer`, `maintainer`, `admin`), `repo_mask` text (`apt/*`, `*/payments`, `maven/releases`), `created_at` ts | rbac.md |
| `keys` | `id`, `repo_id` → repos null (null = server key), `kind` text (`gpg`, `jwt`), `public` blob, `private_enc` blob (encrypted with the master key), `fingerprint` text, `created_at` ts, `retired_at` ts null | apt signing keys; JWT signing key |
| `sessions` | `id`, `repo_id`, `key` text (`g:a:v`), `principal` text, `state` text (`open`, `committed`, `aborted`, `expired`), `files` json (uploaded descriptors), `created_at` ts, `updated_at` ts, `expires_at` ts | publication sessions (maven; later docker stitching) |
| `audit` | `id`, `at` ts, `principal` text, `action` text (`push`, `delete`, `repo.create`, `token.create`, …), `repo` text, `subject` text (package/version or user), `digest` text null, `ip` text, `details` json | append-only; retention by age later |
| `schema_migrations` | `version` int, `applied_at` ts | |

## Coordination

Neither control plane nor derived: a row lives for one publish, so it is neither backed up nor
rebuilt.

| Table | Columns | Notes |
| --- | --- | --- |
| `tag_claims` | `id`, `repo_id` → repos (`ON DELETE CASCADE`), `package` text (the format's spelling, `lajstrom.name`), `tag` text, `digest` text (manifest digest being published), `principal` text, `claimed_at` ts | unique `(repo_id, package, tag)`; written by ingest only |

A `versions` row exists only after the registry accepted the manifest; the claim in
`tag_claims` is the lock that serialises writers of one tag across replicas, and a claim older
than `ingest.claimTTL` is stale: ingest takes it over, `reindex` and the reconcile loop delete
it. Ingest inserts the claim first in its transaction and reads the version of the tag after,
so a concurrent writer of the tag waits for the first one's commit and then sees its outcome.
A claim with the same digest is shared (two replicas publishing the same bytes); a fresh claim
with another digest is a `409`. `ResetDerived` does not touch claims.

## Derived (rebuilt by reindex)

| Table | Columns | Notes |
| --- | --- | --- |
| `packages` | `id`, `repo_id` → repos, `name` text (format spelling), `name_oci` text (escaped path), `created_at` ts | unique `(repo_id, name)`; unique `(repo_id, name_oci)`: one spelling per OCI repository |
| `versions` | `id`, `package_id` → packages, `version` text, `variant` text (`''` when none), `tag` text, `digest` text (manifest digest), `artifact_type` text, `attrs` json (`lajstrom.<format>.*`), `metadata` json null (metadata layer, may be omitted for size), `created_at` ts (from `org.opencontainers.image.created`), `indexed_at` ts | unique `(package_id, version, variant)`; unique `(package_id, tag)` |
| `files` | `id`, `version_id` → versions, `name` text, `media_type` text, `digest` text (layer), `size` int, `attrs` json (layer annotations) | index `(version_id, name)`; apt pool and maven lookups go by `(repo, name)` → add index on `name` |
| `index_cache` | `id`, `repo_id`, `scope` text (`jammy/main/amd64`, `simple/requests`, `com.example/lib-core`), `kind` text (`Packages`, `Release`, `InRelease`, `simple`, `maven-metadata`), `content` blob, `sha256` text, `rendered_at` ts, `signed_at` ts null | invalidated by ingest, delete and reindex; rebuilt lazily on GET |
| `oci_repos` | `id`, `repo_id`, `path` text (full OCI path incl. prefix), `seen_at` ts | repository list for backends without a catalog; reconcile source |
| `proxy_meta` (later) | upstream metadata cache with TTL | after the MVP |

## Migrations

The migrations are `internal/index/migrations/{sqlite,postgres}/NNNN_<name>.sql`, embedded in
the binary; both directories have the same versions (a test asserts it), `0001_init.sql` creates
the control-plane and derived tables, `0002_tag_claims.sql` the coordination table,
`0003_packages_name_oci.sql` the unique index on `packages (repo_id, name_oci)`. The runner,
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
  `oci_repos` only and reported by `reindex --report`, never indexed.
- The control plane can be exported with `lajstrom admin export` (JSON) and, after the MVP,
  snapshotted into the registry as `<prefix>_meta/config`.
