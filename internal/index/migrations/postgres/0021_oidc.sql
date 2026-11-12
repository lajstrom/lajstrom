-- 0021: OIDC login (#69; docs/spec/rbac.md §8, docs/spec/index-schema.md). identities maps
-- the iss and sub of an ID token to a user; identity_groups holds the groups of its latest
-- login, replaced by each login in the transaction that moves auth_version. oidc_logins holds
-- the login requests of the code flow and the device requests, for ten minutes, so that any
-- replica finishes a flow another started; it keeps hashes of the state, the cookie and the
-- device code, never those secrets (PostgreSQL).

CREATE TABLE identities (
    id               bigserial   PRIMARY KEY,
    user_id          bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    issuer           text        COLLATE "C" NOT NULL,
    subject          text        COLLATE "C" NOT NULL,
    created_at       timestamptz NOT NULL,
    login_at         timestamptz NOT NULL,
    groups_expire_at timestamptz
);

CREATE UNIQUE INDEX identities_subject ON identities (issuer, subject);
CREATE INDEX identities_user ON identities (user_id);

CREATE TABLE identity_groups (
    identity_id bigint NOT NULL REFERENCES identities (id) ON DELETE CASCADE,
    name        text   COLLATE "C" NOT NULL,
    PRIMARY KEY (identity_id, name)
);

CREATE TABLE oidc_logins (
    id          bigserial   PRIMARY KEY,
    kind        text        COLLATE "C" NOT NULL,
    state       text        COLLATE "C",
    binding     text        COLLATE "C",
    nonce       text        COLLATE "C",
    verifier    text        COLLATE "C",
    device_id   bigint      REFERENCES oidc_logins (id) ON DELETE CASCADE,
    device_code text        COLLATE "C",
    user_code   text        COLLATE "C",
    status      text        COLLATE "C" NOT NULL,
    user_id     bigint      REFERENCES users (id) ON DELETE CASCADE,
    polled_at   timestamptz,
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL
);

CREATE UNIQUE INDEX oidc_logins_state ON oidc_logins (state) WHERE state IS NOT NULL;
CREATE UNIQUE INDEX oidc_logins_device_code ON oidc_logins (device_code) WHERE device_code IS NOT NULL;
CREATE UNIQUE INDEX oidc_logins_user_code ON oidc_logins (user_code) WHERE user_code IS NOT NULL;
CREATE INDEX oidc_logins_expires ON oidc_logins (expires_at);
