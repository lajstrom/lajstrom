-- 0030: policy (#101; docs/spec/policy.md, docs/spec/index-schema.md, Policy). policy_rules and
-- policy_state are control plane: the rules over coordinates, and the generation that every
-- change of a rule, or of a verdict through ingest, increments in its transaction. verdicts is
-- derived: the verdict attestations that verify, rebuilt by reindex from the referrers.
-- policy_seen is a cache: when a proxy repo with a quarantine first saw a version without a
-- publish time. index_cache.expires_at bounds a rendered document by the policy decisions it
-- took. Expand only (PostgreSQL).

CREATE TABLE policy_rules (
    id         bigserial   PRIMARY KEY,
    repo_mask  text        COLLATE "C" NOT NULL,
    name       text        COLLATE "C" NOT NULL,
    version    text        COLLATE "C" NOT NULL,
    variant    text        COLLATE "C" NOT NULL,
    effect     text        COLLATE "C" NOT NULL,
    reason     text        COLLATE "C" NOT NULL,
    created_by text        COLLATE "C" NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz
);

CREATE TABLE policy_state (
    id         integer     PRIMARY KEY CHECK (id = 1),
    generation bigint      NOT NULL,
    updated_at timestamptz NOT NULL
);

INSERT INTO policy_state (id, generation, updated_at) VALUES (1, 1, now()) ON CONFLICT (id) DO NOTHING;

CREATE TABLE verdicts (
    id            bigserial   PRIMARY KEY,
    repo_id       bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    attachment_id bigint      REFERENCES attachments (id) ON DELETE CASCADE,
    path          text        COLLATE "C" NOT NULL,
    subject       text        COLLATE "C" NOT NULL,
    referrer      text        COLLATE "C" NOT NULL,
    layer         text        COLLATE "C" NOT NULL,
    verdict       text        COLLATE "C" NOT NULL,
    reason        text        COLLATE "C" NOT NULL,
    source        text        COLLATE "C" NOT NULL,
    scope         text        COLLATE "C" NOT NULL,
    valid_until   timestamptz,
    signed_by     text        COLLATE "C" NOT NULL,
    created_at    timestamptz,
    indexed_at    timestamptz NOT NULL,
    UNIQUE (repo_id, referrer, layer)
);
CREATE INDEX verdicts_subject ON verdicts (subject);
CREATE INDEX verdicts_attachment ON verdicts (attachment_id);

CREATE TABLE policy_seen (
    repo_id bigint      NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    name    text        COLLATE "C" NOT NULL,
    version text        COLLATE "C" NOT NULL,
    variant text        COLLATE "C" NOT NULL,
    seen_at timestamptz NOT NULL,
    PRIMARY KEY (repo_id, name, version, variant)
);

ALTER TABLE index_cache ADD COLUMN expires_at timestamptz;
