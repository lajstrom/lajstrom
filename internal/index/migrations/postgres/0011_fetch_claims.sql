-- 0011: fetch_claims, the lock that keeps two replicas from downloading one artifact of a proxy
-- repo (ADR 0013; docs/spec/index-schema.md, Proxy and group repos). Neither control plane nor
-- derived: a row lives for one fetch, beats every 30 seconds and is stale 2 minutes after its
-- last beat (PostgreSQL).

CREATE TABLE fetch_claims (
    id           bigserial   PRIMARY KEY,
    repo_id      bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    package      text        COLLATE "C" NOT NULL,
    tag          text        COLLATE "C" NOT NULL,
    principal    text        COLLATE "C" NOT NULL,
    replica      text        COLLATE "C" NOT NULL,
    claimed_at   timestamptz NOT NULL,
    heartbeat_at timestamptz NOT NULL,
    UNIQUE (repo_id, package, tag)
);
