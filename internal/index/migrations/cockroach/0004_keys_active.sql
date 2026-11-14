-- 0004: one key that is not retired per kind and owner (#41). A repo's apt signing key is made
-- on its first use, and two replicas that make it at once both insert: the index keeps the
-- first, the second insert fails and that replica reads the first one's key. repo_id is NULL
-- for a server key, and NULL is never equal to NULL in a unique index, so the owner is
-- COALESCE(repo_id, 0); repo ids start at 1. A database with two such keys already fails here;
-- retire the younger one (UPDATE keys SET retired_at = ...) and migrate again (CockroachDB).

CREATE UNIQUE INDEX IF NOT EXISTS keys_active ON keys (kind, COALESCE(repo_id, 0)) WHERE retired_at IS NULL;
