// Package format defines the format plugin contract: the Plugin interface, the types plugins
// exchange with ingest and the server, and the registry of plugins. One sub-package per format
// implements it (raw, pypi, deb, maven). See docs/spec/layout.md §2.
//
// The interface and the registry ship with issue #7; the Router implementation, the repo
// middleware, error-to-HTTP mapping and the fake plugin for tests come with issue #11.
package format
