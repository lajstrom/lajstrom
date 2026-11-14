-- 0022: the attachments of a version (#89; docs/spec/index-schema.md, Attachments): one row per
-- layer of a referrer of the version's manifest, a signature, an SBOM, a scan report or another
-- attestation, with its verification. Derived: reindex rebuilds it from the referrers in the
-- registry, and a version's rows go with it (PostgreSQL).

CREATE TABLE attachments (
    id             bigserial   PRIMARY KEY,
    version_id     bigint      NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    digest         text        COLLATE "C" NOT NULL,
    tag            text        COLLATE "C" NOT NULL,
    layer          text        COLLATE "C" NOT NULL,
    media_type     text        COLLATE "C" NOT NULL,
    artifact_type  text        COLLATE "C" NOT NULL,
    kind           text        COLLATE "C" NOT NULL,
    predicate_type text        COLLATE "C" NOT NULL,
    size           bigint      NOT NULL,
    annotations    jsonb,
    tool           text        COLLATE "C" NOT NULL,
    tool_version   text        COLLATE "C" NOT NULL,
    database       text        COLLATE "C" NOT NULL,
    input          text        COLLATE "C" NOT NULL,
    verification   text        COLLATE "C" NOT NULL,
    signed_by      text        COLLATE "C",
    reason         text        COLLATE "C" NOT NULL,
    trust          text        COLLATE "C" NOT NULL,
    created_at     timestamptz,
    indexed_at     timestamptz NOT NULL,
    UNIQUE (version_id, digest, layer)
);
CREATE INDEX attachments_digest ON attachments (digest);
