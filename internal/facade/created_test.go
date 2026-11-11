package facade

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestManifestHookCreated (#551): a version pushed on /v2 whose org.opencontainers.image.created
// is more than MaxCreatedAhead after the facade's clock or more than MaxCreatedBehind before it
// is 400 MANIFEST_INVALID naming the time, the clock and the bounds, and costs no request to the
// registry; a time at a bound is pushed. The defaults are 5 minutes and 1 hour.
func TestManifestHookCreated(t *testing.T) {
	tests := []struct {
		name    string
		behind  time.Duration // Options.MaxCreatedBehind
		offset  time.Duration // of created from the clock
		message string        // in the refusal; "" when the push is 201
	}{
		{name: "a year back", offset: -365 * 24 * time.Hour,
			message: "2025-11-07T10:00:00Z is 8760h0m0s behind the server's clock, 2026-11-07T10:00:00Z: it may be at most 5m0s ahead and 1h0m0s behind"},
		{name: "a year ahead", offset: 365 * 24 * time.Hour, message: "2027-11-07T10:00:00Z is 8760h0m0s ahead of the server's clock"},
		{name: "past the bound ahead", offset: 5*time.Minute + time.Second, message: "is 5m1s ahead of"},
		{name: "past the bound behind", offset: -time.Hour - time.Second, message: "is 1h0m1s behind"},
		{name: "at the bound ahead", offset: 5 * time.Minute},
		{name: "at the bound behind", offset: -time.Hour},
		{name: "the clock's time", offset: 0},
		{name: "behind, within a longer bound", behind: 3 * time.Hour, offset: -2 * time.Hour},
		{name: "past a longer bound", behind: 3 * time.Hour, offset: -3*time.Hour - time.Second, message: "at most 5m0s ahead and 3h0m0s behind"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			e := newEnv(t, envOptions{behind: tt.behind,
				registry: func(f *providertest.Fake) provider.Provider { return countingRegistry{Fake: f, calls: &calls} }})
			f := e.newRawFile("docs/created.txt", fmt.Sprint("a file of case ", i)).at(e.clock.Now().Add(tt.offset))
			body, _ := f.body(t)
			calls.Store(0)
			rr := e.put(f, f.tag(), body)
			if tt.message == "" {
				wantStatus(t, tt.name, rr, http.StatusCreated)
				return
			}
			entry := wantEntry(t, tt.name, rr, http.StatusBadRequest, codeManifestInvalid)
			if !strings.Contains(entry.Message, ocispec.AnnotationCreated+" ") || !strings.Contains(entry.Message, tt.message) {
				t.Errorf("message %q, want %q", entry.Message, tt.message)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d requests to the registry, want none", n)
			}
			if refs := e.prov.PushReferences(); len(refs) != 0 {
				t.Errorf("PushReferences = %+v, want none", refs)
			}
		})
	}
}

// TestManifestHookCreatedRepeat: the repeat of a manifest that the repo has is 201 whatever its
// created time, as it is on a frontend: it writes nothing; another manifest of that time is
// refused.
func TestManifestHookCreatedRepeat(t *testing.T) {
	e := newEnv(t, envOptions{})
	f := e.newRawFile("docs/repeat.txt", "pushed, then pushed again a day later")
	body, d := f.body(t)
	wantStatus(t, "push", e.put(f, f.tag(), body), http.StatusCreated)
	e.clock.advance(24 * time.Hour)
	rr := e.put(f, f.tag(), body)
	wantStatus(t, "the repeat a day later", rr, http.StatusCreated)
	wantHeader(t, "the repeat a day later", rr, "Docker-Content-Digest", d.String())
	if refs := e.prov.PushReferences(); len(refs) != 1 {
		t.Errorf("the repeat pushed again: %+v", refs)
	}
	e.wantIndexed(f, d)

	other := e.newRawFile("docs/other.txt", "another file of the same time")
	body, _ = other.body(t)
	wantEntry(t, "another manifest a day old", e.put(other, other.tag(), body), http.StatusBadRequest, codeManifestInvalid)
}
