// Package ingest is the single write path of Lajstrom: every publish, from a protocol frontend
// or from the OCI facade, goes through Pipeline (validate, push, tag, index, invalidate caches,
// audit). It also defines publication sessions for multi-request uploads.
// See docs/spec/layout.md §2 and docs/architecture.md (flow A).
//
// The pipeline came with issue #12: it claims the tag in the index before the registry write
// and confirms it after (docs/spec/index-schema.md, Coordination). Publication sessions come
// with issue #53.
package ingest
