// Package server owns the HTTP server: net/http ServeMux routing with the reserved first
// segments, the middleware chain and graceful shutdown. See docs/spec/http-routing.md §1 and §5.
//
// The middleware chain, outermost first: the count of running handlers, the security headers
// (#63), panic recovery, request id, the client address (ADR 0011), access log, the refusal of
// cross-origin browser writes (#63), authentication (HTTP Basic, and a personal access token
// as a bearer outside /v2, into the request's principal and credential, docs/auth.md), the
// limit of uploads at once and the bound on stalled bodies (#50). The system
// endpoints are /-/healthz, /-/readyz and /-/version; /-/metrics comes with issue #23.
// Format plugins are validated here and mounted below /<name>/ through format.NewRouter; the
// management API (Options.API, internal/api) is mounted at /api/; the OCI facade (/v2) is
// mounted by its issue. Hardening
// (request limits, shutdown drain of long transfers) is issue #50.
package server
