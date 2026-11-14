-- 0008: the columns of a user's uid (#182; docs/spec/rbac.md §1, index-schema.md, users). Every
-- user gets a uid, a UUID v7 whose time is its created_at. A deleted user will keep its row: its
-- name becomes ~<uid>, which no user name can be, and the name it had goes to deleted_name; the
-- unique constraint of users.name stays, as on SQLite, where dropping it would mean rebuilding
-- users. The audit log gets principal_name, a user's name at the time of an event. The
-- principals of bindings and sessions change to user:<uid> in 0009, with the code that reads them.
-- On CockroachDB the table is empty here (0001), and an UPDATE does not see a column added in its
-- transaction, so the column is NOT NULL from the start and the backfill of Postgres's 0008 is not
-- run: rows here would fail the ADD COLUMN, loudly (CockroachDB).

ALTER TABLE users ADD COLUMN IF NOT EXISTS uid uuid NOT NULL, ADD COLUMN IF NOT EXISTS deleted_at timestamptz,
    ADD COLUMN IF NOT EXISTS deleted_name text;
CREATE UNIQUE INDEX IF NOT EXISTS users_uid ON users (uid);

ALTER TABLE audit ADD COLUMN IF NOT EXISTS principal_name text;
