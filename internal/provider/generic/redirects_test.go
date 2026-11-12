package generic

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/egress"
)

// TestPublicRedirects checks Options.PublicRedirects, the rule of an oci proxy's upstream
// (proxy-and-group.md §11.1): a blob or a manifest redirected to another host than the
// registry's is followed only over https and only to public addresses. The fake is on
// 127.0.0.1; localhost is another host name, which resolves to loopback.
func TestPublicRedirects(t *testing.T) {
	data := []byte("0123456789")
	d := digest.FromBytes(data)
	f := newFakeRegistry(t)
	f.blobs["library/nginx"] = map[digest.Digest][]byte{d: data}
	_, port, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	newProvider := func(guarded bool) *Provider {
		p, err := NewWithOptions(config.Registry{Name: "upstream", Type: Type, URL: f.srv.URL}, nil, Options{PublicRedirects: guarded})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	for _, tc := range []struct {
		name, location string
		guarded, ok    bool
	}{
		{"the registry's own host", f.srv.URL + "/v2/library/nginx/blobs/" + d.String() + "?followed=1", true, true},
		{"another host over http", "http://localhost:" + port + "/v2/library/nginx/blobs/" + d.String() + "?followed=1", true, false},
		{"another host that resolves to loopback", "https://localhost:" + port + "/v2/library/nginx/blobs/" + d.String() + "?followed=1", true, false},
		{"a private address", "https://10.1.2.3/blob", true, false},
		{"the metadata endpoint", "https://169.254.169.254/latest/meta-data", true, false},
		{"another host over http, not guarded", "http://localhost:" + port + "/v2/library/nginx/blobs/" + d.String() + "?followed=1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.Contains(r.URL.Path, "/blobs/") || r.URL.Query().Get("followed") == "1" {
					return false
				}
				w.Header().Set("Location", tc.location)
				w.WriteHeader(http.StatusTemporaryRedirect)
				return true
			}
			b, err := newProvider(tc.guarded).OpenBlob(context.Background(), "library/nginx", d, "", false)
			if b.Body != nil {
				_ = b.Body.Close()
			}
			switch {
			case tc.ok && err != nil:
				t.Fatalf("OpenBlob: %v, want the blob", err)
			case !tc.ok && !errors.Is(err, egress.ErrRefused):
				t.Fatalf("OpenBlob: %v, want egress.ErrRefused", err)
			case err != nil && strings.Contains(err.Error(), tc.location):
				t.Errorf("the error %q names the redirect's URL", err)
			}
		})
	}

	t.Run("a manifest", func(t *testing.T) {
		f.manifests["library/nginx"] = map[string]*fakeManifest{}
		f.hook = func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.Contains(r.URL.Path, "/manifests/") {
				return false
			}
			w.Header().Set("Location", (&url.URL{Scheme: "https", Host: "localhost:" + port, Path: r.URL.Path}).String())
			w.WriteHeader(http.StatusTemporaryRedirect)
			return true
		}
		rr, err := newProvider(true).Repository(context.Background(), "library/nginx")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rr.Resolve(context.Background(), "1.27"); !errors.Is(err, egress.ErrRefused) {
			t.Fatalf("Resolve through a redirect to loopback: %v, want egress.ErrRefused", err)
		}
	})
}
