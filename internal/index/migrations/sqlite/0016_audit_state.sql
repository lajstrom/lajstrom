-- 0016: the state of an audit row (#473; docs/spec/index-schema.md, audit). A registry write has
-- its row before the write, pending, and the row is completed after it: ok, failed with the
-- error, or unknown when its writer stopped in between. Additive: the rows before it, and the
-- rows a release before it writes during a rolling update, are ok, written in the transaction
-- of their change. audit_pending is the sweep's (SQLite).

ALTER TABLE audit ADD COLUMN state TEXT NOT NULL DEFAULT 'ok';
ALTER TABLE audit ADD COLUMN error TEXT;

CREATE INDEX audit_pending ON audit (at) WHERE state = 'pending';
