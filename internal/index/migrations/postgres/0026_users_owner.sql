-- 0026: users.owner (#1020; docs/spec/rbac.md §1, docs/spec/index-schema.md). The owner of a
-- service account, a user of kind service: user:<uid> of a live user, or group:<name>. NULL for
-- every other kind. Expand: the binaries before it read the table by named columns and take a
-- service account for a user without a password, whose tokens work (PostgreSQL).

ALTER TABLE users ADD COLUMN owner text COLLATE "C";
