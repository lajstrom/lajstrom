package rpm

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The documents of a repo (docs/spec/http-routing.md §3, rpm). The index_cache scope is
// repodata and the kind is the file name below repodata/: repomd.xml, <sha256>-primary.xml.gz,
// <sha256>-filelists.xml.gz, <sha256>-other.xml.gz.
const (
	scopeRepodata = "repodata"
	kindRepomd    = "repomd.xml"

	xmlContentType  = "application/xml"
	gzipContentType = "application/gzip"

	// The namespaces of the repodata documents.
	nsRepo      = "http://linux.duke.edu/metadata/repo"
	nsCommon    = "http://linux.duke.edu/metadata/common"
	nsRPM       = "http://linux.duke.edu/metadata/rpm"
	nsFilelists = "http://linux.duke.edu/metadata/filelists"
	nsOther     = "http://linux.duke.edu/metadata/other"

	// senseLess, senseGreater and senseEqual are the comparison of a relation's flags.
	senseLess    = 1 << 1
	senseGreater = 1 << 2
	senseEqual   = 1 << 3
)

// dataTypes are the documents repomd.xml lists, in its order.
var dataTypes = []string{"primary", "filelists", "other"}

// RenderIndex renders the repodata of the repo, the scope repodata, in one call: primary.xml,
// filelists.xml and other.xml, gzipped and named by the sha256 of their bytes, and the
// repomd.xml that lists each with its checksums, sizes and the time of the render, so that the
// repomd.xml format.CachedDocument stores with them, and a client gets, names the files it
// gets. Kind is the file name below repodata/ and Path the URL path below /rpm/<repo>/.
//
// The versions are every version of the repo, sorted by name, version and arch, each rendered
// from its metadata (versions.metadata); when the index has none, the metadata layer is read
// from the registry through the providers of the value Routes made (fetchMetadata). A repo
// without versions has the three documents empty: dnf refuses a repo whose repomd.xml it cannot
// read. Another scope is index.ErrNotFound. The three documents are a function of the versions
// alone; two renders differ in the timestamps of repomd.xml only.
func (p Plugin) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	if scope != scopeRepodata {
		return nil, fmt.Errorf("%w: rpm/%s has no index %q", index.ErrNotFound, repo.Name, scope)
	}
	pkgs, err := p.packages(ctx, repo, st)
	if err != nil {
		return nil, err
	}
	now := p.clock().Unix()
	docs := make([]format.Document, 0, len(dataTypes)+1)
	var repomd bytes.Buffer
	repomd.WriteString(xml.Header)
	fmt.Fprintf(&repomd, "<repomd xmlns=%q xmlns:rpm=%q>\n  <revision>%d</revision>\n", nsRepo, nsRPM, now)
	for _, typ := range dataTypes {
		plain := renderData(typ, pkgs)
		gz, err := gzipped(plain)
		if err != nil {
			return nil, fmt.Errorf("rpm: %s.xml.gz of rpm/%s: %w", typ, repo.Name, err)
		}
		sum, openSum := sha256Hex(gz), sha256Hex(plain)
		kind := sum + "-" + typ + ".xml.gz"
		fmt.Fprintf(&repomd, "  <data type=%q>\n    <checksum type=\"sha256\">%s</checksum>\n"+
			"    <open-checksum type=\"sha256\">%s</open-checksum>\n    <location href=\"repodata/%s\"/>\n"+
			"    <timestamp>%d</timestamp>\n    <size>%d</size>\n    <open-size>%d</open-size>\n  </data>\n",
			typ, sum, openSum, kind, now, len(gz), len(plain))
		docs = append(docs, format.Document{Kind: kind, Path: scopeRepodata + "/" + kind, ContentType: gzipContentType, Content: gz})
	}
	repomd.WriteString("</repomd>\n")
	return append([]format.Document{{Kind: kindRepomd, Path: scopeRepodata + "/" + kindRepomd, ContentType: xmlContentType,
		Content: repomd.Bytes()}}, docs...), nil
}

// rendered is one version as the documents render it: its metadata and its publish time.
type rendered struct {
	md      *metadata
	created int64
}

// packages returns the versions of the repo, sorted by name, version and arch.
func (p Plugin) packages(ctx context.Context, repo index.Repo, st index.Reader) ([]rendered, error) {
	pkgs, err := st.ListPackages(ctx, repo.ID, "")
	if err != nil {
		return nil, fmt.Errorf("rpm: packages of rpm/%s: %w", repo.Name, err)
	}
	var out []rendered
	for _, pkg := range pkgs {
		versions, err := st.ListVersions(ctx, pkg.ID)
		if err != nil {
			return nil, fmt.Errorf("rpm: versions of %s: %w", pkg.Name, err)
		}
		for _, v := range versions {
			what := pkg.Name + " " + v.Version + " (" + v.Variant + ")"
			raw := v.Metadata
			if raw == nil {
				if raw, err = p.fetchMetadata(ctx, repo, pkg, v); err != nil {
					return nil, fmt.Errorf("rpm: %s has no metadata in the index: %w", what, err)
				}
			}
			var md metadata
			if err := json.Unmarshal(raw, &md); err != nil {
				return nil, fmt.Errorf("rpm: the metadata of %s: %w", what, err)
			}
			if md.Name == "" || md.Filename == "" || md.SHA256 == "" {
				return nil, fmt.Errorf("rpm: the metadata of %s lacks name, filename or sha256", what)
			}
			out = append(out, rendered{md: &md, created: v.CreatedAt.Unix()})
		}
	}
	slices.SortFunc(out, func(a, b rendered) int {
		return cmp.Or(strings.Compare(a.md.Name, b.md.Name), cmp.Compare(a.md.Epoch, b.md.Epoch),
			strings.Compare(a.md.Version, b.md.Version), strings.Compare(a.md.Release, b.md.Release), strings.Compare(a.md.Arch, b.md.Arch))
	})
	return out, nil
}

// renderData returns the document typ, primary, filelists or other, of pkgs.
func renderData(typ string, pkgs []rendered) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	switch typ {
	case "primary":
		fmt.Fprintf(&b, "<metadata xmlns=%q xmlns:rpm=%q packages=\"%d\">\n", nsCommon, nsRPM, len(pkgs))
		for _, r := range pkgs {
			writePrimary(&b, r)
		}
		b.WriteString("</metadata>\n")
	case "filelists":
		fmt.Fprintf(&b, "<filelists xmlns=%q packages=\"%d\">\n", nsFilelists, len(pkgs))
		for _, r := range pkgs {
			writeShortHeader(&b, r.md)
			for _, f := range r.md.Files {
				writeFile(&b, f)
			}
			b.WriteString("</package>\n")
		}
		b.WriteString("</filelists>\n")
	case "other":
		fmt.Fprintf(&b, "<otherdata xmlns=%q packages=\"%d\">\n", nsOther, len(pkgs))
		for _, r := range pkgs {
			writeShortHeader(&b, r.md)
			for _, c := range r.md.Changelogs {
				fmt.Fprintf(&b, "  <changelog author=\"%s\" date=\"%d\">%s</changelog>\n", esc(c.Author), c.Date, esc(c.Text))
			}
			b.WriteString("</package>\n")
		}
		b.WriteString("</otherdata>\n")
	}
	return b.Bytes()
}

// writePrimary writes the <package> of one version in primary.xml.
func writePrimary(b *bytes.Buffer, r rendered) {
	md := r.md
	b.WriteString("<package type=\"rpm\">\n")
	fmt.Fprintf(b, "  <name>%s</name>\n  <arch>%s</arch>\n", esc(md.Name), esc(md.Arch))
	fmt.Fprintf(b, "  <version epoch=\"%d\" ver=\"%s\" rel=\"%s\"/>\n", md.Epoch, esc(md.Version), esc(md.Release))
	fmt.Fprintf(b, "  <checksum type=\"sha256\" pkgid=\"YES\">%s</checksum>\n", esc(md.SHA256))
	for _, e := range [][2]string{{"summary", md.Summary}, {"description", md.Description}, {"packager", md.Packager}, {"url", md.URL}} {
		fmt.Fprintf(b, "  <%s>%s</%s>\n", e[0], esc(e[1]), e[0])
	}
	fmt.Fprintf(b, "  <time file=\"%d\" build=\"%d\"/>\n", r.created, md.BuildTime)
	fmt.Fprintf(b, "  <size package=\"%d\" installed=\"%d\" archive=\"%d\"/>\n", md.Size, md.InstalledSize, md.ArchiveSize)
	fmt.Fprintf(b, "  <location href=\"Packages/%s\"/>\n", esc(md.Filename))
	b.WriteString("  <format>\n")
	for _, e := range [][2]string{{"license", md.License}, {"vendor", md.Vendor}, {"group", md.Group}, {"buildhost", md.BuildHost},
		{"sourcerpm", md.SourceRPM}} {
		fmt.Fprintf(b, "    <rpm:%s>%s</rpm:%s>\n", e[0], esc(e[1]), e[0])
	}
	fmt.Fprintf(b, "    <rpm:header-range start=\"%d\" end=\"%d\"/>\n", md.HeaderStart, md.HeaderEnd)
	kinds := RelationKinds()
	for i, rel := range md.relations() {
		entries := *rel
		if kinds[i] == "requires" {
			entries = requirements(entries)
		}
		if len(entries) == 0 {
			continue
		}
		fmt.Fprintf(b, "    <rpm:%s>\n", kinds[i])
		for _, e := range entries {
			writeEntry(b, e, kinds[i] == "requires")
		}
		fmt.Fprintf(b, "    </rpm:%s>\n", kinds[i])
	}
	for _, f := range md.Files {
		if primaryFile(f.Path) {
			b.WriteString("  ")
			writeFile(b, f)
		}
	}
	b.WriteString("  </format>\n</package>\n")
}

// writeShortHeader writes the opening <package> of one version in filelists.xml and other.xml,
// and its <version>.
func writeShortHeader(b *bytes.Buffer, md *metadata) {
	fmt.Fprintf(b, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n", esc(md.SHA256), esc(md.Name), esc(md.Arch))
	fmt.Fprintf(b, "  <version epoch=\"%d\" ver=\"%s\" rel=\"%s\"/>\n", md.Epoch, esc(md.Version), esc(md.Release))
}

// writeFile writes one <file>, with its type when it is a directory or a ghost.
func writeFile(b *bytes.Buffer, f file) {
	if f.Type != "" {
		fmt.Fprintf(b, "  <file type=\"%s\">%s</file>\n", esc(f.Type), esc(f.Path))
		return
	}
	fmt.Fprintf(b, "  <file>%s</file>\n", esc(f.Path))
}

// writeEntry writes one <rpm:entry> of a relation: the name, the comparison of its flags and
// the parts of its version when it has one (epoch 0 when the version has none), and pre="1" on
// a requirement of the install scripts.
func writeEntry(b *bytes.Buffer, e relation, requires bool) {
	fmt.Fprintf(b, "      <rpm:entry name=\"%s\"", esc(e.Name))
	if op := comparison(e.Flags); op != "" {
		fmt.Fprintf(b, " flags=\"%s\"", op)
	}
	if e.Version != "" {
		epoch := e.Epoch
		if epoch == "" {
			epoch = "0"
		}
		fmt.Fprintf(b, " epoch=\"%s\" ver=\"%s\"", esc(epoch), esc(e.Version))
		if e.Release != "" {
			fmt.Fprintf(b, " rel=\"%s\"", esc(e.Release))
		}
	}
	if requires && e.Pre {
		b.WriteString(" pre=\"1\"")
	}
	b.WriteString("/>\n")
}

// comparison returns the flags attribute of a relation's sense flags: EQ, LT, LE, GT, GE, or ""
// when it compares nothing.
func comparison(flags int64) string {
	switch flags & (senseLess | senseGreater | senseEqual) {
	case senseEqual:
		return "EQ"
	case senseLess:
		return "LT"
	case senseLess | senseEqual:
		return "LE"
	case senseGreater:
		return "GT"
	case senseGreater | senseEqual:
		return "GE"
	}
	return ""
}

// requirements returns the requirements primary.xml lists: without those of rpmlib(…), which rpm
// itself satisfies, and without an entry that repeats an earlier one.
func requirements(all []relation) []relation {
	out := make([]relation, 0, len(all))
	seen := map[relation]bool{}
	for _, r := range all {
		if strings.HasPrefix(r.Name, "rpmlib(") || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// primaryFile reports whether primary.xml lists the file path: below /etc/, in a directory
// whose name ends in bin, or /usr/lib/sendmail, the files dnf resolves without filelists.
func primaryFile(path string) bool {
	return strings.HasPrefix(path, "/etc/") || strings.Contains(path, "bin/") || path == "/usr/lib/sendmail"
}

// esc returns s escaped for XML text and attribute values; a character XML does not allow is
// replaced (xml.EscapeText).
func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s)) // into a strings.Builder: never an error
	return b.String()
}

// gzipped returns b compressed with gzip, without a name or a time in the header, so that the
// same document gives the same bytes and the same name.
func gzipped(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sha256Hex returns the lowercase hex sha256 of b.
func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// locationRe finds the locations of the documents a repomd.xml that RenderIndex wrote lists.
var locationRe = regexp.MustCompile(`<location href="repodata/([^"]+)"/>`)

// listed returns the file names below repodata/ that the repomd.xml lists.
func listed(repomd []byte) map[string]bool {
	out := map[string]bool{}
	for _, m := range locationRe.FindAllSubmatch(repomd, -1) {
		out[string(m[1])] = true
	}
	return out
}
