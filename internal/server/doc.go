// Package server owns the HTTP server: net/http ServeMux routing with the reserved first
// segments, the middleware chain and graceful shutdown. See docs/spec/http-routing.md §1 and §5.
//
// The middleware chain, outermost first: panic recovery, request id, access log. The system
// endpoints are /-/healthz, /-/readyz and /-/version; /-/metrics comes with issue #23.
// Format plugins are validated here and mounted below /<name>/ through format.NewRouter; the
// OCI facade (/v2) and the management API (/api/v1) are mounted by their issues. Hardening
// (request limits, shutdown drain of long transfers) is issue #50.
package server
