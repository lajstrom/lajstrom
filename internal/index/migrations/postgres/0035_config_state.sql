-- 0035: config_state, the configuration that serve last recorded with a config.change audit row
-- (#1284; docs/spec/rbac.md §5, docs/spec/index-schema.md). One row at most: the fingerprint of
-- the configuration and each of its keys that counts with its value, never a secret; a start
-- whose fingerprint differs replaces it by the fingerprint it read, in the transaction of its
-- row, so that of replicas that start together one records a change. Expand only: the binaries
-- before it neither read nor write the table (PostgreSQL).

CREATE TABLE config_state (
    id          integer     PRIMARY KEY CHECK (id = 1),
    fingerprint text        COLLATE "C" NOT NULL,
    config      jsonb       NOT NULL,
    release     text        COLLATE "C" NOT NULL,
    host        text        COLLATE "C" NOT NULL,
    recorded_at timestamptz NOT NULL
);
