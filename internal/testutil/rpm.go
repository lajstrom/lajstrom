package testutil

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // the MD5 of an rpm's signature header; not a security use
	"crypto/sha1" //nolint:gosec // the SHA1 header digest of an rpm; not a security use
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// The sense flags of an rpm relation and the file flag of a %ghost, as rpm's headers write
// them, for RPMDep.Flags and the tests.
const (
	RPMSenseLess       = 1 << 1
	RPMSenseGreater    = 1 << 2
	RPMSenseEqual      = 1 << 3
	RPMSensePrereq     = 1 << 6
	RPMSenseScriptPre  = 1 << 9
	RPMSenseScriptPost = 1 << 10
	RPMSenseFindReqs   = 1 << 14
	RPMSenseRpmlib     = 1 << 24
	rpmFileGhost       = 1 << 6
)

// RPMOptions are the options of RPM. Every empty field takes its default, so RPMOptions{} is a
// valid package.
type RPMOptions struct {
	// Name is the NAME tag; default "hello".
	Name string
	// Epoch is the EPOCH tag in decimal; empty (the default) leaves the tag out.
	Epoch string
	// Version and Release are the VERSION and RELEASE tags; default "1.0.0" and "1.el9".
	Version, Release string
	// Arch is the ARCH tag; default "x86_64".
	Arch string
	// Source makes a source package: no SOURCERPM, SOURCEPACKAGE set, the file name and the
	// repodata arch src.
	Source bool
	// Summary, Description, License, URL, Vendor, Packager and Group are the tags of the same
	// names; empty ones take a default, but Vendor and Packager, which are left out.
	Summary, Description, License, URL, Vendor, Packager, Group string
	// Requires and Provides are the relations; nil takes a default: requires on
	// rpmlib(CompressedFileNames), libc.so.6()(64bit) and, for the post script, /bin/sh;
	// provides of the name and of name(x86-64), both = the EVR.
	Requires, Provides []RPMDep
	// Conflicts, Obsoletes, Recommends, Suggests, Supplements and Enhances are written when not
	// empty.
	Conflicts, Obsoletes, Recommends, Suggests, Supplements, Enhances []RPMDep
	// Files are the regular files, an absolute path to its content; default /usr/bin/<name> and
	// /usr/share/doc/<name>/README.
	Files map[string]string
	// Dirs are directories the package owns; default /usr/share/doc/<name>.
	Dirs []string
	// Ghosts are %ghost files: in the header, not in the payload.
	Ghosts []string
	// Changelogs are the changelog entries, the newest first; nil takes one entry.
	Changelogs []RPMChangelog
	// Filler adds a binary tag of that many bytes to the header, to make it large.
	Filler int
	// NoDigests leaves SIZE, MD5, SHA1, SHA256 out of the signature header and the payload
	// digest out of the header, as an rpm of before rpm 4.14 may lack them.
	NoDigests bool
	// Major is the major version of the lead; default 3, as rpmbuild writes it.
	Major byte
}

// RPMDep is a relation as the header holds it: the name, the sense flags and the version,
// [epoch:]version[-release], empty when the relation has none.
type RPMDep struct {
	Name    string
	Flags   uint32
	Version string
}

// RPMChangelog is a changelog entry: its time in seconds since 1970, the author line and the
// text.
type RPMChangelog struct {
	Time   int64
	Author string
	Text   string
}

// RPMFile is a file of the package as the repodata lists it: the path, and "dir", "ghost" or
// "" as its type.
type RPMFile struct {
	Path string
	Type string
}

// RPMInfo is what a parser finds in the package RPM builds.
type RPMInfo struct {
	// Name, Version, Release are the tags; Epoch is the EPOCH tag, 0 without one.
	Name, Version, Release string
	Epoch                  int64
	// Arch is the arch the repodata writes: the ARCH tag, or src for a source package.
	Arch string
	// EVR is [epoch:]version-release, the epoch only when it is not 0: lajstrom.version.
	EVR string
	// Filename is <name>-<version>-<release>.<arch>.rpm.
	Filename string
	// Summary, Description, License, URL, Vendor, Packager, Group, BuildHost, SourceRPM are the
	// strings of the header, SourceRPM empty for a source package.
	Summary, Description, License, URL, Vendor, Packager, Group, BuildHost, SourceRPM string
	// BuildTime is BUILDTIME; InstalledSize the SIZE tag, the sum of the file sizes;
	// ArchiveSize the uncompressed payload's size, PAYLOADSIZE.
	BuildTime, InstalledSize, ArchiveSize int64
	// HeaderStart and HeaderEnd are the offsets of the header in the file.
	HeaderStart, HeaderEnd int64
	// Requires, Provides, … are the relations as written.
	Requires, Provides, Conflicts, Obsoletes, Recommends, Suggests, Supplements, Enhances []RPMDep
	// Files are every file of the header, sorted by path.
	Files []RPMFile
	// Changelogs are the entries as written.
	Changelogs []RPMChangelog
	// Payload is the gzip-compressed cpio archive after the header.
	Payload []byte
	// PayloadSHA256 is the hex sha256 of Payload that the header gives, with SIZE in the
	// signature header; empty with RPMOptions.NoDigests, which writes neither.
	PayloadSHA256 string
	// Size is the size of the whole file in bytes.
	Size int64
	// Checksums are the digests of the whole file.
	Checksums
}

// RPM returns a package in the rpm v4 file format: the lead, a signature header with an
// immutable region, the header with its immutable region and the tags rpmbuild writes for the
// files, relations and changelog, and a gzip-compressed cpio (newc) payload of the regular
// files; and the facts a parser finds in it. The same options give the same bytes. It writes
// what the rpm format says, not what a distribution packed: nothing is vendored. RPM panics
// when the options cannot be written (a relative path).
func RPM(opts RPMOptions) ([]byte, RPMInfo) {
	o := opts.withDefaults()
	info := RPMInfo{
		Name: o.Name, Version: o.Version, Release: o.Release, Arch: o.Arch,
		Summary: o.Summary, Description: o.Description, License: o.License, URL: o.URL,
		Vendor: o.Vendor, Packager: o.Packager, Group: o.Group, BuildHost: "build.lajstrom.dev",
		BuildTime: fixtureTime.Unix(), Requires: o.Requires, Provides: o.Provides, Conflicts: o.Conflicts,
		Obsoletes: o.Obsoletes, Recommends: o.Recommends, Suggests: o.Suggests, Supplements: o.Supplements,
		Enhances: o.Enhances, Changelogs: o.Changelogs,
	}
	info.EVR = o.Version + "-" + o.Release
	if o.Epoch != "" {
		e, err := strconv.ParseInt(o.Epoch, 10, 32)
		must(err)
		info.Epoch = e
		if e != 0 {
			info.EVR = o.Epoch + ":" + info.EVR
		}
	}
	if o.Source {
		info.Arch = "src"
	} else {
		info.SourceRPM = o.Name + "-" + o.Version + "-" + o.Release + ".src.rpm"
	}
	info.Filename = o.Name + "-" + o.Version + "-" + o.Release + "." + info.Arch + ".rpm"

	files := rpmFiles(o)
	info.Files = make([]RPMFile, len(files))
	cpio := cpioNewc(files)
	info.Payload = gzipOf(cpio)
	info.ArchiveSize = int64(len(cpio))
	if !o.NoDigests {
		sum := sha256.Sum256(info.Payload)
		info.PayloadSHA256 = hex.EncodeToString(sum[:])
	}
	for i, f := range files {
		info.Files[i] = RPMFile{Path: f.path, Type: f.kind}
		info.InstalledSize += int64(len(f.body))
	}
	header := rpmHeader(63, rpmTags(o, info, files))
	sig := rpmSignature(o, header, info.Payload, int64(len(cpio)))

	var b bytes.Buffer
	b.Write(rpmLead(o, info))
	b.Write(sig)
	b.Write(make([]byte, (8-len(sig)%8)%8))
	info.HeaderStart = int64(b.Len())
	b.Write(header)
	info.HeaderEnd = int64(b.Len())
	b.Write(info.Payload)
	out := b.Bytes()
	info.Size = int64(len(out))
	info.Checksums = checksumsOf(out)
	return out, info
}

// withDefaults returns the options with every empty field set to its default.
func (o RPMOptions) withDefaults() RPMOptions {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&o.Name, "hello")
	def(&o.Version, "1.0.0")
	def(&o.Release, "1.el9")
	def(&o.Arch, "x86_64")
	def(&o.Summary, "greets the tests of the rpm format")
	def(&o.Description, "A package built in process by internal/testutil.\nNothing in it comes from a distribution.")
	def(&o.License, "Apache-2.0")
	def(&o.URL, "https://lajstrom.dev/")
	def(&o.Group, "Unspecified")
	if o.Major == 0 {
		o.Major = 3
	}
	evr := o.Version + "-" + o.Release
	if o.Epoch != "" {
		evr = o.Epoch + ":" + evr
	}
	if o.Requires == nil {
		o.Requires = []RPMDep{
			{Name: "/bin/sh", Flags: RPMSenseScriptPost},
			{Name: "libc.so.6()(64bit)", Flags: RPMSenseFindReqs},
			{Name: "rpmlib(CompressedFileNames)", Flags: RPMSenseLess | RPMSenseEqual | RPMSenseRpmlib, Version: "3.0.4-1"},
		}
	}
	if o.Provides == nil {
		o.Provides = []RPMDep{{Name: o.Name, Flags: RPMSenseEqual, Version: evr}, {Name: o.Name + "(x86-64)", Flags: RPMSenseEqual, Version: evr}}
	}
	if o.Files == nil {
		o.Files = map[string]string{
			"/usr/bin/" + o.Name:                   "#!/bin/sh\necho hello\n",
			"/usr/share/doc/" + o.Name + "/README": "Built by internal/testutil for the tests.\n",
		}
	}
	if o.Dirs == nil {
		o.Dirs = []string{"/usr/share/doc/" + o.Name}
	}
	if o.Changelogs == nil {
		o.Changelogs = []RPMChangelog{{Time: fixtureTime.Unix(), Author: "Lajstrom Tests <tests@lajstrom.dev> - " + evr, Text: "- First build"}}
	}
	return o
}

// rpmFile is one file of the header: its path, content, mode, flags and repodata type.
type rpmFile struct {
	path  string
	body  []byte
	mode  uint16
	flags uint32
	kind  string
}

// rpmFiles returns the files, directories and ghosts of o sorted by path.
func rpmFiles(o RPMOptions) []rpmFile {
	var files []rpmFile
	for p, body := range o.Files {
		files = append(files, rpmFile{path: rpmPath(p), body: []byte(body), mode: 0o100644})
	}
	for _, p := range o.Dirs {
		files = append(files, rpmFile{path: rpmPath(p), mode: 0o40755, kind: "dir"})
	}
	for _, p := range o.Ghosts {
		files = append(files, rpmFile{path: rpmPath(p), mode: 0o100644, flags: rpmFileGhost, kind: "ghost"})
	}
	slices.SortFunc(files, func(a, b rpmFile) int { return strings.Compare(a.path, b.path) })
	return files
}

// rpmPath checks that p is absolute and returns it cleaned.
func rpmPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		panic(fmt.Sprintf("testutil: rpm file %q is not absolute", p))
	}
	return path.Clean(p)
}

// rpmTag is one tag of a header: its number, its type and its value, a string, []string,
// []uint16, []uint32 or []byte.
type rpmTag struct {
	tag   int32
	typ   uint32
	value any
}

// The header types of rpm.
const (
	rpmInt16       = 3
	rpmInt32       = 4
	rpmString      = 6
	rpmBin         = 7
	rpmStringArray = 8
	rpmI18NString  = 9
)

// rpmTags returns the tags of the header of o.
func rpmTags(o RPMOptions, info RPMInfo, files []rpmFile) []rpmTag {
	str := func(tag int32, s string) rpmTag { return rpmTag{tag, rpmString, s} }
	i18n := func(tag int32, s string) rpmTag { return rpmTag{tag, rpmI18NString, []string{s}} }
	tags := []rpmTag{
		{100, rpmStringArray, []string{"C"}},
		str(1000, o.Name), str(1001, o.Version), str(1002, o.Release),
		i18n(1004, o.Summary), i18n(1005, o.Description),
		{1006, rpmInt32, []uint32{uint32(info.BuildTime)}}, //nolint:gosec // fixtureTime, far below 2^32
		str(1007, info.BuildHost),
		{1009, rpmInt32, []uint32{uint32(info.InstalledSize)}}, //nolint:gosec // a few bytes of test files
		str(1014, o.License), i18n(1016, o.Group), str(1020, o.URL), str(1021, "linux"), str(1022, o.Arch),
		str(1124, "cpio"), str(1125, "gzip"), str(1126, "9"),
	}
	if o.Epoch != "" {
		tags = append(tags, rpmTag{1003, rpmInt32, []uint32{uint32(info.Epoch)}}) //nolint:gosec // parsed as 32 bits
	}
	if o.Vendor != "" {
		tags = append(tags, str(1011, o.Vendor))
	}
	if o.Packager != "" {
		tags = append(tags, str(1015, o.Packager))
	}
	if o.Source {
		tags = append(tags, rpmTag{1106, rpmInt32, []uint32{1}})
	} else {
		tags = append(tags, str(1044, info.SourceRPM))
	}
	for _, rel := range []struct {
		deps                 []RPMDep
		name, flags, version int32
	}{
		{o.Provides, 1047, 1112, 1113}, {o.Requires, 1049, 1048, 1050}, {o.Conflicts, 1054, 1053, 1055},
		{o.Obsoletes, 1090, 1114, 1115}, {o.Recommends, 5046, 5048, 5047}, {o.Suggests, 5049, 5051, 5050},
		{o.Supplements, 5052, 5054, 5053}, {o.Enhances, 5055, 5057, 5056},
	} {
		if len(rel.deps) == 0 {
			continue
		}
		var names, versions []string
		var flags []uint32
		for _, d := range rel.deps {
			names, flags, versions = append(names, d.Name), append(flags, d.Flags), append(versions, d.Version)
		}
		tags = append(tags, rpmTag{rel.name, rpmStringArray, names}, rpmTag{rel.flags, rpmInt32, flags},
			rpmTag{rel.version, rpmStringArray, versions})
	}
	if len(o.Changelogs) > 0 {
		var times []uint32
		var names, texts []string
		for _, c := range o.Changelogs {
			times, names, texts = append(times, uint32(c.Time)), append(names, c.Author), append(texts, c.Text) //nolint:gosec // test times
		}
		tags = append(tags, rpmTag{1080, rpmInt32, times}, rpmTag{1081, rpmStringArray, names}, rpmTag{1082, rpmStringArray, texts})
	}
	if len(files) > 0 {
		tags = append(tags, fileTags(files)...)
	}
	if o.Filler > 0 {
		tags = append(tags, rpmTag{5999, rpmBin, make([]byte, o.Filler)})
	}
	if !o.NoDigests {
		tags = append(tags, rpmTag{5092, rpmStringArray, []string{info.PayloadSHA256}}, rpmTag{5093, rpmInt32, []uint32{8}})
	}
	return tags
}

// fileTags returns the file tags of the header: the compressed file names (BASENAMES,
// DIRNAMES, DIRINDEXES), sizes, modes, times, digests, flags, owners and the digest algorithm.
func fileTags(files []rpmFile) []rpmTag {
	var bases, dirs, digests, users []string
	var indexes, sizes, mtimes, flags []uint32
	var modes []uint16
	dirIndex := map[string]uint32{}
	for _, f := range files {
		dir, base := path.Split(f.path)
		i, ok := dirIndex[dir]
		if !ok {
			i = uint32(len(dirs)) //nolint:gosec // a few test directories
			dirIndex[dir] = i
			dirs = append(dirs, dir)
		}
		digest := ""
		if f.kind == "" {
			sum := sha256.Sum256(f.body)
			digest = hex.EncodeToString(sum[:])
		}
		bases, indexes, digests, users = append(bases, base), append(indexes, i), append(digests, digest), append(users, "root")
		sizes, mtimes, flags, modes = append(sizes, uint32(len(f.body))), append(mtimes, uint32(fixtureTime.Unix())), append(flags, f.flags), append(modes, f.mode) //nolint:gosec // test files
	}
	return []rpmTag{
		{1028, rpmInt32, sizes}, {1030, rpmInt16, modes}, {1034, rpmInt32, mtimes}, {1035, rpmStringArray, digests},
		{1037, rpmInt32, flags}, {1039, rpmStringArray, users}, {1040, rpmStringArray, users},
		{1116, rpmInt32, indexes}, {1117, rpmStringArray, bases}, {1118, rpmStringArray, dirs}, {5011, rpmInt32, []uint32{8}},
	}
}

// rpmSignature returns the signature header of a package whose header and payload are given:
// SIZE, MD5, PAYLOADSIZE, SHA1 and SHA256 of the header, unless o.NoDigests.
func rpmSignature(o RPMOptions, header, payload []byte, archive int64) []byte {
	tags := []rpmTag{{1007, rpmInt32, []uint32{uint32(archive)}}} //nolint:gosec // a small test payload
	if !o.NoDigests {
		md := md5.New() //nolint:gosec // rpm's MD5 of header and payload
		md.Write(header)
		md.Write(payload)
		s1 := sha1.Sum(header) //nolint:gosec // rpm's SHA1 of the header
		s256 := sha256.Sum256(header)
		tags = append(tags,
			rpmTag{1000, rpmInt32, []uint32{uint32(len(header) + len(payload))}}, //nolint:gosec // a small test package
			rpmTag{1004, rpmBin, md.Sum(nil)},
			rpmTag{269, rpmString, hex.EncodeToString(s1[:])},
			rpmTag{273, rpmString, hex.EncodeToString(s256[:])})
	}
	return rpmHeader(62, tags)
}

// rpmHeader returns a header structure with an immutable region, region: the intro (magic,
// four reserved bytes, the number of index entries and the size of the data), the index
// sorted by tag with the region first, and the data, each value aligned as its type asks and
// the region trailer last.
func rpmHeader(region int32, tags []rpmTag) []byte {
	slices.SortFunc(tags, func(a, b rpmTag) int { return int(a.tag - b.tag) })
	var data bytes.Buffer
	type entry struct{ tag, typ, offset, count uint32 }
	entries := []entry{{uint32(region), rpmBin, 0, 16}} //nolint:gosec // a tag number
	be := binary.BigEndian
	for _, t := range tags {
		align := map[uint32]int{rpmInt16: 2, rpmInt32: 4}[t.typ]
		for align > 0 && data.Len()%align != 0 {
			data.WriteByte(0)
		}
		e := entry{tag: uint32(t.tag), typ: t.typ, offset: uint32(data.Len())} //nolint:gosec // small offsets
		switch v := t.value.(type) {
		case string:
			data.WriteString(v + "\x00")
			e.count = 1
		case []string:
			for _, s := range v {
				data.WriteString(s + "\x00")
			}
			e.count = uint32(len(v)) //nolint:gosec // small
		case []uint16:
			for _, n := range v {
				_ = binary.Write(&data, be, n)
			}
			e.count = uint32(len(v)) //nolint:gosec // small
		case []uint32:
			for _, n := range v {
				_ = binary.Write(&data, be, n)
			}
			e.count = uint32(len(v)) //nolint:gosec // small
		case []byte:
			data.Write(v)
			e.count = uint32(len(v)) //nolint:gosec // small
		}
		entries = append(entries, e)
	}
	entries[0].offset = uint32(data.Len())                                              //nolint:gosec // small
	trailer := -int32(len(entries) * 16)                                                //nolint:gosec // a few entries
	_ = binary.Write(&data, be, [4]uint32{uint32(region), rpmBin, uint32(trailer), 16}) //nolint:gosec // rpm's negative offset
	var b bytes.Buffer
	b.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0})
	_ = binary.Write(&b, be, [2]uint32{uint32(len(entries)), uint32(data.Len())}) //nolint:gosec // small
	for _, e := range entries {
		_ = binary.Write(&b, be, [4]uint32{e.tag, e.typ, e.offset, e.count})
	}
	b.Write(data.Bytes())
	return b.Bytes()
}

// rpmLead returns the 96 bytes of the lead: the magic, the version, the type (0 binary, 1
// source), the arch number, the name, the os number and the signature type 5 (a header).
func rpmLead(o RPMOptions, info RPMInfo) []byte {
	lead := make([]byte, 96)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb, o.Major, 0})
	if o.Source {
		lead[7] = 1
	}
	lead[9] = 1 // i386 and x86_64 alike in the lead, which rpm does not read
	copy(lead[10:75], o.Name+"-"+o.Version+"-"+info.Release)
	lead[77] = 1 // linux
	lead[79] = 5 // a header-style signature
	return lead
}

// cpioNewc returns the regular files as a cpio archive of the newc format ("070701"), as rpm's
// payload holds them: the names with a leading ".", each header and body padded to 4 bytes,
// the archive closed with TRAILER!!!.
func cpioNewc(files []rpmFile) []byte {
	var b bytes.Buffer
	write := func(ino int, name string, mode uint32, body []byte) {
		fmt.Fprintf(&b, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
			ino, mode, 0, 0, 1, fixtureTime.Unix(), len(body), 0, 0, 0, 0, len(name)+1, 0)
		b.WriteString(name + "\x00")
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
		b.Write(body)
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
	}
	for i, f := range files {
		if f.kind == "" {
			write(i+1, "."+f.path, uint32(f.mode), f.body)
		}
	}
	write(0, "TRAILER!!!", 0, nil)
	return b.Bytes()
}
