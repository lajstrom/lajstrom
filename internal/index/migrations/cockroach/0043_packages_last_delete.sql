-- 0043: packages.last_delete_at (#1419; docs/spec/index-schema.md, The time of a package's last
-- delete). When a version of the package was last deleted, which raw's Last-Modified takes into
-- account (docs/spec/http-routing.md §3, raw); NULL is no delete known, and there is no
-- backfill. DeleteVersion writes it; reindex does not rebuild it, for the registry keeps no
-- trace of a delete. Expand only: the binaries before it read packages by named columns and
-- neither read nor write the column, so their deletes set nothing (CockroachDB).

ALTER TABLE packages ADD COLUMN IF NOT EXISTS last_delete_at timestamptz;
