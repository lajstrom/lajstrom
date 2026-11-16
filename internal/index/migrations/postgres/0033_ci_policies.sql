-- 0033: ci_policies (#937; docs/spec/rbac.md §10, §11, docs/spec/index-schema.md). The CI trust
-- policies an admin makes through the API, beside those of the configuration, which are not in
-- it: a name, an issuer of auth.ci.issuers, the claims and the grants as JSON, and for one that
-- grants write its state (pending until a second admin approves it), its expiry, a renewal that
-- waits, who changed it last and who approved it. Control plane, backed up and in a snapshot,
-- never rebuilt; every write moves auth_version and the revision. The token of a web UI's OIDC
-- sign-in, and its login request until the code is redeemed, keep the ID token's auth_time and
-- amr: a policy that writes is made only with a fresh sign-in with a second factor. Expand only:
-- the binaries before it do not read the table, read tokens and oidc_logins by named columns,
-- and mint tokens without the two (PostgreSQL).

CREATE TABLE ci_policies (
    id                 bigserial   PRIMARY KEY,
    name               text        COLLATE "C" NOT NULL,
    issuer             text        COLLATE "C" NOT NULL,
    claims             jsonb       NOT NULL,
    grants             jsonb       NOT NULL,
    state              text        COLLATE "C" NOT NULL,
    expires_at         timestamptz,
    renewal_expires_at timestamptz,
    created_by         text        COLLATE "C" NOT NULL,
    created_at         timestamptz NOT NULL,
    updated_by         text        COLLATE "C" NOT NULL,
    updated_at         timestamptz NOT NULL,
    approved_by        text        COLLATE "C",
    approved_at        timestamptz,
    revision           bigint      NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX ci_policies_name ON ci_policies (name);

ALTER TABLE tokens ADD COLUMN auth_time timestamptz;
ALTER TABLE tokens ADD COLUMN amr jsonb;
ALTER TABLE oidc_logins ADD COLUMN auth_time timestamptz;
ALTER TABLE oidc_logins ADD COLUMN amr jsonb;
