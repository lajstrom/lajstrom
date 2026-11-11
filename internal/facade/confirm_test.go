package facade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// TestIndexUnavailableAtConfirm pushes a manifest whose index write fails after the registry
// took it, the database out (#641): 503 UNAVAILABLE with Retry-After: 5 and the status text
// only, logged at warn, not a 500; the claim on the tag is released, and the same PUT again is
// 201.
func TestIndexUnavailableAtConfirm(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, envOptions{})
	e.ingest = ingest.New(ingest.Options{Providers: provider.Map{"local": e.reg}, Index: downAtConfirm(e.st),
		Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: e.clock.Now})
	e.h = e.replica()
	f := e.newRawFile("docs/e.txt", "the database drops out")
	body, d := f.body(t)
	rr := e.put(f, f.tag(), body)
	if msg := wantError(t, "a PUT whose index write fails", rr, http.StatusServiceUnavailable, codeUnavailable); msg != "Service Unavailable" {
		t.Errorf("message %q, want the status text only", msg)
	}
	wantHeader(t, "the 503", rr, "Retry-After", "5")
	if strings.Contains(rr.Body.String(), "10.0.0.7") || !strings.Contains(e.logs.String(), "facade: index unavailable") ||
		strings.Contains(e.logs.String(), "request failed") {
		t.Errorf("the body %s names the database, or the log has no warning or an error:\n%s", rr.Body, e.logs)
	}
	rr2, err := e.prov.Repository(ctx, "raw/assets/docs/e.txt")
	must(t, err)
	if desc, err := rr2.Resolve(ctx, f.tag()); err != nil || desc.Digest != d {
		t.Fatalf("the registry has %v, %v under the tag, want the manifest %s", desc.Digest, err, d)
	}
	if c, err := e.st.GetTagClaim(ctx, e.raw.ID, "docs/e.txt", f.tag()); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the claim on the tag: %+v, %v; want it released", c, err)
	}
	wantStatus(t, "the same PUT again", e.put(f, f.tag(), body), http.StatusCreated)
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
