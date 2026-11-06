// Package provider defines the boundary to the backend OCI registry: the Provider and
// Repository interfaces, Capabilities and sentinel errors. Only provider implementations talk
// to the registry. See docs/spec/layout.md §2 and docs/registries/.
//
// Implementations: internal/provider/generic (issue #9) and internal/provider/ecr (issue #36);
// fallbacks for missing capabilities in issue #37.
package provider
