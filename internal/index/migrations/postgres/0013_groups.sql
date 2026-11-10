-- 0013: group repos (ADR 0013; docs/spec/index-schema.md, Proxy and group repos). repos.group
-- holds a group's settings but its members, {"merge"}, NULL on every other kind. group_members
-- holds the members in order: a group's rows go with it, and a repo that is a member of a group
-- is not deleted (PostgreSQL).

ALTER TABLE repos ADD COLUMN "group" jsonb;

CREATE TABLE group_members (
    group_id  bigint  NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    member_id bigint  NOT NULL REFERENCES repos (id) ON DELETE RESTRICT,
    position  integer NOT NULL,
    PRIMARY KEY (group_id, position),
    UNIQUE (group_id, member_id)
);

CREATE INDEX group_members_member ON group_members (member_id);
