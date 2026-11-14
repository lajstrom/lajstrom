-- 0023: the attachments of a version (#89; docs/spec/index-schema.md, Attachments): one row per
-- layer of a referrer of the version's manifest, a signature, an SBOM, a scan report or another
-- attestation, with its verification. Derived: reindex rebuilds it from the referrers in the
-- registry, and a version's rows go with it (SQLite).

CREATE TABLE attachments (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id     INTEGER NOT NULL REFERENCES versions (id) ON DELETE CASCADE,
    digest         TEXT    NOT NULL,
    tag            TEXT    NOT NULL,
    layer          TEXT    NOT NULL,
    media_type     TEXT    NOT NULL,
    artifact_type  TEXT    NOT NULL,
    kind           TEXT    NOT NULL,
    predicate_type TEXT    NOT NULL,
    size           INTEGER NOT NULL,
    annotations    TEXT,
    tool           TEXT    NOT NULL,
    tool_version   TEXT    NOT NULL,
    database       TEXT    NOT NULL,
    input          TEXT    NOT NULL,
    verification   TEXT    NOT NULL,
    signed_by      TEXT,
    reason         TEXT    NOT NULL,
    trust          TEXT    NOT NULL,
    created_at     TEXT,
    indexed_at     TEXT    NOT NULL,
    UNIQUE (version_id, digest, layer)
);
CREATE INDEX attachments_digest ON attachments (digest);
