package proxy

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
)

// The transit of a blob (docs/spec/proxy-and-group.md §11.8, #875). While a blob's copy into the
// backend runs under its claim, a request that does not hold the claim is not made to wait for
// the commit: containerd gives up on a response whose headers take 30 s
// (ResponseHeaderTimeout), and an ingress after 60 s. It opens a GET of its own from the
// upstream, with the redirect, credential and digest rules of the fetch and its resumes, and
// streams it to its client through the tee's rule: the last byte is held until the bytes have
// the digest, and other bytes abort the connection and write proxy.reject. Nothing is written
// anywhere; the backend gets the claim holder's one copy. upstreams[].maxConcurrentTransits caps
// the transits of one blob on the replica; a request beyond it waits as before.

// fetchTransit is the result of lajstrom_proxy_fetches_total of a request answered by a
// transit.
const fetchTransit = "transit"

// transits counts the transits that run on the replica, by blob (blobFlight).
type transits struct {
	mu sync.Mutex
	m  map[string]int
}

// take takes a transit of key when fewer than most run, any number when most is negative, and
// reports whether it did; release ends the one it took.
func (q *transits) take(key string, most int) (release func(), ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if most >= 0 && q.m[key] >= most {
		return nil, false
	}
	if q.m == nil {
		q.m = map[string]int{}
	}
	q.m[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			q.mu.Lock()
			defer q.mu.Unlock()
			if q.m[key]--; q.m[key] <= 0 {
				delete(q.m, key)
			}
		})
	}, true
}

// running returns how many transits of key run.
func (q *transits) running(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.m[key]
}

// ociTransit answers the GET of the blob desc at p's path by a request that does not hold its
// claim while the claim's copy runs: the verified stream of a GET of its own from the upstream,
// on ctx, the request's. nil when transit is off, when the upstream's cap of transits of the
// blob is reached, or when the upstream did not answer the GET: the request then waits for the
// copy.
func (c *Core) ociTransit(ctx context.Context, p *ociProxied, desc ocispec.Descriptor) io.ReadCloser {
	key := blobFlight(p, desc.Digest)
	release, ok := c.transits.take(key, p.up.transits)
	if !ok {
		return nil
	}
	body, err := p.up.open(ctx, p.path, desc)
	if err != nil {
		release()
		if ctx.Err() == nil {
			c.o.Log.WarnContext(ctx, "proxy: a blob's transit not opened; the request waits for its copy", "repo", p.id(),
				"upstream", p.upstreamName(), "path", p.path, "digest", desc.Digest.String(), "error", err.Error())
		}
		return nil
	}
	c.o.Log.InfoContext(ctx, "proxy: a blob in transit from the upstream while its copy runs", "repo", p.id(), "upstream", p.upstreamName(),
		"path", p.path, "digest", desc.Digest.String(), "size", desc.Size, "transits", c.transits.running(key),
		"principal", auth.PrincipalFrom(ctx).String())
	return &transit{ctx: ctx, c: c, p: p, desc: desc, up: c.resumable(ctx, p, desc, body), h: desc.Digest.Algorithm().Hash(), release: release}
}

// transit is the stream of a blob in transit: the upstream's body, resumed as a fetch's is,
// whose last byte is returned only once the bytes have the blob's digest.
type transit struct {
	ctx     context.Context
	c       *Core
	p       *ociProxied
	desc    ocispec.Descriptor
	up      *upstreamBody
	h       hash.Hash
	n       int64
	release func()
	err     error // the end: io.EOF once verified
}

// Read returns the blob's bytes as they come, the last one once they have its digest.
func (r *transit) Read(b []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.up.Read(b)
	if int64(n) > r.desc.Size-r.n {
		return 0, r.fail(fmt.Errorf("%w: proxy: %s: upstream %s sent more than the %d bytes of %s", format.ErrBadGateway, r.p.id(),
			r.p.upstreamName(), r.desc.Size, r.desc.Digest))
	}
	r.h.Write(b[:n])
	r.n += int64(n)
	if r.n == r.desc.Size {
		if got := digest.NewDigest(r.desc.Digest.Algorithm(), r.h); got != r.desc.Digest {
			r.c.ociReject(r.ctx, r.p, r.desc.Digest, "checksum", map[string]any{"transit": true, "got": got.String()})
			return max(n-1, 0), r.fail(fmt.Errorf("%w: proxy: %s: upstream %s served other bytes for %s in transit: %s", format.ErrBadGateway,
				r.p.id(), r.p.upstreamName(), r.desc.Digest, got))
		}
		r.end(nil) // the last byte goes with its verification; the next read is io.EOF
		return n, nil
	}
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF):
		err = io.ErrUnexpectedEOF // short of the size: upstreamBody resumed what it could
	}
	if r.up.ended != nil && r.ctx.Err() == nil {
		err = fmt.Errorf("proxy: %s: the blob %s in transit broke after %d of its %d bytes: %w", r.p.id(), r.desc.Digest, r.n, r.desc.Size,
			r.p.up.classify(r.ctx, r.up.ended))
	}
	return n, r.fail(err)
}

// fail ends the transit with err, which the reader returns from now on, and counts it as error,
// unless the request's context ended: its client left.
func (r *transit) fail(err error) error {
	r.err = err
	if r.ctx.Err() == nil {
		r.c.o.Log.WarnContext(r.ctx, "proxy: a blob's transit failed; the client's connection is aborted", "repo", r.p.id(),
			"upstream", r.p.upstreamName(), "path", r.p.path, "digest", r.desc.Digest.String(), "got", r.n, "error", err.Error())
		r.end(err)
	}
	return err
}

// end counts the transit once, transit when its bytes had the digest, and releases its
// place under the cap.
func (r *transit) end(err error) {
	if r.release == nil {
		return
	}
	r.release()
	r.release = nil
	if err != nil {
		ociCountFetch(r.p, fetchError)
		return
	}
	ociCountFetch(r.p, fetchTransit)
	r.err = io.EOF
}

// Close closes the upstream's body and releases the transit's place.
func (r *transit) Close() error {
	r.up.Close()
	if r.release != nil {
		r.release()
		r.release = nil
	}
	return nil
}
