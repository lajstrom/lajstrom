-- 0007: one open session per key in a repo (#53; docs/spec/index-schema.md, sessions). Two
-- requests that open the publication session of one version at once, as Gradle's parallel
-- uploads do, must end in the same session, or the files of one would be missing from the
-- commit: the second insert is a conflict, and ingest reads the winner. An open session that a
-- key had twice before this migration keeps the newest; the others are expired.

UPDATE sessions SET state = 'expired'
WHERE state = 'open' AND id NOT IN (SELECT MAX(id) FROM sessions WHERE state = 'open' GROUP BY repo_id, key);
CREATE UNIQUE INDEX sessions_open_key ON sessions (repo_id, key) WHERE state = 'open';
