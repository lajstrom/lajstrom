-- 0015: bootstrap_token, the one-time token that makes the first admin (#440; docs/auth.md,
-- Bootstrap). One row at most: the token that is valid on every replica, as the sha256 of its
-- secret, never the secret. serve makes or replaces it at start while no admin exists, and the
-- exchange that makes the admin sets used_at in its transaction (PostgreSQL).

CREATE TABLE bootstrap_token (
    id         integer     PRIMARY KEY CHECK (id = 1),
    hash       text        COLLATE "C" NOT NULL,
    replica    text        COLLATE "C" NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz
);
