package rpm

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// The repodata documents as a test reads them back with encoding/xml.
type (
	repomdXML struct {
		Revision int64 `xml:"revision"`
		Data     []struct {
			Type         string `xml:"type,attr"`
			Checksum     string `xml:"checksum"`
			OpenChecksum string `xml:"open-checksum"`
			Location     struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
			Timestamp int64 `xml:"timestamp"`
			Size      int   `xml:"size"`
			OpenSize  int   `xml:"open-size"`
		} `xml:"data"`
	}
	versionXML struct {
		Epoch string `xml:"epoch,attr"`
		Ver   string `xml:"ver,attr"`
		Rel   string `xml:"rel,attr"`
	}
	entryXML struct {
		Name  string `xml:"name,attr"`
		Flags string `xml:"flags,attr"`
		Epoch string `xml:"epoch,attr"`
		Ver   string `xml:"ver,attr"`
		Rel   string `xml:"rel,attr"`
		Pre   string `xml:"pre,attr"`
	}
	fileXML struct {
		Type string `xml:"type,attr"`
		Path string `xml:",chardata"`
	}
	primaryXML struct {
		Count    int                 `xml:"packages,attr"`
		Packages []primaryPackageXML `xml:"package"`
	}
	primaryPackageXML struct {
		Name     string     `xml:"name"`
		Arch     string     `xml:"arch"`
		Version  versionXML `xml:"version"`
		Checksum struct {
			Type  string `xml:"type,attr"`
			PkgID string `xml:"pkgid,attr"`
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		Summary     string `xml:"summary"`
		Description string `xml:"description"`
		Time        struct {
			File  int64 `xml:"file,attr"`
			Build int64 `xml:"build,attr"`
		} `xml:"time"`
		Size struct {
			Package   int64 `xml:"package,attr"`
			Installed int64 `xml:"installed,attr"`
			Archive   int64 `xml:"archive,attr"`
		} `xml:"size"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
		Format struct {
			License     string `xml:"license"`
			SourceRPM   string `xml:"sourcerpm"`
			HeaderRange struct {
				Start int64 `xml:"start,attr"`
				End   int64 `xml:"end,attr"`
			} `xml:"header-range"`
			Provides   []entryXML `xml:"provides>entry"`
			Requires   []entryXML `xml:"requires>entry"`
			Recommends []entryXML `xml:"recommends>entry"`
			Files      []fileXML  `xml:"file"`
		} `xml:"format"`
	}
	filelistsXML struct {
		Count    int `xml:"packages,attr"`
		Packages []struct {
			PkgID   string     `xml:"pkgid,attr"`
			Name    string     `xml:"name,attr"`
			Arch    string     `xml:"arch,attr"`
			Version versionXML `xml:"version"`
			Files   []fileXML  `xml:"file"`
		} `xml:"package"`
	}
	otherXML struct {
		Count    int `xml:"packages,attr"`
		Packages []struct {
			Name       string `xml:"name,attr"`
			Changelogs []struct {
				Author string `xml:"author,attr"`
				Date   int64  `xml:"date,attr"`
				Text   string `xml:",chardata"`
			} `xml:"changelog"`
		} `xml:"package"`
	}
)

// repodataOf fetches repomd.xml and each document it lists, as dnf does, checks each against
// the checksums and sizes repomd.xml gives, and returns repomd.xml and the documents,
// uncompressed, by type.
func (e *env) repodataOf() (repomdXML, map[string][]byte) {
	e.t.Helper()
	rr := e.get("repodata/repomd.xml")
	if ct := rr.Header().Get("Content-Type"); ct != xmlContentType {
		e.t.Errorf("repomd.xml Content-Type %q", ct)
	}
	var md repomdXML
	must(e.t, xml.Unmarshal(rr.Body.Bytes(), &md))
	docs := map[string][]byte{}
	for _, d := range md.Data {
		gz := e.get(d.Location.Href)
		if ct := gz.Header().Get("Content-Type"); ct != gzipContentType {
			e.t.Errorf("%s Content-Type %q", d.Location.Href, ct)
		}
		plain := gunzip(e.t, gz.Body.Bytes())
		switch {
		case sha256Hex(gz.Body.Bytes()) != d.Checksum || gz.Body.Len() != d.Size:
			e.t.Errorf("%s: the checksum or size is not what repomd.xml says", d.Type)
		case sha256Hex(plain) != d.OpenChecksum || len(plain) != d.OpenSize:
			e.t.Errorf("%s: the open checksum or size is not what repomd.xml says", d.Type)
		case d.Location.Href != "repodata/"+d.Checksum+"-"+d.Type+".xml.gz" || d.Timestamp != md.Revision:
			e.t.Errorf("%s: location %q, timestamp %d of revision %d", d.Type, d.Location.Href, d.Timestamp, md.Revision)
		}
		docs[d.Type] = plain
	}
	if got := len(docs); got != 3 || docs["primary"] == nil || docs["filelists"] == nil || docs["other"] == nil {
		e.t.Fatalf("repomd.xml lists %d documents: %v", got, md.Data)
	}
	return md, docs
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	must(t, err)
	out, err := io.ReadAll(zr)
	must(t, err)
	return out
}

// fixtureSet publishes the packages of the rendering tests and returns their bytes and facts by
// file name: hello 1:2.0-1 for x86_64 with relations of every kind, a %ghost, a directory and a
// file in /etc, its source package, and hello-doc noarch.
func fixtureSet(e *env) map[string]testutil.RPMInfo {
	e.t.Helper()
	sh := testutil.RPMDep{Name: "/bin/sh", Flags: testutil.RPMSenseScriptPost}
	out := map[string]testutil.RPMInfo{}
	for _, o := range []testutil.RPMOptions{
		{Epoch: "1", Version: "2.0", Release: "1", Ghosts: []string{"/var/log/hello.log"}, Dirs: []string{"/etc/hello"},
			Files: map[string]string{"/etc/hello/hello.conf": "greeting=hi\n", "/usr/bin/hello": "#!/bin/sh\n", "/usr/share/hello/data": "x"},
			Requires: []testutil.RPMDep{sh, sh, {Name: "glibc", Flags: testutil.RPMSenseGreater | testutil.RPMSenseEqual, Version: "2.34"},
				{Name: "rpmlib(PayloadFilesHavePrefix)", Flags: testutil.RPMSenseLess | testutil.RPMSenseEqual | testutil.RPMSenseRpmlib, Version: "4.0-1"}},
			Recommends: []testutil.RPMDep{{Name: "hello-doc"}}},
		{Epoch: "1", Version: "2.0", Release: "1", Source: true},
		{Name: "hello-doc", Arch: "noarch", Summary: "the <manual> & more"},
	} {
		b, info := testutil.RPM(o)
		e.upload(b)
		out[info.Filename] = info
	}
	return out
}

// TestRenderIndex renders the repodata of fixtureSet and reads it back as dnf does: repomd.xml
// lists the three documents with checksums and sizes that are those of the files served; primary
// has the packages sorted, each with its EVR, checksum, sizes, header range, time and location
// (served by the Packages route), the relations as primary writes them (rpmlib(…) and repeats
// left out, flags, pre) and its primary files; filelists every file with its type; other the
// changelog.
func TestRenderIndex(t *testing.T) {
	e := newEnv(t, envOptions{now: func() time.Time { return time.Unix(1767225600, 0) }})
	infos := fixtureSet(e)
	md, docs := e.repodataOf()
	if md.Revision != 1767225600 {
		t.Errorf("revision %d", md.Revision)
	}
	var primary primaryXML
	must(t, xml.Unmarshal(docs["primary"], &primary))
	if primary.Count != 3 || len(primary.Packages) != 3 {
		t.Fatalf("primary has %d packages (%d)", len(primary.Packages), primary.Count)
	}
	checkPrimary(t, e, primary, infos)
	checkHello(t, primary)
	checkFilelists(t, docs["filelists"])
	var other otherXML
	must(t, xml.Unmarshal(docs["other"], &other))
	if other.Count != 3 || len(other.Packages[0].Changelogs) != 1 || other.Packages[0].Changelogs[0].Text != "- First build" {
		t.Errorf("other %+v", other)
	}
}

// checkPrimary checks every package of primary against the facts of the package it locates, and
// that the Packages route serves it there; the packages are sorted by name and arch.
func checkPrimary(t *testing.T, e *env, primary primaryXML, infos map[string]testutil.RPMInfo) {
	t.Helper()
	var names []string
	for _, p := range primary.Packages {
		names = append(names, p.Name+"."+p.Arch)
		info, ok := infos[strings.TrimPrefix(p.Location.Href, "Packages/")]
		if !ok {
			t.Fatalf("primary locates %q", p.Location.Href)
		}
		if diff := primaryDiff(p, info); diff != "" {
			t.Errorf("%s.%s: %s", p.Name, p.Arch, diff)
		}
		if got := e.get(p.Location.Href).Body.Len(); int64(got) != info.Size {
			t.Errorf("%s serves %d bytes", p.Location.Href, got)
		}
	}
	if want := []string{"hello.src", "hello.x86_64", "hello-doc.noarch"}; !slices.Equal(names, want) {
		t.Errorf("primary lists %v, want %v", names, want)
	}
}

// primaryDiff returns what of the package p of primary is not what info says, "" when nothing.
func primaryDiff(p primaryPackageXML, info testutil.RPMInfo) string {
	switch {
	case p.Checksum.Value != info.SHA256 || p.Checksum.Type != "sha256" || p.Checksum.PkgID != "YES":
		return fmt.Sprintf("checksum %+v", p.Checksum)
	case p.Size.Package != info.Size || p.Size.Installed != info.InstalledSize || p.Size.Archive != info.ArchiveSize:
		return fmt.Sprintf("size %+v", p.Size)
	case p.Format.HeaderRange.Start != info.HeaderStart || p.Format.HeaderRange.End != info.HeaderEnd:
		return fmt.Sprintf("header range %+v", p.Format.HeaderRange)
	case p.Time.Build != info.BuildTime || p.Time.File == 0 || p.Summary != info.Summary || p.Format.SourceRPM != info.SourceRPM:
		return fmt.Sprintf("time %+v, summary %q, sourcerpm %q", p.Time, p.Summary, p.Format.SourceRPM)
	}
	return ""
}

// checkHello checks the entry of hello.x86_64 in primary.
func checkHello(t *testing.T, primary primaryXML) {
	t.Helper()
	p := primary.Packages[1]
	if p.Version != (versionXML{Epoch: "1", Ver: "2.0", Rel: "1"}) {
		t.Errorf("hello version %+v", p.Version)
	}
	wantReq := []entryXML{{Name: "/bin/sh", Pre: "1"}, {Name: "glibc", Flags: "GE", Epoch: "0", Ver: "2.34"}}
	if !slices.Equal(p.Format.Requires, wantReq) {
		t.Errorf("hello requires %+v, want %+v", p.Format.Requires, wantReq)
	}
	if want := (entryXML{Name: "hello", Flags: "EQ", Epoch: "1", Ver: "2.0", Rel: "1"}); len(p.Format.Provides) != 2 || p.Format.Provides[0] != want {
		t.Errorf("hello provides %+v", p.Format.Provides)
	}
	if want := []entryXML{{Name: "hello-doc"}}; !slices.Equal(p.Format.Recommends, want) {
		t.Errorf("hello recommends %+v", p.Format.Recommends)
	}
	wantFiles := []fileXML{{Type: "dir", Path: "/etc/hello"}, {Path: "/etc/hello/hello.conf"}, {Path: "/usr/bin/hello"}}
	if !slices.Equal(p.Format.Files, wantFiles) {
		t.Errorf("hello primary files %+v, want %+v", p.Format.Files, wantFiles)
	}
	if doc := primary.Packages[2]; doc.Summary != "the <manual> & more" {
		t.Errorf("hello-doc summary %q", doc.Summary)
	}
}

// checkFilelists checks that filelists lists every file of hello.x86_64 with its type.
func checkFilelists(t *testing.T, b []byte) {
	t.Helper()
	var fl filelistsXML
	must(t, xml.Unmarshal(b, &fl))
	if fl.Count != 3 || len(fl.Packages) != 3 {
		t.Fatalf("filelists has %d packages", len(fl.Packages))
	}
	p := fl.Packages[1]
	want := []fileXML{{Type: "dir", Path: "/etc/hello"}, {Path: "/etc/hello/hello.conf"}, {Path: "/usr/bin/hello"},
		{Path: "/usr/share/hello/data"}, {Type: "ghost", Path: "/var/log/hello.log"}}
	if p.Name != "hello" || p.Arch != "x86_64" || !slices.Equal(p.Files, want) {
		t.Errorf("filelists of %s.%s: %+v, want %+v", p.Name, p.Arch, p.Files, want)
	}
}

// TestRenderEmpty renders the repodata of a repo without versions: three empty documents, not a
// 404, which would make dnf refuse the repo.
func TestRenderEmpty(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, docs := e.repodataOf()
	var primary primaryXML
	must(t, xml.Unmarshal(docs["primary"], &primary))
	if primary.Count != 0 || len(primary.Packages) != 0 {
		t.Errorf("primary of an empty repo has %d packages", len(primary.Packages))
	}
}

// TestRepodataChanges checks that a publish and a delete render the repodata again: the files of
// the earlier render are 404 once repomd.xml no longer lists them, a name it never listed is 404,
// and the deleted version is gone from primary.
func TestRepodataChanges(t *testing.T) {
	e := newEnv(t, envOptions{})
	hello, _ := testutil.RPM(testutil.RPMOptions{})
	e.upload(hello)
	first, _ := e.repodataOf()
	other, _ := testutil.RPM(testutil.RPMOptions{Name: "other"})
	pub := e.upload(other)
	second, docs := e.repodataOf()
	if !strings.Contains(string(docs["primary"]), "<name>other</name>") {
		t.Error("primary lacks the second upload")
	}
	for _, d := range first.Data {
		if d.Type == "primary" {
			if rr := e.send(http.MethodGet, d.Location.Href, nil); rr.Code != http.StatusNotFound {
				t.Errorf("the primary of the earlier render = %d", rr.Code)
			}
		}
	}
	if rr := e.send(http.MethodGet, "repodata/"+strings.Repeat("0", 64)+"-primary.xml.gz", nil); rr.Code != http.StatusNotFound {
		t.Errorf("a name repomd.xml never listed = %d", rr.Code)
	}
	if _, err := e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "other", Reference: pub.Tag}, alice); err != nil {
		t.Fatal(err)
	}
	third, docs := e.repodataOf()
	if strings.Contains(string(docs["primary"]), "<name>other</name>") || !strings.Contains(string(docs["primary"]), "packages=\"1\"") {
		t.Errorf("primary after the delete:\n%s", docs["primary"])
	}
	if checksumOf(second, "primary") == checksumOf(third, "primary") {
		t.Error("the delete did not change primary")
	}
	if rr := e.send(http.MethodGet, "repodata/repomd.xml.asc", nil); rr.Code != http.StatusNotFound {
		t.Errorf("repomd.xml.asc without a Signer = %d", rr.Code)
	}
}

// checksumOf returns the checksum repomd.xml md gives the document typ.
func checksumOf(md repomdXML, typ string) string {
	for _, d := range md.Data {
		if d.Type == typ {
			return d.Checksum
		}
	}
	return ""
}

// TestComparison maps the sense flags of a relation to the flags attribute of primary.
func TestComparison(t *testing.T) {
	for flags, want := range map[int64]string{0: "", 2: "LT", 10: "LE", 8: "EQ", 4: "GT", 12: "GE", 1 << 24: "", 1<<24 | 10: "LE"} {
		if got := comparison(flags); got != want {
			t.Errorf("comparison(%s) = %q, want %q", strconv.FormatInt(flags, 2), got, want)
		}
	}
}
