-- 0003: the escaped package path is unique within a repo (#97). OCI repository names are
-- lowercase, so two spellings of one raw path are one repository: the first spelling holds it,
-- and the index refuses the second, also when both are written at once. A database that holds
-- two such packages already fails here; delete the younger one's row from packages (the
-- cascade takes its versions and files) and migrate again. name_oci is, so the
-- index compares bytes (CockroachDB).

CREATE UNIQUE INDEX IF NOT EXISTS packages_repo_name_oci ON packages (repo_id, name_oci);
