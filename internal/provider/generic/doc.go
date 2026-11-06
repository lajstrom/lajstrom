// Package generic is the oras-go based Provider for registries that follow the OCI
// distribution spec: distribution and Zot, tested (docs/registries/README.md); Harbor and GHCR
// as far as they follow the spec.
//
// It registers the registries[].type "generic" with provider.Register; cmd/lajstrom imports it
// for that. One Provider holds one HTTP client: Basic or bearer credentials from the
// configuration, a token cache, retries with exponential backoff for requests whose body can
// be replayed, and registries[].timeout up to the response headers. The instance prefix is
// added to every repository name and stripped from listings. Capabilities probes the registry
// once and applies the overrides of registries[].capabilities. Every error the Provider and its
// repositories return wraps a sentinel of package provider where one applies, and its message
// never shows the backend URL or the instance prefix.
package generic
