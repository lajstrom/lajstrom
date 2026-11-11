package pypi

import (
	"testing"

	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestRegistryRefusals uploads a wheel as twine does to a registry that refuses it in each way
// of formattest.Refusals (#494).
func TestRegistryRefusals(t *testing.T) {
	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "2.0"})
	for _, r := range formattest.Refusals {
		t.Run(r.Name, func(t *testing.T) {
			e := newEnv(t)
			r.Apply(e.fake)
			r.Check(t, e.post(t, twineForm(wi.Filename, "tool", "2.0", PackagetypeWheel, whl)))
		})
	}
}
