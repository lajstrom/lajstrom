-- 0001: every table of docs/spec/index-schema.md, control plane and derived (PostgreSQL).
-- Every text column is COLLATE "C", so comparison and order are byte order, as in SQLite.
-- Timestamps are timestamptz with microsecond precision; json is jsonb.

-- Control plane.

CREATE TABLE repos (
    id             bigserial   PRIMARY KEY,
    format         text        COLLATE "C" NOT NULL,
    name           text        COLLATE "C" NOT NULL,
    registry       text        COLLATE "C" NOT NULL,
    prefix         text        COLLATE "C" NOT NULL,
    kind           text        COLLATE "C" NOT NULL,
    anonymous_read boolean     NOT NULL,
    policy         jsonb,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    UNIQUE (format, name)
);

CREATE TABLE users (
    id            bigserial   PRIMARY KEY,
    name          text        COLLATE "C" NOT NULL UNIQUE,
    password_hash text        COLLATE "C",
    kind          text        COLLATE "C" NOT NULL,
    disabled      boolean     NOT NULL,
    created_at    timestamptz NOT NULL
);

CREATE TABLE tokens (
    id           bigserial   PRIMARY KEY,
    kind         text        COLLATE "C" NOT NULL,
    user_id      bigint      REFERENCES users (id) ON DELETE CASCADE,
    repo_id      bigint      REFERENCES repos (id) ON DELETE CASCADE,
    name         text        COLLATE "C" NOT NULL,
    hash         text        COLLATE "C" NOT NULL UNIQUE,
    scopes       jsonb,
    expires_at   timestamptz,
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL,
    revoked_at   timestamptz
);
CREATE INDEX tokens_user_id ON tokens (user_id);
CREATE INDEX tokens_repo_id ON tokens (repo_id);

CREATE TABLE bindings (
    id         bigserial   PRIMARY KEY,
    principal  text        COLLATE "C" NOT NULL,
    role       text        COLLATE "C" NOT NULL,
    repo_mask  text        COLLATE "C" NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX bindings_principal ON bindings (principal);

CREATE TABLE keys (
    id          bigserial   PRIMARY KEY,
    repo_id     bigint      REFERENCES repos (id) ON DELETE CASCADE,
    kind        text        COLLATE "C" NOT NULL,
    public      bytea       NOT NULL,
    private_enc bytea       NOT NULL,
    fingerprint text        COLLATE "C" NOT NULL,
    created_at  timestamptz NOT NULL,
    retired_at  timestamptz
);
CREATE INDEX keys_repo_id_kind ON keys (repo_id, kind);

CREATE TABLE sessions (
    id         bigserial   PRIMARY KEY,
    repo_id    bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    key        text        COLLATE "C" NOT NULL,
    principal  text        COLLATE "C" NOT NULL,
    state      text        COLLATE "C" NOT NULL,
    files      jsonb,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_repo_id_key ON sessions (repo_id, key);

CREATE TABLE audit (
    id        bigserial   PRIMARY KEY,
    at        timestamptz NOT NULL,
    principal text        COLLATE "C" NOT NULL,
    action    text        COLLATE "C" NOT NULL,
    repo      text        COLLATE "C" NOT NULL,
    subject   text        COLLATE "C" NOT NULL,
    digest    text        COLLATE "C",
    ip        text        COLLATE "C" NOT NULL,
    details   jsonb
);
CREATE INDEX audit_at ON audit (at);

-- Derived: rebuilt by lajstrom reindex.

CREATE TABLE packages (
    id         bigserial   PRIMARY KEY,
    repo_id    bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name       text        COLLATE "C" NOT NULL,
    name_oci   text        COLLATE "C" NOT NULL,
    created_at timestamptz NOT NULL,
    UNIQUE (repo_id, name)
);

CREATE TABLE versions (
    id            bigserial   PRIMARY KEY,
    package_id    bigint      NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    version       text        COLLATE "C" NOT NULL,
    variant       text        COLLATE "C" NOT NULL DEFAULT '',
    tag           text        COLLATE "C" NOT NULL,
    digest        text        COLLATE "C" NOT NULL,
    artifact_type text        COLLATE "C" NOT NULL,
    attrs         jsonb,
    metadata      jsonb,
    created_at    timestamptz NOT NULL,
    indexed_at    timestamptz NOT NULL,
    UNIQUE (package_id, version, variant),
    UNIQUE (package_id, tag)
);
CREATE INDEX versions_digest ON versions (digest);

CREATE TABLE files (
    id         bigserial PRIMARY KEY,
    version_id bigint    NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    name       text      COLLATE "C" NOT NULL,
    media_type text      COLLATE "C" NOT NULL,
    digest     text      COLLATE "C" NOT NULL,
    size       bigint    NOT NULL,
    attrs      jsonb
);
CREATE INDEX files_version_id_name ON files (version_id, name);
CREATE INDEX files_name ON files (name);

CREATE TABLE index_cache (
    id          bigserial   PRIMARY KEY,
    repo_id     bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    scope       text        COLLATE "C" NOT NULL,
    kind        text        COLLATE "C" NOT NULL,
    content     bytea       NOT NULL,
    sha256      text        COLLATE "C" NOT NULL,
    rendered_at timestamptz NOT NULL,
    signed_at   timestamptz,
    UNIQUE (repo_id, scope, kind)
);

CREATE TABLE oci_repos (
    id      bigserial   PRIMARY KEY,
    repo_id bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    path    text        COLLATE "C" NOT NULL,
    seen_at timestamptz NOT NULL,
    UNIQUE (repo_id, path)
);
