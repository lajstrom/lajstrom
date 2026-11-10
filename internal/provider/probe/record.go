package probe

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Exchange is one request of the probe and the registry's answer, as the spike pages show
// them: the method and URL, the request headers that matter, the status, the answer's headers
// that matter and its body, trimmed. Credentials, tokens and the query values that carry
// state or signatures are never recorded.
type Exchange struct {
	Method  string `json:"method"`
	URL     string `json:"url"`
	Request string `json:"request,omitempty"`
	Status  int    `json:"status,omitempty"`
	Answer  string `json:"answer,omitempty"`
	// Via is "provider" for a request of the generic provider, which logs method, path and
	// status only.
	Via   string `json:"via,omitempty"`
	Error string `json:"error,omitempty"`
}

// maxSnippet bounds the body of an answer as an exchange shows it.
const maxSnippet = 200

// tape records the exchanges of a run into its current cell. The context of every request of
// the run carries it.
type tape struct {
	mu   sync.Mutex
	base *url.URL
	cell *Cell
	last *Exchange
	// tooMany counts the answers 429 of the run.
	tooMany int
}

type tapeKey struct{}

// tapeOf returns the tape of ctx, nil without one.
func tapeOf(ctx context.Context) *tape {
	t, _ := ctx.Value(tapeKey{}).(*tape)
	return t
}

// add appends an exchange to the current cell.
func (t *tape) add(e *Exchange) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e.Status == http.StatusTooManyRequests {
		t.tooMany++
	}
	t.last = e
	if t.cell != nil {
		t.cell.Exchanges = append(t.cell.Exchanges, e)
	}
}

// recordTransport records every round trip of a request whose context carries a tape: the
// requests of the probe, and the token requests the auth client makes for them (their bodies
// are never read here).
type recordTransport struct{ next http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (rt *recordTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.next.RoundTrip(req)
	t := tapeOf(req.Context())
	if t == nil {
		return resp, err
	}
	e := &Exchange{Method: req.Method, URL: sanitiseURL(t.base, req.URL), Request: requestHeaders(req)}
	if err != nil {
		e.Error = err.Error()
	} else {
		e.Status = resp.StatusCode
		e.Answer = answerHeaders(t.base, req, resp)
	}
	t.add(e)
	return resp, err
}

// recordBody adds the body of the answer a to the last exchange, the request that got it: the
// number of bytes for a blob, the body trimmed for anything else.
func recordBody(ctx context.Context, method string, u *url.URL, a answer) {
	t := tapeOf(ctx)
	if t == nil || method == http.MethodHead || a.size == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil || t.last.Method != method || t.last.URL != sanitiseURL(t.base, u) {
		return
	}
	var s string
	if strings.Contains(u.Path, "/blobs/") && (a.status == http.StatusOK || a.status == http.StatusPartialContent) {
		s = fmt.Sprintf("<%d bytes>", a.size)
	} else {
		s = snippet(a.body, a.size)
	}
	t.last.Answer = strings.TrimSpace(t.last.Answer + "  " + s)
}

// snippet is a body on one line, its whitespace collapsed, trimmed to maxSnippet bytes.
func snippet(b []byte, size int64) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > maxSnippet || int64(len(b)) < size {
		s = strings.ToValidUTF8(s[:min(len(s), maxSnippet)], "") + "…"
	}
	return s
}

// shownRequestHeaders and shownAnswerHeaders are the headers an exchange shows.
var (
	shownRequestHeaders = []string{"Content-Type", "Content-Range", "Range"}
	shownAnswerHeaders  = []string{
		"Location", "Range", "Content-Range", "Docker-Content-Digest", "OCI-Subject",
		"OCI-Filters-Applied", "Link", "Accept-Ranges", "WWW-Authenticate", "Retry-After",
	}
)

// requestHeaders describes the headers of req that an exchange shows, and its body's size.
func requestHeaders(req *http.Request) string {
	var parts []string
	for _, k := range shownRequestHeaders {
		if v := req.Header.Get(k); v != "" {
			parts = append(parts, k+": "+v)
		}
	}
	if req.ContentLength > 0 {
		parts = append(parts, fmt.Sprintf("%d bytes", req.ContentLength))
	}
	return strings.Join(parts, ", ")
}

// answerHeaders describes the headers of resp that an exchange shows; a Location is sanitised.
func answerHeaders(base *url.URL, req *http.Request, resp *http.Response) string {
	var parts []string
	for _, k := range shownAnswerHeaders {
		v := resp.Header.Get(k)
		if v == "" {
			continue
		}
		if k == "Location" {
			if u, err := req.URL.Parse(v); err == nil {
				v = sanitiseURL(base, u)
			}
		}
		parts = append(parts, k+": "+v)
	}
	if req.Method == http.MethodHead && resp.ContentLength >= 0 {
		parts = append(parts, fmt.Sprintf("Content-Length: %d", resp.ContentLength))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && resp.StatusCode >= 400 {
		parts = append(parts, "Content-Type: "+ct)
	}
	return strings.Join(parts, ", ")
}

// shownQuery are the query parameters whose values an exchange shows; any other value, such as
// distribution's _state or a presigned URL's signature and credential, is replaced by "…".
var shownQuery = []string{"n", "last", "digest", "mount", "from", "artifactType", "tag", "scope", "service", "X-Amz-Expires"}

// sanitiseURL returns u as an exchange shows it: its path and query on the registry's origin,
// the whole URL elsewhere, and the values of the query hidden but those of shownQuery.
func sanitiseURL(base, u *url.URL) string {
	var b strings.Builder
	if u.Scheme != base.Scheme || u.Host != base.Host {
		b.WriteString(u.Scheme + "://" + u.Host)
	}
	b.WriteString(u.EscapedPath())
	q := u.Query()
	if len(q) == 0 {
		return b.String()
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for i, k := range keys {
		sep := "&"
		if i == 0 {
			sep = "?"
		}
		v := "…"
		if slices.Contains(shownQuery, k) {
			v = strings.Join(q[k], ",")
		}
		b.WriteString(sep + k + "=" + v)
	}
	return b.String()
}

// longDigest matches a sha256 digest, which an exchange shows by its first 8 hex digits.
var longDigest = regexp.MustCompile(`(sha256[:-])([0-9a-f]{8})[0-9a-f]{56}`)

// shorten abbreviates every sha256 digest of s.
func shorten(s string) string {
	return longDigest.ReplaceAllString(s, "$1$2…")
}

// line renders an exchange on one line, digests shortened.
func (e *Exchange) line() string {
	req := e.Method + " " + e.URL
	if e.Request != "" {
		req += " (" + e.Request + ")"
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%-72s", shorten(req))
	switch {
	case e.Error != "":
		b.WriteString("  error: " + e.Error)
	default:
		fmt.Fprintf(&b, "  %d", e.Status)
		if e.Answer != "" {
			b.WriteString("  " + shorten(e.Answer))
		}
	}
	if e.Via != "" {
		b.WriteString("  (" + e.Via + ")")
	}
	return strings.TrimRight(b.String(), " ")
}
