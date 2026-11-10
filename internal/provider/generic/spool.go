package generic

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
)

// The spool of registries[].uploads.spoolUnknownLength (#425, docs/spec/http-routing.md §2).
// Gitea and Forgejo refuse a chunk without Content-Range into a session that holds bytes (400),
// JFrog too (416); a chunk of unknown length can be sent to them only once its length is known.
// With the flag on, AppendBlobChunk writes such a chunk to a temporary file in os.TempDir
// ($TMPDIR, else /tmp) for the one request, and sends it from there. The file is unlinked as
// soon as it is made, before a byte is in it, so that nothing can open it and a crash leaves
// nothing (a system that cannot unlink an open file gets no spool), and it is closed on every
// path out of the request. Its size is bounded by what the caller lets through: the facade cuts
// a body of unknown length at the repo's maxFileSize.

// chunkPattern is the name of a spooled chunk in os.TempDir; no client input is in it.
const chunkPattern = "lajstrom-chunk-*"

// openChunks counts the spooled chunks whose file is open, for the tests.
var openChunks atomic.Int64

// chunkFile is a chunk of unknown length in an unlinked temporary file; size is its length.
type chunkFile struct {
	f    *os.File
	size int64
}

// spoolChunk writes body to a new temporary file and returns it. When body fails, as one that
// the facade cut at the repo's maxFileSize or whose client went away, the error wraps body's,
// and the file is closed.
func spoolChunk(body io.Reader) (*chunkFile, error) {
	f, err := os.CreateTemp("", chunkPattern)
	if err != nil {
		return nil, fmt.Errorf("spool the chunk: %w", err)
	}
	openChunks.Add(1)
	c := &chunkFile{f: f}
	if err := os.Remove(f.Name()); err != nil {
		return nil, errors.Join(fmt.Errorf("spool the chunk: unlink its file: %w", err), c.close())
	}
	if c.size, err = io.Copy(f, body); err != nil {
		return nil, errors.Join(fmt.Errorf("spool the chunk: %w", err), c.close())
	}
	return c, nil
}

// reader reads the chunk from its start; it is not a Closer, so the request does not close the
// file.
func (c *chunkFile) reader() io.Reader { return io.NewSectionReader(c.f, 0, c.size) }

// close closes the file, which frees its space.
func (c *chunkFile) close() error {
	openChunks.Add(-1)
	return c.f.Close()
}
