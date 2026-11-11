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
// of formattest.Refusals (#494). Then each file that streams into the registry, a .tar.gz sdist,
// a wheel and a .zip sdist, goes to a registry that refuses only its upload session in each way
// of formattest.UploadRefusals, at the first chunk while the body streams and at the commit
// after it: the answer is the refusal's, and nothing is stored, neither in the registry nor in
// the index (#685).
func TestRegistryRefusals(t *testing.T) {
	whl, wi := testutil.Wheel(testutil.WheelOptions{Name: "tool", Version: "2.0"})
	for _, r := range formattest.Refusals {
		t.Run(r.Name, func(t *testing.T) {
			e := newEnv(t)
			r.Apply(e.fake)
			r.Check(t, e.post(t, twineForm(wi.Filename, "tool", "2.0", PackagetypeWheel, whl)))
		})
	}
	files := []struct {
		filename string
		content  []byte
	}{
		{"tool-2.0.tar.gz", sdistOf(t, "2.0", "PKG-INFO", pkgInfo("2.0"), "tool/__init__.py", "x = 1\n")},
		{wi.Filename, whl},
		{"tool-2.0.zip", storedZip(t, false, "tool-2.0/PKG-INFO", pkgInfo("2.0"), "tool-2.0/tool/__init__.py", "x = 1\n")},
	}
	for _, f := range files {
		for _, at := range []struct {
			name  string
			chunk int // the fake's ChunkSize
		}{{"while the body streams", 64}, {"at the commit", len(f.content) + 1}} {
			for _, r := range formattest.UploadRefusals {
				t.Run(f.filename+"/"+at.name+"/"+r.Name, func(t *testing.T) {
					e := newEnv(t)
					e.fake.ChunkSize = at.chunk
					r.Apply(e.fake)
					r.Check(t, e.post(t, twineForm(f.filename, "tool", "2.0", filetype(f.filename), f.content)))
					formattest.NothingStored(t, e.fake)
					e.nothingStored(t, "tool", f.content, 0)
				})
			}
		}
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
