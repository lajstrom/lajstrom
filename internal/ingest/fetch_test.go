package ingest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// wholeBody is a registry that answers every range with the whole blob, 200.
type wholeBody struct{ *providertest.Fake }

func (w wholeBody) OpenBlob(ctx context.Context, name string, d digest.Digest, _ string, redirect bool) (provider.Blob, error) {
	return w.Fake.OpenBlob(ctx, name, d, "", redirect)
}

// TestFetchedBlob checks the reader of the fetcher (#161): ReadAt across block boundaries, at
// the end and past it, with a request per block and the cache in front of them, also from a
// registry that ignores the range; Read streams the whole blob checked against its digest.
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
	for _, prov := range []provider.Provider{fake, wholeBody{fake}} {
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
		if got := len(fake.OpenBlobs()) - before; got != 4 {
			t.Errorf("%T: %d requests for the four blocks read, want 4", prov, got)
		}
		_ = b.Close()
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
