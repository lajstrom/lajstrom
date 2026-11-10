-- 0012: the npm journal (ADR 0012; docs/spec/index-schema.md, npm). journal has a row per entry
-- of a package's journal in the registry, the manifests under the tags _j<seq>; journal_state is
-- their fold, the package's current dist-tags and deprecations. Both are derived: reindex
-- rebuilds them from the registry, and they go with their package (SQLite).

CREATE TABLE journal (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    package_id    INTEGER NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    tag           TEXT    NOT NULL,
    digest        TEXT    NOT NULL,
    artifact_type TEXT    NOT NULL,
    attrs         TEXT,
    created_at    TEXT    NOT NULL,
    indexed_at    TEXT    NOT NULL,
    UNIQUE (package_id, seq),
    UNIQUE (package_id, tag)
);

CREATE TABLE journal_state (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    package_id INTEGER NOT NULL REFERENCES packages (id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,
    key        TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    seq        INTEGER NOT NULL,
    UNIQUE (package_id, kind, key)
);
