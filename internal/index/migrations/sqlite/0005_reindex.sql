-- 0005: reindex (#29; docs/spec/index-schema.md). versions.degraded flags a row written from the
-- annotations alone, its metadata layer missing or unreadable (manifest-profile.md §2).
-- packages.created_at becomes the earliest created_at of the package's versions, which the
-- store keeps from now on; a package without versions keeps its own. reindex_runs is the
-- coordination table of the runs: a row in state running is the lock of its scope, repo_id NULL
-- the scope of every repo (SQLite).

ALTER TABLE versions ADD COLUMN degraded INTEGER NOT NULL DEFAULT 0;

UPDATE packages SET created_at = (SELECT MIN(v.created_at) FROM versions v WHERE v.package_id = packages.id)
WHERE EXISTS (SELECT 1 FROM versions v WHERE v.package_id = packages.id);

CREATE TABLE reindex_runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id      INTEGER REFERENCES repos (id) ON DELETE CASCADE,
    state        TEXT    NOT NULL,
    source       TEXT    NOT NULL,
    principal    TEXT    NOT NULL,
    replica      TEXT    NOT NULL,
    options      TEXT,
    started_at   TEXT    NOT NULL,
    heartbeat_at TEXT    NOT NULL,
    finished_at  TEXT,
    report       TEXT,
    error        TEXT
);

CREATE INDEX reindex_runs_scope ON reindex_runs (COALESCE(repo_id, 0), started_at);
