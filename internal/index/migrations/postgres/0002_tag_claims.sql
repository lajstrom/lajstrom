-- 0002: tag_claims, the lock that serialises the writers of one tag across replicas
-- (docs/spec/index-schema.md, Coordination). Neither control plane nor derived: a row lives
-- for one publish, from the claim before the registry write to the confirm after it
-- (PostgreSQL).

CREATE TABLE tag_claims (
    id         bigserial   PRIMARY KEY,
    repo_id    bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    package    text        COLLATE "C" NOT NULL,
    tag        text        COLLATE "C" NOT NULL,
    digest     text        COLLATE "C" NOT NULL,
    principal  text        COLLATE "C" NOT NULL,
    claimed_at timestamptz NOT NULL,
    UNIQUE (repo_id, package, tag)
);
