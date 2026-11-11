package maven

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format/formattest"
)

// TestRegistryRefusals deploys a release as mvn does to a registry that refuses it in each way
// of formattest.Refusals (#494): a refused blob answers the PUT of the first file, a refused
// manifest the PUT of maven-metadata.xml, which commits the version.
func TestRegistryRefusals(t *testing.T) {
	for _, r := range formattest.Refusals {
		t.Run(r.Name, func(t *testing.T) {
			e := newDeployEnv(t, nil)
			r.Apply(e.fake)
			files := release("1.2.0", "")
			rr := e.put("alice", files[0].path, files[0].body)
			if r.Push == nil {
				if rr.Code != http.StatusCreated {
					t.Fatalf("PUT %s: %d %s", files[0].path, rr.Code, rr.Body)
				}
				for _, f := range files[1:] {
					e.wantPut(f.path, f.body, http.StatusCreated)
				}
				rr = e.put("alice", metadataPath, metadataDoc("1.2.0"))
			}
			r.Check(t, rr)
		})
	}
}

// TestDeployStalled: a PUT whose client stops sending part way, its body cut by
// server.stallTimeout, is 408, not a 500 at ERROR (#576).
func TestDeployStalled(t *testing.T) {
	e := newDeployEnv(t, nil)
	f := release("1.2.0", "")[0]
	req := httptest.NewRequest(http.MethodPut, "/maven/releases/"+f.path, formattest.Stalled(bytes.NewReader(f.body[:len(f.body)/2])))
	req.Header.Set("X-Test-User", "alice")
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	formattest.CheckStalled(t, rr)
}
