-- 0033: ci_policies (#937; docs/spec/rbac.md §11, docs/spec/index-schema.md). The CI trust
-- policies an admin makes through the API, beside those of the configuration, which are not in
-- it: a name, an issuer of auth.ci.issuers, the claims and the grants as JSON. Control plane,
-- backed up and in a snapshot, never rebuilt; every write moves auth_version. Expand only: the
-- binaries before it do not read the table (SQLite).

CREATE TABLE ci_policies (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    issuer     TEXT    NOT NULL,
    claims     TEXT    NOT NULL,
    grants     TEXT    NOT NULL,
    created_by TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
);

CREATE UNIQUE INDEX ci_policies_name ON ci_policies (name);
