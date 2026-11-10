-- 0011: fetch_claims, the lock that keeps two replicas from downloading one artifact of a proxy
-- repo (ADR 0013; docs/spec/index-schema.md, Proxy and group repos). Neither control plane nor
-- derived: a row lives for one fetch, beats every 30 seconds and is stale 2 minutes after its
-- last beat (SQLite).

CREATE TABLE fetch_claims (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id      INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    package      TEXT    NOT NULL,
    tag          TEXT    NOT NULL,
    principal    TEXT    NOT NULL,
    replica      TEXT    NOT NULL,
    claimed_at   TEXT    NOT NULL,
    heartbeat_at TEXT    NOT NULL,
    UNIQUE (repo_id, package, tag)
);
