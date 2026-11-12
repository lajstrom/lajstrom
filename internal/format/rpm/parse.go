package rpm

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // the SHA1 header digest of an rpm's signature; not a security use
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
)

// MaxHeader is the largest head of an rpm the parser reads, as stored: the lead, the signature
// header with its padding and the header, 32 MiB. The file list is the bulk of a header; the
// largest of a distribution are a few MiB.
const MaxHeader = 32 << 20

// The tags of the signature header the parser reads.
const (
	sigSize            = 1000 // header and payload, in bytes
	sigPayloadSize     = 1007 // the uncompressed payload, in bytes
	sigSHA1            = 269  // the SHA1 of the header, hex
	sigLongSize        = 270
	sigLongArchiveSize = 271
	sigSHA256          = 273 // the SHA256 of the header, hex
)

// The tags of the header the parser reads.
const (
	tagName              = 1000
	tagVersion           = 1001
	tagRelease           = 1002
	tagEpoch             = 1003
	tagSummary           = 1004
	tagDescription       = 1005
	tagBuildTime         = 1006
	tagBuildHost         = 1007
	tagSize              = 1009
	tagVendor            = 1011
	tagLicense           = 1014
	tagPackager          = 1015
	tagGroup             = 1016
	tagURL               = 1020
	tagArch              = 1022
	tagOldFilenames      = 1027
	tagFileModes         = 1030
	tagFileFlags         = 1037
	tagSourceRPM         = 1044
	tagArchiveSize       = 1046
	tagNoSource          = 1051
	tagNoPatch           = 1052
	tagChangelogTime     = 1080
	tagChangelogName     = 1081
	tagChangelogText     = 1082
	tagSourcePackage     = 1106
	tagDirIndexes        = 1116
	tagBaseNames         = 1117
	tagDirNames          = 1118
	tagLongSize          = 5009
	tagPayloadDigest     = 5092
	tagPayloadDigestAlgo = 5093
)

// relationTags are the tags of the eight relations: name, flags and version of each, in the
// order of Package.Relations.
var relationTags = [8]struct {
	kind                 string
	name, flags, version int32
}{
	{"provides", 1047, 1112, 1113},
	{"requires", 1049, 1048, 1050},
	{"conflicts", 1054, 1053, 1055},
	{"obsoletes", 1090, 1114, 1115},
	{"recommends", 5046, 5048, 5047},
	{"suggests", 5049, 5051, 5050},
	{"supplements", 5052, 5054, 5053},
	{"enhances", 5055, 5057, 5056},
}

// The flags of a relation and of a file the parser reads.
const (
	// SensePre are the flags of a requirement of the install scripts: PREREQ, SCRIPT_PRE and
	// SCRIPT_POST, what primary marks pre="1".
	SensePre  = 1<<6 | 1<<9 | 1<<10
	fileGhost = 1 << 6
	// hashSHA256 is the OpenPGP number of SHA-256, the algorithm of a payload digest.
	hashSHA256 = 8
	modeDir    = 0o40000
	modeType   = 0o170000
)

var (
	// nameGrammar is the grammar of a package name: letters, digits and ._+-, as rpmbuild
	// allows, the first a letter, a digit or "_", at most 255 bytes.
	nameGrammar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]{0,254}$`)
	// verGrammar is the grammar of a version and of a release: letters, digits and ._+~^,
	// never "-", which separates them.
	verGrammar = regexp.MustCompile(`^[A-Za-z0-9._+~^]{1,255}$`)
	// archGrammar is the grammar of an arch: x86_64, aarch64, noarch, i686, ppc64le.
	archGrammar = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
)

// Package is what an rpm says about itself in its head: the tags the repodata needs, the place
// of its header in the file and, from Parse, the size and the sha256 of the whole file.
type Package struct {
	// Name, Version, Release are the tags NAME, VERSION, RELEASE; Epoch is EPOCH, 0 without one.
	Name, Version, Release string
	Epoch                  int64
	// Arch is the arch the repodata writes: ARCH for a binary package; src for a source
	// package (one without SOURCERPM), nosrc when it has NOSOURCE or NOPATCH.
	Arch string
	// Source is true for a source package.
	Source bool
	// Summary, Description, Packager, URL, License, Vendor, Group, BuildHost, SourceRPM are the
	// strings of the header, the C locale's of an internationalised one, "" without the tag.
	Summary, Description, Packager, URL, License, Vendor, Group, BuildHost, SourceRPM string
	// BuildTime is BUILDTIME in seconds since 1970.
	BuildTime int64
	// InstalledSize is LONGSIZE, else SIZE; ArchiveSize the uncompressed payload's size, the
	// signature header's LONGARCHIVESIZE or PAYLOADSIZE, else the header's ARCHIVESIZE; 0
	// without one.
	InstalledSize, ArchiveSize int64
	// HeaderStart and HeaderEnd are the offsets of the header in the file: after the lead and
	// the padded signature header, and before the payload.
	HeaderStart, HeaderEnd int64
	// Relations are the provides, requires, conflicts, obsoletes, recommends, suggests,
	// supplements and enhances, in that order (RelationKinds), each in the order of the header.
	Relations [8][]Relation
	// Files are the files of the header in its order.
	Files []File
	// Changelogs are the changelog entries in the order of the header, the newest first.
	Changelogs []Changelog
	// Filename is Filename(Name, Version, Release, Arch).
	Filename string
	// Size is the size of the whole file; SHA256 its lowercase hex sha256. Both are zero from
	// ParseHead.
	Size   int64
	SHA256 string

	// signed is the size of header and payload the signature header gives, SIZE or LONGSIZE;
	// 0 without one.
	signed int64
	// payloadDigest is the hex sha256 of the payload the header gives; "" without one, or
	// with one of another algorithm.
	payloadDigest string
}

// Relation is one entry of a relation: the name, the sense flags of the header, the parts of
// the version, "" each when it has none, and whether it is a requirement of the install scripts.
type Relation struct {
	Name                    string
	Flags                   int64
	Epoch, Version, Release string
	Pre                     bool
}

// File is one file of a package: its path and its type in the repodata, "dir", "ghost" or "".
type File struct {
	Path string
	Type string
}

// Changelog is one changelog entry: the author line, its time in seconds since 1970 and its
// text.
type Changelog struct {
	Author string
	Date   int64
	Text   string
}

// RelationKinds are the names of the relations in the order of Package.Relations, the keys of
// the metadata layer and the elements of primary.xml without "rpm:".
func RelationKinds() [8]string {
	var k [8]string
	for i, r := range relationTags {
		k[i] = r.kind
	}
	return k
}

// EVR returns the version of the package as lajstrom.version spells it: version-release, with
// the epoch and ":" before it when it is not 0.
func (pkg *Package) EVR() string { return EVR(pkg.Epoch, pkg.Version, pkg.Release) }

// EVR returns [epoch:]version-release, the epoch only when it is not 0
// (docs/spec/manifest-profile.md §3).
func EVR(epoch int64, version, release string) string {
	if epoch == 0 {
		return version + "-" + release
	}
	return strconv.FormatInt(epoch, 10) + ":" + version + "-" + release
}

// Filename returns the file name of a package, <name>-<version>-<release>.<arch>.rpm, without
// the epoch (docs/spec/manifest-profile.md §2).
func Filename(name, version, release, arch string) string {
	return name + "-" + version + "-" + release + "." + arch + ".rpm"
}

// SplitEVR splits a version as relations and lajstrom.version write it, [epoch:]version[-release],
// into its parts: the epoch is what comes before a ":" when it is all digits, the release what
// follows the last "-".
func SplitEVR(s string) (epoch, version, release string) {
	if i := strings.IndexByte(s, ':'); i > 0 && strings.Trim(s[:i], "0123456789") == "" {
		epoch, s = s[:i], s[i+1:]
	}
	if i := strings.LastIndexByte(s, '-'); i >= 0 {
		return epoch, s[:i], s[i+1:]
	}
	return epoch, s, ""
}

// Parse reads an rpm from r to its end in one pass: the head, which ParseHead reads, and the
// payload, which it does not open; it checks that the file has the size the signature header
// gives and that the payload has the sha256 the header gives, when they give them. Every byte
// goes through sha256 on the way, and through the inspector of a streamed upload, which does the
// checking. Errors about the bytes wrap format.ErrInvalid and say what was wrong; a failure of
// r is returned as it is, wrapped.
func Parse(r io.Reader) (*Package, error) {
	src := &source{r: r}
	sum := &counter{r: src, h: sha256.New()}
	in := &inspector{}
	_, err := io.Copy(in, sum)
	if err == nil {
		err = in.Close()
	}
	if err != nil {
		return nil, src.blame(err)
	}
	pkg := in.pkg
	pkg.Size, pkg.SHA256 = sum.n, hex.EncodeToString(sum.h.Sum(nil))
	return pkg, nil
}

// ParseHead reads the head of an rpm from r, the lead, the signature header and the header, at
// most limit bytes, and nothing after it: Validate learns what a blob says about itself without
// reading its payload. Size and SHA256 of the result are zero. Errors as Parse.
func ParseHead(r io.Reader, limit int64) (*Package, error) {
	src := &source{r: r}
	head, err := readHead(src, limit)
	if err != nil {
		return nil, src.blame(err)
	}
	pkg, err := parseHead(head, limit)
	if err != nil {
		return nil, src.blame(err)
	}
	return pkg, nil
}

// readHead reads the head of an rpm from r, as far as headLen asks.
func readHead(r io.Reader, limit int64) ([]byte, error) {
	var head []byte
	for {
		n, known := headLen(head, limit)
		if len(head) >= n {
			return head, nil
		}
		buf := make([]byte, n-len(head))
		k, err := io.ReadFull(r, buf)
		head = append(head, buf[:k]...)
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return head, nil // parseHead says what is missing
		case err != nil:
			return nil, err
		case known:
			return head, nil
		}
	}
}

// headLen returns the length of the head of an rpm whose first bytes are b: known is false when
// b is too short to tell, and n is then the length b must reach to tell more; it never asks for
// more than limit bytes. A lead or an intro that is not one, or a head over limit, ends the
// head where it is: parseHead refuses it there.
func headLen(b []byte, limit int64) (n int, known bool) {
	if len(b) < leadLen+introLen {
		return leadLen + introLen, false
	}
	if !bytes.Equal(b[:4], leadMagic) {
		return len(b), true
	}
	sig, err := headerLen("", b[leadLen:])
	if err != nil {
		return len(b), true
	}
	at := int64(leadLen) + sig
	at += (8 - at%8) % 8
	if at+introLen > limit {
		return len(b), true
	}
	if int64(len(b)) < at+introLen {
		return int(at + introLen), false
	}
	hdr, err := headerLen("", b[at:])
	if err != nil || at+hdr > limit {
		return len(b), true
	}
	return int(at + hdr), true
}

// parseHead reads the head b of an rpm: the lead, which must have the magic, version 3 or 4 and
// a header-style signature; the signature header and its padding; and the header, at most
// limit bytes in all. The header digests of the signature header, SHA256 and SHA1, are checked
// when present. Errors wrap format.ErrInvalid.
func parseHead(b []byte, limit int64) (*Package, error) {
	if len(b) < leadLen {
		return nil, fmt.Errorf("%w: the file has %d bytes, fewer than the lead of %d", format.ErrInvalid, len(b), leadLen)
	}
	switch major, sigType := b[4], binary.BigEndian.Uint16(b[78:]); {
	case !bytes.Equal(b[:4], leadMagic):
		return nil, fmt.Errorf("%w: the file does not start with the rpm lead magic", format.ErrInvalid)
	case major != 3 && major != 4:
		return nil, fmt.Errorf("%w: the lead is of rpm version %d, not 3 or 4", format.ErrInvalid, major)
	case sigType != sigTypeHeader:
		return nil, fmt.Errorf("%w: the lead has the signature type %d, not %d (a header)", format.ErrInvalid, sigType, sigTypeHeader)
	}
	sig, end, err := headerAt(b, leadLen, "the signature header", limit)
	if err != nil {
		return nil, err
	}
	start := end + (8-end%8)%8
	if int64(len(b)) < start {
		return nil, fmt.Errorf("%w: truncated: the padding of the signature header has %d of %d bytes", format.ErrInvalid, int64(len(b))-end, start-end)
	}
	hdr, end, err := headerAt(b, start, "the header", limit)
	if err != nil {
		return nil, err
	}
	if err := checkDigests(sig, b[start:end]); err != nil {
		return nil, err
	}
	pkg, err := packageOf(sig, hdr)
	if err != nil {
		return nil, err
	}
	pkg.HeaderStart, pkg.HeaderEnd = start, end
	return pkg, nil
}

// headerAt reads the header structure what at offset at of b, which must end within limit, and
// returns it with its end.
func headerAt(b []byte, at int64, what string, limit int64) (*header, int64, error) {
	if int64(len(b)) < at+introLen {
		return nil, 0, fmt.Errorf("%w: truncated: %s has %d of the %d bytes of its intro", format.ErrInvalid, what, max(int64(len(b))-at, 0), introLen)
	}
	n, err := headerLen(what, b[at:])
	switch {
	case err != nil:
		return nil, 0, err
	case at+n > limit:
		return nil, 0, fmt.Errorf("%w: %s ends at byte %d, past the bound of %d bytes of the head of an rpm", format.ErrInvalid, what, at+n, limit)
	case int64(len(b)) < at+n:
		return nil, 0, fmt.Errorf("%w: truncated: %s has %d of %d bytes", format.ErrInvalid, what, int64(len(b))-at, n)
	}
	h, err := parseHeader(what, b[at:at+n])
	return h, at + n, err
}

// checkDigests checks the header digests of the signature header sig, SHA256 and SHA1 in hex,
// against the bytes of the header, when it has them.
func checkDigests(sig *header, hdr []byte) error {
	for _, d := range []struct {
		tag  int32
		name string
		h    func() hash.Hash
	}{{sigSHA256, "SHA256", sha256.New}, {sigSHA1, "SHA1", sha1.New}} {
		want, ok, err := sig.str(d.tag)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		h := d.h()
		h.Write(hdr)
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(want, got) {
			return fmt.Errorf("%w: the header has the %s %s, the signature header says %q", format.ErrInvalid, d.name, got, want)
		}
	}
	return nil
}

// packageOf reads the Package out of the signature header sig and the header hdr.
func packageOf(sig, hdr *header) (*Package, error) {
	pkg, err := identity(hdr)
	if err != nil {
		return nil, err
	}
	if err := pkg.readStrings(hdr); err != nil {
		return nil, err
	}
	if pkg.Source {
		pkg.SourceRPM = ""
	}
	if err := pkg.readSizes(sig, hdr); err != nil {
		return nil, err
	}
	for i, rt := range relationTags {
		if pkg.Relations[i], err = relations(hdr, rt.kind, rt.name, rt.flags, rt.version); err != nil {
			return nil, err
		}
	}
	if pkg.Files, err = files(hdr); err != nil {
		return nil, err
	}
	if pkg.Changelogs, err = changelogs(hdr); err != nil {
		return nil, err
	}
	if pkg.payloadDigest, err = payloadDigest(hdr); err != nil {
		return nil, err
	}
	pkg.Filename = Filename(pkg.Name, pkg.Version, pkg.Release, pkg.Arch)
	return pkg, nil
}

// identity reads what names the package: NAME, VERSION, RELEASE and ARCH of their grammars,
// EPOCH, and whether it is a source package, whose arch is then src or nosrc.
func identity(hdr *header) (*Package, error) {
	pkg := &Package{}
	var errs []error
	for _, f := range []struct {
		tag     int32
		dst     *string
		grammar *regexp.Regexp
		what    string
	}{
		{tagName, &pkg.Name, nameGrammar, "NAME"},
		{tagVersion, &pkg.Version, verGrammar, "VERSION"},
		{tagRelease, &pkg.Release, verGrammar, "RELEASE"},
		{tagArch, &pkg.Arch, archGrammar, "ARCH"},
	} {
		v, ok, err := hdr.str(f.tag)
		switch {
		case err != nil:
			errs = append(errs, err)
		case !ok:
			errs = append(errs, fmt.Errorf("%w: the header has no %s", format.ErrInvalid, f.what))
		case !f.grammar.MatchString(v):
			errs = append(errs, fmt.Errorf("%w: the %s %q is not of the grammar %s", format.ErrInvalid, f.what, v, f.grammar))
		}
		*f.dst = v
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	var err error
	if pkg.Epoch, err = epochOf(hdr); err != nil {
		return nil, err
	}
	pkg.Source = !hdr.has(tagSourceRPM) || hdr.has(tagSourcePackage)
	switch {
	case !pkg.Source && (pkg.Arch == "src" || pkg.Arch == "nosrc"):
		return nil, fmt.Errorf("%w: a binary package (it has a SOURCERPM) of the arch %q, which names source packages", format.ErrInvalid, pkg.Arch)
	case pkg.Source && (hdr.has(tagNoSource) || hdr.has(tagNoPatch)):
		pkg.Arch = "nosrc"
	case pkg.Source:
		pkg.Arch = "src"
	}
	return pkg, nil
}

// epochOf returns EPOCH, 0 without it; one that rpm reads as a negative number wraps
// format.ErrInvalid.
func epochOf(hdr *header) (int64, error) {
	epoch, _, err := hdr.num(tagEpoch)
	switch {
	case err != nil:
		return 0, err
	case epoch > 1<<31-1:
		return 0, fmt.Errorf("%w: the EPOCH %d is negative as rpm reads it", format.ErrInvalid, epoch)
	}
	return epoch, nil
}

// readStrings reads the strings of the header that the repodata shows.
func (pkg *Package) readStrings(hdr *header) error {
	for _, f := range []struct {
		tag int32
		dst *string
	}{
		{tagSummary, &pkg.Summary}, {tagDescription, &pkg.Description}, {tagPackager, &pkg.Packager},
		{tagURL, &pkg.URL}, {tagLicense, &pkg.License}, {tagVendor, &pkg.Vendor}, {tagGroup, &pkg.Group},
		{tagBuildHost, &pkg.BuildHost}, {tagSourceRPM, &pkg.SourceRPM},
	} {
		v, _, err := hdr.str(f.tag)
		if err != nil {
			return err
		}
		*f.dst = v
	}
	var err error
	pkg.BuildTime, _, err = hdr.num(tagBuildTime)
	return err
}

// readSizes reads the installed size, the archive size and the size of header and payload.
func (pkg *Package) readSizes(sig, hdr *header) error {
	first := func(h *header, tags ...int32) (int64, error) {
		for _, t := range tags {
			if n, ok, err := h.num(t); err != nil || ok {
				return n, err
			}
		}
		return 0, nil
	}
	var err error
	if pkg.InstalledSize, err = first(hdr, tagLongSize, tagSize); err != nil {
		return err
	}
	if pkg.ArchiveSize, err = first(sig, sigLongArchiveSize, sigPayloadSize); err == nil && pkg.ArchiveSize == 0 {
		pkg.ArchiveSize, err = first(hdr, tagArchiveSize)
	}
	if err != nil {
		return err
	}
	pkg.signed, err = first(sig, sigLongSize, sigSize)
	return err
}

// relations reads one relation of the header: the names, and the flags and versions beside
// them, as many of each; a relation without flags or versions has none.
func relations(hdr *header, kind string, nameTag, flagsTag, versionTag int32) ([]Relation, error) {
	names, err := hdr.strings(nameTag)
	if err != nil || names == nil {
		return []Relation{}, err
	}
	flags, err := hdr.ints(flagsTag)
	if err != nil {
		return nil, err
	}
	versions, err := hdr.strings(versionTag)
	if err != nil {
		return nil, err
	}
	if (flags != nil && len(flags) != len(names)) || (versions != nil && len(versions) != len(names)) {
		return nil, fmt.Errorf("%w: the header has %d %s names, %d flags and %d versions", format.ErrInvalid,
			len(names), kind, len(flags), len(versions))
	}
	if err := hdr.spend(int64(len(names)) * relationSize); err != nil {
		return nil, err
	}
	out := make([]Relation, len(names))
	for i, n := range names {
		r := Relation{Name: n}
		if flags != nil {
			r.Flags = flags[i]
		}
		if versions != nil {
			r.Epoch, r.Version, r.Release = SplitEVR(versions[i])
		}
		r.Pre = kind == "requires" && r.Flags&SensePre != 0
		out[i] = r
	}
	return out, nil
}

// files reads the file list of the header: the compressed file names, BASENAMES with
// DIRINDEXES into DIRNAMES, or OLDFILENAMES of an old package, with FILEMODES and FILEFLAGS
// for their types.
func files(hdr *header) ([]File, error) {
	paths, err := hdr.strings(tagOldFilenames)
	if err != nil {
		return nil, err
	}
	if hdr.has(tagBaseNames) {
		if paths, err = compressedNames(hdr); err != nil {
			return nil, err
		}
	}
	if len(paths) == 0 {
		return []File{}, nil
	}
	return typed(hdr, paths)
}

// typed returns the files of paths with their types: ghost when FILEFLAGS says %ghost, dir
// when FILEMODES says a directory; FILEMODES and FILEFLAGS, when present, have one value a file.
func typed(hdr *header, paths []string) ([]File, error) {
	modes, err := hdr.ints(tagFileModes)
	if err != nil {
		return nil, err
	}
	flags, err := hdr.ints(tagFileFlags)
	if err != nil {
		return nil, err
	}
	if (modes != nil && len(modes) != len(paths)) || (flags != nil && len(flags) != len(paths)) {
		return nil, fmt.Errorf("%w: the header has %d files, %d modes and %d flags", format.ErrInvalid, len(paths), len(modes), len(flags))
	}
	if err := hdr.spend(int64(len(paths)) * fileSize); err != nil {
		return nil, err
	}
	out := make([]File, len(paths))
	for i, p := range paths {
		out[i].Path = p
		switch {
		case flags != nil && flags[i]&fileGhost != 0:
			out[i].Type = "ghost"
		case modes != nil && modes[i]&modeType == modeDir:
			out[i].Type = "dir"
		}
	}
	return out, nil
}

// compressedNames joins BASENAMES with the DIRNAMES that DIRINDEXES name.
func compressedNames(hdr *header) ([]string, error) {
	bases, err := hdr.strings(tagBaseNames)
	if err != nil {
		return nil, err
	}
	dirs, err := hdr.strings(tagDirNames)
	if err != nil {
		return nil, err
	}
	indexes, err := hdr.ints(tagDirIndexes)
	if err != nil {
		return nil, err
	}
	if len(indexes) != len(bases) {
		return nil, fmt.Errorf("%w: the header has %d BASENAMES and %d DIRINDEXES", format.ErrInvalid, len(bases), len(indexes))
	}
	size := int64(len(bases)) * stringSize
	for i, b := range bases {
		if indexes[i] >= int64(len(dirs)) {
			return nil, fmt.Errorf("%w: file %d is in directory %d of %d", format.ErrInvalid, i+1, indexes[i], len(dirs))
		}
		size += int64(len(dirs[indexes[i]]) + len(b))
	}
	if err := hdr.spend(size); err != nil { // the paths, before they are joined
		return nil, err
	}
	paths := make([]string, len(bases))
	for i, b := range bases {
		paths[i] = dirs[indexes[i]] + b
	}
	return paths, nil
}

// changelogs reads the changelog entries: CHANGELOGTIME, CHANGELOGNAME and CHANGELOGTEXT, as
// many of each.
func changelogs(hdr *header) ([]Changelog, error) {
	times, err := hdr.ints(tagChangelogTime)
	if err != nil {
		return nil, err
	}
	names, err := hdr.strings(tagChangelogName)
	if err != nil {
		return nil, err
	}
	texts, err := hdr.strings(tagChangelogText)
	if err != nil {
		return nil, err
	}
	if len(names) != len(times) || len(texts) != len(times) {
		return nil, fmt.Errorf("%w: the header has %d changelog times, %d names and %d texts", format.ErrInvalid, len(times), len(names), len(texts))
	}
	if err := hdr.spend(int64(len(times)) * changelogSize); err != nil {
		return nil, err
	}
	out := make([]Changelog, len(times))
	for i := range times {
		out[i] = Changelog{Author: names[i], Date: times[i], Text: texts[i]}
	}
	return out, nil
}

// payloadDigest returns the hex sha256 of the payload that the header gives, "" when it gives
// none or one of another algorithm.
func payloadDigest(hdr *header) (string, error) {
	digests, err := hdr.strings(tagPayloadDigest)
	if err != nil || len(digests) == 0 {
		return "", err
	}
	algo, ok, err := hdr.num(tagPayloadDigestAlgo)
	if err != nil || !ok || algo != hashSHA256 {
		return "", err
	}
	d := strings.ToLower(digests[0])
	if len(d) != sha256.Size*2 || strings.Trim(d, "0123456789abcdef") != "" {
		return "", fmt.Errorf("%w: the payload digest %q is not a sha256 in hex", format.ErrInvalid, digests[0])
	}
	return d, nil
}

// source is the reader a parse reads from. It keeps the first error of r other than io.EOF,
// so that blame can tell a failure to read from a malformed package.
type source struct {
	r   io.Reader
	err error
}

func (s *source) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && s.err == nil {
		s.err = err
	}
	return n, err
}

// blame returns the error of a parse: the failure of the reader when there was one, since what
// the parser made of the missing bytes says nothing about the package; else err.
func (s *source) blame(err error) error {
	if s.err != nil {
		return fmt.Errorf("rpm: read the package: %w", s.err)
	}
	return err
}

// counter passes the bytes of r through h and counts them.
type counter struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	c.n += int64(n)
	return n, err
}
