package format

import (
	"cmp"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// ServeBlobOptions says how ServeBlob answers a blob.
type ServeBlobOptions struct {
	// ContentType is the Content-Type of the blob; required.
	ContentType string
	// ETag is the entity tag, quoted; "" means the blob digest, quoted.
	ETag string
	// LastModified is Last-Modified, for If-Modified-Since, If-Unmodified-Since and If-Range;
	// zero means none.
	LastModified time.Time
	// ManifestDigest is the Docker-Content-Digest when it is not the blob's own (raw: the
	// manifest of the version); "" means the blob's.
	ManifestDigest digest.Digest
	// Redirect lets the provider answer with a redirect to the backend's storage (307); false
	// streams even where the provider could redirect (policy.blobRedirect: never, which
	// BlobRedirect reads).
	Redirect bool
	// Filename is sent as Content-Disposition: attachment with this file name when it is set;
	// "" means no Content-Disposition.
	Filename string
	// Fail answers an error met before the response started; nil means WriteError. The facade
	// answers in the error format of the distribution spec, a frontend through its own fail,
	// which logs the cause of a 5xx: ServeBlob takes no logger.
	Fail func(w http.ResponseWriter, err error)
}

// ServeBlob answers GET and HEAD of the blob d of the OCI repository name (as the provider
// names it) with one request to the backend at most, the one read path of blob bytes for the
// facade and every frontend (docs/spec/http-routing.md §2 and §3, docs/adr/0009):
//
//   - preconditions first, without a request: If-Match without the ETag, or
//     If-Unmodified-Since before LastModified, is 412; If-None-Match with the ETag, or
//     If-Modified-Since not before LastModified when there is no If-None-Match, is 304;
//   - HEAD is prov.StatBlob: 200 with the headers below and Content-Length, no body;
//   - any other method is a GET: prov.OpenBlob with opts.Redirect and the client's Range when
//     it is one byte range and If-Range, if any, matches. Another Range is not sent and the
//     answer is the whole blob with 200, which HTTP allows: a backend answers several ranges
//     with a multipart/byteranges body, and a provider.Blob does not carry its Content-Type.
//     A redirect is 307 with Location and Docker-Content-Digest. A body is answered with the
//     backend's status (200, 206 or 416), Content-Length, Content-Range and Accept-Ranges,
//     and Content-Type, ETag, Last-Modified, Docker-Content-Digest and Content-Disposition
//     from opts, and streamed. A whole blob (200) is hashed as it streams, its last byte held
//     back until the hash is known.
//
// An error before the response started, the provider's among them (provider.ErrNotFound is
// 404), is answered through opts.Fail. The error ServeBlob returns is a stream that broke
// after the headers went out, which the caller logs: the client got a short body. A stream
// that stops because the client went away (the request's context is done) is not an error.
// A whole blob whose bytes do not have the digest d is not returned either: ServeBlob aborts
// the response with a panic that wraps http.ErrAbortHandler, which the caller lets through to
// net/http (docs/spec/guarantees.md, INV-40).
func ServeBlob(w http.ResponseWriter, r *http.Request, prov provider.Provider, name string, d digest.Digest, opts ServeBlobOptions) error {
	a := &blobAnswer{w: w, r: r, prov: prov, name: name, d: d, opts: opts, fail: opts.Fail}
	if a.fail == nil {
		a.fail = func(w http.ResponseWriter, err error) { WriteError(w, err) }
	}
	if opts.ContentType == "" || prov == nil {
		a.fail(w, fmt.Errorf("format: serve blob %s@%s: no content type or no provider", name, d))
		return nil
	}
	a.v = validators{etag: opts.ETag, modified: opts.LastModified.Truncate(time.Second)}
	if a.v.etag == "" {
		a.v.etag = `"` + d.String() + `"`
	}
	if status := a.v.precondition(r); status != 0 {
		w.Header().Set("ETag", a.v.etag)
		w.WriteHeader(status)
		return nil
	}
	if r.Method == http.MethodHead {
		a.head()
		return nil
	}
	return a.get()
}

// blobAnswer is one ServeBlob call.
type blobAnswer struct {
	w    http.ResponseWriter
	r    *http.Request
	prov provider.Provider
	name string
	d    digest.Digest
	opts ServeBlobOptions
	fail func(w http.ResponseWriter, err error)
	v    validators
}

// head answers HEAD with the headers of StatBlob.
func (a *blobAnswer) head() {
	b, err := a.prov.StatBlob(a.r.Context(), a.name, a.d)
	if err != nil {
		a.fail(a.w, err)
		return
	}
	setBlobHeaders(a.w.Header(), b, a.opts, a.v)
	a.w.WriteHeader(http.StatusOK)
}

// get answers GET with one OpenBlob: the redirect, or the backend's answer streamed.
func (a *blobAnswer) get() error {
	rng := ""
	if a.v.rangeAllowed(a.r) {
		rng = singleByteRange(a.r.Header.Get("Range"))
	}
	b, err := a.prov.OpenBlob(a.r.Context(), a.name, a.d, rng, a.opts.Redirect)
	if err != nil {
		a.fail(a.w, err)
		return nil
	}
	if b.Body != nil {
		defer func() { _ = b.Body.Close() }()
	}
	hdr := a.w.Header()
	switch {
	case b.RedirectURL != "":
		// The URL is the backend's answer for a blob the caller named, never a value of the
		// request, and absolute (the provider resolves it): it goes out as it is.
		hdr.Set("Location", b.RedirectURL)
		hdr.Set("Docker-Content-Digest", contentDigest(b, a.opts))
		a.w.WriteHeader(http.StatusTemporaryRedirect)
		return nil
	case b.Body == nil && b.Status != http.StatusRequestedRangeNotSatisfiable:
		a.fail(a.w, fmt.Errorf("format: serve blob %s@%s: the provider answered %d without a body", a.name, a.d, b.Status))
		return nil
	}
	setBlobHeaders(hdr, b, a.opts, a.v)
	a.w.WriteHeader(cmp.Or(b.Status, http.StatusOK))
	if b.Body == nil {
		return nil
	}
	return a.copy(b)
}

// copy streams the body of b to the client. A short body is an error too, unless the client
// went away. A whole blob (200) is hashed as it streams, and its last byte is held back until
// the body has ended: it goes out when the bytes have the digest d, and otherwise the response
// is aborted, so that no client gets all of a blob whose bytes are not its digest
// (docs/spec/guarantees.md, INV-40). A range (206) is not the whole blob and is not hashed.
func (a *blobAnswer) copy(b provider.Blob) error {
	var dst io.Writer = a.w
	var dw *digestWriter
	if cmp.Or(b.Status, http.StatusOK) == http.StatusOK && a.d.Validate() == nil {
		dw = &digestWriter{w: a.w, h: a.d.Algorithm().Hash()}
		dst = dw
	}
	n, err := io.Copy(dst, b.Body)
	if err == nil && b.Size >= 0 && n < b.Size {
		err = io.ErrUnexpectedEOF
	}
	if err == nil && dw != nil {
		if digest.NewDigest(a.d.Algorithm(), dw.h) != a.d {
			a.abort(n)
		}
		err = dw.release()
	}
	if err != nil && a.r.Context().Err() == nil {
		return fmt.Errorf("format: blob %s@%s: the stream failed after %d bytes: %w", a.name, a.d, n, err)
	}
	return nil
}

// abort ends the answer of a blob whose n bytes from the registry do not have its digest:
// counted in lajstrom_download_digest_mismatches_total, then a panic of an error that wraps
// http.ErrAbortHandler, which net/http answers by closing the connection (HTTP/1: without the
// rest of the body, or the last chunk) or resetting the stream (HTTP/2), never by ending the
// body. The server's recovery logs the error.
func (a *blobAnswer) abort(n int64) {
	metrics.DownloadDigestMismatches.WithLabelValues(a.prov.Name()).Inc()
	panic(fmt.Errorf("format: blob %s@%s: the %d bytes from registry %s do not have the digest, the response is aborted: %w",
		a.name, a.d, n, a.prov.Name(), http.ErrAbortHandler))
}

// digestWriter passes the bytes of a blob on to w and hashes them, and holds the last byte of
// each write back until the next write or release: the last byte of a blob goes out only once
// its hash is known. Nothing is buffered but that byte.
type digestWriter struct {
	w       io.Writer
	h       hash.Hash
	held    [1]byte
	holding bool
}

func (dw *digestWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	_, _ = dw.h.Write(p) // a hash.Hash never fails
	if dw.holding {
		if _, err := dw.w.Write(dw.held[:]); err != nil {
			return 0, err
		}
	}
	last := len(p) - 1
	if last > 0 {
		if _, err := dw.w.Write(p[:last]); err != nil {
			return 0, err
		}
	}
	dw.held[0], dw.holding = p[last], true
	return len(p), nil
}

// release writes the byte held back, if there is one.
func (dw *digestWriter) release() error {
	if !dw.holding {
		return nil
	}
	dw.holding = false
	_, err := dw.w.Write(dw.held[:])
	return err
}

// setBlobHeaders sets the headers of a blob answer, a HEAD's or a GET's (200, 206, 416):
// Content-Type (not on a 416), ETag, Last-Modified, Docker-Content-Digest,
// Content-Disposition, Accept-Ranges, Content-Range and Content-Length.
func setBlobHeaders(hdr http.Header, b provider.Blob, opts ServeBlobOptions, v validators) {
	if b.Status != http.StatusRequestedRangeNotSatisfiable {
		hdr.Set("Content-Type", opts.ContentType)
	}
	hdr.Set("ETag", v.etag)
	if !v.modified.IsZero() {
		hdr.Set("Last-Modified", v.modified.UTC().Format(http.TimeFormat))
	}
	hdr.Set("Docker-Content-Digest", contentDigest(b, opts))
	if opts.Filename != "" {
		if cd := mime.FormatMediaType("attachment", map[string]string{"filename": opts.Filename}); cd != "" {
			hdr.Set("Content-Disposition", cd)
		}
	}
	if b.AcceptRanges || b.Status == http.StatusPartialContent {
		hdr.Set("Accept-Ranges", "bytes")
	}
	if b.ContentRange != "" {
		hdr.Set("Content-Range", b.ContentRange)
	}
	if b.Size >= 0 && b.Status != http.StatusRequestedRangeNotSatisfiable {
		hdr.Set("Content-Length", strconv.FormatInt(b.Size, 10))
	}
}

// contentDigest is the Docker-Content-Digest of an answer: opts.ManifestDigest, or the blob's.
func contentDigest(b provider.Blob, opts ServeBlobOptions) string {
	if opts.ManifestDigest != "" {
		return opts.ManifestDigest.String()
	}
	return b.Digest.String()
}

// singleByteRange returns the Range header h as "bytes=<range>" when it is one byte range
// (first-last, first-, -suffix, with first not after last), and "" for anything else.
func singleByteRange(h string) string {
	unit, spec, ok := strings.Cut(strings.TrimSpace(h), "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return ""
	}
	spec = strings.TrimSpace(spec)
	first, last, ok := strings.Cut(spec, "-")
	if !ok || (first == "" && last == "") {
		return ""
	}
	a, errA := parseOffset(first)
	b, errB := parseOffset(last)
	if errA != nil || errB != nil || (first != "" && last != "" && a > b) {
		return ""
	}
	return "bytes=" + spec
}

// parseOffset parses a byte position of a range, digits only; "" is -1.
func parseOffset(s string) (int64, error) {
	if s == "" {
		return -1, nil
	}
	if strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("not a byte position: %q", s)
	}
	return strconv.ParseInt(s, 10, 64)
}

// validators are the ETag and Last-Modified of a blob, for the conditional requests of RFC
// 9110 §13.
type validators struct {
	etag     string
	modified time.Time // truncated to the second; zero: none
}

// precondition returns 412 or 304 when the conditional headers of r say so, else 0, in the
// order of RFC 9110 §13.2.2.
func (v validators) precondition(r *http.Request) int {
	h := r.Header
	if im := h.Get("If-Match"); im != "" {
		if !etagListHas(im, v.etag, false) {
			return http.StatusPreconditionFailed
		}
	} else if t, ok := v.date(h.Get("If-Unmodified-Since")); ok && v.modified.After(t) {
		return http.StatusPreconditionFailed
	}
	if inm := h.Get("If-None-Match"); inm != "" {
		if etagListHas(inm, v.etag, true) {
			return http.StatusNotModified
		}
	} else if t, ok := v.date(h.Get("If-Modified-Since")); ok && !v.modified.After(t) {
		return http.StatusNotModified
	}
	return 0
}

// rangeAllowed reports whether the Range of r may be sent: there is no If-Range, or it is
// the ETag (strong comparison) or the Last-Modified date.
func (v validators) rangeAllowed(r *http.Request) bool {
	ir := strings.TrimSpace(r.Header.Get("If-Range"))
	switch {
	case ir == "":
		return true
	case strings.HasPrefix(ir, `"`) || strings.HasPrefix(ir, "W/"):
		return etagListHas(ir, v.etag, false)
	}
	t, ok := v.date(ir)
	return ok && v.modified.Equal(t)
}

// date parses an HTTP date when there is a Last-Modified to compare it with.
func (v validators) date(s string) (time.Time, bool) {
	if s == "" || v.modified.IsZero() {
		return time.Time{}, false
	}
	t, err := http.ParseTime(s)
	return t, err == nil
}

// etagListHas reports whether the list of entity tags of an If-Match, If-None-Match or
// If-Range header has etag, or is "*": with weak, by their opaque tags (W/"x" matches "x");
// otherwise only two strong tags that are the same match.
func etagListHas(list, etag string, weak bool) bool {
	if strings.TrimSpace(list) == "*" {
		return true
	}
	for rest := list; ; {
		rest = strings.TrimLeft(rest, " \t,")
		if rest == "" {
			return false
		}
		tagWeak := strings.HasPrefix(rest, "W/")
		rest = strings.TrimPrefix(rest, "W/")
		end := strings.IndexByte(rest[min(1, len(rest)):], '"')
		if !strings.HasPrefix(rest, `"`) || end < 0 {
			return false // not an entity tag: the rest cannot be read
		}
		tag := rest[:end+2]
		rest = rest[end+2:]
		etagWeak := strings.HasPrefix(etag, "W/")
		switch {
		case weak && tag == strings.TrimPrefix(etag, "W/"):
			return true
		case !weak && !tagWeak && !etagWeak && tag == etag:
			return true
		}
	}
}

// BlobRedirect reads policy.blobRedirect of the repo for ServeBlobOptions.Redirect: the string
// "never" is false, every blob of the repo streams through Lajstrom (for clients that cannot
// follow a redirect to another host); "auto", no such key and anything else are true, a blob
// is redirected where the backend gives a URL (docs/api.md). The policy is read leniently: a
// policy that does not parse is no policy, since api checks no policy key yet.
func BlobRedirect(repo index.Repo) bool {
	if len(repo.Policy) == 0 {
		return true
	}
	var p struct {
		BlobRedirect json.RawMessage `json:"blobRedirect"`
	}
	if json.Unmarshal(repo.Policy, &p) != nil {
		return true
	}
	var value string
	return json.Unmarshal(p.BlobRedirect, &value) != nil || value != "never"
}
