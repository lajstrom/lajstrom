-- 0029: versions.summary and files_digest (#954, #956; docs/spec/index-schema.md, Derived). The
-- summary of a manifest, one line of its metadata layer (docs/spec/manifest-profile.md §2), which
-- ingest writes, and reindex when it reads the manifest; NULL in the rows before, until serve
-- fills them from their metadata at start, or a reindex with verify those without. The index
-- serves the search by a digest's prefix. Expand: the binaries before it neither read nor write
-- the column, and their rows are NULL (CockroachDB).

ALTER TABLE versions ADD COLUMN IF NOT EXISTS summary text;
CREATE INDEX IF NOT EXISTS files_digest ON files (digest);
