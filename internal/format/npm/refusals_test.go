package npm

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestRegistryRefusals publishes a package as npm does to a registry that refuses it in each
// way of formattest.Refusals (#494).
func TestRegistryRefusals(t *testing.T) {
	b, info := testutil.NpmTarball(testutil.NpmOptions{})
	for _, r := range formattest.Refusals {
		t.Run(r.Name, func(t *testing.T) {
			p := newPipeline(t)
			r.Apply(p.fake)
			r.Check(t, do(p.routes(nil), http.MethodPut, "/npm/internal/"+info.Name, npmBody(t, b, info, nil), false))
		})
	}
}

// TestPublishStalled: a publish whose client stops sending part way, its body cut by
// server.stallTimeout, is 408, not a 400 that names the connection (#576).
func TestPublishStalled(t *testing.T) {
	b, info := testutil.NpmTarball(testutil.NpmOptions{})
	body := npmBody(t, b, info, nil)
	req := httptest.NewRequest(http.MethodPut, "/npm/internal/"+info.Name, formattest.Stalled(bytes.NewReader(body[:len(body)/2])))
	req = req.WithContext(auth.WithPrincipal(req.Context(), alice))
	rr := httptest.NewRecorder()
	newPipeline(t).routes(nil).ServeHTTP(rr, req)
	formattest.CheckStalled(t, rr)
}
