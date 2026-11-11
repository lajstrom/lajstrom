package facade

import (
	"bytes"
	"log/slog"
	"net/http"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestManifestHookReadsBack: a layer the registry does not serve yet, on a registry that misses
// the first reads of each blob it took, is read again rather than answered
// MANIFEST_BLOB_UNKNOWN; a layer never uploaded is MANIFEST_BLOB_UNKNOWN once the visibility
// timeout has passed, and at once on a strongly consistent registry.
func TestManifestHookReadsBack(t *testing.T) {
	var lag *providertest.Lag
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider {
		lag = &providertest.Lag{Fake: f, Blobs: 2}
		return lag
	}})
	timeout := time.Second
	e.ingest = ingest.New(ingest.Options{Providers: provider.Map{"local": e.reg}, Index: e.st,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: e.clock.Now, VisibilityTimeout: timeout})
	h := New(Options{Index: e.st, Providers: provider.Map{"local": e.reg}, Ingest: e.ingest, Tokens: e.tokens,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: e.clock.Now, VisibilityTimeout: timeout})

	f := rawFileOf("docs/lag.txt", "uploaded a moment ago")
	rr, err := e.reg.Repository(t.Context(), "raw/assets/docs/lag.txt")
	must(t, err)
	must(t, rr.Push(t.Context(), ocispec.Descriptor{MediaType: octetStream, Digest: f.digest, Size: int64(len(f.content))},
		bytes.NewReader(f.content)))
	body, _ := f.body(t)
	put := func(h http.Handler, f *rawFile, body []byte) int {
		return e.do(h, http.MethodPut, "/v2/raw/assets/"+f.path+"/manifests/"+f.tag(), bytes.NewReader(body),
			"Content-Type", ocispecManifest).Code
	}
	if code := put(h, f, body); code != http.StatusCreated || lag.Misses() == 0 {
		t.Errorf("a layer the registry serves after %d misses: %d, want 201", lag.Misses(), code)
	}

	missing := rawFileOf("docs/missing.txt", "never uploaded")
	body, _ = missing.body(t)
	for _, strong := range []bool{false, true} {
		h := h
		if strong {
			h = New(Options{Index: e.st, Providers: provider.Map{"local": e.prov}, Ingest: e.ingest, Tokens: e.tokens,
				Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: e.clock.Now, VisibilityTimeout: timeout})
		}
		start := time.Now()
		rec := e.do(h, http.MethodPut, "/v2/raw/assets/"+missing.path+"/manifests/"+missing.tag(), bytes.NewReader(body),
			"Content-Type", ocispecManifest)
		wantError(t, "a layer never uploaded", rec, http.StatusBadRequest, codeManifestBlobUnknown)
		waited := time.Since(start)
		if strong && waited > timeout/2 || !strong && waited < timeout {
			t.Errorf("strong %v: answered after %s, want %s", strong, waited, map[bool]string{true: "at once", false: "the timeout"}[strong])
		}
	}
}
