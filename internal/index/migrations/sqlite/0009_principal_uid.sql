-- 0009: the principals of bindings and sessions refer to users by their uid (#182;
-- docs/spec/rbac.md §1): user:<name> becomes user:<uid> where a user has that name, and stays
-- where none has, a binding that grants nothing. The audit log is not rewritten, being
-- append-only: a row of user:<name> gets that name as its principal_name (SQLite).

UPDATE bindings SET principal = 'user:' || (SELECT uid FROM users WHERE users.name = substr(bindings.principal, 6))
WHERE principal LIKE 'user:%' AND EXISTS (SELECT 1 FROM users WHERE users.name = substr(bindings.principal, 6));
UPDATE sessions SET principal = 'user:' || (SELECT uid FROM users WHERE users.name = substr(sessions.principal, 6))
WHERE principal LIKE 'user:%' AND EXISTS (SELECT 1 FROM users WHERE users.name = substr(sessions.principal, 6));
UPDATE audit SET principal_name = substr(principal, 6) WHERE principal LIKE 'user:%' AND principal_name IS NULL;
