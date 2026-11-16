-- 0031: the mapping of an encrypted repo (#114; docs/spec/index-schema.md, The mapping of an
-- encrypted repo): per OCI repository, the plaintext digest of every encrypted layer and of every
-- manifest whose layers are encrypted, the stored digest it maps to, and what a read needs to
-- decrypt the layer. Derived: reindex rebuilds it with the repo's keys, and a repo's rows go with
-- it (PostgreSQL).

CREATE TABLE blob_map (
    id              bigserial   PRIMARY KEY,
    repo_id         bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    path            text        COLLATE "C" NOT NULL,
    kind            text        COLLATE "C" NOT NULL,
    plain_digest    text        COLLATE "C" NOT NULL,
    stored_digest   text        COLLATE "C" NOT NULL,
    size            bigint      NOT NULL,
    key_fingerprint text        COLLATE "C" NOT NULL,
    wrapped         text        COLLATE "C" NOT NULL,
    pubopts         text        COLLATE "C" NOT NULL,
    indexed_at      timestamptz NOT NULL,
    UNIQUE (repo_id, path, stored_digest)
);
CREATE INDEX blob_map_plain ON blob_map (repo_id, path, plain_digest);
