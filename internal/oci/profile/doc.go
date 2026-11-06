// Package profile is the one place that knows what a Lajstrom manifest looks like: the artifact
// types and layer media types, the annotation keys, the empty config, Escape and
// EscapeComponent for tags and repository paths, and Build, Encode and Validate for manifests.
// Plugins, the facade's manifest hook, ingest and reindex use it.
//
// The normative text is docs/spec/manifest-profile.md
// (https://github.com/lajstrom/lajstrom/blob/main/docs/spec/manifest-profile.md); the decision
// behind it is ADR 0005, docs/adr/0005-manifest-profile-and-immutable-tags.md
// (https://github.com/lajstrom/lajstrom/blob/main/docs/adr/0005-manifest-profile-and-immutable-tags.md).
//
// The package is pure Go without I/O: it imports the standard library, the OCI image-spec types
// and go-digest only. Every error about invalid input wraps ErrInvalid.
package profile
