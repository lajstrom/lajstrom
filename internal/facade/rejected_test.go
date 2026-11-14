package facade

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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
// headers v2. It refuses the requests on any other path that refuse picks, which sees the bytes
// an upload session holds, with status, the headers hdr and, when code is not empty, an error
// of that code whose message names internals and a credential; without a code the body is
// empty, as a proxy in front of a registry answers.
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
	case !upload:
		http.NotFound(w, r)
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
// of f, with the uploads settings up when given, and f's server.
func refusingEnv(t *testing.T, f *refusingRegistry, up ...config.RegistryUploads) (*env, provider.Provider, *httptest.Server) {
	t.Helper()
	f.held = map[string]int64{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := config.Registry{Name: "local", Type: generic.Type, URL: srv.URL, Retries: -1}
	if len(up) > 0 {
		cfg.Uploads = up[0]
	}
	p, err := generic.New(cfg, slog.New(slog.DiscardHandler))
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

// TestUploadCancelRefused is #1044: a registry that refuses to cancel an upload session with 405,
// as JFrog (no body) and GHCR (UNSUPPORTED) do, expires it: the client's cancel is 204 and the
// session is gone. Any other failure of the cancel keeps the session, and its message is the
// facade's, never the provider's error, which names the registry's session.
func TestUploadCancelRefused(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		code     string
		want     int
		wantCode string
	}{
		{name: "405 without a body, as JFrog", status: http.StatusMethodNotAllowed, want: http.StatusNoContent},
		{name: "405 UNSUPPORTED, as GHCR", status: http.StatusMethodNotAllowed, code: codeUnsupported, want: http.StatusNoContent},
		{name: "a conflict", status: http.StatusConflict, want: http.StatusConflict, wantCode: codeBlobUploadInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deletes := 0
			f := &refusingRegistry{status: tt.status, code: tt.code, refuse: func(r *http.Request, _ int64) bool {
				if r.Method == http.MethodDelete {
					deletes++
					return true
				}
				return false
			}}
			e, _, _ := refusingEnv(t, f)
			id := e.start(e.h)
			wantStatus(t, "chunk", e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("abc"), "Content-Range", "0-2"), http.StatusAccepted)
			rr := e.do(e.h, http.MethodDelete, uploads+id, nil)
			if deletes != 1 {
				t.Errorf("the registry got %d DELETEs of its session, want 1", deletes)
			}
			after := e.do(e.h, http.MethodGet, uploads+id, nil)
			if tt.want == http.StatusNoContent {
				wantStatus(t, "DELETE", rr, http.StatusNoContent)
				if rr.Body.Len() != 0 {
					t.Errorf("DELETE: body %q, want none", rr.Body)
				}
				wantError(t, "GET after the cancel", after, http.StatusNotFound, codeBlobUploadUnknown)
				return
			}
			if msg := wantError(t, "DELETE", rr, tt.want, tt.wantCode); msg != "the registry did not cancel the upload" {
				t.Errorf("DELETE: message %q, want the facade's own", msg)
			}
			wantStatus(t, "GET after the failed cancel", after, http.StatusNoContent)
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

// TestUploadSplitChunk is #697: on a registry that takes at most 4 MiB in a PATCH (GHCR, here by
// registries[].uploads.chunkSize), a client's chunk of 10 MiB reaches it as three PATCHes, each
// with its Content-Range, and the client sees one chunk: one 202 with Range 0-10485759. The next
// chunk starts where that one ended, a chunk of unknown length after the first write is refused
// with a hint before anything is sent, the session stays, and the close with a last chunk above
// the bound takes the blob. A first write of unknown length streams as it came. Without the
// bound, the chunk goes as it came, and the registry's 416 reaches the client, as before.
func TestUploadSplitChunk(t *testing.T) {
	const limit, mib = 4 << 20, 1 << 20
	newRegistry := func(patches *[]string) *refusingRegistry {
		return &refusingRegistry{status: http.StatusRequestedRangeNotSatisfiable, code: codeBlobUploadInvalid,
			refuse: func(r *http.Request, _ int64) bool {
				if r.Method != http.MethodPatch {
					return false
				}
				*patches = append(*patches, fmt.Sprintf("%q %d", r.Header.Get("Content-Range"), r.ContentLength))
				return r.ContentLength > limit
			}}
	}
	sent := func(f *refusingRegistry, patches *[]string) []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		got := *patches
		*patches = nil
		return got
	}
	wantSent := func(t *testing.T, what string, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s: the registry got PATCHes %q, want %q", what, got, want)
		}
	}
	blob := bytes.Repeat([]byte("0123456789abcdef"), 16*mib/16)

	t.Run("bound", func(t *testing.T) {
		var patches []string
		f := newRegistry(&patches)
		e, _, srv := refusingEnv(t, f, config.RegistryUploads{ChunkSize: limit})
		id := e.start(e.h)
		rr := e.do(e.h, http.MethodPatch, uploads+id, bytes.NewReader(blob[:10*mib]))
		wantStatus(t, "a chunk of 10 MiB", rr, http.StatusAccepted)
		wantHeader(t, "a chunk of 10 MiB", rr, "Range", "0-10485759")
		wantSent(t, "a chunk of 10 MiB", sent(f, &patches),
			`"0-4194303" 4194304`, `"4194304-8388607" 4194304`, `"8388608-10485759" 2097152`)

		rr = e.do(e.h, http.MethodPatch, uploads+id, bytes.NewReader(blob[10*mib:11*mib]), "Content-Range", "10485760-11534335")
		wantStatus(t, "the next chunk", rr, http.StatusAccepted)
		wantHeader(t, "the next chunk", rr, "Range", "0-11534335")
		wantSent(t, "the next chunk", sent(f, &patches), `"10485760-11534335" 1048576`)

		rr = e.do(e.h, http.MethodPatch, uploads+id, strings.NewReader("de"), "Content-Length", "-1")
		msg := wantRefusal(t, "a chunk of unknown length", rr, srv, http.StatusBadRequest, codeBlobUploadInvalid, true)
		if !strings.Contains(msg, "hint:") || !strings.Contains(msg, "registries[].uploads.spoolUnknownLength") {
			t.Errorf("message %q does not hint at registries[].uploads.spoolUnknownLength", msg)
		}
		wantSent(t, "a chunk of unknown length", sent(f, &patches))
		rr = e.do(e.h, http.MethodGet, uploads+id, nil)
		wantStatus(t, "the session after the refusal", rr, http.StatusNoContent)
		wantHeader(t, "the session after the refusal", rr, "Range", "0-11534335")

		rr = e.do(e.h, http.MethodPut, uploads+id+"?digest="+digest.FromBytes(blob).String(), bytes.NewReader(blob[11*mib:]),
			"Content-Range", "11534336-16777215")
		wantStatus(t, "the close with a last chunk of 5 MiB", rr, http.StatusCreated)
		wantSent(t, "the close with a last chunk of 5 MiB", sent(f, &patches), `"11534336-15728639" 4194304`, `"15728640-16777215" 1048576`)

		id = e.start(e.h)
		rr = e.do(e.h, http.MethodPatch, uploads+id, bytes.NewReader(blob[:10*mib]), "Content-Length", "-1")
		wantStatus(t, "a first write of unknown length", rr, http.StatusAccepted)
		wantHeader(t, "a first write of unknown length", rr, "Range", "0-10485759")
		wantSent(t, "a first write of unknown length", sent(f, &patches), `"" -1`)
	})

	t.Run("no bound", func(t *testing.T) {
		var patches []string
		f := newRegistry(&patches)
		e, _, srv := refusingEnv(t, f)
		id := e.start(e.h)
		rr := e.do(e.h, http.MethodPatch, uploads+id, bytes.NewReader(blob[:10*mib]))
		wantRefusal(t, "a chunk of 10 MiB", rr, srv, http.StatusRequestedRangeNotSatisfiable, codeBlobUploadInvalid, true)
		wantSent(t, "a chunk of 10 MiB", sent(f, &patches), `"0-10485759" 10485760`)
	})
}
