-- 0016: the state of an audit row (#473; docs/spec/index-schema.md, audit). A registry write has
-- its row before the write, pending, and the row is completed after it: ok, failed with the
-- error, or unknown when its writer stopped in between. Additive: the rows before it, and the
-- rows a release before it writes during a rolling update, are ok, written in the transaction
-- of their change. A constant default does not rewrite the table. audit_pending is the sweep's
-- (CockroachDB).

ALTER TABLE audit ADD COLUMN IF NOT EXISTS state text NOT NULL DEFAULT 'ok';
ALTER TABLE audit ADD COLUMN IF NOT EXISTS error text;

CREATE INDEX IF NOT EXISTS audit_pending ON audit (at) WHERE state = 'pending';
