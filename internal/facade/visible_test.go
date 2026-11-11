package facade

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestManifestHookNotVisible: a manifest the registry took and does not serve within the
// visibility timeout is 503 UNAVAILABLE with Retry-After, whose message and detail name what it
// does not serve, the manifest and its tag, by the name the client pushed to (#565); the same
// PUT again, once the registry serves it, is 201.
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
	body, d := f.body(t)
	rr := e.put(f, f.tag(), body)
	msg := wantError(t, "a manifest the registry does not serve", rr, http.StatusServiceUnavailable, codeUnavailable)
	wantHeader(t, "the 503", rr, "Retry-After", "30")
	name := "raw/assets/docs/e.txt"
	if want := "the registry took " + name + "@" + d.String() + " and does not serve the manifest, the tag " + f.tag() +
		" yet; the same push again completes it"; msg != want {
		t.Errorf("message %q, want %q", msg, want)
	}
	var answer struct {
		Errors []struct {
			Detail notServedDetail `json:"detail"`
		} `json:"errors"`
	}
	must(t, json.Unmarshal(rr.Body.Bytes(), &answer))
	want := notServedDetail{Name: name, Digest: d, Missing: missingDetail{Manifest: true, Tag: f.tag()}}
	if len(answer.Errors) != 1 || !reflect.DeepEqual(answer.Errors[0].Detail, want) {
		t.Errorf("detail of %s, want %+v", rr.Body, want)
	}
	lag.Serve()
	wantStatus(t, "the same PUT again", e.put(f, f.tag(), body), http.StatusCreated)
}
