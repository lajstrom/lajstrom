package facade

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestManifestHookNotVisible: a manifest the registry took and does not serve within the
// visibility timeout is 503 UNAVAILABLE with Retry-After, and the same PUT again, once the
// registry serves it, is 201.
func TestManifestHookNotVisible(t *testing.T) {
	var lag *providertest.Lag
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider {
		lag = &providertest.Lag{Fake: f, Manifests: -1}
		return lag
	}})
	e.ingest = ingest.New(ingest.Options{Providers: provider.Map{"local": e.reg}, Index: e.st,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: e.clock.Now, VisibilityTimeout: 100 * time.Millisecond})
	e.h = e.replica()
	f := e.newRawFile("docs/e.txt", "not served yet")
	body, _ := f.body(t)
	rr := e.put(f, f.tag(), body)
	wantError(t, "a manifest the registry does not serve", rr, http.StatusServiceUnavailable, codeUnavailable)
	wantHeader(t, "the 503", rr, "Retry-After", "30")
	lag.Serve()
	wantStatus(t, "the same PUT again", e.put(f, f.tag(), body), http.StatusCreated)
}
