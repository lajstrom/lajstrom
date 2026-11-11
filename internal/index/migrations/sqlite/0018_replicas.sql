-- 0018: replicas (#478; docs/spec/index-schema.md, Coordination). One row per serve process on
-- the database: written at its start, beaten every 30 seconds, deleted when it stops, stale 2
-- minutes after its last beat. SQLite serves one replica, whose locks are its process's: a serve
-- that finds a fresh row of another refuses to start. Binaries of earlier releases neither read
-- nor write it (SQLite).

CREATE TABLE replicas (
    id           TEXT    PRIMARY KEY,
    host         TEXT    NOT NULL,
    pid          INTEGER NOT NULL,
    version      TEXT    NOT NULL,
    started_at   TEXT    NOT NULL,
    heartbeat_at TEXT    NOT NULL
);
