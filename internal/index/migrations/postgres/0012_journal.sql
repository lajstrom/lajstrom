-- 0012: the npm journal (ADR 0012; docs/spec/index-schema.md, npm). journal has a row per entry
-- of a package's journal in the registry, the manifests under the tags _j<seq>; journal_state is
-- their fold, the package's current dist-tags and deprecations. Both are derived: reindex
-- rebuilds them from the registry, and they go with their package (PostgreSQL).

CREATE TABLE journal (
    id            bigserial   PRIMARY KEY,
    package_id    bigint      NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    seq           bigint      NOT NULL,
    tag           text        COLLATE "C" NOT NULL,
    digest        text        COLLATE "C" NOT NULL,
    artifact_type text        COLLATE "C" NOT NULL,
    attrs         jsonb,
    created_at    timestamptz NOT NULL,
    indexed_at    timestamptz NOT NULL,
    UNIQUE (package_id, seq),
    UNIQUE (package_id, tag)
);

CREATE TABLE journal_state (
    id         bigserial PRIMARY KEY,
    package_id bigint    NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    kind       text      COLLATE "C" NOT NULL,
    key        text      COLLATE "C" NOT NULL,
    value      text      COLLATE "C" NOT NULL,
    seq        bigint    NOT NULL,
    UNIQUE (package_id, kind, key)
);
