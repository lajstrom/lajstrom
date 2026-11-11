package npm

import (
	"net/http"
	"testing"

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
