-- 0014: schema_migrations.requires (#225; docs/spec/index-schema.md, Upgrades). The runner
-- records there the version that a contract migration names in its line "-- lajstrom:requires
-- NNNN", and 0 for every other migration. A binary that finds versions it does not know in
-- schema_migrations runs on the schema only when it knows the highest requires. Binaries of
-- earlier releases ignore the column, as they ignore every column they do not name (CockroachDB).

ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS requires integer NOT NULL DEFAULT 0;
