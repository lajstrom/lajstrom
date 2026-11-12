package rpm

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// env is the rpm routes over a pipeline.
type env struct {
	*pipeline
	h http.Handler
}

// envOptions change what newEnv builds.
type envOptions struct {
	signer func(index.Store) format.Signer // the Signer over the store; nil: none
	now    func() time.Time                // the clock of a render and a signature; nil: time.Now
}

func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	e := &env{pipeline: newPipeline(t)}
	deps := format.Deps{Index: e.st, Ingest: e.pipe, Providers: provider.Map{"local": e.prov}, Log: slog.New(slog.DiscardHandler)}
	if o.signer != nil {
		deps.Signer = o.signer(e.st)
	}
	mux := http.NewServeMux()
	Plugin{svc: &services{now: o.now}}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name := r.Header.Get("X-Test-User"); name != "" {
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindUser, Name: name}))
		}
		mux.ServeHTTP(w, r)
	})
	return e
}

// send sends a request with body, which may be nil, to /rpm/el9/<path>; hdr are header name and
// value pairs.
func (e *env) send(method, path string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/rpm/el9/"+path, body)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// get sends a GET of path, which must answer 200, and returns the response.
func (e *env) get(path string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	rr := e.send(http.MethodGet, path, nil, hdr...)
	if rr.Code != http.StatusOK {
		e.t.Fatalf("GET %s = %d %s", path, rr.Code, rr.Body)
	}
	return rr
}

// put uploads b as alice.
func (e *env) put(b []byte, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.send(http.MethodPut, "upload", bytes.NewReader(b), append([]string{"X-Test-User", "alice"}, hdr...)...)
}
