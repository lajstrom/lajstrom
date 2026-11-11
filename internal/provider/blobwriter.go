package provider

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ErrWriterClosed is the error of a BlobWriter's Write or Commit after Commit or Cancel ended
// it.
var ErrWriterClosed = errors.New("provider: the blob writer is committed or cancelled")

// minChunk is the first buffer of a chunked writer: a small file is held in a small buffer, and
// the buffer doubles up to the chunk size.
const minChunk = 64 << 10

// NewChunkedWriter returns the BlobWriter of a BlobUploader that sends chunks of chunkSize
// bytes (a non-positive one is taken as 1). The session is opened by the first chunk that is
// full, or by Commit: a blob of at most chunkSize bytes is one monolithic PUT, and one that is
// cancelled before its first chunk sends nothing. Write, the chunks and the POST use ctx.
func NewChunkedWriter(ctx context.Context, u BlobUploader, chunkSize int) BlobWriter {
	return &chunkedWriter{ctx: ctx, up: u, chunk: max(chunkSize, 1)}
}

// chunkedWriter is the BlobWriter of NewChunkedWriter.
type chunkedWriter struct {
	ctx   context.Context
	up    BlobUploader
	chunk int

	session Upload // the session; zero until started
	started bool
	buf     []byte // the bytes not sent yet, fewer than chunk
	size    int64
	ended   bool  // Commit succeeded, or Cancel ran
	err     error // the failure of a Write or Commit; the session is left to Cancel
}

func (w *chunkedWriter) Write(p []byte) (int, error) {
	switch {
	case w.ended:
		return 0, ErrWriterClosed
	case w.err != nil:
		return 0, w.err
	}
	n := 0
	for len(p) > 0 {
		k := min(len(p), w.chunk-len(w.buf))
		if len(w.buf)+k > cap(w.buf) {
			grown := make([]byte, len(w.buf), min(w.chunk, max(2*cap(w.buf), len(w.buf)+k, minChunk)))
			copy(grown, w.buf)
			w.buf = grown
		}
		w.buf = append(w.buf, p[:k]...)
		p, n, w.size = p[k:], n+k, w.size+int64(k)
		if len(w.buf) == w.chunk {
			if err := w.flush(); err != nil {
				w.err = err
				return n, err
			}
		}
	}
	return n, nil
}

// flush sends the buffer as a chunk of known size, the session's next PATCH.
func (w *chunkedWriter) flush() error {
	if err := w.start(w.ctx); err != nil {
		return err
	}
	u, err := w.up.AppendBlobChunk(w.ctx, w.session, bytes.NewReader(w.buf), int64(len(w.buf)))
	if u.Location != "" {
		w.session = u
	}
	if err != nil {
		return err
	}
	w.buf = w.buf[:0]
	return nil
}

// start opens the session unless it is open.
func (w *chunkedWriter) start(ctx context.Context) error {
	if w.started {
		return nil
	}
	u, err := w.up.StartBlobUpload(ctx)
	if err != nil {
		return err
	}
	w.session, w.started = u, true
	return nil
}

func (w *chunkedWriter) Size() int64 { return w.size }

// Commit closes the session with d: the buffer is the last chunk of FinishBlobUpload, which sends
// it as the body of the PUT when the session holds no bytes yet and as a PATCH of its own before
// the PUT otherwise.
func (w *chunkedWriter) Commit(ctx context.Context, d digest.Digest) (ocispec.Descriptor, error) {
	switch {
	case w.ended:
		return ocispec.Descriptor{}, ErrWriterClosed
	case w.err != nil:
		return ocispec.Descriptor{}, w.err
	}
	if err := w.start(ctx); err != nil {
		w.err = err
		return ocispec.Descriptor{}, err
	}
	var last io.Reader
	if len(w.buf) > 0 {
		last = bytes.NewReader(w.buf)
	}
	desc, err := w.up.FinishBlobUpload(ctx, w.session, d, last, int64(len(w.buf)))
	if err != nil {
		w.err = err
		return ocispec.Descriptor{}, err
	}
	w.ended, w.buf = true, nil
	return desc, nil
}

// Cancel drops the session: CancelBlobUpload when it was opened, nothing otherwise.
func (w *chunkedWriter) Cancel(ctx context.Context) error {
	if w.ended {
		return nil
	}
	w.ended, w.buf = true, nil
	if !w.started {
		return nil
	}
	return w.up.CancelBlobUpload(ctx, w.session)
}
