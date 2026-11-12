-- 0020: replicas.process (#720; docs/spec/index-schema.md, Coordination). The boot, the PID
-- namespace and the start of the row's process, as its operating system tells them: a start on
-- SQLite that a fresh row of this host refuses deletes the row when that process is gone; on
-- Postgres the rows coexist and nothing reads it. Expand: the binaries before it insert without
-- it, and their rows are '' (PostgreSQL).

ALTER TABLE replicas ADD COLUMN process text COLLATE "C" NOT NULL DEFAULT '';
