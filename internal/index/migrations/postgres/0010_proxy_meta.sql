-- 0010: proxy repos (ADR 0013; docs/spec/index-schema.md, Proxy and group repos). repos.upstream
-- holds a proxy's settings, NULL on every other kind. proxy_meta is a cache of the upstream's
-- documents, one row per proxy repo, name key and document kind: reindex neither reads nor
-- writes it, and dropping it costs upstream requests and nothing else (PostgreSQL).

ALTER TABLE repos ADD COLUMN upstream jsonb;

CREATE TABLE proxy_meta (
    id            bigserial   PRIMARY KEY,
    repo_id       bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name          text        COLLATE "C" NOT NULL,
    kind          text        COLLATE "C" NOT NULL,
    state         text        COLLATE "C" NOT NULL,
    content       bytea,
    content_type  text        COLLATE "C",
    sha256        text        COLLATE "C",
    etag          text        COLLATE "C",
    last_modified text        COLLATE "C",
    checked_at    timestamptz NOT NULL,
    UNIQUE (repo_id, name, kind)
);
