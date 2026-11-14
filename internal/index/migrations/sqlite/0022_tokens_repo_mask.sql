-- 0022: tokens.repo_mask (#911; docs/spec/rbac.md §1, docs/spec/index-schema.md). The mask of a
-- deploy token over a mask, oci/* or */talos in the grammar of a binding: the token reaches every
-- repo the mask matches, the repos created later included. A deploy token has repo_id or
-- repo_mask, never both. Expand: the binaries before it read the table by named columns and
-- refuse a mask token as a deploy token without a repo (SQLite).

ALTER TABLE tokens ADD COLUMN repo_mask TEXT;
