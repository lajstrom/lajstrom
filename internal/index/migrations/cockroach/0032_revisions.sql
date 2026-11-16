-- 0032: repos.revision, users.revision and webhooks.revision (#796; docs/api.md, Revisions;
-- docs/spec/index-schema.md). The revision of the row: 1 on insert, incremented by every update
-- of it; the API's ETag names it, and a PATCH or DELETE with If-Match is made only while the
-- row has the revision the client read. Expand: the binaries before it neither read nor write
-- it, and their updates leave it as it is (CockroachDB).

ALTER TABLE repos ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT 1;
ALTER TABLE users ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT 1;
ALTER TABLE webhooks ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT 1;
