-- 0010: proxy repos (ADR 0013; docs/spec/index-schema.md, Proxy and group repos). repos.upstream
-- holds a proxy's settings, NULL on every other kind. proxy_meta is a cache of the upstream's
-- documents, one row per proxy repo, name key and document kind: reindex neither reads nor
-- writes it, and dropping it costs upstream requests and nothing else (SQLite).

ALTER TABLE repos ADD COLUMN upstream TEXT;

CREATE TABLE proxy_meta (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id       INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name          TEXT    NOT NULL,
    kind          TEXT    NOT NULL,
    state         TEXT    NOT NULL,
    content       BLOB,
    content_type  TEXT,
    sha256        TEXT,
    etag          TEXT,
    last_modified TEXT,
    checked_at    TEXT    NOT NULL,
    UNIQUE (repo_id, name, kind)
);
