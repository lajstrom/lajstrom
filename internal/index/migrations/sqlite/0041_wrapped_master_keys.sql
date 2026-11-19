-- 0041: the data keys of the master key in a KMS (#1388; docs/spec/index-schema.md, A master key
-- in a KMS). wrapped_master_keys holds a row per data key of keys.masterKeyKMS: its id, the URI of
-- the symmetric KMS key that wraps it, and the KMS's ciphertext of its 32 bytes, never the key in
-- clear. lajstrom admin key wrap inserts a row, or replaces its ciphertext by compare-and-set
-- (--id), and lajstrom admin key drop --master deletes it, each with its audit row in its
-- transaction; every process that names a data key reads its row at its start. Expand only: the
-- binaries before it neither read nor write the table, and refuse keys.masterKeyKMS (SQLite).

CREATE TABLE wrapped_master_keys (
    key_id     TEXT PRIMARY KEY,
    kms        TEXT NOT NULL,
    wrapped    BLOB NOT NULL,
    created_at TEXT NOT NULL,
    wrapped_at TEXT NOT NULL,
    audit_id   INTEGER
);
