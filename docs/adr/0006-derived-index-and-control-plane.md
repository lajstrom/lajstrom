# 0006. Derived index plus a small control plane; SQLite and Postgres

- Status: accepted
- Date: 2026-11-06
- Deciders: gecube

## Context

ADR 0001 makes the registry the only storage and the database an index. The database still has
to hold things that cannot live in the registry: repository configuration, users, tokens,
permission bindings, signing keys, publication sessions and the audit log. Operators range from
a single binary on a laptop to a Kubernetes deployment with a managed Postgres.

## Considered options

- One table set, all "derived", with configuration in files only.
- Two table sets: derived (rebuildable) and control plane (authoritative, backed up).
- Postgres only; SQLite only; both.

## Decision

Two table sets in one logical schema (`docs/spec/index-schema.md`):

- **Derived**: `packages`, `versions`, `files`, `index_cache`, `oci_repos`. Written only by
  ingest, reindex and delete; `lajstrom reindex` rebuilds them from manifest annotations and
  removes orphans; losing them costs a reindex.
- **Control plane**: `repos`, `users`, `tokens`, `bindings`, `keys`, `sessions`, `audit`. Small,
  authoritative, backed up by the operator, exportable as JSON; optionally snapshotted into the
  registry after the MVP.

Both SQLite (`modernc.org/sqlite`, no cgo) and Postgres (`pgx`) are supported from the first
release, with hand-written SQL, embedded migration files per dialect, and one conformance test
suite that both engines must pass. No ORM.

## Consequences

- Disaster recovery is "restore the small control-plane backup, run reindex"; the documented
  runbook is part of the MVP.
- Every change to derived tables must remain derivable from the registry; a feature that needs
  non-derivable per-version data has to put it in the manifest (annotations or metadata layer).
- Two dialects mean two migration files per change and CI on both engines.
- SQLite makes the single-binary demo and tests trivial; Postgres is the production path.
