-- 0005: reindex (#29; docs/spec/index-schema.md). versions.degraded flags a row written from the
-- annotations alone, its metadata layer missing or unreadable (manifest-profile.md §2).
-- packages.created_at becomes the earliest created_at of the package's versions, which the
-- store keeps from now on; a package without versions keeps its own. reindex_runs is the
-- coordination table of the runs: a row in state running is the lock of its scope, repo_id NULL
-- the scope of every repo (PostgreSQL).

ALTER TABLE versions ADD COLUMN degraded boolean NOT NULL DEFAULT false;

UPDATE packages p SET created_at = m.created_at
FROM (SELECT package_id, MIN(created_at) AS created_at FROM versions GROUP BY package_id) m
WHERE m.package_id = p.id;

CREATE TABLE reindex_runs (
    id           bigserial   PRIMARY KEY,
    repo_id      bigint      REFERENCES repos (id) ON DELETE CASCADE,
    state        text        COLLATE "C" NOT NULL,
    source       text        COLLATE "C" NOT NULL,
    principal    text        COLLATE "C" NOT NULL,
    replica      text        COLLATE "C" NOT NULL,
    options      jsonb,
    started_at   timestamptz NOT NULL,
    heartbeat_at timestamptz NOT NULL,
    finished_at  timestamptz,
    report       jsonb,
    error        text
);

CREATE INDEX reindex_runs_scope ON reindex_runs ((COALESCE(repo_id, 0)), started_at);
