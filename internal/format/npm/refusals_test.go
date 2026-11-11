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
// way of formattest.Refusals (#494); then to one that refuses only the upload session of the
// tarball, which streams into the registry, in each way of formattest.UploadRefusals, at the
// first chunk while the tarball streams and at the commit after it: the answer is the
// refusal's, and nothing is stored, neither in the registry nor in the index (#685).
func TestRegistryRefusals(t *testing.T) {
	b, info := testutil.NpmTarball(testutil.NpmOptions{})
	for _, r := range formattest.Refusals {
		t.Run(r.Name, func(t *testing.T) {
			p := newPipeline(t)
			r.Apply(p.fake)
			r.Check(t, do(p.routes(nil), http.MethodPut, "/npm/internal/"+info.Name, npmBody(t, b, info, nil), false))
		})
	}
	for _, at := range []struct {
		name  string
		chunk int // the fake's ChunkSize
	}{{"while the tarball streams", 64}, {"at the commit", len(b) + 1}} {
		for _, r := range formattest.UploadRefusals {
			t.Run(at.name+"/"+r.Name, func(t *testing.T) {
				p := newPipeline(t)
				p.fake.ChunkSize = at.chunk
				r.Apply(p.fake)
				r.Check(t, do(p.routes(nil), http.MethodPut, "/npm/internal/"+info.Name, npmBody(t, b, info, nil), false))
				formattest.NothingStored(t, p.fake)
				pkgs, err := p.st.ListPackages(t.Context(), p.repo.ID, "")
				must(t, err)
				if len(pkgs) != 0 {
					t.Errorf("the index has the packages %+v; want none", pkgs)
				}
			})
		}
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
