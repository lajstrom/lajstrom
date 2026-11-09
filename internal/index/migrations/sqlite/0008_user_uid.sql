-- 0008: the columns of a user's uid (#182; docs/spec/rbac.md §1, index-schema.md, users). Every
-- user gets a uid, a UUID v7 whose time is its created_at. A deleted user will keep its row: its
-- name becomes ~<uid>, which no user name can be, and the name it had goes to deleted_name; the
-- unique constraint of users.name stays, because dropping it would mean rebuilding users, which
-- would cascade into tokens. The audit log gets principal_name, a user's name at the time of an
-- event. The principals of bindings and sessions change to user:<uid> in 0009, with the code that
-- reads them (SQLite).

ALTER TABLE users ADD COLUMN uid TEXT;
ALTER TABLE users ADD COLUMN deleted_at TEXT;
ALTER TABLE users ADD COLUMN deleted_name TEXT;

-- The uid of an existing user: the milliseconds of its created_at in 48 bits, the version 7,
-- random bits, the variant 10, random bits (RFC 9562 §5.7).
UPDATE users SET uid = (
    SELECT substr(t, 1, 8) || '-' || substr(t, 9, 4) || '-7' || substr(lower(hex(randomblob(2))), 1, 3) || '-' ||
           substr('89ab', 1 + (abs(random()) % 4), 1) || substr(lower(hex(randomblob(2))), 1, 3) || '-' ||
           lower(hex(randomblob(6)))
    FROM (SELECT printf('%012x', CAST(round((julianday(users.created_at) - 2440587.5) * 86400000) AS INTEGER)) AS t)
);
CREATE UNIQUE INDEX users_uid ON users (uid);

ALTER TABLE audit ADD COLUMN principal_name TEXT;
