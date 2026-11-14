-- 0022: webhooks (#77; docs/spec/webhooks.md, docs/spec/index-schema.md, Webhooks). Control
-- plane, backed up, never rebuilt: webhooks holds the URL, the repo mask and the secret
-- encrypted with the master key; webhook_deliveries is the outbox, written in the transaction
-- of each event's audit row, and the delivery log. Expand only (PostgreSQL).

CREATE TABLE webhooks (
    id          bigserial   PRIMARY KEY,
    uid         text        COLLATE "C" NOT NULL,
    mask        text        COLLATE "C" NOT NULL,
    url         text        COLLATE "C" NOT NULL,
    events      jsonb,
    secret_enc  bytea       NOT NULL,
    active      boolean     NOT NULL,
    description text        COLLATE "C" NOT NULL,
    created_by  text        COLLATE "C" NOT NULL,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);

CREATE UNIQUE INDEX webhooks_uid ON webhooks (uid);

CREATE TABLE webhook_deliveries (
    id           bigserial   PRIMARY KEY,
    webhook_id   bigint      NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event_id     text        COLLATE "C" NOT NULL,
    event_type   text        COLLATE "C" NOT NULL,
    payload      bytea       NOT NULL,
    state        text        COLLATE "C" NOT NULL,
    attempts     integer     NOT NULL,
    next_at      timestamptz NOT NULL,
    claimed_by   text        COLLATE "C",
    claimed_at   timestamptz,
    last_at      timestamptz,
    last_status  integer,
    last_error   text        COLLATE "C",
    created_at   timestamptz NOT NULL,
    delivered_at timestamptz
);

CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_at) WHERE state = 'pending';
CREATE INDEX webhook_deliveries_hook ON webhook_deliveries (webhook_id, id);
CREATE INDEX webhook_deliveries_created ON webhook_deliveries (created_at);
