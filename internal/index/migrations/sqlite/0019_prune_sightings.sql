-- 0019: prune_sightings (#446; docs/spec/index-schema.md, Coordination). A row is the
-- registry's answer 404 MANIFEST_UNKNOWN to the digest of an index row's manifest in the OCI
-- repository path: reindex removes the index row only on another such answer at least 10
-- minutes after first_at. Operational state, neither derived nor backed up: losing a row
-- delays a removal (SQLite).

CREATE TABLE prune_sightings (
    repo_id  INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    path     TEXT    NOT NULL,
    digest   TEXT    NOT NULL,
    first_at TEXT    NOT NULL,
    last_at  TEXT    NOT NULL,
    PRIMARY KEY (repo_id, path, digest)
);
