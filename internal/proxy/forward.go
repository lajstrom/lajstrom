package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// maxForwardBody bounds the body of a forwarded request (format.Proxy.Forward), which is held
// in memory so that a retry and a redirect send it again.
const maxForwardBody = 16 << 20

// Forward sends req to the upstream of the proxy repo repo and returns its answer
// (format.Proxy, docs/spec/proxy-and-group.md §9.2): the core's own request, through the
// upstream's client, with the parts of req and nothing else of the client's request. Nothing
// is cached; the answer's body streams, bounded by the upstream's maxMetadataSize.
func (c *Core) Forward(ctx context.Context, repo index.Repo, req format.ForwardRequest) (*http.Response, error) {
	p, err := c.resolve(repo)
	if err != nil {
		return nil, err
	}
	if p.up == nil {
		return nil, fmt.Errorf("%w: %s is offline and forwards nothing to its upstream", index.ErrNotFound, p.id())
	}
	target, err := p.up.below(req.Path)
	if err != nil {
		return nil, err
	}
	body, err := readForwardBody(req.Body)
	if err != nil {
		return nil, err
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	resp, err := p.up.send(ctx, call{operation: "forward", method: method, target: target, body: body,
		header: map[string]string{"Content-Type": req.ContentType, "Accept": req.Accept}})
	if err != nil {
		return nil, p.up.forwardError(err, target)
	}
	limit := p.up.MaxMetadataSize()
	switch {
	case resp.Body == nil: // a 304, which a request without validators does not ask for
		return nil, &statusError{upstream: p.up.Name(), status: resp.Status}
	case resp.ContentLength > limit:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: upstream %s: an answer of %d bytes, above maxMetadataSize %d", format.ErrBadGateway,
			p.up.Name(), resp.ContentLength, limit)
	}
	return &http.Response{Status: fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)), StatusCode: resp.Status,
		Header: resp.Header, ContentLength: resp.ContentLength,
		Body: &forwardBody{r: &bounded{r: resp.Body, left: limit}, c: resp.Body, up: p.up}}, nil
}

// readForwardBody reads the body of a forwarded request whole: at most maxForwardBody bytes, a
// larger one wraps *http.MaxBytesError; nil for none.
func readForwardBody(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(r, maxForwardBody+1)); err != nil {
		return nil, fmt.Errorf("proxy: the body of a forwarded request: %w", err)
	}
	if buf.Len() > maxForwardBody {
		return nil, fmt.Errorf("proxy: the body of a forwarded request is larger than %d bytes: %w", maxForwardBody,
			&http.MaxBytesError{Limit: maxForwardBody})
	}
	return buf.Bytes(), nil
}

// below returns path, relative to the upstream's URL, as an absolute URL below it: an absolute
// URL, a path from the root, or dot segments that leave the URL are an error of the caller.
func (u *Upstream) below(path string) (*url.URL, error) {
	ref, err := url.Parse(path)
	if err == nil && ref.Scheme == "" && ref.Host == "" && ref.User == nil && !strings.HasPrefix(ref.Path, "/") {
		if t := u.base.ResolveReference(ref); sameOrigin(t, u.base) && strings.HasPrefix(t.Path, u.base.Path) {
			return t, nil
		}
	}
	return nil, fmt.Errorf("proxy: upstream %s: a forwarded path %q that is not below its URL", u.name, path)
}

// forwardError is the error of a forwarded request that the upstream did not answer with a 2xx:
// its 404 and 410 wrap index.ErrNotFound, its 400, 413, 415 and 422 format.ErrInvalid (the
// body that the client sent is refused), and every other error is send's (§6).
func (u *Upstream) forwardError(err error, target *url.URL) error {
	var se *statusError
	switch {
	case errors.Is(err, errMissing):
		return fmt.Errorf("%w: upstream %s does not answer %s", index.ErrNotFound, u.name, strings.TrimPrefix(target.Path, u.base.Path))
	case errors.As(err, &se) && (se.status == http.StatusBadRequest || se.status == http.StatusRequestEntityTooLarge ||
		se.status == http.StatusUnsupportedMediaType || se.status == http.StatusUnprocessableEntity):
		return fmt.Errorf("%w: upstream %s refused the request: %d", format.ErrInvalid, u.name, se.status)
	}
	return err
}

// forwardBody is the body of a forwarded answer: r, which fails with errTooLarge past the
// upstream's maxMetadataSize, read with the errors of §6.
type forwardBody struct {
	r  io.Reader
	c  io.Closer
	up *Upstream
}

func (b *forwardBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return n, err
	case errors.Is(err, errTooLarge):
		return n, fmt.Errorf("%w: upstream %s: an answer above maxMetadataSize %d", format.ErrBadGateway, b.up.name, b.up.maxMetadataSize)
	}
	return n, b.up.transportError(context.Background(), err)
}

// Close releases the upstream's request.
func (b *forwardBody) Close() error { return b.c.Close() }
