// Package audit records writes, control-plane changes and token issuance in the audit log.
// See docs/spec/rbac.md §5 and docs/spec/index-schema.md (audit).
//
// The Writer interface ships with issue #7; the implementation comes with issue #46.
package audit
