-- 0022: versions.oci_repo (#642; docs/spec/index-schema.md, Derived, Invariants and rules). The
-- package path of the OCI repository that holds a version's manifest when it is not the
-- package's name_oci: a package split across two forms of its OCI name (ADR 0017), whose versions
-- at the other form reindex indexes with the form where it found them. NULL otherwise, and in
-- every row ingest writes. Expand: the binaries before it neither read nor write it, and their
-- rows are NULL (PostgreSQL).

ALTER TABLE versions ADD COLUMN oci_repo text COLLATE "C";
