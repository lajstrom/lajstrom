-- 0016: the state of an audit row (#473; docs/spec/index-schema.md, audit). A registry write has
-- its row before the write, pending, and the row is completed after it: ok, failed with the
-- error, or unknown when its writer stopped in between. Additive: the rows before it, and the
-- rows a release before it writes during a rolling update, are ok, written in the transaction
-- of their change. A constant default does not rewrite the table. audit_pending is the sweep's
-- (PostgreSQL).

ALTER TABLE audit ADD COLUMN state text COLLATE "C" NOT NULL DEFAULT 'ok';
ALTER TABLE audit ADD COLUMN error text;

CREATE INDEX audit_pending ON audit (at) WHERE state = 'pending';
