package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/version"
)

// maxBody bounds how much of an answer's body is kept: a catalog page, a tag list, an error.
// The rest is read, counted and hashed, never kept.
const maxBody = 1 << 20

// client sends the probe's requests to one registry, without following redirects, and records
// every request and answer into the cell its context names.
type client struct {
	base *url.URL     // scheme://host of the registry
	auth *auth.Client // the registry's credentials, its challenges answered
	anon *http.Client // no credentials: a hop that has left the registry's origin
}

// newClient builds the client of cfg: the registry's Basic or bearer challenge is answered with
// cred, else with registries[].auth, and a bearer challenge whose token realm is on another host
// is refused (realmGuard) before anything is sent there.
func newClient(cfg config.Registry, base *url.URL, cred auth.CredentialFunc) *client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = config.DefaultRegistryTimeout
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: min(timeout, 10*time.Second)}).DialContext
	t.ResponseHeaderTimeout = timeout
	// A blob is measured as the registry keeps it: no gzip offered (Nexus compresses when it is).
	t.DisableCompression = true
	rt := &recordTransport{next: &realmGuard{base: base, next: t}}
	noFollow := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if cred == nil && cfg.Auth.Username != "" {
		cred = auth.StaticCredential(base.Host, auth.Credential{Username: cfg.Auth.Username, Password: cfg.Auth.Password})
	}
	plain := &http.Client{Transport: rt, CheckRedirect: noFollow}
	return &client{
		base: base,
		auth: &auth.Client{
			Client:     plain,
			Header:     http.Header{"User-Agent": {"lajstrom-probe/" + version.Get().Version}},
			Credential: cred,
			Cache:      auth.NewCache(),
		},
		anon: plain,
	}
}

// request is one request of the probe.
type request struct {
	method string
	// target is a path with its query below the registry ("/v2/…"), or an absolute URL: a
	// Location the registry answered.
	target string
	header http.Header
	// body is the request body, or nil; stream, when set, makes a body of size bytes anew for
	// each attempt (the auth client resends after a challenge).
	body   []byte
	stream func() io.Reader
	size   int64
	// anonymous sends the request without credentials: a redirect that left the registry.
	anonymous bool
}

// answer is the registry's answer to a request: its status and headers, the first maxBody
// bytes of its body, and the size and digest of the whole body.
type answer struct {
	status int
	header http.Header
	body   []byte
	size   int64
	digest digest.Digest
	url    *url.URL
}

// do sends req and reads its answer whole. An error is a request that got no answer.
func (c *client) do(ctx context.Context, req request) (answer, error) {
	u, err := c.base.Parse(req.target)
	if err != nil {
		return answer{}, fmt.Errorf("probe: target %q: %w", req.target, err)
	}
	var body io.Reader
	switch {
	case req.stream != nil:
		body = req.stream()
	case req.body != nil:
		body = bytes.NewReader(req.body)
	}
	hr, err := http.NewRequestWithContext(withScopes(ctx, c.base.Host, req.method, u.Path), req.method, u.String(), body)
	if err != nil {
		return answer{}, fmt.Errorf("probe: %s %s: %w", req.method, req.target, err)
	}
	for k, v := range req.header {
		hr.Header[k] = v
	}
	switch {
	case req.stream != nil:
		hr.ContentLength = req.size
		hr.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(req.stream()), nil }
	case req.body != nil:
		b := req.body
		hr.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}
	var resp *http.Response
	if req.anonymous || u.Host != c.base.Host {
		resp, err = c.anon.Do(hr)
	} else {
		resp, err = c.auth.Do(hr)
	}
	if err != nil {
		return answer{}, fmt.Errorf("probe: %s %s: %w", req.method, sanitiseURL(c.base, u), err)
	}
	defer func() { _ = resp.Body.Close() }()
	a := answer{status: resp.StatusCode, header: resp.Header, url: u}
	h := sha256.New()
	var kept bytes.Buffer
	n, err := io.Copy(io.MultiWriter(h, &limitedWriter{w: &kept, n: maxBody}), resp.Body)
	a.body, a.size, a.digest = kept.Bytes(), n, digestOf(h)
	recordBody(ctx, req.method, u, a)
	if err != nil {
		return a, fmt.Errorf("probe: %s %s: read the answer: %w", req.method, sanitiseURL(c.base, u), err)
	}
	return a, nil
}

// withScopes adds the token scopes a request of method to path needs, for a registry with
// token auth: pull for a read, push and pull for a write, delete as well for a delete; the
// catalog's for the catalog.
func withScopes(ctx context.Context, host, method, path string) context.Context {
	if strings.HasPrefix(path, "/v2/_catalog") {
		return auth.AppendScopesForHost(ctx, host, auth.ScopeRegistryCatalog)
	}
	name := repositoryOf(path)
	if name == "" {
		return ctx
	}
	actions := []string{auth.ActionPull}
	switch method {
	case http.MethodGet, http.MethodHead:
	case http.MethodDelete:
		actions = append(actions, auth.ActionPush, auth.ActionDelete)
	default:
		actions = append(actions, auth.ActionPush)
	}
	return auth.AppendRepositoryScope(ctx, registry.Reference{Registry: host, Repository: name}, actions...)
}

// repositoryOf returns the repository name of a path of the distribution API, "" for another
// path. The route is matched at the end of the path and the name is everything before it, so
// that a name with a component the routes use (the row "Routing words in a name") is whole.
func repositoryOf(path string) string {
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return ""
	}
	segs := strings.Split(rest, "/")
	n := len(segs)
	k := 0
	switch {
	case n >= 4 && segs[n-3] == "blobs" && segs[n-2] == "uploads": // blobs/uploads/ and blobs/uploads/<id>
		k = 3
	case n < 3:
	case segs[n-2] == "tags" && segs[n-1] == "list", segs[n-2] == "blobs", segs[n-2] == "manifests", segs[n-2] == "referrers":
		k = 2
	}
	if k == 0 {
		return ""
	}
	return strings.Join(segs[:n-k], "/")
}

// digestOf returns the sha256 digest that h computed.
func digestOf(h hash.Hash) digest.Digest {
	return digest.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

// limitedWriter keeps the first n bytes written to it and drops the rest without an error.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		if _, err := l.w.Write(p[:k]); err != nil {
			return 0, err
		}
		l.n -= k
	}
	return len(p), nil
}

// errorBody is the error document of the distribution API.
type errorBody struct {
	Errors []struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	} `json:"errors"`
}

// errorOf returns the first error of an answer's error document, if it has one.
func (a answer) errorOf() (code, message, detail string) {
	var e errorBody
	if json.Unmarshal(firstJSON(a.body), &e) != nil || len(e.Errors) == 0 {
		return "", "", ""
	}
	first := e.Errors[0]
	var s string
	if json.Unmarshal(first.Detail, &s) == nil {
		detail = s
	}
	return first.Code, first.Message, detail
}

// firstJSON returns the first JSON value of b: distribution writes some error documents
// twice into one body.
func firstJSON(b []byte) []byte {
	d := json.NewDecoder(bytes.NewReader(b))
	var raw json.RawMessage
	if d.Decode(&raw) != nil {
		return b
	}
	return raw
}

// code is the code of the first error of the answer, "" without one.
func (a answer) code() string {
	c, _, _ := a.errorOf()
	return c
}

// refusal describes an answer that refused: "405 `UNSUPPORTED`" with an error code, else by
// its body: "a plain 404" (text), "an HTML 404", "an empty 404", "a 404" (anything else).
func (a answer) refusal() string {
	if c := a.code(); c != "" {
		return fmt.Sprintf("%d `%s`", a.status, c)
	}
	ct := strings.ToLower(a.header.Get("Content-Type"))
	switch {
	case a.size == 0:
		return fmt.Sprintf("an empty %d", a.status)
	case strings.HasPrefix(ct, "text/html"):
		return fmt.Sprintf("an HTML %d", a.status)
	case strings.HasPrefix(ct, "text/plain"):
		return fmt.Sprintf("a plain %d", a.status)
	}
	return fmt.Sprintf("a %d", a.status)
}

// ok reports whether the answer has one of the statuses.
func (a answer) ok(statuses ...int) bool {
	for _, s := range statuses {
		if a.status == s {
			return true
		}
	}
	return false
}

// location resolves the answer's Location against its request URL; nil without one.
func (a answer) location() *url.URL {
	l := a.header.Get("Location")
	if l == "" || a.url == nil {
		return nil
	}
	u, err := a.url.Parse(l)
	if err != nil {
		return nil
	}
	return u
}

// realmGuard refuses a bearer challenge of the registry whose token realm is on another host
// name than the registry's, or plain http for a registry on https: the auth client would send
// the registry's username and password there. The generic provider refuses the same realms
// (threat model P-3); until registries[].tokenRealmHosts exists (#354), a registry with its
// token service elsewhere, Docker Hub or GitLab.com, cannot be probed.
type realmGuard struct {
	base *url.URL
	next http.RoundTripper
}

// errRealm is a bearer challenge that realmGuard refused.
var errRealm = errors.New("the bearer challenge names a token realm on another host or downgrades to http; " +
	"the probe does not send the credentials there (registries[].tokenRealmHosts, #354, is not built yet)")

// RoundTrip implements http.RoundTripper.
func (g *realmGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.next.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !strings.EqualFold(req.URL.Hostname(), g.base.Hostname()) {
		return resp, err
	}
	scheme, params, _ := strings.Cut(resp.Header.Get("WWW-Authenticate"), " ")
	if !strings.EqualFold(scheme, "bearer") {
		return resp, nil
	}
	realm := challengeParam(params, "realm")
	u, perr := url.Parse(realm)
	if perr == nil && u.Host != "" && strings.EqualFold(u.Hostname(), g.base.Hostname()) &&
		(u.Scheme == "https" || (u.Scheme == "http" && g.base.Scheme == "http")) {
		return resp, nil
	}
	_ = resp.Body.Close()
	return nil, errRealm
}

// challengeParam returns the value of the parameter name of the parameters of a
// WWW-Authenticate challenge, quoted or not; "" when it has none.
func challengeParam(params, name string) string {
	for _, p := range splitParams(params) {
		k, v, ok := strings.Cut(p, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// splitParams splits challenge parameters at the commas outside quotes.
func splitParams(s string) []string {
	var out []string
	quoted, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
