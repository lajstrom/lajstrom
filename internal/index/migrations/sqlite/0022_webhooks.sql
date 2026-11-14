-- 0022: webhooks (#77; docs/spec/webhooks.md, docs/spec/index-schema.md, Webhooks). Control
-- plane, backed up, never rebuilt: webhooks holds the URL, the repo mask and the secret
-- encrypted with the master key; webhook_deliveries is the outbox, written in the transaction
-- of each event's audit row, and the delivery log. Expand only (SQLite).

CREATE TABLE webhooks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    uid         TEXT    NOT NULL,
    mask        TEXT    NOT NULL,
    url         TEXT    NOT NULL,
    events      TEXT,
    secret_enc  BLOB    NOT NULL,
    active      INTEGER NOT NULL,
    description TEXT    NOT NULL,
    created_by  TEXT    NOT NULL,
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL
);

CREATE UNIQUE INDEX webhooks_uid ON webhooks (uid);

CREATE TABLE webhook_deliveries (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    webhook_id   INTEGER NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event_id     TEXT    NOT NULL,
    event_type   TEXT    NOT NULL,
    payload      BLOB    NOT NULL,
    state        TEXT    NOT NULL,
    attempts     INTEGER NOT NULL,
    next_at      TEXT    NOT NULL,
    claimed_by   TEXT,
    claimed_at   TEXT,
    last_at      TEXT,
    last_status  INTEGER,
    last_error   TEXT,
    created_at   TEXT    NOT NULL,
    delivered_at TEXT
);

CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_at) WHERE state = 'pending';
CREATE INDEX webhook_deliveries_hook ON webhook_deliveries (webhook_id, id);
CREATE INDEX webhook_deliveries_created ON webhook_deliveries (created_at);
