package deb

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz/lzma"

	"github.com/lajstrom/lajstrom/internal/format"
)

// The file list of a binary package (#79): the paths of the members of its data.tar that are not
// directories, which the Contents indexes list. The inspector reads the data member as it
// streams, through a lister of its own (format.ReadInspector), and a data member it cannot read
// within the bounds below leaves the package without a list: the upload goes on, since the data
// member was never read before #79, and the version is listed in Contents by its md5sums.

const (
	// dataBase and dataRatio bound what the lister decompresses of a data member of n bytes:
	// dataBase + dataRatio*n bytes, a bomb's work (#243), above the ratio of the packages of a
	// distribution (xz of text is about ten).
	dataBase  = 64 << 20
	dataRatio = 100
	// dataXZBlockBytes is the compressed bytes per xz block a data.tar.xz may have beyond
	// maxXZBlocks: xz's multithreaded encoder makes a block of three dictionaries, dpkg-deb's
	// 8 MiB a block of 24 MiB, compressed to more than this.
	dataXZBlockBytes = 64 << 10
	// maxFileList is the most bytes of paths a file list keeps, a metadata layer of 16 MiB with
	// room for the control fields and md5sums; a longer list is dropped whole.
	maxFileList = 12 << 20
)

// errNoList is the error of a lister that gave up: a list it could not finish is no list.
var errNoList = errors.New("deb: no file list")

// lister reads a data member, name of size bytes, as it streams and keeps the paths of its
// members that are not directories (filesOf). It never fails the upload: its reader returns nil
// whatever happened, and err says why there is no list.
type lister struct {
	format.Inspector
	files []string
	err   error
}

// newLister returns the lister of the data member name of size bytes.
func newLister(name string, size int64) *lister {
	l := &lister{}
	l.Inspector = format.ReadInspector(func(r io.Reader) error {
		l.files, l.err = listData(name, size, r)
		if l.err != nil {
			l.files = nil
			_, _ = io.Copy(io.Discard, r) // the rest of the member, which Write passes on
		}
		return nil
	})
	return l
}

// list returns the file list, nil when the lister gave up.
func (l *lister) list() []string {
	if l == nil || l.err != nil {
		return nil
	}
	if l.files == nil {
		return []string{}
	}
	return l.files
}

// listData decompresses the data member name, size bytes, from r, at most dataBase +
// dataRatio*size bytes, and returns filesOf its tar archive.
func listData(name string, size int64, r io.Reader) ([]string, error) {
	dr, closeFn, err := dataDecompressor(name, size, r)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	limit := int64(dataBase) + dataRatio*size
	return filesOf(tar.NewReader(&capReader{r: dr, n: limit, limit: limit, name: name}))
}

// dataDecompressor returns the reader of the tar archive in the data member name of size bytes:
// data.tar as it is, .gz, .xz (at most maxXZBlocks blocks and one per dataXZBlockBytes), .zst,
// .bz2 or .lzma, each declaring at most maxDictionary.
func dataDecompressor(name string, size int64, body io.Reader) (io.Reader, func(), error) {
	nop := func() {}
	switch name {
	case "data.tar":
		return body, nop, nil
	case "data.tar.gz":
		r, err := gzip.NewReader(body)
		return r, nop, err
	case "data.tar.xz":
		r, err := newXZReaderBlocks(body, maxXZBlocks+int(size/dataXZBlockBytes))
		return r, nop, err
	case "data.tar.zst":
		d, err := zstd.NewReader(body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(maxDictionary))
		if err != nil {
			return nil, nil, err
		}
		return d, d.Close, nil
	case "data.tar.bz2":
		return bzip2.NewReader(body), nop, nil
	case "data.tar.lzma":
		r, err := newLZMAReader(body)
		return r, nop, err
	}
	return nil, nil, fmt.Errorf("deb: %s is not a data member", name)
}

// newLZMAReader reads the header of an .lzma stream (the props byte, the dictionary size and the
// uncompressed size, 13 bytes) and refuses a dictionary over maxDictionary before the decoder
// allocates it.
func newLZMAReader(body io.Reader) (io.Reader, error) {
	var h [13]byte
	if _, err := io.ReadFull(body, h[:]); err != nil {
		return nil, fmt.Errorf("lzma: header: %w", err)
	}
	if dict := binary.LittleEndian.Uint32(h[1:5]); dict > maxDictionary {
		return nil, fmt.Errorf("lzma: a dictionary of %d bytes, at most %d", dict, maxDictionary)
	}
	return lzma.NewReader(io.MultiReader(strings.NewReader(string(h[:])), body))
}

// filesOf returns the path of every member of t that is not a directory, in the order of the
// archive, without a leading "./" or "/": regular files, symbolic and hard links, devices. A path
// that is not UTF-8, has a control character, or a "." or ".." component after the leading
// "./" is left out, and so is the archive's root. Paths of more than maxFileList bytes in all
// are errNoList.
func filesOf(t *tar.Reader) ([]string, error) {
	files := []string{}
	total := 0
	for {
		h, err := t.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeDir || strings.HasSuffix(h.Name, "/") {
			continue
		}
		p := listedPath(h.Name)
		if p == "" {
			continue
		}
		if total += len(p) + 4; total > maxFileList {
			return nil, fmt.Errorf("%w: the paths of data.tar have more than %d bytes", errNoList, maxFileList)
		}
		files = append(files, p)
	}
}

// listedPath returns the path of a data.tar member name as a file list has it, or "" when the
// list leaves it out (filesOf).
func listedPath(name string) string {
	p := strings.TrimLeft(strings.TrimPrefix(name, "./"), "/")
	if !validListedPath(p) {
		return ""
	}
	return p
}

// validListedPath reports whether p is a path a file list may hold: not empty, relative, UTF-8
// without control characters, every component neither empty nor "." nor "..".
func validListedPath(p string) bool {
	if p == "" || !utf8.ValidString(p) || strings.IndexFunc(p, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 || path.IsAbs(p) {
		return false
	}
	for c := range strings.SplitSeq(p, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return true
}
