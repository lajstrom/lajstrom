package generic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// The blob upload sessions of provider.BlobUploader, on the registry's upload endpoints
// (distribution spec, "Pushing blobs"), sent through the provider's auth client. oras-go has
// no primitives for them: blobStore.Push runs POST and PUT in one call, and the facade needs
// each step on its own, from whichever replica gets the request. A session is the Upload the
// registry answered: the Location as it was sent (distribution's carries signed state in its
// query and changes with every chunk, Zot's is a path), the upload id and the offset.

// octetStream is the Content-Type of a chunk.
const octetStream = "application/octet-stream"

var _ provider.BlobUploader = (*repository)(nil)

// StartBlobUpload opens a session: POST /v2/<name>/blobs/uploads/, 202 with the Location.
func (r *repository) StartBlobUpload(ctx context.Context) (provider.Upload, error) {
	op := r.op("start upload", "")
	resp, err := r.send(ctx, op, http.MethodPost, r.uploadsURL(nil), nil, 0, nil, http.StatusAccepted)
	if err != nil {
		return provider.Upload{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	return r.session(op, resp, provider.Upload{}, 0)
}

// MountBlob asks for the blob d from the repository from: POST …/blobs/uploads/?mount=&from=.
// 201 is a mount; 202 is a session the registry opened instead. Without from, the query has
// no from, and the registry may look for the blob anywhere.
func (r *repository) MountBlob(ctx context.Context, d digest.Digest, from string) (bool, provider.Upload, error) {
	op := r.op("mount", d.String())
	if err := d.Validate(); err != nil {
		return false, provider.Upload{}, fmt.Errorf("generic: %s: %w", op, err)
	}
	q := url.Values{"mount": {d.String()}}
	if from != "" {
		fromRef := registry.Reference{Registry: r.p.host, Repository: r.p.repoName(from)}
		ctx = auth.AppendRepositoryScope(ctx, fromRef, auth.ActionPull)
		q.Set("from", fromRef.Repository)
	}
	resp, err := r.send(ctx, op, http.MethodPost, r.uploadsURL(q), nil, 0, nil, http.StatusCreated, http.StatusAccepted)
	if err != nil {
		return false, provider.Upload{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusCreated {
		return true, provider.Upload{}, nil
	}
	u, err := r.session(op, resp, provider.Upload{}, 0)
	return false, u, err
}

// AppendBlobChunk streams body to the session: PATCH <location>, 202 with the new Location
// and Range. A chunk of known size carries Content-Range from the session's offset, which the
// registry checks (416 when it does not hold that many bytes); one of unknown size goes out
// with chunked encoding and no Content-Range, the streamed PATCH that docker sends. A chunk of
// 0 bytes sends nothing.
func (r *repository) AppendBlobChunk(ctx context.Context, u provider.Upload, body io.Reader, size int64) (provider.Upload, error) {
	op := r.op("append to upload", u.ID)
	loc, err := r.p.location(u.Location)
	if err != nil {
		return u, fmt.Errorf("generic: %s: %w", op, err)
	}
	if size == 0 || body == nil {
		return u, nil
	}
	r.prime(ctx, loc)
	var hdr http.Header
	if size > 0 {
		hdr = http.Header{"Content-Range": {contentRange(u.Offset, size)}}
	}
	cr := &countingReader{r: body}
	resp, err := r.send(ctx, op, http.MethodPatch, loc.String(), cr, size, hdr, http.StatusAccepted)
	if err != nil {
		return u, err
	}
	defer func() { _ = resp.Body.Close() }()
	return r.session(op, resp, u, u.Offset+cr.n)
}

// FinishBlobUpload closes the session: PUT <location>?digest=<d>, 201. The last chunk is the
// body of the PUT only when its size is known and the session holds no bytes yet, a monolithic
// upload. Otherwise it is sent first as a PATCH of its own (AppendBlobChunk), and the PUT has
// no body: registries read the body of the closing PUT by its Content-Length, and JFrog drops
// the body of a closing PUT that follows a PATCH and answers 201 for the short blob
// (docs/registries/jfrog-jcr.md, item 2). When that PATCH fails, no PUT is sent. The
// registry's DIGEST_INVALID wraps content.ErrMismatchedDigest.
func (r *repository) FinishBlobUpload(ctx context.Context, u provider.Upload, d digest.Digest, body io.Reader, size int64) (ocispec.Descriptor, error) {
	op := r.op("finish upload", u.ID)
	if err := d.Validate(); err != nil {
		return ocispec.Descriptor{}, &backendError{op: op, kind: content.ErrMismatchedDigest, detail: "invalid digest", cause: err}
	}
	if body != nil && size != 0 && (size < 0 || u.Offset > 0) {
		var err error
		if u, err = r.AppendBlobChunk(ctx, u, body, size); err != nil {
			return ocispec.Descriptor{}, err
		}
		body, size = nil, 0
	}
	loc, err := r.p.location(u.Location)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("generic: %s: %w", op, err)
	}
	put := *loc
	q := put.Query()
	q.Set("digest", d.String())
	put.RawQuery = q.Encode()
	var (
		hdr http.Header
		rd  io.Reader
		cr  = &countingReader{}
	)
	if body != nil && size > 0 {
		r.prime(ctx, loc)
		hdr = http.Header{"Content-Range": {contentRange(u.Offset, size)}}
		cr.r, rd = body, cr
	}
	resp, err := r.send(ctx, op, http.MethodPut, put.String(), rd, size, hdr, http.StatusCreated)
	var er *errcode.ErrorResponse
	switch {
	case errors.As(err, &er) && er.StatusCode == http.StatusBadRequest && hasCode(er, errcode.ErrorCodeDigestInvalid):
		return ocispec.Descriptor{}, &backendError{op: op, kind: content.ErrMismatchedDigest, detail: responseDetail(er), cause: err}
	case err != nil:
		return ocispec.Descriptor{}, err
	}
	_ = resp.Body.Close()
	return ocispec.Descriptor{MediaType: octetStream, Digest: d, Size: u.Offset + cr.n}, nil
}

// BlobUploadStatus asks for the session's offset: GET <location>, 204 with Range and Location.
func (r *repository) BlobUploadStatus(ctx context.Context, u provider.Upload) (provider.Upload, error) {
	op := r.op("upload status", u.ID)
	loc, err := r.p.location(u.Location)
	if err != nil {
		return u, fmt.Errorf("generic: %s: %w", op, err)
	}
	resp, err := r.send(ctx, op, http.MethodGet, loc.String(), nil, 0, nil, http.StatusNoContent, http.StatusOK)
	if err != nil {
		return u, err
	}
	defer func() { _ = resp.Body.Close() }()
	return r.session(op, resp, u, u.Offset)
}

// CancelBlobUpload ends the session: DELETE <location>, 204.
func (r *repository) CancelBlobUpload(ctx context.Context, u provider.Upload) error {
	op := r.op("cancel upload", u.ID)
	loc, err := r.p.location(u.Location)
	if err != nil {
		return fmt.Errorf("generic: %s: %w", op, err)
	}
	resp, err := r.send(ctx, op, http.MethodDelete, loc.String(), nil, 0, nil,
		http.StatusNoContent, http.StatusOK, http.StatusAccepted)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// uploadsURL is the upload endpoint of the repository, with query q.
func (r *repository) uploadsURL(q url.Values) string {
	u := r.p.base + "/v2/" + r.p.repoName(r.name) + "/blobs/uploads/"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// scoped is ctx with the pull and push scopes of the repository, which a registry with token
// auth wants for an upload.
func (r *repository) scoped(ctx context.Context) context.Context {
	ref := registry.Reference{Registry: r.p.host, Repository: r.p.repoName(r.name)}
	return auth.AppendRepositoryScope(ctx, ref, auth.ActionPull, auth.ActionPush)
}

// send sends one request of an upload session and returns the response when its status is
// one of want; any other status is the translated error of the response, whose body is closed
// then. A body of size bytes (-1: unknown, sent chunked) goes out as application/octet-stream.
func (r *repository) send(ctx context.Context, op, method, target string, body io.Reader, size int64, hdr http.Header, want ...int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.scoped(ctx), method, target, body)
	if err != nil {
		return nil, fmt.Errorf("generic: %s: %w", op, withoutURL(err))
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", octetStream)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := r.p.clients.follow.Do(req)
	if err != nil {
		return nil, translate(op, withoutURL(err))
	}
	if slices.Contains(want, resp.StatusCode) {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	er := parseErrorResponse(resp)
	if er.URL != nil {
		u := *er.URL
		u.RawQuery = "" // distribution's upload state; the log never carries it
		er.URL = &u
	}
	return nil, translate(op, er)
}

// prime readies the auth client for a request with a streamed body to the session at loc.
// oras-go's auth client answers a 401 by sending the request again, which a streamed body
// cannot be; so, against a registry with credentials, a bodyless GET of the session (its
// status, harmless) takes the challenge first. A Basic scheme in the cache needs nothing; a
// bearer token may have expired since it was cached, so it is primed every time. A replica
// that continues another's session starts with an empty cache: this is what lets it.
func (r *repository) prime(ctx context.Context, loc *url.URL) {
	if !r.p.clients.credentials {
		return
	}
	if s, err := r.p.clients.cache.GetScheme(ctx, r.p.host); err == nil && s == auth.SchemeBasic {
		return
	}
	ctx = r.scoped(ctx)
	var challenge string
	req, err := http.NewRequestWithContext(context.WithValue(ctx, challengeKey{}, &challenge), http.MethodGet, loc.String(), nil)
	if err != nil {
		return
	}
	resp, err := r.p.clients.follow.Do(req)
	if err != nil {
		return // the request with the body reports the failure
	}
	_ = resp.Body.Close()
	aliasToken(ctx, r.p.clients.cache, r.p.host, challenge)
}

// aliasToken caches the bearer token that the auth client got after challenge also under the
// scopes of ctx alone. The auth client keys a token by the request's scopes merged with the
// challenge's scope parameter, but looks a request up by its scopes alone before it is
// challenged: when the challenge names a scope the request does not (Gitea's scope="*", on
// every challenge), the streamed request after prime would go out without the token, and
// could not be sent again after its 401.
func aliasToken(ctx context.Context, cache auth.Cache, host, challenge string) {
	scheme, scope := challengeParam(challenge, "scope")
	if scheme != "bearer" || scope == "" {
		return
	}
	scopes := auth.GetAllScopesForHost(ctx, host)
	own := strings.Join(scopes, " ")
	merged := strings.Join(auth.CleanScopes(append(slices.Clone(scopes), strings.Split(scope, " ")...)), " ")
	if merged == own {
		return
	}
	token, err := cache.GetToken(ctx, host, auth.SchemeBearer, merged)
	if err != nil {
		return
	}
	_, _ = cache.Set(ctx, host, auth.SchemeBearer, own, func(context.Context) (string, error) { return token, nil })
}

// session returns prev updated from the registry's answer: its Location (resolved and checked
// by Provider.location, kept as sent), its Docker-Upload-UUID or else the last segment of the
// location, and the offset of its Range, where counted is what Lajstrom knows the session
// holds.
func (r *repository) session(op string, resp *http.Response, prev provider.Upload, counted int64) (provider.Upload, error) {
	u := prev
	if l := resp.Header.Get("Location"); l != "" {
		u.Location = l
	}
	loc, err := r.p.location(u.Location)
	if err != nil {
		return prev, fmt.Errorf("generic: %s: %w", op, err)
	}
	switch id := resp.Header.Get("Docker-Upload-UUID"); {
	case id != "":
		u.ID = id
	case u.ID == "":
		u.ID = path.Base(loc.Path)
	}
	u.Offset = offsetOf(resp.Header.Get("Range"), counted)
	return u, nil
}

// location resolves an upload location against the registry URL. A location on another host,
// or one that downgrades https to http, is refused, as oras-go refuses it: the provider's
// credentials would follow the request. The error does not repeat the location.
func (p *Provider) location(loc string) (*url.URL, error) {
	if loc == "" {
		return nil, errors.New("the registry gave no upload location")
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return nil, errors.New("the upload location does not parse")
	}
	base, err := url.Parse(p.base)
	if err != nil {
		return nil, fmt.Errorf("registry url: %w", err)
	}
	u := base.ResolveReference(ref)
	switch {
	case u.Hostname() != base.Hostname() || port(u) != port(base):
		return nil, errors.New("the upload location is on another host than the registry")
	case base.Scheme == "https" && u.Scheme != "https":
		return nil, errors.New("the upload location downgrades https to http")
	}
	return u, nil
}

// port is the port of u, with the default of its scheme when it has none.
func port(u *url.URL) string {
	switch {
	case u.Port() != "":
		return u.Port()
	case u.Scheme == "https":
		return "443"
	}
	return "80"
}

// contentRange is the Content-Range of size bytes at offset: <first>-<last>, inclusive.
func contentRange(offset, size int64) string {
	return strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(offset+size-1, 10)
}

// offsetOf returns the offset of a session from its Range header, 0-<last> (bytes=0-<last>
// too): last+1. Registries answer an empty session and one of one byte both with 0-0, so 0-0
// is counted when that is 0 or 1; a header that is missing or does not parse is counted.
func offsetOf(rng string, counted int64) int64 {
	first, last, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(rng), "bytes="), "-")
	n, err := strconv.ParseInt(last, 10, 64)
	switch {
	case !ok || first != "0" || err != nil || n < 0:
		return counted
	case n == 0:
		return min(counted, 1)
	}
	return n + 1
}

// withoutURL drops the URL that net/http puts in its errors: an upload URL carries the
// registry's state in its query, and the log does not.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
