// Package deb is the apt format plugin (plugin name "apt", artifact type deb): .deb control
// parsing, Packages and Release rendering, signing through format.Signer, and pool routes.
// See docs/spec/http-routing.md §3 (apt) and docs/spec/manifest-profile.md.
//
// The model: a package version is one .deb, an ar archive (deb(5)) whose control file names
// it, in one suite and component of the repo. lajstrom.name is Package, lajstrom.version is
// Version as dpkg spells it, epoch included, and lajstrom.variant is
// <suite>/<component>/<Architecture>, so the tag of nginx 1:1.18.0-6ubuntu14.4 for amd64 in
// jammy/main is 1_3a1.18.0-6ubuntu14.4__jammy_2fmain_2famd64. Suite and component are
// properties of the upload, not separate repos; lajstrom.deb.suite, .component and .arch carry
// the three parts as well, for the indexes to group by. The same .deb in a second suite is a
// second manifest over the same blobs (#110). The one file layer is titled with the pool file
// name, nginx_1.18.0-6ubuntu14.4_amd64.deb, whatever the suite; the metadata layer holds the
// control fields as written, md5sums, and the size, md5, sha1 and sha256 of the file, which
// apt's Packages lists.
//
// A source package (#79) is a version too: its .dsc and the files the .dsc names, in one suite
// and component, lajstrom.name its Source, lajstrom.version its Version, lajstrom.deb.arch
// ArchSource and the variant <suite>/<component>/source. Its layers are the .dsc and the files
// in the order of Checksums-Sha256; its metadata layer holds the fields of the .dsc and the
// .dsc's own size and checksums. ParseDsc reads a .dsc, signed or not (the signature is not
// checked); Build checks each file against the checksums the .dsc gives it, and Validate the
// .dsc against the metadata and the file layers. A source and a binary of one name share the
// OCI repository, as they share a directory of a Debian pool.
//
// An upload streams into the registry (ADR 0014): the upload route reads the head of the body,
// up to the end of the control member, for the package name, and the whole body passes the
// inspector on its way, which checks the archive in one pass as Parse does (Parse is the
// inspector over a reader); ParseControl reads only up to the end of the control member, for
// the upload route and for Validate. Issue #39 is the parser, Build and Validate; #40 renders the
// indexes, #41 signs them and #42 serves the pool and the upload route.
//
// The indexes of a suite are one render, so that the Release a client gets lists the hashes of
// the Packages files it gets: RenderIndex with the suite as the scope returns the Release and,
// for every component and architecture of the suite, Packages, Packages.gz and Packages.xz,
// which format.CachedDocument keeps together in index_cache under the suite, each by its path
// below dists/<suite>/. Suites, components and architectures are those of the versions
// (lajstrom.deb.suite, .component, .arch); packages of architecture all are in the index of
// every architecture. The routes serve dists/ from the cache, by name and by hash; ingest and
// delete invalidate it. With a format.Deps.Signer (internal/keys) the routes sign the bytes of
// the cached Release into InRelease and Release.gpg, kept beside it until the Signer no longer
// calls them current, and serve the repo's keyring at key.asc; without one the Release is
// unsigned and clients mark the source
// [trusted=yes]. The plugin registers itself with format.Register when the package is
// imported; the binary imports it in cmd/lajstrom/formats.go.
package deb
