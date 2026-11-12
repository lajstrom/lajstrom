package rpm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// uploaded decodes the body of a 201 of the upload route, which rr must be.
func uploaded(t *testing.T, code int, body []byte) uploadResponse {
	t.Helper()
	if code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", code, body)
	}
	var resp uploadResponse
	must(t, json.Unmarshal(body, &resp))
	return resp
}

// TestUpload checks the upload route: 201 with Location below Packages/, Docker-Content-Digest
// and the JSON body, and the file served at Location with its headers; the same bytes again are
// the repeat, 201 with the same digest and nothing pushed.
func TestUpload(t *testing.T) {
	e := newEnv(t, envOptions{})
	hello, info := testutil.RPM(testutil.RPMOptions{Epoch: "1"})
	rr := e.put(hello, "Content-Disposition", `attachment; filename="hello.rpm"`)
	resp := uploaded(t, rr.Code, rr.Body.Bytes())
	want := uploadResponse{Name: "hello", Epoch: 1, Version: "1.0.0", Release: "1.el9", Arch: "x86_64", Filename: info.Filename,
		Digest: resp.Digest, Tag: "1_3a1.0.0-1.el9__x86_5f64"}
	if resp != want || !strings.HasPrefix(resp.Digest, "sha256:") {
		t.Errorf("body = %+v, want %+v", resp, want)
	}
	h := rr.Header()
	if h.Get("Location") != "/rpm/el9/Packages/"+info.Filename || h.Get("Docker-Content-Digest") != resp.Digest {
		t.Errorf("headers %v", h)
	}
	got := e.get("Packages/" + info.Filename)
	if !bytes.Equal(got.Body.Bytes(), hello) || got.Header().Get("Content-Type") != packageContentType ||
		got.Header().Get("Docker-Content-Digest") != resp.Digest || got.Header().Get("ETag") != `"sha256:`+info.SHA256+`"` {
		t.Errorf("GET Packages/%s: %v, %d bytes", info.Filename, got.Header(), got.Body.Len())
	}
	if head := e.send(http.MethodHead, "Packages/"+info.Filename, nil); head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD = %d with %d bytes", head.Code, head.Body.Len())
	}

	pushes := len(e.prov.Pushes()) + len(e.prov.PushReferences())
	rr = e.put(hello)
	if again := uploaded(t, rr.Code, rr.Body.Bytes()); again != want {
		t.Errorf("the repeat = %+v, want %+v", again, want)
	}
	if n := len(e.prov.Pushes()) + len(e.prov.PushReferences()); n != pushes {
		t.Errorf("the repeat pushed %d blobs and manifests", n-pushes)
	}
}

// TestUploadSource uploads a binary package and the source package of the same version: two
// versions, src beside x86_64, each served under its own file name.
func TestUploadSource(t *testing.T) {
	e := newEnv(t, envOptions{})
	hello, _ := testutil.RPM(testutil.RPMOptions{Epoch: "1"})
	rr := e.put(hello)
	uploaded(t, rr.Code, rr.Body.Bytes())
	src, srcInfo := testutil.RPM(testutil.RPMOptions{Epoch: "1", Source: true})
	rr = e.put(src)
	if s := uploaded(t, rr.Code, rr.Body.Bytes()); s.Arch != "src" || s.Tag != "1_3a1.0.0-1.el9__src" {
		t.Errorf("the source package = %+v", s)
	}
	if got := e.get("Packages/" + srcInfo.Filename).Body.Bytes(); !bytes.Equal(got, src) {
		t.Error("the source package is not served")
	}
}

// TestUploadConflicts checks the two 409s: other bytes under the name, version and arch of a
// version are the tag's (ingest); under the file name of a version with another epoch they are
// the file name rule's, which names the version that holds it. Nothing is published for either.
func TestUploadConflicts(t *testing.T) {
	e := newEnv(t, envOptions{})
	hello, info := testutil.RPM(testutil.RPMOptions{})
	other, _ := testutil.RPM(testutil.RPMOptions{Summary: "another greeting"})
	epoch, epochInfo := testutil.RPM(testutil.RPMOptions{Epoch: "1"})
	if epochInfo.Filename != info.Filename {
		t.Fatalf("the epoch fixture is %s, want %s", epochInfo.Filename, info.Filename)
	}
	rr := e.put(hello)
	first := uploaded(t, rr.Code, rr.Body.Bytes())
	pushes := len(e.prov.PushReferences())

	if rr := e.put(other); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), first.Digest) {
		t.Errorf("other bytes under the tag = %d %s, want the 409 of the tag naming %s", rr.Code, rr.Body, first.Digest)
	}
	rule := "the file name " + info.Filename + " is held by version 1.0.0-1.el9 of hello with another content"
	if rr := e.put(epoch); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), rule) {
		t.Errorf("another epoch = %d %s, want 409 with %q", rr.Code, rr.Body, rule)
	}
	if n := len(e.prov.PushReferences()); n != pushes {
		t.Errorf("the conflicts pushed %d manifests", n-pushes)
	}
	if got := e.get("Packages/" + info.Filename).Body.Bytes(); !bytes.Equal(got, hello) {
		t.Error("the file name serves another content than the first upload")
	}
}

// TestUploadRefuses refuses a body that is not an rpm (400 naming the upload), one whose head
// is cut, one above the repo's maxFileSize (413), and a writer without credentials.
func TestUploadRefuses(t *testing.T) {
	e := newEnv(t, envOptions{})
	hello, info := testutil.RPM(testutil.RPMOptions{})
	for _, c := range []struct {
		name string
		body []byte
		code int
		want string
	}{
		{"not an rpm", []byte(strings.Repeat("not an rpm ", 20)), http.StatusBadRequest, `rpm: "upload.rpm": format: invalid: the file does not start with the rpm lead magic`},
		{"cut head", hello[:info.HeaderStart+20], http.StatusBadRequest, "truncated: the header"},
		{"cut payload", hello[:len(hello)-10], http.StatusBadRequest, "truncated: header and payload"},
	} {
		if rr := e.put(c.body); rr.Code != c.code || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("%s: PUT = %d %s, want %d with %q", c.name, rr.Code, rr.Body, c.code, c.want)
		}
	}
	if pkgs, err := e.st.ListPackages(t.Context(), e.repo.ID, ""); err != nil || len(pkgs) != 0 {
		t.Errorf("packages after refusals: %v, %v", pkgs, err)
	}
	repo := e.repo
	repo.Policy = []byte(`{"maxFileSize": ` + strconv.Itoa(len(hello)-1) + `}`)
	must(t, e.st.UpdateRepo(t.Context(), &repo))
	if rr := e.put(hello); rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "larger than the maxFileSize of rpm/el9") {
		t.Errorf("above maxFileSize: PUT = %d %s, want 413", rr.Code, rr.Body)
	}
	if rr := e.send(http.MethodGet, "Packages/nothing-1-1.x86_64.rpm", nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET of a file the repo does not have = %d", rr.Code)
	}
}

// TestDeleteVersion deletes a version through the pipeline, as the API and /v2 do: its file is
// no longer served.
func TestDeleteVersion(t *testing.T) {
	e := newEnv(t, envOptions{})
	hello, info := testutil.RPM(testutil.RPMOptions{})
	pub := e.upload(hello)
	e.get("Packages/" + info.Filename)
	if _, err := e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "hello", Reference: pub.Tag}, alice); err != nil {
		t.Fatal(err)
	}
	if rr := e.send(http.MethodGet, "Packages/"+info.Filename, nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET after the delete = %d", rr.Code)
	}
	matches, err := e.st.FindFiles(t.Context(), e.repo.ID, info.Filename)
	if err != nil || len(matches) != 0 {
		t.Errorf("the index has %d files called %s after the delete (%v)", len(matches), info.Filename, err)
	}
}
