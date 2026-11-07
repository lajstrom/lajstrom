// Package facade implements the OCI distribution API under /v2: the proxy to the backend
// registry through internal/provider, the token endpoint, and the manifest PUT hook that hands
// manifests to internal/ingest. See docs/spec/http-routing.md §2 and docs/spec/rbac.md §4.
//
// The proxy is #24: names, blob uploads driven in the backend's own session from any replica
// (uploads.go), blob and manifest reads, the manifest pass-through of oci repos, tags/list and
// _catalog. The manifest hook of managed repos (hook.go) is #25. Still to come: #26 (token),
// #27 (blob redirect and ranges), #28 (referrers) and #61 (delete).
package facade
