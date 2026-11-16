-- 0034: users.last_seen_at (#1283; docs/spec/rbac.md §1, Inactive users, docs/spec/index-schema.md,
-- A user's last activity). The time of the user's last successful authentication, which
-- auth.disableInactiveUsersAfter counts from. Filled for every user that is not deleted: a user
-- with a password from the time of the migration, since its password logins left no record; a
-- user without one, a service account or an OIDC user, from the latest use of its tokens and
-- the latest login of its identities, or NULL, which counts from created_at. Each UPDATE fills
-- only what is NULL or earlier, so that it may run again. Expand only: the binaries before it
-- read users by named columns and neither read nor write the column (SQLite).

ALTER TABLE users ADD COLUMN last_seen_at TEXT;

UPDATE users SET last_seen_at = strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000Z'
WHERE deleted_at IS NULL AND last_seen_at IS NULL AND password_hash IS NOT NULL AND password_hash <> '';

UPDATE users SET last_seen_at = (SELECT MAX(t.last_used_at) FROM tokens t WHERE t.user_id = users.id)
WHERE deleted_at IS NULL AND last_seen_at IS NULL AND (password_hash IS NULL OR password_hash = '');

UPDATE users SET last_seen_at = (SELECT MAX(i.login_at) FROM identities i WHERE i.user_id = users.id)
WHERE deleted_at IS NULL AND (password_hash IS NULL OR password_hash = '')
AND EXISTS (SELECT 1 FROM identities i WHERE i.user_id = users.id AND (users.last_seen_at IS NULL OR i.login_at > users.last_seen_at));
