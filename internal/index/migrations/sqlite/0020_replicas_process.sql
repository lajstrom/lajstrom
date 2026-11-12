-- 0020: replicas.process (#720; docs/spec/index-schema.md, Coordination). The boot, the PID
-- namespace and the start of the row's process, as its operating system tells them: a start on
-- SQLite that a fresh row of this host refuses deletes the row when that process is gone. Expand:
-- the binaries before it insert without it, and their rows are '' (SQLite).

ALTER TABLE replicas ADD COLUMN process TEXT NOT NULL DEFAULT '';
