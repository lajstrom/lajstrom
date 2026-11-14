package deb

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/md5"  //nolint:gosec // the MD5Sum section of a Release; not a security use
	"crypto/sha1" //nolint:gosec // the SHA1 section of a Release; not a security use
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/ulikunitz/xz"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// The documents of a suite (docs/spec/http-routing.md §3, apt). The index_cache scope is the
// suite and the kind is the path below dists/<suite>/: Release, main/binary-amd64/Packages,
// main/binary-amd64/Packages.gz, …
const (
	kindRelease  = "Release"
	packagesFile = "Packages"

	releaseContentType  = "text/plain"
	packagesContentType = "text/plain; charset=utf-8"
	gzipContentType     = "application/gzip"
	xzContentType       = "application/x-xz"

	// origin is the Origin of every Release.
	origin = "Lajstrom"
	// releaseDate is the layout of the Date of a Release: RFC 1123 in UTC, the zone spelled
	// UTC, as the Debian archive writes it.
	releaseDate = "Mon, 02 Jan 2006 15:04:05 UTC"
	// archAll is the architecture of the packages that run on every one.
	archAll = "all"
	// maxManifest is the largest manifest read from the registry for a metadata layer the
	// index does not hold: the limit of the facade for a manifest PUT.
	maxManifest = 4 << 20
	// maxXZDictionary is the dictionary of Packages.xz for an index of that size or more, the
	// writer's default; a smaller index gets a dictionary of its own size, at least lzma's
	// minimum of 4 KiB. With the default an index of 1 KiB allocated 50 MB, sized 170 KB.
	maxXZDictionary = 8 << 20
)

// variants are the three files of every Packages index, in the order the Release lists them.
var variants = []struct {
	suffix   string
	compress func([]byte) ([]byte, error)
}{
	{"", nil},
	{".gz", func(b []byte) ([]byte, error) { return compressed(b, newGzip) }},
	{".xz", func(b []byte) ([]byte, error) { return compressed(b, newXZ(len(b))) }},
}

// RenderIndex renders every document of one suite, the scope, in one call: for every component
// and architecture of the suite Packages, Packages.gz and Packages.xz, and the Release whose
// MD5Sum, SHA1 and SHA256 sections list each of them with its size, so that the Release and
// the Packages files format.CachedDocument stores together, and a client gets, agree. Kind is
// the path below dists/<suite>/ (Release, main/binary-amd64/Packages.gz) and Path the URL path
// below /apt/<repo>/.
//
// The suite, its components and its architectures are those of the versions of the repo,
// grouped by lajstrom.deb.suite, .component and .arch (versions.attrs); the variant is not
// parsed. The architectures of the suite are the concrete ones its versions have, and the
// packages of architecture all are in the index of each of them; a suite with packages of
// architecture all only has binary-all. Every component has an index for every architecture of
// the suite, empty when it has no package for it, so that each pair the Release names exists.
// A scope that is not a suite name, or a suite without a version, is index.ErrNotFound. The
// Packages files are a function of the versions alone; two renders differ in the Date of the
// Release only.
//
// A paragraph comes from the metadata of the version (versions.metadata); when the index has
// none, the metadata layer is read from the registry through the providers of the value Routes
// made (fetchMetadata). The value format.Register holds has none, and fails on such a version.
func (p Plugin) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	suite := string(scope)
	if checkDistName("suite", suite) != nil {
		return nil, fmt.Errorf("%w: apt/%s: %q is not a suite", index.ErrNotFound, repo.Name, suite)
	}
	paras, err := p.paragraphs(ctx, repo, suite, st)
	if err != nil {
		return nil, err
	}
	if len(paras) == 0 {
		return nil, fmt.Errorf("%w: apt/%s has no package in the suite %s", index.ErrNotFound, repo.Name, suite)
	}
	components, archs := layout(paras)
	var docs []format.Document
	for _, c := range components {
		for _, a := range archs {
			plain := packagesOf(paras, c, a)
			dir := c + "/binary-" + a + "/"
			for _, v := range variants {
				content := plain
				if v.compress != nil {
					if content, err = v.compress(plain); err != nil {
						return nil, fmt.Errorf("deb: %s%s%s of %s: %w", dir, packagesFile, v.suffix, suite, err)
					}
				}
				kind := dir + packagesFile + v.suffix
				docs = append(docs, format.Document{Kind: kind, Path: "dists/" + suite + "/" + kind,
					ContentType: contentType(kind), Content: content})
			}
		}
	}
	release := releaseOf(repo, suite, components, archs, docs, p.clock().UTC().Format(releaseDate))
	return append([]format.Document{release}, docs...), nil
}

// contentType returns the Content-Type of the document kind.
func contentType(kind string) string {
	switch {
	case kind == kindRelease, kind == kindInRelease:
		return releaseContentType
	case kind == kindReleaseGPG:
		return signatureContentType
	case strings.HasSuffix(kind, ".gz"):
		return gzipContentType
	case strings.HasSuffix(kind, ".xz"):
		return xzContentType
	}
	return packagesContentType
}

// paragraph is one version of a suite in a Packages index.
type paragraph struct {
	name, version, arch, component string
	// text is the paragraph, each line ending with "\n", without the blank line after it.
	text []byte
}

// paragraphs returns the paragraphs of the versions of the suite, sorted by package name, then
// version string, then architecture.
func (p Plugin) paragraphs(ctx context.Context, repo index.Repo, suite string, st index.Reader) ([]paragraph, error) {
	pkgs, err := st.ListPackages(ctx, repo.ID, "")
	if err != nil {
		return nil, fmt.Errorf("deb: packages of apt/%s: %w", repo.Name, err)
	}
	var paras []paragraph
	for _, pkg := range pkgs {
		versions, err := st.ListVersions(ctx, pkg.ID)
		if err != nil {
			return nil, fmt.Errorf("deb: versions of %s: %w", pkg.Name, err)
		}
		for _, v := range versions {
			if v.Attrs[profile.AnnotationDebSuite] != suite || isSource(v) {
				continue
			}
			para, err := p.paragraphOf(ctx, repo, pkg, v)
			if err != nil {
				return nil, err
			}
			paras = append(paras, para)
		}
	}
	slices.SortFunc(paras, func(a, b paragraph) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.version, b.version), strings.Compare(a.arch, b.arch))
	})
	return paras, nil
}

// layout returns the components and the architectures of the indexes of a suite, sorted: the
// architectures of its versions, all among them when it has all packages. apt on a host of an
// architecture the suite has no package for reads binary-all alone (#725), so the all packages
// get it beside the concrete indexes; the Release has no No-Support-for-Architecture-all, which
// would make apt skip binary-all again.
func layout(paras []paragraph) (components, archs []string) {
	cs, as := map[string]bool{}, map[string]bool{}
	for _, p := range paras {
		cs[p.component], as[p.arch] = true, true
	}
	return slices.Sorted(maps.Keys(cs)), slices.Sorted(maps.Keys(as))
}

// packagesOf returns the Packages index of a component and an architecture: the paragraphs of
// that component whose architecture is arch or all, each followed by a blank line. The index of
// all holds the all packages alone; those of the concrete architectures hold them too, which apt
// merges with binary-all without a warning.
func packagesOf(paras []paragraph, component, arch string) []byte {
	var b bytes.Buffer
	for _, p := range paras {
		if p.component == component && (p.arch == arch || p.arch == archAll) {
			b.Write(p.text)
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// packagesFields is what a Packages paragraph needs of the deb metadata layer
// (docs/spec/manifest-profile.md §2); md5sums is not decoded.
type packagesFields struct {
	Control  map[string]string `json:"control"`
	Filename string            `json:"filename"`
	Size     int64             `json:"size"`
	MD5      string            `json:"md5"`
	SHA1     string            `json:"sha1"`
	SHA256   string            `json:"sha256"`
}

// paragraphOf returns the paragraph of version v of pkg: the control fields of its metadata
// (controlOrder), then Filename, pool/<component>/PoolPath(source, filename) with the source
// of the Source field or the package name, Size, MD5sum and SHA1 when the metadata has them,
// and SHA256. A field that could end the paragraph or start another one is refused: ingest
// validated the metadata against the package, so that is an index that went wrong.
func (p Plugin) paragraphOf(ctx context.Context, repo index.Repo, pkg index.Package, v index.Version) (paragraph, error) {
	what := pkg.Name + " " + v.Version + " (" + v.Variant + ")"
	component, arch := v.Attrs[profile.AnnotationDebComponent], v.Attrs[profile.AnnotationDebArch]
	if component == "" || arch == "" {
		return paragraph{}, fmt.Errorf("deb: %s has no %s or %s", what, profile.AnnotationDebComponent, profile.AnnotationDebArch)
	}
	raw := v.Metadata
	if raw == nil {
		var err error
		if raw, err = p.fetchMetadata(ctx, repo, pkg, v); err != nil {
			return paragraph{}, fmt.Errorf("deb: %s has no metadata in the index: %w", what, err)
		}
	}
	var f packagesFields
	if err := json.Unmarshal(raw, &f); err != nil {
		return paragraph{}, fmt.Errorf("deb: the metadata of %s: %w", what, err)
	}
	if f.Control == nil || f.Filename == "" || f.SHA256 == "" {
		return paragraph{}, fmt.Errorf("deb: the metadata of %s lacks control, filename or sha256", what)
	}
	source := pkg.Name
	if s, ok := lookup(f.Control, FieldSource); ok {
		var err error
		if source, _, err = SplitSource(s); err != nil { // not a 400 of the request: the message only
			return paragraph{}, fmt.Errorf("deb: the metadata of %s: %s", what, err.Error())
		}
	}
	var b bytes.Buffer
	for _, name := range controlOrder(f.Control) {
		if !safeField(name, f.Control[name]) {
			return paragraph{}, fmt.Errorf("deb: the metadata of %s has the control field %q, which cannot go into Packages as it is", what, name)
		}
		writeField(&b, name, f.Control[name])
	}
	writeField(&b, "Filename", "pool/"+component+"/"+PoolPath(source, f.Filename))
	writeField(&b, "Size", strconv.FormatInt(f.Size, 10))
	if f.MD5 != "" {
		writeField(&b, "MD5sum", f.MD5)
	}
	if f.SHA1 != "" {
		writeField(&b, "SHA1", f.SHA1)
	}
	writeField(&b, "SHA256", f.SHA256)
	return paragraph{name: pkg.Name, version: v.Version, arch: arch, component: component, text: b.Bytes()}, nil
}

// writeField writes one field of a paragraph: the name, ":", the value with its continuation
// lines as they are, and "\n". A value that starts on the next line (a folded Depends) gets no
// space after the colon.
func writeField(b *bytes.Buffer, name, value string) {
	b.WriteString(name)
	b.WriteByte(':')
	if value != "" && value[0] != '\n' {
		b.WriteByte(' ')
	}
	b.WriteString(value)
	b.WriteByte('\n')
}

// safeField reports whether a control field goes into a paragraph as it is: a field name, and
// a value whose continuation lines start with a space or a tab and are not blank, so that it
// neither ends the paragraph nor starts a field of its own.
func safeField(name, value string) bool {
	if !isFieldName(name) {
		return false
	}
	lines := strings.Split(value, "\n")
	for _, l := range lines[1:] {
		if strings.TrimLeft(l, " \t") == "" || (l[0] != ' ' && l[0] != '\t') {
			return false
		}
	}
	return true
}

// fieldOrder is the order of the control fields in a paragraph. The metadata layer keeps the
// control fields as a JSON object, which Build writes with its keys sorted, so the order of
// the control file is not there to keep: a paragraph has the order the control file of a
// Debian binary package usually has (Package, Source, Version, Architecture, the maintainer
// and the size, the relations, Section, Priority, Multi-Arch, Homepage), then the fields not
// named here sorted by name, and Description last, its continuation lines as they are. The
// fields of the file, Filename to SHA256, follow and are never taken from the control file
// (written).
var fieldOrder = []string{
	"package", "package-type", "source", "version", "architecture", "essential", "protected", "origin", "bugs",
	"maintainer", "original-maintainer", "installed-size",
	"provides", "pre-depends", "depends", "recommends", "suggests", "enhances", "breaks", "conflicts", "replaces",
	"built-using", "static-built-using", "section", "priority", "multi-arch", "homepage",
}

var (
	// fieldRank maps a lowercase field name to its place in fieldOrder.
	fieldRank = func() map[string]int {
		m := make(map[string]int, len(fieldOrder))
		for i, f := range fieldOrder {
			m[f] = i
		}
		return m
	}()
	// written are the fields a paragraph has from the file, never from the control file; a
	// SHA512 there would be a claim apt checks first.
	written = map[string]bool{"filename": true, "size": true, "md5sum": true, "sha1": true, "sha256": true, "sha512": true}
)

// controlOrder returns the names of the control fields in the order of fieldOrder, without the
// fields of written. Names are matched without regard to case and written as they are.
func controlOrder(control map[string]string) []string {
	rank := func(name string) int {
		if r, ok := fieldRank[name]; ok {
			return r
		}
		if name == "description" {
			return len(fieldOrder) + 1
		}
		return len(fieldOrder)
	}
	names := make([]string, 0, len(control))
	for name := range control {
		if !written[strings.ToLower(name)] {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		la, lb := strings.ToLower(a), strings.ToLower(b)
		return cmp.Or(cmp.Compare(rank(la), rank(lb)), strings.Compare(la, lb), strings.Compare(a, b))
	})
	return names
}

// releaseOf returns the Release of a suite: the fields of the suite, Acquire-By-Hash: yes, and
// the MD5Sum, SHA1 and SHA256 sections with a line " <hash> <size> <path>" for each of docs,
// the path below dists/<suite>/. The routes sign these bytes into InRelease and Release.gpg
// (handlers.signed).
func releaseOf(repo index.Repo, suite string, components, archs []string, docs []format.Document, date string) format.Document {
	var b bytes.Buffer
	for _, f := range [][2]string{
		{"Origin", origin}, {"Label", repo.Name}, {"Suite", suite}, {"Codename", suite}, {"Date", date},
		{"Architectures", strings.Join(archs, " ")}, {"Components", strings.Join(components, " ")},
		{"Description", repo.Format + "/" + repo.Name + " " + suite}, {"Acquire-By-Hash", "yes"},
	} {
		writeField(&b, f[0], f[1])
	}
	for _, s := range []struct {
		name string
		hash func() hash.Hash
	}{{"MD5Sum", md5.New}, {"SHA1", sha1.New}, {"SHA256", sha256.New}} {
		b.WriteString(s.name + ":\n")
		for _, d := range docs {
			h := s.hash()
			h.Write(d.Content)
			fmt.Fprintf(&b, " %s %d %s\n", hex.EncodeToString(h.Sum(nil)), len(d.Content), d.Kind)
		}
	}
	return format.Document{Kind: kindRelease, Path: "dists/" + suite + "/" + kindRelease, ContentType: releaseContentType, Content: b.Bytes()}
}

// listed returns the SHA256 section of a Release that releaseOf wrote: the path of each
// document below dists/<suite>/ to its hex sha256.
func listed(release []byte) map[string]string {
	sums := map[string]string{}
	_, section, _ := strings.Cut(string(release), "\nSHA256:\n")
	for line := range strings.Lines(section) {
		f := strings.Fields(line)
		if len(f) != 3 || line[0] != ' ' {
			break
		}
		sums[f[2]] = f[0]
	}
	return sums
}

// compressed returns b written through the compressor open makes.
func compressed(b []byte, open func(io.Writer) (io.WriteCloser, error)) ([]byte, error) {
	var buf bytes.Buffer
	w, err := open(&buf)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// newGzip is the gzip writer of Packages.gz: the default level, no name and no time in the
// header, so that the same Packages gives the same bytes.
func newGzip(w io.Writer) (io.WriteCloser, error) { return gzip.NewWriter(w), nil }

// newXZ returns the xz writer of a Packages.xz of n bytes: CRC64, one block, and a dictionary
// of n bytes between lzma's minimum and maxXZDictionary.
func newXZ(n int) func(io.Writer) (io.WriteCloser, error) {
	return func(w io.Writer) (io.WriteCloser, error) {
		return xz.WriterConfig{DictCap: min(max(n, 4<<10), maxXZDictionary)}.NewWriter(w)
	}
}

// fetchMetadata reads the metadata layer of version v of pkg from the registry: the manifest
// by its digest (Resolve for its size, then Fetch), checked against the profile, and its
// metadata layer, each read up to its limit and checked against its digest. It runs inside the
// transaction of format.CachedDocument, so it is for the rows whose metadata the index left out
// (versions.metadata may be omitted for size, docs/spec/index-schema.md), not for every
// render. The errors of the provider are kept in the message only (onlyUnavailable): a manifest
// that is gone makes the render fail (500), not the suite 404; a registry that is out is
// provider.ErrUnavailable, a 503 with Retry-After.
func (p Plugin) fetchMetadata(ctx context.Context, repo index.Repo, pkg index.Package, v index.Version) ([]byte, error) {
	var prov provider.Provider
	if p.svc != nil && p.svc.providers != nil {
		prov, _ = p.svc.providers.Provider(repo.Registry)
	}
	if prov == nil {
		return nil, fmt.Errorf("deb: the registry %q of apt/%s is not configured for the renderer", repo.Registry, repo.Name)
	}
	name := ociName(repo, pkg.NameOCI)
	rr, err := prov.Repository(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("deb: repository %s: %w", name, onlyUnavailable(err))
	}
	desc, err := rr.Resolve(ctx, v.Digest.String())
	if err != nil {
		return nil, fmt.Errorf("deb: resolve %s in %s: %w", v.Digest, name, onlyUnavailable(err))
	}
	b, err := readBlob(ctx, rr, ocispec.Descriptor{MediaType: desc.MediaType, Digest: v.Digest, Size: desc.Size}, maxManifest)
	if err != nil {
		return nil, fmt.Errorf("deb: the manifest in %s: %w", name, err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("deb: the manifest %s in %s: %w", v.Digest, name, err)
	}
	parsed, err := profile.Validate(m)
	if err != nil {
		return nil, fmt.Errorf("deb: the manifest %s in %s: %w", v.Digest, name, err)
	}
	l := parsed.Metadata
	if l == nil {
		return nil, fmt.Errorf("deb: the manifest %s in %s has no metadata layer", v.Digest, name)
	}
	b, err = readBlob(ctx, rr, ocispec.Descriptor{MediaType: l.MediaType, Digest: l.Digest, Size: l.Size}, maxMetadata)
	if err != nil {
		return nil, fmt.Errorf("deb: the metadata layer in %s: %w", name, err)
	}
	return b, nil
}

// readBlob fetches desc from rr, at most limit bytes, and checks the bytes against its size and
// digest.
func readBlob(ctx context.Context, rr provider.Repository, desc ocispec.Descriptor, limit int64) ([]byte, error) {
	if desc.Size > limit {
		return nil, fmt.Errorf("%s has %d bytes, at most %d", desc.Digest, desc.Size, limit)
	}
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", desc.Digest, onlyUnavailable(err))
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("read %s: %s", desc.Digest, err.Error())
	case int64(len(b)) != desc.Size || desc.Digest.Validate() != nil || desc.Digest.Algorithm().FromBytes(b) != desc.Digest:
		return nil, fmt.Errorf("the %d bytes fetched are not %s of %d bytes", len(b), desc.Digest, desc.Size)
	}
	return b, nil
}

// onlyUnavailable returns an error with the message of err, an error of the provider, that
// wraps nothing of it: a manifest or a layer the registry does not have (provider.ErrNotFound)
// fails the render, a 500, instead of making the suite a 404. A registry that is out is the
// exception (#669): when err is provider.ErrUnavailable, the error wraps that, and the wait err
// says (provider.RetryAfter), so that the render is a 503 with Retry-After, and still nothing
// else of err.
func onlyUnavailable(err error) error {
	switch after, ok := provider.RetryAfter(err); {
	case !errors.Is(err, provider.ErrUnavailable):
		return errors.New(err.Error())
	case ok:
		return &unavailableError{msg: err.Error(), kind: &provider.UnavailableError{RetryAfter: after}}
	}
	return &unavailableError{msg: err.Error(), kind: provider.ErrUnavailable}
}

// unavailableError is a provider.ErrUnavailable of fetchMetadata (onlyUnavailable): the message
// of the provider's error, and kind, provider.ErrUnavailable or a *provider.UnavailableError
// with its wait.
type unavailableError struct {
	msg  string
	kind error
}

func (e *unavailableError) Error() string { return e.msg }

func (e *unavailableError) Unwrap() error { return e.kind }

// ociName is the OCI repository of a package as the provider names it: repo.Prefix, or
// <format>/<repo> when it is empty, and the package path (docs/spec/manifest-profile.md §4.1),
// as ingest computes it.
func ociName(repo index.Repo, pkgPath string) string {
	base := strings.Trim(repo.Prefix, "/")
	if base == "" {
		base = repo.Format + "/" + repo.Name
	}
	return base + "/" + pkgPath
}
