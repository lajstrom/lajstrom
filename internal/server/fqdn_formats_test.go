package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/format/pypi"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// TestRepoHostPypiRedirects: pip's index URL on the repo host of the FQDN mode is
// https://internal.art.test/simple/, and the redirects of the pypi frontend, a project name not
// normalized and a page without its slash, stay below that host's root instead of naming the
// path form (docs/spec/http-routing.md §1.1). On the main host they are the path form, as before.
func TestRepoHostPypiRedirects(t *testing.T) {
	idx := &formattest.Index{Repos: []index.Repo{{ID: 1, Format: "pypi", Name: "internal"}}}
	s, err := server.New(server.Options{Listen: "127.0.0.1:0", Plugins: []format.Plugin{pypi.Plugin{}}, Deps: format.Deps{Index: idx},
		ExternalURL: "https://art.test", FQDNDomain: "art.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ host, path, location string }{
		{"internal.art.test", "/simple/My_Pkg/", "/simple/my-pkg/"},
		{"internal.art.test", "/simple/My_Pkg", "/simple/my-pkg/"},
		{"internal.art.test", "/simple", "/simple/"},
		{"internal.pypi.art.test", "/simple/My.Pkg/", "/simple/my-pkg/"},
		{"art.test", "/pypi/internal/simple/My_Pkg/", "/pypi/internal/simple/my-pkg/"},
		{"art.test", "/pypi/internal/simple", "/pypi/internal/simple/"},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://placeholder"+tt.path, nil)
		req.Host = tt.host
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusMovedPermanently || rr.Header().Get("Location") != tt.location {
			t.Errorf("GET %s%s: %d Location %q, want 301 %q", tt.host, tt.path, rr.Code, rr.Header().Get("Location"), tt.location)
		}
	}
}
