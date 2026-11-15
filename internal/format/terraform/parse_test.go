package terraform

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

func TestParseModule(t *testing.T) {
	ok := []struct{ name, system, path string }{
		{"vpc", "aws", "vpc/aws"},
		{"My_Network", "aws", "my_5fnetwork/aws"},
		{"a", "x1", "a/x1"},
		{"consul-cluster", "azurerm", "consul-cluster/azurerm"},
		{strings.Repeat("n", 64), strings.Repeat("s", 64), strings.Repeat("n", 64) + "/" + strings.Repeat("s", 64)},
	}
	for _, c := range ok {
		m, err := ParseModule(c.name, c.system)
		if err != nil {
			t.Errorf("ParseModule(%q, %q): %v", c.name, c.system, err)
			continue
		}
		if got, err := (Plugin{}).EscapeName(m.PackageName()); err != nil || got != c.path {
			t.Errorf("EscapeName(%q) = %q, %v, want %q", m.PackageName(), got, err, c.path)
		}
	}
	bad := []struct{ name, system string }{
		{"", "aws"}, {"-vpc", "aws"}, {"vpc_", "aws"}, {"v.pc", "aws"}, {"vpc", "AWS"}, {"vpc", "a-ws"},
		{"vpc", ""}, {strings.Repeat("n", 65), "aws"}, {"vpc", strings.Repeat("s", 65)}, {"vp c", "aws"},
	}
	for _, c := range bad {
		if _, err := ParseModule(c.name, c.system); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("ParseModule(%q, %q) = %v, want ErrInvalid", c.name, c.system, err)
		}
	}
	for _, name := range []string{"vpc", "vpc/aws/x", "/aws", "vpc/"} {
		if _, err := (Plugin{}).EscapeName(name); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("EscapeName(%q) = %v, want ErrInvalid", name, err)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, v := range []string{"0.0.0", "1.2.0", "10.20.30", "2.0.0-rc.1", "1.0.0-alpha.beta-1", "1.0.0-0A.is.legal", "1.0.0-RC1"} {
		if err := CheckVersion(v); err != nil {
			t.Errorf("CheckVersion(%q): %v", v, err)
		}
	}
	for _, v := range []string{"", "v1.2.0", "1.2", "1.2.0.0", "01.2.0", "1.02.0", "1.2.0+build.7", "1.2.0-01", "1.2.0-", "1.2.0-a..b", "1.2.0 "} {
		if err := CheckVersion(v); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("CheckVersion(%q) = %v, want ErrInvalid", v, err)
		}
	}
}

// tarEntry is one entry of a hand-made archive.
type tarEntry struct {
	name, link string
	typ        byte
	body       string
}

// handTarGz returns the entries as a gzip-compressed tar, with types testutil does not write.
func handTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
		switch e.typ {
		case tar.TypeXGlobalHeader:
			h = &tar.Header{Typeflag: e.typ, PAXRecords: map[string]string{"comment": "lajstrom"}}
		case tar.TypeReg:
		default:
			h.Size = 0
		}
		must(t, tw.WriteHeader(h))
		if h.Size > 0 {
			_, err := tw.Write([]byte(e.body))
			must(t, err)
		}
	}
	must(t, tw.Close())
	must(t, gz.Close())
	return buf.Bytes()
}

func TestReadArchive(t *testing.T) {
	module, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	nested, _ := testutil.TerraformModule(testutil.TerraformOptions{
		Dirs:  []string{"modules/", "modules/sub/"},
		Files: map[string]string{"./main.tf": "", "modules/sub/main.tf": "", "README.md": "x"},
		Links: map[string]string{"docs.md": "README.md", "modules/sub/up.md": "modules/x.md"},
	})
	for name, b := range map[string][]byte{
		"module": module, "nested": nested,
		"hard link":  handTarGz(t, tarEntry{name: "a.tf", typ: tar.TypeReg, body: "x"}, tarEntry{name: "b.tf", link: "a.tf", typ: tar.TypeLink}),
		"pax global": handTarGz(t, tarEntry{name: "pax_global_header", typ: tar.TypeXGlobalHeader}, tarEntry{name: "a.tf", typ: tar.TypeReg, body: "x"}),
	} {
		if err := ReadArchive(bytes.NewReader(b)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	var trailing bytes.Buffer
	trailing.Write(module)
	trailing.WriteString("garbage")
	cut := module[:len(module)-6]
	refused := map[string][]byte{
		"empty":           nil,
		"not gzip":        []byte("PK\x03\x04 a zip"),
		"gzip of no tar":  gzipped(t, []byte(strings.Repeat("x", 1000))),
		"cut":             cut,
		"trailing bytes":  trailing.Bytes(),
		"no regular file": handTarGz(t, tarEntry{name: "dir/", typ: tar.TypeDir}),
		"dot dot":         handTarGz(t, tarEntry{name: "../main.tf", typ: tar.TypeReg, body: "x"}),
		"inner dot dot":   handTarGz(t, tarEntry{name: "a/../../main.tf", typ: tar.TypeReg, body: "x"}),
		"backslash":       handTarGz(t, tarEntry{name: `a\..\..\main.tf`, typ: tar.TypeReg, body: "x"}),
		"absolute":        handTarGz(t, tarEntry{name: "/etc/main.tf", typ: tar.TypeReg, body: "x"}),
		"link out":        handTarGz(t, tarEntry{name: "a.tf", typ: tar.TypeReg, body: "x"}, tarEntry{name: "b.tf", link: "../x", typ: tar.TypeSymlink}),
		"link absolute":   handTarGz(t, tarEntry{name: "a.tf", typ: tar.TypeReg, body: "x"}, tarEntry{name: "b.tf", link: "/etc/passwd", typ: tar.TypeSymlink}),
		"hard link out":   handTarGz(t, tarEntry{name: "a.tf", typ: tar.TypeReg, body: "x"}, tarEntry{name: "b.tf", link: "../a.tf", typ: tar.TypeLink}),
		"device":          handTarGz(t, tarEntry{name: "a.tf", typ: tar.TypeReg, body: "x"}, tarEntry{name: "null", typ: tar.TypeChar}),
		"fifo":            handTarGz(t, tarEntry{name: "a.tf", typ: tar.TypeReg, body: "x"}, tarEntry{name: "pipe", typ: tar.TypeFifo}),
	}
	for name, b := range refused {
		if err := ReadArchive(bytes.NewReader(b)); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func TestReadArchiveBounds(t *testing.T) {
	entries, unpacked := maxEntries, maxUnpacked
	t.Cleanup(func() { maxEntries, maxUnpacked = entries, unpacked })
	maxEntries, maxUnpacked = 3, 1<<20
	three := handTarGz(t, tarEntry{name: "a", typ: tar.TypeReg, body: "x"}, tarEntry{name: "b", typ: tar.TypeReg, body: "x"},
		tarEntry{name: "c", typ: tar.TypeReg, body: "x"})
	if err := ReadArchive(bytes.NewReader(three)); err != nil {
		t.Errorf("three entries: %v", err)
	}
	four := handTarGz(t, tarEntry{name: "a", typ: tar.TypeReg, body: "x"}, tarEntry{name: "b", typ: tar.TypeReg, body: "x"},
		tarEntry{name: "c", typ: tar.TypeReg, body: "x"}, tarEntry{name: "d", typ: tar.TypeReg, body: "x"})
	if err := ReadArchive(bytes.NewReader(four)); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "more than 3 entries") {
		t.Errorf("four entries: %v", err)
	}
	big := handTarGz(t, tarEntry{name: "a", typ: tar.TypeReg, body: strings.Repeat("0", 2<<20)})
	if err := ReadArchive(bytes.NewReader(big)); !errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), "unpacks to more than") {
		t.Errorf("2 MiB unpacked: %v", err)
	}
}

// TestReadArchiveReadError keeps the error of the reader: a registry that fails is no 400.
func TestReadArchiveReadError(t *testing.T) {
	module, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	boom := errors.New("the registry went away")
	err := ReadArchive(&failing{b: module[:len(module)/2], err: boom})
	if !errors.Is(err, boom) || errors.Is(err, format.ErrInvalid) {
		t.Errorf("ReadArchive = %v, want the reader's error", err)
	}
}

// failing reads b, then fails with err.
type failing struct {
	b   []byte
	err error
}

func (f *failing) Read(p []byte) (int, error) {
	if len(f.b) == 0 {
		return 0, f.err
	}
	n := copy(p, f.b)
	f.b = f.b[n:]
	return n, nil
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(b)
	must(t, err)
	must(t, gz.Close())
	return buf.Bytes()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
