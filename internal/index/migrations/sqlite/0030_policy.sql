-- 0030: policy (#101; docs/spec/policy.md, docs/spec/index-schema.md, Policy). policy_rules and
-- policy_state are control plane: the rules over coordinates, and the generation that every
-- change of a rule, or of a verdict through ingest, increments in its transaction. verdicts is
-- derived: the verdict attestations that verify, rebuilt by reindex from the referrers.
-- policy_seen is a cache: when a proxy repo with a quarantine first saw a version without a
-- publish time. index_cache.expires_at bounds a rendered document by the policy decisions it
-- took. Expand only (SQLite).

CREATE TABLE policy_rules (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_mask  TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    version    TEXT    NOT NULL,
    variant    TEXT    NOT NULL,
    effect     TEXT    NOT NULL,
    reason     TEXT    NOT NULL,
    created_by TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    expires_at TEXT
);

CREATE TABLE policy_state (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    generation INTEGER NOT NULL,
    updated_at TEXT    NOT NULL
);

INSERT INTO policy_state (id, generation, updated_at) VALUES (1, 1, strftime('%Y-%m-%dT%H:%M:%S', 'now') || '.000000Z');

CREATE TABLE verdicts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id       INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    attachment_id INTEGER REFERENCES attachments (id) ON DELETE CASCADE,
    path          TEXT    NOT NULL,
    subject       TEXT    NOT NULL,
    referrer      TEXT    NOT NULL,
    layer         TEXT    NOT NULL,
    verdict       TEXT    NOT NULL,
    reason        TEXT    NOT NULL,
    source        TEXT    NOT NULL,
    scope         TEXT    NOT NULL,
    valid_until   TEXT,
    signed_by     TEXT    NOT NULL,
    created_at    TEXT,
    indexed_at    TEXT    NOT NULL,
    UNIQUE (repo_id, referrer, layer)
);
CREATE INDEX verdicts_subject ON verdicts (subject);
CREATE INDEX verdicts_attachment ON verdicts (attachment_id);

CREATE TABLE policy_seen (
    repo_id INTEGER NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name    TEXT    NOT NULL,
    version TEXT    NOT NULL,
    variant TEXT    NOT NULL,
    seen_at TEXT    NOT NULL,
    PRIMARY KEY (repo_id, name, version, variant)
);

ALTER TABLE index_cache ADD COLUMN expires_at TEXT;
