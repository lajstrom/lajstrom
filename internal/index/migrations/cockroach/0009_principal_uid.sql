-- 0009: the principals of bindings and sessions refer to users by their uid (#182;
-- docs/spec/rbac.md §1): user:<name> becomes user:<uid> where a user has that name, and stays
-- where none has, a binding that grants nothing. The audit log is not rewritten, being
-- append-only: a row of user:<name> gets that name as its principal_name.

UPDATE bindings b SET principal = 'user:' || u.uid::text FROM users u
WHERE b.principal LIKE 'user:%' AND u.name = substr(b.principal, 6);
UPDATE sessions s SET principal = 'user:' || u.uid::text FROM users u
WHERE s.principal LIKE 'user:%' AND u.name = substr(s.principal, 6);
UPDATE audit SET principal_name = substr(principal, 6) WHERE principal LIKE 'user:%' AND principal_name IS NULL;
