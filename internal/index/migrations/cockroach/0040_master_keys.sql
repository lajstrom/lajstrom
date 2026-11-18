-- 0040: the master keys (#1288; docs/spec/index-schema.md, The master key and its replacement).
-- sealing_key, one row at most, is the id of the master key every process seals with, never a
-- key: inserted if absent by the first start of a process that holds one key, and changed by
-- lajstrom admin key rewrap, by compare-and-set on the id it read. replicas.master_keys is the
-- list of the ids of the master keys a serve process holds, NULL for none. Expand only: the
-- binaries before it neither read nor write either, and their rows are NULL (CockroachDB).

CREATE TABLE IF NOT EXISTS sealing_key (
    id       integer     PRIMARY KEY CHECK (id = 1),
    key_id   text        NOT NULL,
    source   text        NOT NULL,
    host     text        NOT NULL,
    audit_id bigint,
    set_at   timestamptz NOT NULL
);

ALTER TABLE replicas ADD COLUMN IF NOT EXISTS master_keys jsonb;
