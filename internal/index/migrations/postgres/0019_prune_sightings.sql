-- 0019: prune_sightings (#446; docs/spec/index-schema.md, Coordination). A row is the
-- registry's answer 404 MANIFEST_UNKNOWN to the digest of an index row's manifest in the OCI
-- repository path: reindex removes the index row only on another such answer at least 10
-- minutes after first_at. Operational state, neither derived nor backed up: losing a row
-- delays a removal (PostgreSQL).

CREATE TABLE prune_sightings (
    repo_id  bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    path     text        COLLATE "C" NOT NULL,
    digest   text        COLLATE "C" NOT NULL,
    first_at timestamptz NOT NULL,
    last_at  timestamptz NOT NULL,
    PRIMARY KEY (repo_id, path, digest)
);
