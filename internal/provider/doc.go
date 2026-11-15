// Package provider defines the boundary to the backend OCI registry: the Provider and
// Repository interfaces, Capabilities and sentinel errors. Only provider implementations talk
// to the registry. See docs/spec/layout.md §2 and docs/registries/.
//
// New and NewSet build Providers by registries[].type from the constructors that the
// implementations Register from init: internal/provider/generic (issue #9),
// internal/provider/ecr (issue #36) and internal/provider/harbor (issue #73); cmd/lajstrom
// imports them. Fallbacks for missing capabilities come with issue #37.
package provider
