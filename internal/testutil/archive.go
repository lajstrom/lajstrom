package testutil

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/md5"  //nolint:gosec // apt and maven name md5 checksums; not a security use
	"crypto/sha1" //nolint:gosec // apt and maven name sha1 checksums; not a security use
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

// fixtureTime is the modification time of every archive member the builders write, so that the
// same options give the same bytes.
var fixtureTime = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// Checksums are the lowercase hex digests of a whole file: sha256 for the metadata layers,
// md5, sha1 and sha256 for the Packages index of apt, and all four for the checksum files of
// maven.
type Checksums struct {
	MD5    string
	SHA1   string
	SHA256 string
	SHA512 string
}

// checksumsOf returns the Checksums of b.
func checksumsOf(b []byte) Checksums {
	m := md5.Sum(b)   //nolint:gosec // a checksum file, not a security use
	s1 := sha1.Sum(b) //nolint:gosec // a checksum file, not a security use
	s256 := sha256.Sum256(b)
	s512 := sha512.Sum512(b)
	return Checksums{
		MD5:    hex.EncodeToString(m[:]),
		SHA1:   hex.EncodeToString(s1[:]),
		SHA256: hex.EncodeToString(s256[:]),
		SHA512: hex.EncodeToString(s512[:]),
	}
}

// member is one member of a tar or zip archive; a name that ends with "/" is a directory.
type member struct {
	name string
	body []byte
}

// isDir reports whether the member is a directory.
func (m member) isDir() bool { return strings.HasSuffix(m.name, "/") }

// tarGz returns the members in order as a gzip-compressed ustar archive, gzipOf(tarOf(members)).
func tarGz(members []member) []byte { return gzipOf(tarOf(members)) }

// gzipOf returns b compressed with gzip; the gzip header carries no name and no time.
func gzipOf(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(b)
	must(err)
	must(zw.Close())
	return buf.Bytes()
}

// tarOf returns the members in order as a ustar archive: owned by root, mode 0755 for a
// directory and 0644 for a file, modified at fixtureTime.
func tarOf(members []member) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range members {
		h := &tar.Header{
			Name:    m.name,
			Mode:    0o644,
			Size:    int64(len(m.body)),
			ModTime: fixtureTime,
			Uname:   "root",
			Gname:   "root",
			Format:  tar.FormatUSTAR,
		}
		if m.isDir() {
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		}
		must(tw.WriteHeader(h))
		_, err := tw.Write(m.body)
		must(err)
	}
	must(tw.Close())
	return buf.Bytes()
}

// zipOf returns the members in order as a zip archive: a file deflated with mode 0644, a
// directory stored with mode 0755, both modified at fixtureTime.
func zipOf(members []member) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate, Modified: fixtureTime}
		h.SetMode(0o644)
		if m.isDir() {
			h.Method = zip.Store
			h.SetMode(fs.ModeDir | 0o755)
		}
		w, err := zw.CreateHeader(h)
		must(err)
		_, err = w.Write(m.body)
		must(err)
	}
	must(zw.Close())
	return buf.Bytes()
}

// must panics on err. The builders write to memory, so an error means options that cannot be
// written, such as a path too long for a ustar header.
func must(err error) {
	if err != nil {
		panic(fmt.Sprintf("testutil: %v", err))
	}
}
