// Package raw is the raw format plugin: any file stored under a path, uploaded with PUT and
// downloaded with GET. See docs/spec/http-routing.md §3 (raw) and docs/spec/manifest-profile.md.
//
// The model: the package is the path exactly as the client sent it after /raw/<repo>/ (case
// preserved, "/"-separated), carried by lajstrom.name, lajstrom.raw.path and the title of the one
// file layer; the version is the first 16 hex digits of the sha256 of the bytes; there is no
// variant, and the tag is the version. GET resolves a path to its newest version, ?digest= pins
// a manifest, and a path that ends with "/" is a JSON listing of everything below it. DELETE
// removes every version of a path.
//
// The plugin registers itself with format.Register when the package is imported; the binary
// imports it in cmd/lajstrom/formats.go.
package raw
