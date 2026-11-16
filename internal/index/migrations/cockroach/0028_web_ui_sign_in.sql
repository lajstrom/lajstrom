-- 0028: the sign-in of a web UI (#1170; docs/spec/rbac.md §10, docs/spec/index-schema.md).
-- A code login that a web UI started keeps the page of the UI to return to, the S256 challenge
-- of the UI's PKCE verifier and the thumbprint of its DPoP key; its callback keeps the sha256 of
-- the code it hands to the UI, which POST /api/v1/login redeems once. The token of a sign-in
-- keeps the thumbprint of the DPoP key it is bound to (RFC 9449), and dpop_proofs the proofs
-- used, until their window ends, so that each is used once on any replica. Expand: the binaries
-- before it read the tables by named columns, finish a web UI's login as their own, on the token
-- page, and know no bound token (CockroachDB).

ALTER TABLE oidc_logins ADD COLUMN IF NOT EXISTS return_to text;
ALTER TABLE oidc_logins ADD COLUMN IF NOT EXISTS code_challenge text;
ALTER TABLE oidc_logins ADD COLUMN IF NOT EXISTS handoff text;
ALTER TABLE oidc_logins ADD COLUMN IF NOT EXISTS dpop_jkt text;
CREATE UNIQUE INDEX IF NOT EXISTS oidc_logins_handoff ON oidc_logins (handoff) WHERE handoff IS NOT NULL;

ALTER TABLE tokens ADD COLUMN IF NOT EXISTS dpop_jkt text;

CREATE TABLE IF NOT EXISTS dpop_proofs (
    jti        text        PRIMARY KEY,
    expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS dpop_proofs_expires ON dpop_proofs (expires_at);
