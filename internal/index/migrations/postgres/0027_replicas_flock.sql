-- 0027: replicas.flock (#752; docs/spec/index-schema.md, Coordination). The device and inode of
-- the lock file next to the SQLite file whose flock(2) lock the row's process holds, '' when it
-- holds none: a start on SQLite that holds that lock and a fresh row of this boot refuses deletes
-- the row, whatever its host and PID namespace; on Postgres no process holds one and nothing
-- reads it. Expand: the binaries before it insert without it, and their rows are '' (PostgreSQL).

ALTER TABLE replicas ADD COLUMN flock text COLLATE "C" NOT NULL DEFAULT '';
