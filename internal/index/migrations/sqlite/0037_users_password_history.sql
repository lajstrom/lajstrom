-- 0037: users.must_change and users.password_history (#1281; docs/spec/rbac.md §1, Local
-- passwords, docs/spec/index-schema.md, A password's history). Whether the user's password must
-- be changed before it is used for anything else, and the argon2id hashes of its three passwords
-- before the current one, a JSON list in the PHC string format, the latest first, NULL when it
-- has none. Every user starts with neither: the first change after it compares the new password
-- with the current one only. Expand only: the binaries before it read users by named columns and
-- neither read nor write the columns, so that a password whose must_change is set works there,
-- and a password set there leaves both as they were (SQLite).

ALTER TABLE users ADD COLUMN must_change INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN password_history TEXT;
