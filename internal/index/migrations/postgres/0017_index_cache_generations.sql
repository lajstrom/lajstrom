-- 0017: index_cache_generations (#477; docs/spec/index-schema.md, Derived). One row per repo
-- with the generation of its rendered documents: every invalidation of the repo's index_cache
-- increments it in its transaction, and a render stores its documents only while the generation
-- it read before rendering is current. A repo without a row is at generation 0. Binaries of
-- earlier releases neither read nor write it (PostgreSQL).

CREATE TABLE index_cache_generations (
    repo_id    bigint PRIMARY KEY REFERENCES repos (id) ON DELETE CASCADE,
    generation bigint NOT NULL
);
