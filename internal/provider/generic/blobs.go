package generic

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
//     Lajstrom's credentials. Each hop is one more GET with the Range, through the provider's
//     auth client, which answers the challenges of the registry's host only, so another
//     origin never gets Lajstrom's credentials; at most maxRedirects hops;
//   - 200 and 206 give the open body with Size from Content-Length (-1 without one),
//     ContentRange (206), Digest and AcceptRanges;
//   - 416 gives Status 416 and ContentRange, and no body;
//   - 404 is provider.ErrNotFound, and every other answer goes through translate.
//
// A Docker-Content-Digest that does not parse, or names another digest than d, is an error
// that wraps content.ErrMismatchedDigest: the bytes are not the blob asked for.
func (p *Provider) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	op := "open blob " + name + "@" + d.String()
	if o := p.override.BlobRedirect; o != nil && !*o {
		redirect = false
	}
	resp, loc, err := p.blobRequest(ctx, op, http.MethodGet, name, d, rangeHeader, redirect)
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
		return b, nil
	case http.StatusRequestedRangeNotSatisfiable:
		defer func() { _ = resp.Body.Close() }()
		b, err := headersOf(op, resp, d)
		b.Size = 0
		return b, err
	default:
		defer func() { _ = resp.Body.Close() }()
		return provider.Blob{}, translate(op, parseErrorResponse(resp))
	}
}

// StatBlob sends one HEAD of the blob and follows every redirect (up to maxRedirects, as
// OpenBlob does): Status 200 with Size, Digest and AcceptRanges; 404 is provider.ErrNotFound.
func (p *Provider) StatBlob(ctx context.Context, name string, d digest.Digest) (provider.Blob, error) {
	op := "stat blob " + name + "@" + d.String()
	resp, _, err := p.blobRequest(ctx, op, http.MethodHead, name, d, "", false)
	if err != nil {
		return provider.Blob{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return provider.Blob{}, translate(op, parseErrorResponse(resp))
	}
	return headersOf(op, resp, d)
}

// blobRequest sends method to the blob d of name and follows the redirects it gets itself,
// each a new request with the Range. With returnRedirect a redirect to another origin ends
// the chain: its response is closed and the Location returned. Otherwise the response is
// returned open, whatever its status.
func (p *Provider) blobRequest(ctx context.Context, op, method, name string, d digest.Digest, rangeHeader string, returnRedirect bool) (*http.Response, *url.URL, error) {
	if err := d.Validate(); err != nil {
		return nil, nil, fmt.Errorf("generic: %s: %w", op, err)
	}
	ref := registry.Reference{Registry: p.host, Repository: p.repoName(name), Reference: d.String()}
	if err := ref.ValidateRepository(); err != nil {
		return nil, nil, translate(op, err)
	}
	ctx = auth.AppendRepositoryScope(ctx, ref, auth.ActionPull)
	target := p.base + "/v2/" + ref.Repository + "/blobs/" + ref.Reference
	for hops := 0; ; hops++ {
		req, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("generic: %s: %w", op, err)
		}
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, err := p.clients.noFollow.Do(req)
		if err != nil {
			return nil, nil, translate(op, err)
		}
		if !isRedirect(resp.StatusCode) {
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
		case hops == maxRedirects:
			return nil, nil, fmt.Errorf("generic: %s: more than %d redirects", op, maxRedirects)
		}
		target = loc.String()
	}
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
