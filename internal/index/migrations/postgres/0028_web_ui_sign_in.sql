-- 0028: the sign-in of a web UI (#1170; docs/spec/rbac.md §10, docs/spec/index-schema.md).
-- A code login that a web UI started keeps the page of the UI to return to, the S256 challenge
-- of the UI's PKCE verifier and the thumbprint of its DPoP key; its callback keeps the sha256 of
-- the code it hands to the UI, which POST /api/v1/login redeems once. The token of a sign-in
-- keeps the thumbprint of the DPoP key it is bound to (RFC 9449), and dpop_proofs the proofs
-- used, until their window ends, so that each is used once on any replica. Expand: the binaries
-- before it read the tables by named columns, finish a web UI's login as their own, on the token
-- page, and know no bound token (PostgreSQL).

ALTER TABLE oidc_logins ADD COLUMN return_to text COLLATE "C";
ALTER TABLE oidc_logins ADD COLUMN code_challenge text COLLATE "C";
ALTER TABLE oidc_logins ADD COLUMN handoff text COLLATE "C";
ALTER TABLE oidc_logins ADD COLUMN dpop_jkt text COLLATE "C";
CREATE UNIQUE INDEX oidc_logins_handoff ON oidc_logins (handoff) WHERE handoff IS NOT NULL;

ALTER TABLE tokens ADD COLUMN dpop_jkt text COLLATE "C";

CREATE TABLE dpop_proofs (
    jti        text        COLLATE "C" PRIMARY KEY,
    expires_at timestamptz NOT NULL
);

CREATE INDEX dpop_proofs_expires ON dpop_proofs (expires_at);
