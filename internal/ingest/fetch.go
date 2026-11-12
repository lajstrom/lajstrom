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
// answers a range with the whole blob (200), as Nexus oci does (#352), is asked for no range
// again (#695): ReadAt reads that one body forward, the blocks it passes kept in the cache, and
// opens the whole blob again only for a block behind the body and out of the cache, so a pass
// over the blob costs one read of it and not one per block, and each jump back out of the cache
// one more. Nothing of it is kept but the cache, maxBlocks blocks in memory, and no file
// (ADR 0014); the body is closed at the end of the blob, by Close, or at a read that fails.
// ReadAt may be called from several goroutines; Read, from one.
type fetchedBlob struct {
	ctx  context.Context
	t    *target
	desc ocispec.Descriptor

	stream io.ReadCloser // opened by the first Read
	read   io.Reader     // the stream through the verifier

	mu      sync.Mutex
	blocks  map[int64][]byte // by block index
	order   []int64          // the cached blocks, oldest first
	noRange bool             // the backend answered a range with the whole blob
	whole   io.ReadCloser    // that whole blob, read up to block next
	next    int64
	closed  bool // by Close: a ReadAt after it keeps no body open
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

// Close closes the stream, when Read opened one, and the whole blob ReadAt reads.
func (b *fetchedBlob) Close() error {
	b.mu.Lock()
	b.closed = true
	b.dropWhole()
	b.mu.Unlock()
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

// block returns block i of the blob: from the cache, with one range request, or from the whole
// blob of a backend that ignores ranges.
func (b *fetchedBlob) block(i int64) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer func() {
		if b.closed {
			b.dropWhole()
		}
	}()
	if data, ok := b.blocks[i]; ok {
		return data, nil
	}
	if b.whole != nil && i >= b.next {
		return b.forward(i)
	}
	start := i * blockSize
	end := min(start+blockSize, b.desc.Size) - 1
	if b.t.prov == nil {
		return nil, fmt.Errorf("ingest: no registry for the range reads of %s", b.desc.Digest)
	}
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
	if b.noRange {
		rangeHeader = ""
	}
	blob, err := b.t.prov.OpenBlob(b.ctx, b.t.name, b.desc.Digest, rangeHeader, false)
	if err != nil {
		return nil, fmt.Errorf("ingest: read bytes %d-%d of %s from %s: %w", start, end, b.desc.Digest, b.t.name, err)
	}
	if blob.Body == nil {
		return nil, fmt.Errorf("ingest: read bytes %d-%d of %s: the backend answered %d without a body", start, end, b.desc.Digest, blob.Status)
	}
	switch {
	case blob.Status == http.StatusPartialContent && !b.noRange:
	case blob.Status == http.StatusOK: // the backend ignores ranges: read this body forward from now on
		b.dropWhole()
		b.noRange, b.whole, b.next = true, blob.Body, 0
		return b.forward(i)
	default:
		_ = blob.Body.Close()
		return nil, fmt.Errorf("ingest: read bytes %d-%d of %s: the backend answered %d", start, end, b.desc.Digest, blob.Status)
	}
	defer func() { _ = blob.Body.Close() }()
	data := make([]byte, end-start+1)
	if _, err := io.ReadFull(blob.Body, data); err != nil {
		return nil, fmt.Errorf("ingest: read bytes %d-%d of %s: %w", start, end, b.desc.Digest, err)
	}
	b.keep(i, data)
	return data, nil
}

// forward reads the whole blob on, from block next up to block i, and returns block i. The
// blocks it passes go to the cache: a zip's end record, read first, is a pass to the end, after
// which its directory, just before, is in the cache. The whole blob is closed when read to its
// end, and at a failure to read; the next block out of the cache opens it again.
func (b *fetchedBlob) forward(i int64) ([]byte, error) {
	for {
		start := b.next * blockSize
		end := min(start+blockSize, b.desc.Size)
		data := make([]byte, end-start)
		if _, err := io.ReadFull(b.whole, data); err != nil {
			b.dropWhole()
			return nil, fmt.Errorf("ingest: read bytes %d-%d of the whole %s: %w", start, end-1, b.desc.Digest, err)
		}
		b.keep(b.next, data)
		b.next++
		if end == b.desc.Size {
			b.dropWhole()
		}
		if b.next > i {
			return data, nil
		}
	}
}

// keep puts block i in the cache, which drops its oldest block when full.
func (b *fetchedBlob) keep(i int64, data []byte) {
	if _, ok := b.blocks[i]; ok {
		b.blocks[i] = data
		return
	}
	if len(b.order) >= maxBlocks {
		delete(b.blocks, b.order[0])
		b.order = slices.Delete(b.order, 0, 1)
	}
	b.blocks[i], b.order = data, append(b.order, i)
}

// dropWhole closes the whole blob that ReadAt reads, if open.
func (b *fetchedBlob) dropWhole() {
	if b.whole != nil {
		_ = b.whole.Close()
		b.whole = nil
	}
}
