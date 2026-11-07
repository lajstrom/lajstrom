-- 0002: tag_claims, the lock that serialises the writers of one tag across replicas
-- (docs/spec/index-schema.md, Coordination). Neither control plane nor derived: a row lives
-- for one publish, from the claim before the registry write to the confirm after it (SQLite).

CREATE TABLE tag_claims (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id    INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    package    TEXT    NOT NULL,
    tag        TEXT    NOT NULL,
    digest     TEXT    NOT NULL,
    principal  TEXT    NOT NULL,
    claimed_at TEXT    NOT NULL,
    UNIQUE (repo_id, package, tag)
);
