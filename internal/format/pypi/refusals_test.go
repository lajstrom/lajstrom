package pypi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestUploadStalled: an upload whose client stops sending part way, its body cut by
// server.stallTimeout, is 408, not a 400 that names the connection (#576).
func TestUploadStalled(t *testing.T) {
	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "2.0"})
	body, ctype := twineForm(wi.Filename, "tool", "2.0", PackagetypeWheel, whl).body(t)
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/pypi/internal/", formattest.Stalled(bytes.NewReader(b[:len(b)/2])))
	req.Header.Set("Content-Type", ctype)
	rr := httptest.NewRecorder()
	newEnv(t).h.ServeHTTP(rr, req)
	formattest.CheckStalled(t, rr)
}
