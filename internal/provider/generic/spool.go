package generic

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
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
//
// server.spoolBudget (#427) bounds the bytes that the spooled chunks of this process hold at
// once, of every registry. A chunk's length is unknown until it ends, so it takes its bytes from
// the budget as it writes them, before each write, and gives them back when its file is closed.
// A write that would pass the budget refuses the chunk with an *provider.UnavailableError, 503
// with Retry-After: the file is closed, and nothing of the chunk has gone to the registry, so the
// session keeps its offset and the client may send the chunk again.

// chunkPattern is the name of a spooled chunk in os.TempDir; no client input is in it.
const chunkPattern = "lajstrom-chunk-*"

// openChunks counts the spooled chunks whose file is open, for the tests.
var openChunks atomic.Int64

var (
	// spoolBudget is server.spoolBudget, in bytes; zero is no budget.
	spoolBudget atomic.Int64
	// spoolHeld is the bytes the open spooled chunks hold.
	spoolHeld atomic.Int64
)

// spoolRetryAfter is the Retry-After of a chunk refused for the budget, as of an upload refused
// for server.maxConcurrentUploads.
const spoolRetryAfter = 5 * time.Second

// SetSpoolBudget sets server.spoolBudget for the providers of every registry of this process,
// the bytes their spooled chunks may hold at once; zero lifts it. serve calls it before it
// builds them.
func SetSpoolBudget(bytes int64) { spoolBudget.Store(max(bytes, 0)) }

// takeSpool takes n bytes from the budget; false, and nothing taken, when they would pass it.
func takeSpool(n int64) bool {
	for limit := spoolBudget.Load(); ; {
		held := spoolHeld.Load()
		if limit > 0 && held+n > limit {
			return false
		}
		if spoolHeld.CompareAndSwap(held, held+n) {
			metrics.SpoolBytes.Add(float64(n))
			return true
		}
	}
}

// chunkFile is a chunk of unknown length in an unlinked temporary file; size is its length, held
// the bytes it took from the budget.
type chunkFile struct {
	f    *os.File
	size int64
	held int64
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
	if c.size, err = io.Copy(c, body); err != nil {
		return nil, errors.Join(fmt.Errorf("spool the chunk: %w", err), c.close())
	}
	return c, nil
}

// Write writes p to the file once the budget gave its bytes; past the budget it writes nothing
// and refuses the chunk.
func (c *chunkFile) Write(p []byte) (int, error) {
	if !takeSpool(int64(len(p))) {
		metrics.SpoolRefusals.Inc()
		return 0, &provider.UnavailableError{RetryAfter: spoolRetryAfter,
			Reason: fmt.Sprintf("the spooled chunks of this replica would pass server.spoolBudget, %d bytes", spoolBudget.Load())}
	}
	c.held += int64(len(p))
	return c.f.Write(p)
}

// reader reads the chunk from its start; it is not a Closer, so the request does not close the
// file.
func (c *chunkFile) reader() io.Reader { return io.NewSectionReader(c.f, 0, c.size) }

// close closes the file, which frees its space, and gives its bytes back to the budget.
func (c *chunkFile) close() error {
	openChunks.Add(-1)
	spoolHeld.Add(-c.held)
	metrics.SpoolBytes.Sub(float64(c.held))
	c.held = 0
	return c.f.Close()
}
