-- 0001: every table of docs/spec/index-schema.md, control plane and derived (SQLite).
-- Ids are AUTOINCREMENT so that an id is never reused: bindings name deploy tokens by id.
-- Timestamps are UTC RFC 3339 text with six fractional digits, so text order is time order.
-- Booleans are 0 or 1; json is text. The store turns foreign keys on for every connection.

-- Control plane.

CREATE TABLE repos (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    format         TEXT    NOT NULL,
    name           TEXT    NOT NULL,
    registry       TEXT    NOT NULL,
    prefix         TEXT    NOT NULL,
    kind           TEXT    NOT NULL,
    anonymous_read INTEGER NOT NULL,
    policy         TEXT,
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    UNIQUE (format, name)
);

CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL UNIQUE,
    password_hash TEXT,
    kind          TEXT    NOT NULL,
    disabled      INTEGER NOT NULL,
    created_at    TEXT    NOT NULL
);

CREATE TABLE tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL,
    user_id      INTEGER REFERENCES users (id) ON DELETE CASCADE,
    repo_id      INTEGER REFERENCES repos (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    hash         TEXT NOT NULL UNIQUE,
    scopes       TEXT,
    expires_at   TEXT,
    last_used_at TEXT,
    created_at   TEXT NOT NULL,
    revoked_at   TEXT
);
CREATE INDEX tokens_user_id ON tokens (user_id);
CREATE INDEX tokens_repo_id ON tokens (repo_id);

CREATE TABLE bindings (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    principal  TEXT NOT NULL,
    role       TEXT NOT NULL,
    repo_mask  TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX bindings_principal ON bindings (principal);

CREATE TABLE keys (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id     INTEGER REFERENCES repos (id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    public      BLOB NOT NULL,
    private_enc BLOB NOT NULL,
    fingerprint TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    retired_at  TEXT
);
CREATE INDEX keys_repo_id_kind ON keys (repo_id, kind);

CREATE TABLE sessions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id    INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    key        TEXT    NOT NULL,
    principal  TEXT    NOT NULL,
    state      TEXT    NOT NULL,
    files      TEXT,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,
    expires_at TEXT    NOT NULL
);
CREATE INDEX sessions_repo_id_key ON sessions (repo_id, key);

CREATE TABLE audit (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    at        TEXT NOT NULL,
    principal TEXT NOT NULL,
    action    TEXT NOT NULL,
    repo      TEXT NOT NULL,
    subject   TEXT NOT NULL,
    digest    TEXT,
    ip        TEXT NOT NULL,
    details   TEXT
);
CREATE INDEX audit_at ON audit (at);

-- Derived: rebuilt by lajstrom reindex.

CREATE TABLE packages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id    INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    name_oci   TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    UNIQUE (repo_id, name)
);

CREATE TABLE versions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    package_id    INTEGER NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    version       TEXT    NOT NULL,
    variant       TEXT    NOT NULL DEFAULT '',
    tag           TEXT    NOT NULL,
    digest        TEXT    NOT NULL,
    artifact_type TEXT    NOT NULL,
    attrs         TEXT,
    metadata      TEXT,
    created_at    TEXT    NOT NULL,
    indexed_at    TEXT    NOT NULL,
    UNIQUE (package_id, version, variant),
    UNIQUE (package_id, tag)
);
CREATE INDEX versions_digest ON versions (digest);

CREATE TABLE files (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id INTEGER NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    media_type TEXT    NOT NULL,
    digest     TEXT    NOT NULL,
    size       INTEGER NOT NULL,
    attrs      TEXT
);
CREATE INDEX files_version_id_name ON files (version_id, name);
CREATE INDEX files_name ON files (name);

CREATE TABLE index_cache (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id     INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    scope       TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    content     BLOB    NOT NULL,
    sha256      TEXT    NOT NULL,
    rendered_at TEXT    NOT NULL,
    signed_at   TEXT,
    UNIQUE (repo_id, scope, kind)
);

CREATE TABLE oci_repos (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    path    TEXT    NOT NULL,
    seen_at TEXT    NOT NULL,
    UNIQUE (repo_id, path)
);
