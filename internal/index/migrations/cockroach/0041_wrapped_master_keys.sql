-- 0041: the data keys of the master key in a KMS (#1388; docs/spec/index-schema.md, A master key
-- in a KMS). wrapped_master_keys holds a row per data key of keys.masterKeyKMS: its id, the URI of
-- the symmetric KMS key that wraps it, and the KMS's ciphertext of its 32 bytes, never the key in
-- clear. lajstrom admin key wrap inserts a row, or replaces its ciphertext by compare-and-set
-- (--id), and lajstrom admin key drop --master deletes it, each with its audit row in its
-- transaction; every process that names a data key reads its row at its start. Expand only: the
-- binaries before it neither read nor write the table, and refuse keys.masterKeyKMS (CockroachDB).

CREATE TABLE IF NOT EXISTS wrapped_master_keys (
    key_id     text        PRIMARY KEY,
    kms        text        NOT NULL,
    wrapped    bytea       NOT NULL,
    created_at timestamptz NOT NULL,
    wrapped_at timestamptz NOT NULL,
    audit_id   bigint
);
