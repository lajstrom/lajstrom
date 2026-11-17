-- 0036: users.password_failures and users.locked_until, the lockout (#1280; docs/spec/rbac.md §1,
-- Local passwords, docs/spec/index-schema.md, The lockout). The wrong passwords of the user in a
-- row, since its last verified password, its last lock or its last unlock, and the end of its
-- last lock, NULL when it has none. The count starts at 0, so that an upgrade locks no user.
-- Expand only: the binaries before it read users by named columns and neither read nor write
-- the columns, so that their failures count for nothing and a lock refuses no password there
-- (PostgreSQL).

ALTER TABLE users ADD COLUMN password_failures integer NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN locked_until timestamptz;
