// Package facade implements the OCI distribution API under /v2: the proxy to the backend
// registry through internal/provider, the token endpoint, and the manifest PUT hook that hands
// manifests to internal/ingest. See docs/spec/http-routing.md §2 and docs/spec/rbac.md §4.
//
// Filled in by issues #24 (proxy), #25 (manifest hook), #26 (token), #27 (blob redirect),
// #28 (referrers) and #61 (delete).
package facade
