package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/iotest"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// wholeBody is a registry that answers every range with the whole blob, 200, as Nexus oci does
// (#352): data, or else the fake's blob, larger than which it keeps none (MaxKept). It records
// the Range of each request and counts the bytes read of its bodies and the bodies left open.
// With breakAt, the next body of data breaks off after that many bytes.
type wholeBody struct {
	*providertest.Fake
	data       []byte
	breakAt    int64
	ranges     []string
	read, open int64
}

func (w *wholeBody) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	w.ranges = append(w.ranges, rangeHeader)
	body := io.Reader(bytes.NewReader(w.data))
	if w.breakAt > 0 {
		body, w.breakAt = io.MultiReader(io.LimitReader(body, w.breakAt), iotest.ErrReader(errors.New("connection reset"))), 0
	}
	b, err := provider.Blob{Status: http.StatusOK, Size: int64(len(w.data)), Digest: d, Body: io.NopCloser(body)}, error(nil)
	if w.data == nil {
		b, err = w.Fake.OpenBlob(ctx, name, d, "", redirect)
	}
	if err == nil && b.Body != nil {
		w.open++
		b.Body = &countedBody{ReadCloser: b.Body, w: w}
	}
	return b, err
}

type countedBody struct {
	io.ReadCloser
	w      *wholeBody
	closed bool
}

func (c *countedBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.w.read += int64(n)
	return n, err
}

func (c *countedBody) Close() error {
	if !c.closed {
		c.closed = true
		c.w.open--
	}
	return c.ReadCloser.Close()
}

// TestFetchedBlob checks the reader of the fetcher (#161): ReadAt across block boundaries, at
// the end and past it, with a request per block and the cache in front of them; from a registry
// that ignores the range, one whole body read forward (#695); Read streams the whole blob
// checked against its digest.
func TestFetchedBlob(t *testing.T) {
	ctx := t.Context()
	data := make([]byte, 3*blockSize+1000)
	_, _ = rand.Read(data)
	desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(data), Size: int64(len(data))}
	fake := &providertest.Fake{RegistryName: "local"}
	rr, err := fake.Repository(ctx, "raw/x/blob")
	if err != nil {
		t.Fatal(err)
	}
	if err := rr.Push(ctx, desc, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		prov     provider.Provider
		requests int
	}{{fake, 4}, {&wholeBody{Fake: fake}, 1}} {
		prov := tt.prov
		pub := &publish{target: &target{name: "raw/x/blob", rr: rr, prov: prov}}
		b := &fetchedBlob{ctx: ctx, t: pub.target, desc: desc, blocks: map[int64][]byte{}}
		before := len(fake.OpenBlobs())
		for _, c := range []struct {
			off  int64
			n    int
			want error
		}{
			{0, 10, nil},
			{blockSize - 5, 10, nil},              // across the first boundary
			{2*blockSize - 1, blockSize + 2, nil}, // three blocks
			{int64(len(data)) - 7, 7, nil},        // the end
			{int64(len(data)) - 3, 10, io.EOF},
			{int64(len(data)) + 5, 1, io.EOF},
		} {
			p := make([]byte, c.n)
			n, err := b.ReadAt(p, c.off)
			if !errors.Is(err, c.want) {
				t.Fatalf("%T: ReadAt(%d, %d): %v, want %v", prov, c.n, c.off, err, c.want)
			}
			end := min(c.off+int64(c.n), int64(len(data)))
			if c.off < int64(len(data)) && !bytes.Equal(p[:n], data[c.off:end]) {
				t.Errorf("%T: ReadAt(%d, %d): the wrong bytes", prov, c.n, c.off)
			}
		}
		if got := len(fake.OpenBlobs()) - before; got != tt.requests {
			t.Errorf("%T: %d requests for the four blocks read, want %d", prov, got, tt.requests)
		}
		_ = b.Close()
		if w, ok := prov.(*wholeBody); ok && (w.read != desc.Size || w.open != 0) {
			t.Errorf("%T: %d bytes read of a blob of %d, %d bodies left open; want it read once and closed", prov, w.read, desc.Size, w.open)
		}
	}

	pub := &publish{target: &target{name: "raw/x/blob", rr: rr, prov: fake}}
	whole, err := io.ReadAll(&fetchedBlob{ctx: ctx, t: pub.target, desc: desc, blocks: map[int64][]byte{}})
	if err != nil || !bytes.Equal(whole, data) {
		t.Errorf("Read: %d bytes, %v", len(whole), err)
	}
	wrong := desc
	wrong.Digest = digest.FromString("a blob the registry does not have")
	if _, err := io.ReadAll(&fetchedBlob{ctx: ctx, t: pub.target, desc: wrong, blocks: map[int64][]byte{}}); err == nil {
		t.Error("Read of a blob the registry does not have: no error")
	}
}

// TestFetchedBlobIgnoredRange reads a zip from a registry that ignores the range, as a wheel's
// Validate and read-back do (#695): its end record and directory, a member near the end, then
// one from the start. The whole blob is read forward once to the end, the directory then in the
// cache, and once again from the start for the member behind the cache: two requests, where a
// request per block made 67 and read 35 times the blob.
func TestFetchedBlobIgnoredRange(t *testing.T) {
	ctx := t.Context()
	big := make([]byte, 2*maxBlocks*blockSize)
	_, _ = rand.Read(big)
	meta := []byte("Metadata-Version: 2.1\nName: x\nVersion: 1.0\n")
	data := storedZip(t, zipMember{"x/data.bin", big}, zipMember{"x-1.0.dist-info/METADATA", meta})
	desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(data), Size: int64(len(data))}
	prov := &wholeBody{Fake: &providertest.Fake{}, data: data}
	b := &fetchedBlob{ctx: ctx, t: &target{name: "pypi/x/blob", prov: prov}, desc: desc, blocks: map[int64][]byte{}}

	zr, err := zip.NewReader(b, desc.Size)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 0} {
		rc, err := zr.File[i].Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if want := [][]byte{big, meta}[i]; err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %d bytes, %v; want its %d", zr.File[i].Name, len(got), err, len(want))
		}
	}
	if r := prov.ranges; len(r) != 2 || r[0] == "" || r[1] != "" {
		t.Errorf("%d requests, ranges %q; want a range, then the whole blob once", len(r), r)
	}
	if prov.read > 2*desc.Size {
		t.Errorf("%d bytes read of a blob of %d, want at most twice that", prov.read, desc.Size)
	}
	if err := b.Close(); err != nil || prov.open != 0 {
		t.Errorf("Close: %v, %d bodies left open", err, prov.open)
	}
}

// TestFetchedBlobWholeBodyBreaks checks the whole body of a registry that ignores the range
// (#695) when it breaks off: the ReadAt that reads it fails and closes it, the next opens the
// blob again; Close closes it, and a ReadAt after Close keeps none open.
func TestFetchedBlobWholeBodyBreaks(t *testing.T) {
	data := make([]byte, 4*blockSize)
	_, _ = rand.Read(data)
	desc := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(data), Size: int64(len(data))}
	prov := &wholeBody{Fake: &providertest.Fake{}, data: data, breakAt: blockSize + 10}
	b := &fetchedBlob{ctx: t.Context(), t: &target{name: "raw/x/blob", prov: prov}, desc: desc, blocks: map[int64][]byte{}}
	readAt := func(off int64) error {
		p := make([]byte, 10)
		n, err := b.ReadAt(p, off)
		if err == nil && !bytes.Equal(p[:n], data[off:off+10]) {
			t.Errorf("ReadAt(10, %d): the wrong bytes", off)
		}
		return err
	}

	if err := readAt(blockSize); err == nil || prov.open != 0 {
		t.Errorf("ReadAt of a body that breaks off: %v, %d bodies open; want an error, none", err, prov.open)
	}
	if err := readAt(blockSize); err != nil || prov.open != 1 {
		t.Errorf("ReadAt again: %v, %d bodies open; want the blob opened again", err, prov.open)
	}
	if err := b.Close(); err != nil || prov.open != 0 {
		t.Errorf("Close: %v, %d bodies open", err, prov.open)
	}
	if err := readAt(2 * blockSize); err != nil || prov.open != 0 {
		t.Errorf("ReadAt after Close: %v, %d bodies open; want none", err, prov.open)
	}
	if r := prov.ranges; len(r) != 3 || r[0] == "" || r[1] != "" || r[2] != "" {
		t.Errorf("ranges %q; want a range, then the whole blob twice", r)
	}
}

// zipMember is a member of storedZip's zip.
type zipMember struct {
	name string
	data []byte
}

// storedZip returns a zip of members, in their order, stored uncompressed.
func storedZip(t *testing.T, members ...zipMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Store})
		if err == nil {
			_, err = w.Write(m.data)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
