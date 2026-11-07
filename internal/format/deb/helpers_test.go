package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	"github.com/ulikunitz/xz"

	"github.com/lajstrom/lajstrom/internal/format"
)

// arMember is a member of an ar archive built by a test.
type arMember struct {
	name string
	data []byte
}

// arOf returns the members as an ar archive: the global header, a 60-byte header per member as
// dpkg-deb writes it (the name has no trailing "/" unless the test gives one) and the padding
// byte after an odd member. The plugin's tests need archives the fixture builder does not make:
// members out of order, unknown names, a control file written by hand.
func arOf(members ...arMember) []byte {
	var b bytes.Buffer
	b.WriteString(arMagic)
	for _, m := range members {
		fmt.Fprintf(&b, "%-16s%-12d%-6d%-6d%-8o%-10d`\n", m.name, 1767225600, 0, 0, 0o100644, len(m.data))
		b.Write(m.data)
		if len(m.data)%2 == 1 {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// tarEntry is a member of a tar archive built by a test; typ 0 is a regular file.
type tarEntry struct {
	name string
	body string
	typ  byte
}

// tarOf returns the entries as a ustar archive.
func tarOf(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg,
			ModTime: time.Unix(1767225600, 0), Format: tar.FormatUSTAR}
		switch e.typ {
		case 0:
		case tar.TypeSymlink:
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.body, 0
		default:
			h.Typeflag, h.Size = e.typ, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// compress returns b compressed as the control member extension ext says: "gz", "xz", "zst",
// or "" for none.
func compress(t testing.TB, ext string, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	var w io.WriteCloser
	var err error
	switch ext {
	case "":
		return b
	case "gz":
		w = gzip.NewWriter(&out)
	case "xz":
		w, err = xz.NewWriter(&out)
	case "zst":
		w, err = zstd.NewWriter(&out, zstd.WithEncoderConcurrency(1))
	default:
		t.Fatalf("compress: unknown extension %q", ext)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// controlName is the name of the control member with extension ext.
func controlName(ext string) string {
	if ext == "" {
		return "control.tar"
	}
	return "control.tar." + ext
}

// debOf returns a .deb whose control member holds the control file control and, unless
// md5sums is nil, md5sums, compressed with ext; debian-binary says 2.0 and data.tar.gz holds
// one file.
func debOf(t testing.TB, control string, md5sums *string, ext string) []byte {
	t.Helper()
	entries := []tarEntry{{name: "./", typ: tar.TypeDir}, {name: "./control", body: control}}
	if md5sums != nil {
		entries = append(entries, tarEntry{name: "./md5sums", body: *md5sums})
	}
	return arOf(
		arMember{"debian-binary", []byte("2.0\n")},
		arMember{controlName(ext), compress(t, ext, tarOf(t, entries...))},
		arMember{"data.tar.gz", compress(t, "gz", tarOf(t, tarEntry{name: "./usr/share/x", body: "x\n"}))},
	)
}

// ptr returns a pointer to s.
func ptr(s string) *string { return &s }

// controlEnd returns the offset just past the control member of the .deb b: the global
// header, debian-binary with its header (and padding), the control member's header and data.
func controlEnd(t testing.TB, b []byte) int {
	t.Helper()
	off := len(arMagic)
	for range 2 {
		h, err := parseArHeader(b[off : off+arHeaderLen])
		if err != nil {
			t.Fatal(err)
		}
		off += arHeaderLen + int(h.size)
		if h.name == "debian-binary" && h.size%2 == 1 {
			off++
		}
	}
	return off
}

// errBoom is the failure of a reader or a fetcher in the tests.
var errBoom = errors.New("boom")

// failAfter is a reader of b that fails with errBoom once b is read: a blob whose bytes after
// b cannot be fetched.
func failAfter(b []byte) io.Reader {
	return io.MultiReader(bytes.NewReader(b), iotestErr{})
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errBoom }

// fetcherOf is a BlobFetcher over blobs; override replaces the reader of one digest.
func fetcherOf(blobs map[digest.Digest][]byte, override map[digest.Digest]io.Reader) format.BlobFetcher {
	return func(_ context.Context, d digest.Digest) (io.ReadCloser, error) {
		if r, ok := override[d]; ok {
			if r == nil {
				return nil, errBoom
			}
			return io.NopCloser(r), nil
		}
		b, ok := blobs[d]
		if !ok {
			return nil, fmt.Errorf("no blob %s", d)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}
