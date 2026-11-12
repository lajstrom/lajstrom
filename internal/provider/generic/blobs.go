package generic

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// maxRedirects bounds the redirects that OpenBlob and StatBlob follow for one blob.
const maxRedirects = 5

// OpenBlob sends one GET of the blob, with rangeHeader as the Range when it is not empty,
// through the client that does not follow redirects, and uses the first answer as it is:
//
//   - a redirect (301, 302, 303, 307, 308) to another origin than the registry's gives its
//     Location, resolved against the request URL, as RedirectURL, and the body is closed; with
//     redirect false or capabilities.blobRedirect false it is followed instead. A redirect to
//     the registry's own origin is always followed: a client could not use it without
//     Lajstrom's credentials. Each hop is one more GET with the Range (blobRequest): with
//     Lajstrom's credentials while the chain stays on the registry's origin, without them
//     once it has left it, and never from https to http; at most maxRedirects hops;
//   - 200 and 206 give the open body with Size from Content-Length (-1 without one),
//     ContentRange (206), Digest and AcceptRanges;
//   - 416 gives Status 416 and ContentRange, and no body;
//   - 404 is provider.ErrNotFound, and every other answer goes through translate.
//
// A Docker-Content-Digest that does not parse, or names another digest than d, is an error
// that wraps content.ErrMismatchedDigest: the bytes are not the blob asked for. A registry
// whose vendor fails ranges (quirks.wholeBlobs) is asked without rangeHeader: the answer is
// the whole blob, 200, without AcceptRanges; the vendor is known before the first request
// (readQuirks).
func (p *Provider) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	op := "open blob " + name + "@" + d.String()
	ref, err := p.blobRef(op, name, d)
	if err != nil {
		return provider.Blob{}, err
	}
	if o := p.override.BlobRedirect; o != nil && !*o {
		redirect = false
	}
	q, err := p.readQuirks(ctx)
	if err != nil {
		return provider.Blob{}, err
	}
	whole := q.wholeBlobs
	if whole {
		rangeHeader = ""
	}
	resp, loc, err := p.blobRequest(ctx, op, http.MethodGet, ref, rangeHeader, redirect)
	if err != nil {
		return provider.Blob{}, err
	}
	if loc != nil {
		return provider.Blob{RedirectURL: loc.String(), Status: http.StatusTemporaryRedirect, Size: -1, Digest: d}, nil
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		b, err := headersOf(op, resp, d)
		if err != nil {
			_ = resp.Body.Close()
			return provider.Blob{}, err
		}
		b.Body = resp.Body
		b.AcceptRanges = b.AcceptRanges && !whole
		return b, nil
	case http.StatusRequestedRangeNotSatisfiable:
		defer func() { _ = resp.Body.Close() }()
		b, err := headersOf(op, resp, d)
		b.Size = 0
		return b, err
	default:
		defer func() { _ = resp.Body.Close() }()
		return provider.Blob{}, p.translate(op, parseErrorResponse(resp))
	}
}

// StatBlob sends one HEAD of the blob and follows every redirect (up to maxRedirects, as
// OpenBlob does): Status 200 with Size, Digest and AcceptRanges; 404 is provider.ErrNotFound.
// A registry whose HEAD says a blob exists where GET does not (quirks.blobExistsByGET) is asked
// with a GET of the first byte instead (statByGET); the vendor is known before the first
// request (readQuirks).
func (p *Provider) StatBlob(ctx context.Context, name string, d digest.Digest) (provider.Blob, error) {
	return p.statBlob(ctx, name, d, nil)
}

// statBlob is StatBlob with the vendor's quirks q, or, for nil, readQuirks' once the name and
// the digest are valid.
func (p *Provider) statBlob(ctx context.Context, name string, d digest.Digest, q *quirks) (provider.Blob, error) {
	op := "stat blob " + name + "@" + d.String()
	ref, err := p.blobRef(op, name, d)
	if err != nil {
		return provider.Blob{}, err
	}
	if q == nil {
		read, err := p.readQuirks(ctx)
		if err != nil {
			return provider.Blob{}, err
		}
		q = &read
	}
	if q.blobExistsByGET {
		return p.statByGET(ctx, op, ref, d)
	}
	resp, _, err := p.blobRequest(ctx, op, http.MethodHead, ref, "", false)
	if err != nil {
		return provider.Blob{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return provider.Blob{}, p.translate(op, parseErrorResponse(resp))
	}
	b, err := headersOf(op, resp, d)
	b.AcceptRanges = b.AcceptRanges && !q.wholeBlobs
	return b, err
}

// statByGET answers StatBlob with a GET of the blob's first byte, Range: bytes=0-0, whose
// body is closed unread. The size is the complete length of the 206's Content-Range, or of a
// 416's (an empty blob), or the Content-Length of a 200 that ignored the range; -1 when the
// answer does not say.
func (p *Provider) statByGET(ctx context.Context, op string, ref registry.Reference, d digest.Digest) (provider.Blob, error) {
	resp, _, err := p.blobRequest(ctx, op, http.MethodGet, ref, "bytes=0-0", false)
	if err != nil {
		return provider.Blob{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	default:
		return provider.Blob{}, p.translate(op, parseErrorResponse(resp))
	}
	b, err := headersOf(op, resp, d)
	if err != nil {
		return provider.Blob{}, err
	}
	if resp.StatusCode != http.StatusOK {
		b.Size = completeLength(b.ContentRange)
	}
	b.Status, b.ContentRange = http.StatusOK, ""
	return b, nil
}

// completeLength is the length after the "/" of a Content-Range, bytes 0-0/13 or bytes */0;
// -1 for "*" or a header that does not parse.
func completeLength(contentRange string) int64 {
	_, total, ok := strings.Cut(contentRange, "/")
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if !ok || err != nil || n < 0 {
		return -1
	}
	return n
}

// blobRef is the reference of the blob d of name, for blobRequest: an error, before any
// request, when the digest or the repository name is invalid.
func (p *Provider) blobRef(op, name string, d digest.Digest) (registry.Reference, error) {
	if err := d.Validate(); err != nil {
		return registry.Reference{}, fmt.Errorf("generic: %s: %w", op, err)
	}
	ref := registry.Reference{Registry: p.host, Repository: p.repoName(name), Reference: d.String()}
	if err := ref.ValidateRepository(); err != nil {
		return registry.Reference{}, p.translate(op, err)
	}
	return ref, nil
}

// blobRequest sends method to the blob of ref (blobRef) and follows the redirects it gets itself,
// each a new request with the Range. With returnRedirect a redirect to another origin ends
// the chain: its response is closed and the Location returned. Otherwise the response is
// returned open, whatever its status.
//
// The registry's credentials go only to its origin (threat model P-2): the auth client's
// token cache is keyed by host and port without the scheme, so a hop from https to http on
// the same host would carry them in clear text, and such a hop is refused. Once a hop leaves
// the origin, the chain goes on without credentials, even back to the origin, as oras-go's
// own redirects do.
func (p *Provider) blobRequest(ctx context.Context, op, method string, ref registry.Reference, rangeHeader string, returnRedirect bool) (*http.Response, *url.URL, error) {
	ctx = auth.AppendRepositoryScope(ctx, ref, auth.ActionPull)
	target := p.base + "/v2/" + ref.Repository + "/blobs/" + ref.Reference
	left := false // a hop has left the registry's origin
	for hops := 0; ; hops++ {
		req, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("generic: %s: %w", op, err)
		}
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, err := p.hop(req, left)
		if err != nil {
			return nil, nil, p.viaRedirect(req.URL, p.translate(op, err))
		}
		if !isRedirect(resp.StatusCode) {
			if !p.sameOrigin(req.URL) {
				resp.Body = &redirectBody{ReadCloser: resp.Body, host: req.URL.Host}
			}
			return resp, nil, nil
		}
		loc, err := resp.Location()
		// A redirect's body is a line of HTML at most; reading it keeps the connection.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		switch {
		case err != nil:
			return nil, nil, fmt.Errorf("generic: %s: a redirect without a usable Location: %w", op, err)
		case returnRedirect && !p.sameOrigin(loc):
			return nil, loc, nil
		}
		if err := p.checkHop(op, req.URL, loc, hops); err != nil {
			return nil, nil, err
		}
		left = left || !p.sameOrigin(loc)
		target = loc.String()
	}
}

// checkHop refuses the redirect from to loc, the hops-th of a chain: past maxRedirects, from
// https to another scheme, or to a URL the guard does not allow (Options.PublicRedirects).
func (p *Provider) checkHop(op string, from, loc *url.URL, hops int) error {
	switch {
	case hops == maxRedirects:
		return fmt.Errorf("generic: %s: more than %d redirects", op, maxRedirects)
	case from.Scheme == "https" && loc.Scheme != "https":
		return fmt.Errorf("generic: %s: a redirect from https to %s refused", op, loc.Scheme)
	case p.guard != nil:
		if err := p.guard.CheckURL(loc); err != nil {
			return fmt.Errorf("generic: %s: a redirect: %w", op, err)
		}
	}
	return nil
}

// hop sends one request of a blob's redirect chain, without redirects: through the auth
// client, or, once the chain has left the registry's origin, without credentials.
func (p *Provider) hop(req *http.Request, left bool) (*http.Response, error) {
	if !left {
		return p.clients.noFollow.Do(req)
	}
	for k, v := range p.clients.header {
		req.Header[k] = v
	}
	return p.clients.anonymous.Do(req)
}

// isRedirect reports whether status is a redirect with a Location.
func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// sameOrigin reports whether u is on the registry's origin: its scheme, host and port, the
// default port of the scheme written or not.
func (p *Provider) sameOrigin(u *url.URL) bool {
	scheme, host, _ := strings.Cut(p.base, "://")
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(canonicalHost(u.Scheme, u.Host), canonicalHost(scheme, host))
}

// canonicalHost is host without the default port of scheme.
func canonicalHost(scheme, host string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return strings.TrimSuffix(host, ":80")
	case "https":
		return strings.TrimSuffix(host, ":443")
	}
	return host
}

// headersOf reads what a Blob carries from the headers of a 200, 206 or 416: Status, Size,
// ContentRange (206 and 416), Digest (checked against d) and AcceptRanges.
func headersOf(op string, resp *http.Response, d digest.Digest) (provider.Blob, error) {
	b := provider.Blob{
		Status:       resp.StatusCode,
		Size:         resp.ContentLength,
		Digest:       d,
		AcceptRanges: strings.EqualFold(strings.TrimSpace(resp.Header.Get("Accept-Ranges")), "bytes"),
	}
	if resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		b.ContentRange = resp.Header.Get("Content-Range")
	}
	if v := resp.Header.Get("Docker-Content-Digest"); v != "" {
		got, err := digest.Parse(v)
		switch {
		case err != nil:
			return provider.Blob{}, fmt.Errorf("generic: %s: the backend's Docker-Content-Digest %q: %w: %w", op, v, err, content.ErrMismatchedDigest)
		case got != d:
			return provider.Blob{}, fmt.Errorf("generic: %s: the backend answered with the blob %s: %w", op, got, content.ErrMismatchedDigest)
		}
	}
	return b, nil
}
