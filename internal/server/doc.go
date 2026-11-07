// Package server owns the HTTP server: net/http ServeMux routing with the reserved first
// segments, the middleware chain and graceful shutdown. See docs/spec/http-routing.md §1 and §5.
//
// The middleware chain, outermost first: panic recovery, request id, access log,
// authentication (HTTP Basic into the request's principal, docs/auth.md). The system
// endpoints are /-/healthz, /-/readyz and /-/version; /-/metrics comes with issue #23.
// Format plugins are validated here and mounted below /<name>/ through format.NewRouter; the
// management API (Options.API, internal/api) is mounted at /api/; the OCI facade (/v2) is
// mounted by its issue. Hardening
// (request limits, shutdown drain of long transfers) is issue #50.
package server
