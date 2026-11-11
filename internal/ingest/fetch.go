package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The block cache of fetchedBlob.ReadAt: a range request reads one aligned block, and the last
// maxBlocks blocks are kept, so that reading a zip's directory, at the end of the blob, in small
// pieces is a request or two, not one per piece.
const (
	blockSize = 64 << 10
	maxBlocks = 32
)

// fetchedBlob is the reader the fetcher of Validate returns (format.BlobFetcher), and the file
// of a read-back (readBack). Read streams the whole blob from the backend, opened at the first
// call and checked against the descriptor as it is read (a wrong digest or size fails the
// read). ReadAt reads parts of the blob with a
// Range of one block each (Provider.OpenBlob, redirects followed), and never the whole: a plugin
// that reads an archive whose directory is at its end calls ReadAt and not Read. The bytes of
// ReadAt are not checked against the digest, which only the whole blob can be. A backend that
// answers a range with the whole blob (200) is read up to the block it was asked for. ReadAt
// may be called from several goroutines; Read, from one.
type fetchedBlob struct {
	ctx  context.Context
	t    *target
	desc ocispec.Descriptor

	stream io.ReadCloser // opened by the first Read
	read   io.Reader     // the stream through the verifier

	mu     sync.Mutex
	blocks map[int64][]byte // by block index
	order  []int64          // the cached blocks, oldest first
}

// Read implements io.Reader.
func (b *fetchedBlob) Read(p []byte) (int, error) {
	if b.stream == nil {
		rc, err := b.t.rr.Fetch(b.ctx, b.desc)
		if err != nil {
			return 0, fmt.Errorf("ingest: fetch %s from %s: %w", b.desc.Digest, b.t.name, err)
		}
		b.stream, b.read = rc, newVerifier(rc, b.desc)
	}
	return b.read.Read(p)
}

// Close closes the stream, when Read opened one.
func (b *fetchedBlob) Close() error {
	if b.stream == nil {
		return nil
	}
	return b.stream.Close()
}

// ReadAt implements io.ReaderAt over the blob, of the descriptor's size.
func (b *fetchedBlob) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("ingest: ReadAt at a negative offset")
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= b.desc.Size {
			return n, io.EOF
		}
		block, err := b.block(pos / blockSize)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], block[pos%blockSize:])
	}
	return n, nil
}

// block returns block i of the blob, from the cache or with one range request.
func (b *fetchedBlob) block(i int64) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if data, ok := b.blocks[i]; ok {
		return data, nil
	}
	start := i * blockSize
	end := min(start+blockSize, b.desc.Size) - 1
	if b.t.prov == nil {
		return nil, fmt.Errorf("ingest: no registry for the range reads of %s", b.desc.Digest)
	}
	blob, err := b.t.prov.OpenBlob(b.ctx, b.t.name, b.desc.Digest, fmt.Sprintf("bytes=%d-%d", start, end), false)
	if err != nil {
		return nil, fmt.Errorf("ingest: read bytes %d-%d of %s from %s: %w", start, end, b.desc.Digest, b.t.name, err)
	}
	if blob.Body == nil {
		return nil, fmt.Errorf("ingest: read bytes %d-%d of %s: the backend answered %d without a body", start, end, b.desc.Digest, blob.Status)
	}
	defer func() { _ = blob.Body.Close() }()
	body := io.Reader(blob.Body)
	switch blob.Status {
	case http.StatusPartialContent:
	case http.StatusOK: // the backend ignored the range: skip to the block
		if _, err := io.CopyN(io.Discard, body, start); err != nil {
			return nil, fmt.Errorf("ingest: read up to byte %d of %s: %w", start, b.desc.Digest, err)
		}
	default:
		return nil, fmt.Errorf("ingest: read bytes %d-%d of %s: the backend answered %d", start, end, b.desc.Digest, blob.Status)
	}
	data := make([]byte, end-start+1)
	if _, err := io.ReadFull(body, data); err != nil {
		return nil, fmt.Errorf("ingest: read bytes %d-%d of %s: %w", start, end, b.desc.Digest, err)
	}
	if len(b.order) >= maxBlocks {
		delete(b.blocks, b.order[0])
		b.order = slices.Delete(b.order, 0, 1)
	}
	b.blocks[i], b.order = data, append(b.order, i)
	return data, nil
}
