package pypi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestUploadIndexUnavailableAtConfirm uploads a wheel whose index write fails after the
// registry took its manifest, the database out (#641): 503 with Retry-After: 5 and the status
// text only, which twine retries, not a 500; the same upload again is 200 and the wheel is on
// the page. The upload again builds its manifest a second later, as twine's retry does, so
// with another digest: it confirms the manifest the registry has, which its tag keeps.
func TestUploadIndexUnavailableAtConfirm(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	rebuilt := &formattest.Rebuilding{Streamer: Plugin{}}
	e.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": e.fake}, Index: downAtConfirm(e.st), Log: log,
		Plugins: func(name string) (format.Plugin, bool) { return rebuilt, name == Name }})
	deps := format.Deps{Index: e.st, Ingest: e.pipe, Providers: provider.Map{"local": e.fake}, Log: log}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	e.h = mux

	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "1.0"})
	f := twineForm(wi.Filename, "tool", "1.0", PackagetypeWheel, whl)
	rr := e.post(t, f)
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != "Service Unavailable\n" || rr.Header().Get("Retry-After") != "5" {
		t.Fatalf("upload while the index is out = %d %q, Retry-After %q; want 503 Service Unavailable, 5",
			rr.Code, rr.Body, rr.Header().Get("Retry-After"))
	}
	if len(e.fake.PushReferences()) != 1 {
		t.Errorf("%d manifest pushes, want the one the index write failed after", len(e.fake.PushReferences()))
	}
	if rr := e.post(t, f); rr.Code != http.StatusOK {
		t.Fatalf("the same upload again: %d %s", rr.Code, rr.Body)
	}
	if refs := e.fake.PushReferences(); len(refs) != 2 || refs[1].Digest != refs[0].Digest {
		t.Errorf("manifest pushes %+v, want the first manifest pushed again", refs)
	}
	if page := e.do(http.MethodGet, "simple/tool/").Body.String(); !strings.Contains(page, wi.Filename) {
		t.Errorf("the page after the upload again:\n%s", page)
	}
}

// downAtConfirm wraps st: the first transaction that writes a package, the confirm of a publish
// after the registry took its manifest, fails as a database that dropped out does, with
// index.ErrUnavailable and a cause that names its host.
func downAtConfirm(st index.Store) index.Store { return &confirmDown{Store: st} }

type confirmDown struct {
	index.Store
	done atomic.Bool
}

func (s *confirmDown) Tx(ctx context.Context, fn func(index.Writer) error) error {
	return s.Store.Tx(ctx, func(w index.Writer) error { return fn(confirmDownWriter{Writer: w, s: s}) })
}

type confirmDownWriter struct {
	index.Writer
	s *confirmDown
}

func (w confirmDownWriter) UpsertPackage(ctx context.Context, p *index.Package) error {
	if w.s.done.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: packages: dial tcp 10.0.0.7:5432: connect: connection refused", index.ErrUnavailable)
	}
	return w.Writer.UpsertPackage(ctx, p)
}
