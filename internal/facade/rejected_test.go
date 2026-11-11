package facade

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/generic"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// internals is in every refusal of a refusingRegistry, in its message and its challenge: what
// the registry says about itself must not reach the client.
const internals = "registry.internal"

// refusingRegistry is a registry with the upload endpoints of the distribution API and nothing
// else (any other path is 404, which the capability probe takes), whose GET /v2/ answers the
// headers v2. It refuses the requests that refuse picks, which sees the bytes the session holds,
// with status, the headers hdr and, when code is not empty, an error of that code whose message
// names internals and a credential; without a code the body is empty, as a proxy in front of a
// registry answers.
type refusingRegistry struct {
	v2     http.Header
	refuse func(r *http.Request, held int64) bool
	status int
	code   string
	hdr    http.Header

	mu   sync.Mutex
	held map[string]int64 // upload id → bytes
	seq  int
}

func (f *refusingRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base, id, upload := strings.Cut(r.URL.Path, "/blobs/uploads/")
	held, known := f.held[id]
	switch {
	case r.URL.Path == "/v2/":
		for k, v := range f.v2 {
			w.Header()[k] = v
		}
		return
	case !upload:
		http.NotFound(w, r)
		return
	case f.refuse(r, held):
		for k, v := range f.hdr {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		if f.code != "" {
			_, _ = fmt.Fprintf(w, `{"errors":[{"code":%q,"message":"refused by %s for robot:s3cret"}]}`, f.code, internals)
		}
		return
	case r.Method == http.MethodPost:
		f.seq++
		id = strconv.Itoa(f.seq)
	case !known:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errors":[{"code":"BLOB_UPLOAD_UNKNOWN","message":"unknown"}]}`)
		return
	}
	n, _ := io.Copy(io.Discard, r.Body)
	f.held[id] = held + n
	status := map[string]int{http.MethodPost: http.StatusAccepted, http.MethodPatch: http.StatusAccepted,
		http.MethodGet: http.StatusNoContent, http.MethodPut: http.StatusCreated, http.MethodDelete: http.StatusNoContent}[r.Method]
	if r.Method == http.MethodPut || r.Method == http.MethodDelete {
		delete(f.held, id)
	}
	w.Header().Set("Location", base+"/blobs/uploads/"+id)
	w.Header().Set("Range", rangeOf(f.held[id]))
	w.WriteHeader(status)
}

// refusingEnv returns an env whose registry "local" is the generic provider, without retries,
// of f, and f's server.
func refusingEnv(t *testing.T, f *refusingRegistry) (*env, provider.Provider, *httptest.Server) {
	t.Helper()
	f.held = map[string]int64{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p, err := generic.New(config.Registry{Name: "local", Type: generic.Type, URL: srv.URL, Retries: -1}, slog.New(slog.DiscardHandler))
	must(t, err)
	return newEnv(t, envOptions{registry: func(*providertest.Fake) provider.Provider { return p }}), p, srv
}

// wantRefusal fails the test when rr is not status with code, or names the registry's
// internals, its URL or a credential; when named, its message names the registry by its
// configured name, and the status.
func wantRefusal(t *testing.T, what string, rr *httptest.ResponseRecorder, srv *httptest.Server, status int, code string, named bool) string {
	t.Helper()
	msg := wantError(t, what, rr, status, code)
	answer := fmt.Sprint(rr.Header(), rr.Body)
	for _, leak := range []string{internals, "s3cret", strings.TrimPrefix(srv.URL, "http://"), "realm"} {
		if strings.Contains(answer, leak) {
			t.Errorf("%s: the answer names %q: %s", what, leak, answer)
		}
	}
	if named && !strings.HasPrefix(msg, fmt.Sprintf("registry local refused the request: %d", status)) {
		t.Errorf("%s: message %q does not name the registry and its status", what, msg)
	}
	return msg
}

// TestRegistryRefusals is #449: a registry that refuses a request of a blob upload with a 4xx
// gets the client a 4xx, with the registry's code where the spec has it for a blob upload, and
// nothing else of the registry's; a busy registry's 429 or 503 is 503 UNAVAILABLE with its
// Retry-After, 1 s when it sent none; another 5xx stays a 5xx, and a refusal of Lajstrom's
// credentials is 502. The client opens a session, sends a chunk, asks its status
// and closes it, or cancels it; or sends the blob in one POST. A session ends when the registry
// no longer knows it, or refused its digest; otherwise it stays.
func TestRegistryRefusals(t *testing.T) {
	ra := func(s string) http.Header { return http.Header{"Retry-After": {s}} }
	tests := []struct {
		name       string
		refuse     string // the method the registry refuses
		monolithic bool   // the client sends the blob in one POST ?digest=
		status     int
		code       string
		hdr        http.Header
		want       int
		wantCode   string
		retryAfter string
		ends       bool
	}{
		{name: "streamed chunk refused, as Gitea does", refuse: http.MethodPatch, status: 400, code: codeBlobUploadInvalid, want: 400, wantCode: codeBlobUploadInvalid},
		{name: "chunk too large", refuse: http.MethodPatch, status: 400, code: codeSizeInvalid, want: 400, wantCode: codeSizeInvalid},
		{name: "a proxy's 413 without a body", refuse: http.MethodPatch, status: 413, want: 413, wantCode: codeSizeInvalid},
		{name: "range, distribution's code", refuse: http.MethodPatch, status: 416, code: "RANGE_INVALID", want: 416, wantCode: codeBlobUploadInvalid},
		{name: "a code of the vendor's own", refuse: http.MethodPatch, status: 400, code: "PROJECTPOLICYVIOLATION", want: 400, wantCode: codeBlobUploadInvalid},
		{name: "chunk throttled", refuse: http.MethodPatch, status: 429, code: codeTooManyRequests, hdr: ra("7"), want: 503, wantCode: codeUnavailable, retryAfter: "7"},
		{name: "registry busy", refuse: http.MethodPatch, status: 503, code: codeUnavailable, hdr: ra("3"), want: 503, wantCode: codeUnavailable, retryAfter: "3"},
		{name: "registry busy, no Retry-After", refuse: http.MethodPatch, status: 503, want: 503, wantCode: codeUnavailable, retryAfter: "1"},
		{name: "session gone", refuse: http.MethodPatch, status: 404, code: codeBlobUploadUnknown, want: 404, wantCode: codeBlobUploadUnknown, ends: true},
		{name: "session gone, with a 400", refuse: http.MethodPatch, status: 400, code: codeBlobUploadUnknown, want: 404, wantCode: codeBlobUploadUnknown, ends: true},
		{
			name: "Lajstrom's credentials refused", refuse: http.MethodPatch, status: 401, code: codeUnauthorized,
			hdr: http.Header{"Www-Authenticate": {`Basic realm="` + internals + `"`}}, want: 502, wantCode: codeUnknown,
		},
		{name: "Lajstrom's credentials denied", refuse: http.MethodPatch, status: 403, code: codeDenied, want: 502, wantCode: codeUnknown},
		{name: "registry fails", refuse: http.MethodPatch, status: 500, code: codeUnknown, want: 500, wantCode: codeUnknown},
		{name: "name refused", refuse: http.MethodPost, status: 400, code: codeNameInvalid, want: 400, wantCode: codeNameInvalid},
		{name: "start throttled", refuse: http.MethodPost, status: 429, code: codeTooManyRequests, hdr: ra("30"), want: 503, wantCode: codeUnavailable, retryAfter: "30"},
		{name: "status throttled, no Retry-After", refuse: http.MethodGet, status: 429, want: 503, wantCode: codeUnavailable, retryAfter: "1"},
		{name: "close refused", refuse: http.MethodPut, status: 400, code: codeSizeInvalid, want: 400, wantCode: codeSizeInvalid},
		{name: "digest refused", refuse: http.MethodPut, status: 400, code: codeDigestInvalid, want: 400, wantCode: codeDigestInvalid, ends: true},
		{name: "cancel refused", refuse: http.MethodDelete, status: 400, code: codeBlobUploadInvalid, want: 400, wantCode: codeBlobUploadInvalid},
		{name: "one POST too large", refuse: http.MethodPut, monolithic: true, status: 413, want: 413, wantCode: codeSizeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &refusingRegistry{status: tt.status, code: tt.code, hdr: tt.hdr,
				refuse: func(r *http.Request, _ int64) bool { return r.Method == tt.refuse }}
			e, _, srv := refusingEnv(t, f)
			named := !tt.ends && tt.want < http.StatusInternalServerError
			d := digest.FromString("abc")
			if tt.monolithic {
				rr := e.do(e.h, http.MethodPost, uploads+"?digest="+d.String(), strings.NewReader("abc"))
				wantRefusal(t, "POST ?digest=", rr, srv, tt.want, tt.wantCode, named)
				return
			}
			last := http.MethodPut
			if tt.refuse == http.MethodDelete {
				last = http.MethodDelete
			}
			var (
				rr *httptest.ResponseRecorder
				id string
			)
			for _, step := range []string{http.MethodPost, http.MethodPatch, http.MethodGet, last} {
				switch step {
				case http.MethodPost:
					rr = e.do(e.h, step, uploads, nil)
				case http.MethodPatch:
					rr = e.do(e.h, step, uploads+id, strings.NewReader("abc"), "Content-Range", "0-2")
				case http.MethodPut:
					rr = e.do(e.h, step, uploads+id+"?digest="+d.String(), nil)
				default:
					rr = e.do(e.h, step, uploads+id, nil)
				}
				if step == tt.refuse {
					break
				}
				if rr.Code >= 300 {
					t.Fatalf("%s: status %d before the refusal; body %s", step, rr.Code, rr.Body)
				}
				if step == http.MethodPost {
					id = uploadID(t, rr)
				}
			}
			wantRefusal(t, tt.refuse, rr, srv, tt.want, tt.wantCode, named)
			wantHeader(t, tt.refuse, rr, "Retry-After", tt.retryAfter)
			if tt.refuse == http.MethodPost || tt.refuse == http.MethodGet {
				return
			}
			after, status := e.do(e.h, http.MethodGet, uploads+id, nil), http.StatusNoContent
			if tt.ends {
				status = http.StatusNotFound
			}
			wantStatus(t, "the session after the refusal", after, status)
		})
	}
}

// TestRegistryRefusalHint: a registry that the vendor table knows for refusing a chunk of unknown
// length after the first write (JFrog, told by its header, with its 416) refuses one while
// registries[].uploads.spoolUnknownLength is off: the client's message names the key.
func TestRegistryRefusalHint(t *testing.T) {
	f := &refusingRegistry{v2: http.Header{"X-Jfrog-Version": {"Artifactory/7.161.26 86126900"}},
		status: http.StatusRequestedRangeNotSatisfiable, code: codeBlobUploadInvalid,
		refuse: func(r *http.Request, held int64) bool {
			return r.Method == http.MethodPatch && r.Header.Get("Content-Range") == "" && held > 0
		}}
	e, p, srv := refusingEnv(t, f)
	if _, err := p.Capabilities(t.Context()); err != nil { // as serve probes at start
		t.Fatal(err)
	}
	id := e.start(e.h)
	rr := e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("abc"), "Content-Range", "0-2")
	wantStatus(t, "the first chunk", rr, http.StatusAccepted)
	rr = e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("de"), "Content-Length", "-1")
	msg := wantRefusal(t, "a chunk of unknown length", rr, srv, http.StatusRequestedRangeNotSatisfiable, codeBlobUploadInvalid, true)
	if !strings.Contains(msg, "hint:") || !strings.Contains(msg, "registries[].uploads.spoolUnknownLength") {
		t.Errorf("message %q does not hint at registries[].uploads.spoolUnknownLength", msg)
	}
}
