// Package ingest is the single write path of Lajstrom: every publish, from a protocol frontend
// or from the OCI facade, goes through Pipeline (validate, push, tag, index, invalidate caches,
// audit). It also defines publication sessions for multi-request uploads.
// See docs/spec/layout.md §2 and docs/architecture.md (flow A).
//
// The pipeline is filled in by issue #12, publication sessions by issue #53.
package ingest
