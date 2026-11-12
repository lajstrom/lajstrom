// Package rpm is the rpm format plugin (plugin name and artifact type "rpm"): rpm header
// parsing, the repodata of a yum repository (repomd.xml, primary, filelists, other), its
// signature through format.Signer, and the Packages routes. See docs/spec/http-routing.md §3
// (rpm) and docs/spec/manifest-profile.md.
//
// The model: a package version is one .rpm, whose header names it, in one repo, which is one
// yum repository of every architecture. lajstrom.name is NAME, lajstrom.version is
// [EPOCH:]VERSION-RELEASE with the epoch only when it is not 0, and lajstrom.variant the arch as
// the repodata writes it, src or nosrc for a source package; so the tag of hello 2.12-1.el9 for
// x86_64 is 2.12-1.el9__x86_5f64. The one file layer is titled <name>-<version>-<release>.<arch>.rpm.
//
// The parser (#64) reads the head of an rpm, as rpm 4 writes it: the lead, the signature header
// and its padding, and the header, each header an index of tags into a data store, bounded by
// MaxHeader and checked entry by entry, since the bytes are hostile. ParseHead reads the head
// and nothing after it, for Validate; Parse reads the whole file through the inspector of a
// streamed upload, which also checks the size the signature header gives and the payload digest
// of the header, without opening the payload.
package rpm
