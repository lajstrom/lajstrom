package proxy_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// garV2 is Artifact Registry's /v2 in front of the hub: a bearer challenge whose realm is on
// its own host, /v2/token, which grants the hub's token to Basic oauth2accesstoken:token-1 only
// (the challenge measured on europe-west8-docker.pkg.dev, 2026-11-13).
func garV2(t *testing.T, h *hub, granted *atomic.Int32) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/token" {
			if u, p, ok := r.BasicAuth(); !ok || u != "oauth2accesstoken" || p != "token-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			granted.Add(1)
			_, _ = io.WriteString(w, `{"token":"anon"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer anon" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/v2/token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOCIUpstreamGCP: an oci upstream with auth.gcp pulls with an access token of the metadata
// server that GCE_METADATA_HOST names, sent to the token realm as Basic oauth2accesstoken; a
// token that cannot be had is 502 with no request to the upstream; a GCE_METADATA_HOST that is
// not a host is refused, naming the key (docs/spec/proxy-and-group.md §11.1).
func TestOCIUpstreamGCP(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	var asked, flavoured atomic.Int32
	var status atomic.Int32
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.Header.Get("Metadata-Flavor") == "Google" && r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/token" {
			flavoured.Add(1)
		}
		w.Header().Set("Metadata-Flavor", "Google")
		if s := status.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"token-1","expires_in":3599,"token_type":"Bearer"}`)
	}))
	t.Cleanup(metadata.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	var granted atomic.Int32
	v2 := garV2(t, e.hub, &granted)
	load := func() (map[string]*proxy.OCIUpstream, error) {
		return proxy.LoadOCIUpstreams([]config.Upstream{{Name: "hub", Format: config.FormatOCI, URL: v2.URL, Retries: -1,
			Auth: config.UpstreamAuth{GCP: &config.UpstreamGCP{}}}}, slog.New(slog.DiscardHandler))
	}

	ups, err := load()
	must(t, err)
	e.up = ups["hub"]
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	desc, _, err := e.core("a", proxy.Options{}).OCIManifest(t.Context(), e.repo, "library/nginx", "1.27")
	if err != nil || desc.Digest != img.index.Digest {
		t.Fatalf("OCIManifest = %s, %v; want %s", desc.Digest, err, img.index.Digest)
	}
	if asked.Load() != 1 || flavoured.Load() != 1 || granted.Load() == 0 {
		t.Errorf("metadata requests %d (%d with Metadata-Flavor), tokens granted %d; want one request and a token", asked.Load(),
			flavoured.Load(), granted.Load())
	}

	status.Store(http.StatusNotFound)
	ups, err = load()
	must(t, err)
	e.up = ups["hub"]
	before := e.hub.count("")
	_, _, err = e.core("b", proxy.Options{}).OCIManifest(t.Context(), e.repo, "library/redis", "7.4")
	if !errors.Is(err, format.ErrBadGateway) || strings.Contains(err.Error(), "token-1") {
		t.Errorf("OCIManifest without a token = %v, want ErrBadGateway", err)
	}
	if n := e.hub.count(""); n != before {
		t.Errorf("%d requests reached the upstream with a token, want none", n-before)
	}

	t.Setenv("GCE_METADATA_HOST", "http://metadata.google.internal/")
	if _, err := load(); err == nil || !strings.Contains(err.Error(), "upstreams[0].auth.gcp") {
		t.Errorf("LoadOCIUpstreams with GCE_METADATA_HOST a URL = %v, want an error naming upstreams[0].auth.gcp", err)
	}
}
