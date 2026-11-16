// Package testutil holds test fixtures: in-process builders for package files, archive bombs
// and the helpers of the fuzz tests. Deb, Wheel, Sdist, Jar, POM and NpmTarball build a package
// in memory from options whose zero value is a valid package, and return its bytes with the
// facts a parser of the format finds in it (control fields, METADATA, normalised names, file
// names, repository paths, file lists, checksums), so that a plugin test compares what it
// parsed with them.
//
// NewCA makes certificate authorities and the certificates they issue, in memory, for the
// tests of client certificates: no private key is written to the repository.
//
// NewOIDCIssuer serves an OIDC issuer in process, its discovery document and its JWKS, and signs
// the tokens of the tests of CI and cluster tokens with a key of the process.
//
// The same options give the same bytes: archive members carry a fixed time and owner. The
// compressed bytes are those of the Go release's compress/flate, so a test compares with what
// the builder returns, not with a digest written into the test.
//
// Nothing here is vendored from a distribution. See docs/spec/layout.md §4.
package testutil
