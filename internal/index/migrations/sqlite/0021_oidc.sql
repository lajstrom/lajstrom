-- 0021: OIDC login (#69; docs/spec/rbac.md §8, docs/spec/index-schema.md). identities maps
-- the iss and sub of an ID token to a user; identity_groups holds the groups of its latest
-- login, replaced by each login in the transaction that moves auth_version. oidc_logins holds
-- the login requests of the code flow and the device requests, for ten minutes, so that any
-- replica finishes a flow another started; it keeps hashes of the state, the cookie and the
-- device code, never those secrets (SQLite).

CREATE TABLE identities (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id          INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    issuer           TEXT    NOT NULL,
    subject          TEXT    NOT NULL,
    created_at       TEXT    NOT NULL,
    login_at         TEXT    NOT NULL,
    groups_expire_at TEXT
);

CREATE UNIQUE INDEX identities_subject ON identities (issuer, subject);
CREATE INDEX identities_user ON identities (user_id);

CREATE TABLE identity_groups (
    identity_id INTEGER NOT NULL REFERENCES identities (id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    PRIMARY KEY (identity_id, name)
);

CREATE TABLE oidc_logins (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT    NOT NULL,
    state       TEXT,
    binding     TEXT,
    nonce       TEXT,
    verifier    TEXT,
    device_id   INTEGER REFERENCES oidc_logins (id) ON DELETE CASCADE,
    device_code TEXT,
    user_code   TEXT,
    status      TEXT    NOT NULL,
    user_id     INTEGER REFERENCES users (id) ON DELETE CASCADE,
    polled_at   TEXT,
    created_at  TEXT    NOT NULL,
    expires_at  TEXT    NOT NULL
);

CREATE UNIQUE INDEX oidc_logins_state ON oidc_logins (state) WHERE state IS NOT NULL;
CREATE UNIQUE INDEX oidc_logins_device_code ON oidc_logins (device_code) WHERE device_code IS NOT NULL;
CREATE UNIQUE INDEX oidc_logins_user_code ON oidc_logins (user_code) WHERE user_code IS NOT NULL;
CREATE INDEX oidc_logins_expires ON oidc_logins (expires_at);
