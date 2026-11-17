-- 0038: tokens.acr and oidc_logins.acr (#1276; docs/spec/rbac.md §10, §12,
-- docs/spec/index-schema.md). The acr of the ID token of a web UI's OIDC sign-in, at most 256
-- bytes, beside its auth_time and amr: kept by the callback on the login request, and by the
-- token of its code, so that a step that needs a second factor reads it against
-- auth.oidc.mfa.acr at each request. Expand only: the binaries before it read both tables by
-- named columns and mint tokens without it; a token without acr meets no condition on it
-- (PostgreSQL).

ALTER TABLE tokens ADD COLUMN acr text COLLATE "C";
ALTER TABLE oidc_logins ADD COLUMN acr text COLLATE "C";
