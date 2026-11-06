// Package format defines the format plugin contract: the Plugin interface, the types plugins
// exchange with ingest and the server, and the registry of plugins. One sub-package per format
// implements it (raw, pypi, deb, maven). See docs/spec/layout.md §2.
//
// The interface and the registry ship with issue #7. Issue #11 adds the helpers plugins share:
// the Router implementation with the repo middleware (NewRouter, RepoFrom), the error-to-HTTP
// mapping (WriteError), Authorize, the index_cache helpers (CachedDocument, ServeDocument), and
// the fake plugin for tests in package formattest.
package format
