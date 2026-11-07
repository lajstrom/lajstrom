// Package api serves the management REST API under /api/v1 (repos, users, tokens, bindings,
// audit) as JSON. See docs/spec/http-routing.md §4 and docs/api.md.
//
// Repos is the repo service that the handlers, lajstrom admin repo and the config bootstrap
// share; New returns the handler the server mounts at /api/. Repos came with #14; users,
// tokens and bindings come with #47, audit with #46.
package api
