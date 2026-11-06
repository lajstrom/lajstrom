---
id: M0-04
title: "feat(index): Store interface, migrations, SQLite and Postgres drivers, conformance suite"
milestone: M0
lane: core
size: L
priority: p0
labels: [area/index, kind/feature, contract]
depends: [M0-01]
---
**Goal**

The index database behind one interface, with both engines passing one conformance suite, and
`lajstrom migrate` applying embedded migrations.

**Spec and context**

`docs/spec/index-schema.md`; ADR 0006; `docs/spec/layout.md` §2 (Store).

**Scope**

- `internal/index`: models, `Store`/`Reader`/`Writer`, sentinel errors, `Migrate`, `Tx`.
- Migrations `0001_init.sql` for both dialects with every table of the schema spec (control plane
  and derived), unique constraints and indexes as specified; a tiny embedded runner with a
  `schema_migrations` table.
- `internal/index/sqlite` (modernc, WAL mode, busy timeout) and `internal/index/postgres` (pgx
  via `database/sql`), hand-written SQL.
- `internal/index/storetest`: the conformance suite (CRUD for every model, uniqueness →
  `ErrConflict`, tx rollback, upsert semantics for `versions`, cache invalidation, listing with
  pagination).

**Acceptance criteria**

- [ ] `go test ./internal/index/...` runs the suite on SQLite in-memory; with
      `LAJSTROM_TEST_POSTGRES_DSN` set it also runs on Postgres; CI (M0-10) runs both.
- [ ] `lajstrom migrate --config …` is idempotent and prints applied versions.
- [ ] Inserting a `versions` row that violates `(package_id, tag)` returns `ErrConflict` carrying
      the existing digest (ingest needs it for the 409 message).
- [ ] `DropDerived(ctx)` exists for reindex and tests, and never touches control-plane tables
      (test asserts).
- [ ] docs: index-schema.md updated to the merged column names if they changed.

**Out of scope**

Reindex itself (M1-06), auth tables' logic (M2), proxy tables.

**Pointers**

Keep SQL in `.sql` files or constants per dialect; the only dialect differences expected are
types, `RETURNING`, placeholders (`?` vs `$1`) and upsert syntax.
