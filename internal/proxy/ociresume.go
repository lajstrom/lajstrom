package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The resume of an upstream's blob whose body broke (docs/spec/proxy-and-group.md §11.5, #885).
// On a link that drops every N seconds at R bytes per second, a blob larger than N × R never
// arrives in one GET: the fetch asks again with Range from the byte it reached and goes on with
// the same stream, so that the backend's upload, the tee and the digest check never see the
// break.

// errNotResumed is the cause of a body that broke and was not resumed: the upstream answered the
// Range with something else than the rest of the blob.
var errNotResumed = errors.New("proxy: the upstream did not answer the Range with the rest of the blob")

// upstreamBody is the upstream's body of a blob as the fetch reads it. A read that fails before
// the last byte, or a body that ends short, reopens the blob with Range: bytes=<n>- from the n
// bytes read (resume) and the read goes on there; each resume is logged. A resume after a body
// that got no byte since the previous one waits the backoff of the retries, and more than
// upstreams[].retries of them in a row end the fetch. The answer to a Range must be a 206 whose
// Content-Range starts at n, of the blob's size: a 200, or a 206 of other bytes, ends it too. n
// holds the bytes read and err the read error that ended it, so that the fetch tells the
// upstream's failure from the backend's.
type upstreamBody struct {
	ctx     context.Context
	c       *Core
	p       *ociProxied
	desc    ocispec.Descriptor
	rc      io.ReadCloser // nil once closed
	n       int64         // the bytes read
	from    int64         // the offset the body that reads started at
	idle    int           // resumes in a row whose previous body got no byte
	resumes int           // resumes in all
	err     error         // the read that broke, last
}

// resumable returns the body rc of the blob desc at p's path, resumed on ctx.
func (c *Core) resumable(ctx context.Context, p *ociProxied, desc ocispec.Descriptor, rc io.ReadCloser) *upstreamBody {
	return &upstreamBody{ctx: ctx, c: c, p: p, desc: desc, rc: rc}
}

// Read reads the blob, resuming its body after a read that broke.
func (b *upstreamBody) Read(p []byte) (int, error) {
	for {
		if b.rc == nil {
			return 0, b.err
		}
		n, err := b.rc.Read(p)
		b.n += int64(n)
		switch {
		case err == nil:
			return n, nil
		case errors.Is(err, io.EOF) && b.n >= b.desc.Size:
			return n, io.EOF // more than the size is the digest check's
		case errors.Is(err, io.EOF):
			err = io.ErrUnexpectedEOF
		}
		b.err = err
		if b.ctx.Err() != nil || !b.resume() {
			b.Close()
			return n, b.err
		}
		if n > 0 {
			return n, nil
		}
	}
}

// Close closes the body that reads.
func (b *upstreamBody) Close() {
	if b.rc != nil {
		_ = b.rc.Close()
		b.rc = nil
	}
}

// resume reopens the blob from b.n after the read that broke, b.err, and reports whether a body
// reads again. A reopen that fails counts as a resume that got no byte, and is tried again
// within the bound; one the upstream answers with other bytes ends it at once.
func (b *upstreamBody) resume() bool {
	for {
		if b.n > b.from {
			b.idle = 0
		} else {
			b.idle++
		}
		if b.idle > b.p.up.retries {
			b.err = fmt.Errorf("%w; not resumed: %d resumes in a row got no byte", b.err, b.idle-1)
			return false
		}
		if b.idle > 0 {
			select {
			case <-b.ctx.Done():
				return false
			case <-time.After(min(retryFirstWait<<(b.idle-1), retryLongest)):
			}
		}
		b.Close()
		b.from = b.n
		b.resumes++
		b.c.o.Log.InfoContext(b.ctx, "proxy: an upstream blob's body broke; the fetch resumes with Range", "repo", b.p.id(),
			"upstream", b.p.upstreamName(), "path", b.p.path, "digest", b.desc.Digest.String(), "offset", b.n, "size", b.desc.Size,
			"resume", b.resumes, "error", b.err.Error())
		rc, err := b.p.up.reopen(b.ctx, b.p.path, b.desc, b.n)
		switch {
		case err == nil:
			b.rc = rc
			return true
		case errors.Is(err, errNotResumed):
			b.err = fmt.Errorf("%w; %w", b.err, err)
			return false
		case b.ctx.Err() != nil:
			return false
		}
		b.err = err
	}
}

// reopen asks the upstream for the blob desc from byte off, as open does with Range:
// bytes=<off>-, and returns the body of a 206 whose Content-Range starts at off, of desc's size.
// Any other answer to the Range is errNotResumed; an error of the request is the provider's.
func (u *OCIUpstream) reopen(ctx context.Context, path string, desc ocispec.Descriptor, off int64) (io.ReadCloser, error) {
	b, err := u.client.OpenBlob(ctx, path, desc.Digest, "bytes="+strconv.FormatInt(off, 10)+"-", false)
	if err != nil {
		return nil, err
	}
	if b.Body != nil && b.Status == http.StatusPartialContent && rangeFrom(b.ContentRange, off, desc.Size) &&
		(b.Size < 0 || b.Size == desc.Size-off) {
		return b.Body, nil
	}
	if b.Body != nil {
		_ = b.Body.Close()
	}
	return nil, fmt.Errorf("%w: upstream %s answered bytes=%d- of %s with %d, Content-Range %q", errNotResumed, u.name, off, desc.Digest,
		b.Status, b.ContentRange)
}

// rangeFrom reports whether cr, a Content-Range, is bytes <off>-<last>/<size>, the size unknown
// (*) or size.
func rangeFrom(cr string, off, size int64) bool {
	rest, ok := strings.CutPrefix(cr, "bytes "+strconv.FormatInt(off, 10)+"-")
	if !ok {
		return false
	}
	_, total, ok := strings.Cut(rest, "/")
	return ok && (total == "*" || total == strconv.FormatInt(size, 10))
}
