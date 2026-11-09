-- 0006: auth_version and unique bindings (#45; docs/spec/index-schema.md, rbac.md §3).
-- auth_version is one row, whose version every insert or delete of bindings and every insert,
-- update or delete of repos increments in that write's transaction. A replica reads it before it
-- evaluates a request and reloads its snapshot of the bindings and of the repos' anonymous_read
-- when it moved.

CREATE TABLE auth_version (
    id         integer     PRIMARY KEY CHECK (id = 1),
    version    bigint      NOT NULL,
    updated_at timestamptz NOT NULL
);

INSERT INTO auth_version (id, version, updated_at) VALUES (1, 1, now());

-- The same (principal, role, repo_mask) twice is a conflict (rbac.md §3). A binding made twice
-- before this migration is kept once, the first one.
DELETE FROM bindings WHERE id NOT IN (SELECT MIN(id) FROM bindings GROUP BY principal, role, repo_mask);
CREATE UNIQUE INDEX bindings_unique ON bindings (principal, role, repo_mask);
