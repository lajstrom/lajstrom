// Package server owns the HTTP server: net/http ServeMux routing with the reserved first
// segments, the middleware chain (request id, access log, metrics, auth, panic recovery) and
// graceful shutdown. It mounts the format plugins, the OCI facade and the management API.
// See docs/spec/http-routing.md §1.
//
// Filled in by issue #7 (second pull request); observability by #23, hardening by #50.
package server
