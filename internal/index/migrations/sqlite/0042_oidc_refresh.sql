-- 0042: asking the IdP again about each OIDC login (#1282; docs/spec/rbac.md §8, Asking the IdP
-- again; docs/spec/index-schema.md, Coordination). oidc_refresh holds a row per OIDC login that
-- keeps a refresh token: the token sealed with the master key, never in clear; the login request
-- until its token is minted, then that token; when the IdP last confirmed the login, when it is
-- due again, and the claim of the replica that refreshes it. Expand only: the binaries before it
-- neither read nor write the table, and refuse auth.oidc.refresh (SQLite).

CREATE TABLE oidc_refresh (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    login_id      INTEGER,
    token_id      INTEGER REFERENCES tokens (id) ON DELETE CASCADE,
    identity_id   INTEGER NOT NULL REFERENCES identities (id) ON DELETE CASCADE,
    refresh_enc   BLOB    NOT NULL,
    created_at    TEXT    NOT NULL,
    refreshed_at  TEXT    NOT NULL,
    next_at       TEXT    NOT NULL,
    failures      INTEGER NOT NULL DEFAULT 0,
    claimed_by    TEXT,
    claimed_until TEXT
);

CREATE UNIQUE INDEX oidc_refresh_token ON oidc_refresh (token_id) WHERE token_id IS NOT NULL;
CREATE UNIQUE INDEX oidc_refresh_login ON oidc_refresh (login_id) WHERE login_id IS NOT NULL;
CREATE INDEX oidc_refresh_next ON oidc_refresh (next_at);
