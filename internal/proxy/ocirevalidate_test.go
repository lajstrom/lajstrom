package proxy_test

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// TestOCIRevalidateBudget: with the upstream accepting the connection and never answering, an
// expired tag is answered from its row within revalidateTimeout, after one HEAD and no retry;
// a tag without a row still gets the full timeout and retries, and then 504 (#888).
func TestOCIRevalidateBudget(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	const timeout, budget = 400 * time.Millisecond, 100 * time.Millisecond
	ups, err := proxy.LoadOCIUpstreams([]config.Upstream{{Name: "hub", Format: config.FormatOCI, URL: e.hub.srv.URL, Timeout: timeout,
		Retries: 1, RevalidateTimeout: budget}}, slog.New(slog.DiscardHandler))
	must(t, err)
	e.up = ups["hub"]
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err = c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	must(t, err)

	e.hub.hang.Store(true)
	e.advance(11 * time.Minute)
	heads := e.hub.count("HEAD /v2/library/nginx/manifests/1.27")
	start := time.Now()
	desc, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	took := time.Since(start)
	if err != nil || desc.Digest != img.index.Digest {
		t.Fatalf("an expired tag, the upstream hanging: %v, %v; want the row's %s", desc.Digest, err, img.index.Digest)
	}
	if took >= timeout {
		t.Errorf("an expired tag took %s, want it within revalidateTimeout %s, before one full timeout %s", took, budget, timeout)
	}
	if n := e.hub.count("HEAD /v2/library/nginx/manifests/1.27") - heads; n != 1 {
		t.Errorf("%d HEADs of the expired tag, want one: no retry", n)
	}

	start = time.Now()
	_, _, err = c.OCIManifest(t.Context(), e.repo, "library/nginx", "new")
	took = time.Since(start)
	if !errors.Is(err, format.ErrGatewayTimeout) {
		t.Fatalf("a tag without a row, the upstream hanging: %v, want format.ErrGatewayTimeout", err)
	}
	if n := e.hub.count("HEAD /v2/library/nginx/manifests/new"); n != 2 || took < 2*timeout {
		t.Errorf("a tag without a row: %d HEADs in %s, want 2 (the timeout and a retry), at least %s", n, took, 2*timeout)
	}
}
