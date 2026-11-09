// Package generic is the oras-go based Provider for registries that follow the OCI
// distribution spec: distribution and Zot, tested in CI; the GitLab container registry, Harbor
// and GHCR, tested by hand, as far as they follow it (docs/registries/README.md).
//
// It registers the registries[].type "generic" with provider.Register; cmd/lajstrom imports it
// for that. One Provider holds one HTTP client: Basic or bearer credentials from the
// configuration, a token cache, retries with jittered exponential backoff for idempotent
// requests and for whole blob uploads from content that can be rewound, a circuit breaker
// that fails requests at once while the registry keeps failing, and registries[].timeout up to
// the response headers. The instance prefix is
// added to every repository name and stripped from listings. Capabilities probes the registry
// once, applies the vendor table for what the probe cannot measure (GitLab, GHCR, Harbor), then
// the overrides of registries[].capabilities. Every error the Provider and its repositories
// return wraps a sentinel of package provider where one applies, and its message never shows
// the backend URL or the instance prefix.
package generic
