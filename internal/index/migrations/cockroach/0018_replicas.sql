-- 0018: replicas (#478; docs/spec/index-schema.md, Coordination). One row per serve process on
-- the database: written at its start, beaten every 30 seconds, deleted when it stops, stale 2
-- minutes after its last beat. Postgres serves many replicas, and their rows coexist. Binaries of
-- earlier releases neither read nor write it (CockroachDB).

CREATE TABLE IF NOT EXISTS replicas (
    id           text        PRIMARY KEY,
    host         text        NOT NULL,
    pid          integer     NOT NULL,
    version      text        NOT NULL,
    started_at   timestamptz NOT NULL,
    heartbeat_at timestamptz NOT NULL
);
