-- 0008: the columns of a user's uid (#182; docs/spec/rbac.md §1, index-schema.md, users). Every
-- user gets a uid, a UUID v7 whose time is its created_at. A deleted user will keep its row: its
-- name becomes ~<uid>, which no user name can be, and the name it had goes to deleted_name; the
-- unique constraint of users.name stays, as on SQLite, where dropping it would mean rebuilding
-- users. The audit log gets principal_name, a user's name at the time of an event. The
-- principals of bindings and sessions change to user:<uid> in 0009, with the code that reads them.

ALTER TABLE users ADD COLUMN uid uuid, ADD COLUMN deleted_at timestamptz, ADD COLUMN deleted_name text COLLATE "C";

-- The uid of an existing user: the milliseconds of its created_at in 48 bits, the version 7,
-- random bits, the variant 10, random bits (RFC 9562 §5.7), as 32 hex digits.
UPDATE users SET uid = (
    lpad(to_hex((extract(epoch FROM created_at) * 1000)::bigint), 12, '0') || '7' ||
    substr(md5(gen_random_uuid()::text), 1, 3) || substr('89ab', 1 + floor(random() * 4)::int, 1) ||
    substr(md5(gen_random_uuid()::text), 1, 15)
)::uuid;
ALTER TABLE users ALTER COLUMN uid SET NOT NULL;
CREATE UNIQUE INDEX users_uid ON users (uid);

ALTER TABLE audit ADD COLUMN principal_name text COLLATE "C";
