-- 0031: the mapping of an encrypted repo (#114; docs/spec/index-schema.md, The mapping of an
-- encrypted repo): per OCI repository, the plaintext digest of every encrypted layer and of every
-- manifest whose layers are encrypted, the stored digest it maps to, and what a read needs to
-- decrypt the layer. Derived: reindex rebuilds it with the repo's keys, and a repo's rows go with
-- it (SQLite).

CREATE TABLE blob_map (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id         INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    path            TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    plain_digest    TEXT    NOT NULL,
    stored_digest   TEXT    NOT NULL,
    size            INTEGER NOT NULL,
    key_fingerprint TEXT    NOT NULL,
    wrapped         TEXT    NOT NULL,
    pubopts         TEXT    NOT NULL,
    indexed_at      TEXT    NOT NULL,
    UNIQUE (repo_id, path, stored_digest)
);
CREATE INDEX blob_map_plain ON blob_map (repo_id, path, plain_digest);
