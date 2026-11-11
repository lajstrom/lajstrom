-- 0015: bootstrap_token, the one-time token that makes the first admin (#440; docs/auth.md,
-- Bootstrap). One row at most: the token that is valid on every replica, as the sha256 of its
-- secret, never the secret. serve makes or replaces it at start while no admin exists, and the
-- exchange that makes the admin sets used_at in its transaction (SQLite).

CREATE TABLE bootstrap_token (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    hash       TEXT    NOT NULL,
    replica    TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    expires_at TEXT    NOT NULL,
    used_at    TEXT
);
