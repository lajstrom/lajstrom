package deb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// put sends a PUT of deb to upload?<query> as alice, with the header pairs hdr.
func (e *aptEnv) put(query string, deb []byte, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.send(http.MethodPut, "upload?"+query, bytes.NewReader(deb), append([]string{"X-Test-User", "alice"}, hdr...)...)
}

// uploaded decodes the body of a 201 of the upload route, which rr must be.
func uploaded(t *testing.T, rr *httptest.ResponseRecorder) uploadResponse {
	t.Helper()
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	var resp uploadResponse
	must(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

// TestUpload checks the upload route: 201 with Location in the pool, Docker-Content-Digest and
// the JSON body, the file served at Location and listed in Packages; the same bytes again are
// the repeat, 201 with the same digest and nothing pushed; the same bytes in another suite are
// a new version over the same blob, which the pool serves as one file. arch, when given, is
// the control file's (TestUploadRefuses has another).
func TestUpload(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	hello, info := testutil.Deb(testutil.DebOptions{})
	rr := e.put("suite=jammy&component=main&arch=amd64", hello)
	resp := uploaded(t, rr)
	want := uploadResponse{Package: "hello", Version: "1.0.0-1", Architecture: "amd64", Suite: "jammy", Component: "main",
		Filename: info.Filename, Digest: resp.Digest, Tag: "1.0.0-1__jammy_2fmain_2famd64"}
	if resp != want || !strings.HasPrefix(resp.Digest, "sha256:") {
		t.Errorf("body = %+v, want %+v", resp, want)
	}
	h := rr.Header()
	if h.Get("Location") != "/apt/ubuntu/pool/main/h/hello/"+info.Filename || h.Get("Docker-Content-Digest") != resp.Digest ||
		h.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", h)
	}
	if got := e.get(strings.TrimPrefix(h.Get("Location"), "/apt/ubuntu/")).Body.Bytes(); !bytes.Equal(got, hello) {
		t.Error("the file at Location is not the .deb uploaded")
	}
	if !strings.Contains(e.get("dists/jammy/main/binary-amd64/Packages").Body.String(), "Filename: pool/main/h/hello/"+info.Filename+"\n") {
		t.Error("Packages does not list the upload")
	}
	if v := e.versionOf(t, "hello", "jammy/main/amd64"); v.Digest.String() != resp.Digest {
		t.Errorf("the index has %s, the answer %s", v.Digest, resp.Digest)
	}

	pushes := len(e.prov.Pushes()) + len(e.prov.PushReferences())
	if again := uploaded(t, e.put("suite=jammy&component=main", hello)); again != want {
		t.Errorf("the repeat = %+v, want %+v", again, want)
	}
	if n := len(e.prov.Pushes()) + len(e.prov.PushReferences()); n != pushes {
		t.Errorf("the repeat pushed %d blobs and manifests", n-pushes)
	}

	updates := uploaded(t, e.put("suite=jammy-updates&component=main", hello))
	if updates.Tag != "1.0.0-1__jammy-updates_2fmain_2famd64" || updates.Digest == resp.Digest {
		t.Errorf("the upload to jammy-updates = %+v", updates)
	}
	if got := e.get("pool/main/h/hello/" + info.Filename).Body.Bytes(); !bytes.Equal(got, hello) {
		t.Error("the pool file of two suites is not the .deb")
	}
}

// TestUploadConflicts checks the two 409s: other bytes under the name, version and
// architecture of a version in the same suite and component are the tag's (ingest); in another
// suite, or with another epoch, which leaves the pool file name as it is, they are the pool file
// name rule's, which names the version that holds the name. Nothing is published for either.
func TestUploadConflicts(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	hello, info := testutil.Deb(testutil.DebOptions{})
	other, _ := testutil.Deb(testutil.DebOptions{Description: "another greeting"})
	epoch, epochInfo := testutil.Deb(testutil.DebOptions{Version: "1:1.0.0-1"})
	if epochInfo.Filename != info.Filename {
		t.Fatalf("the epoch fixture is %s, want %s", epochInfo.Filename, info.Filename)
	}
	first := uploaded(t, e.put("suite=jammy&component=main", hello))
	pushes := len(e.prov.PushReferences())

	rr := e.put("suite=jammy&component=main", other)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "1.0.0-1__jammy_2fmain_2famd64") ||
		!strings.Contains(rr.Body.String(), first.Digest) {
		t.Errorf("other bytes under the tag = %d %s, want the 409 of the tag naming %s", rr.Code, rr.Body, first.Digest)
	}
	poolRule := "pool file name " + info.Filename + " is held by version 1.0.0-1 in jammy/main with another content; " +
		"a Debian pool holds one content per file name"
	for _, c := range []struct {
		query string
		deb   []byte
	}{
		{"suite=jammy-updates&component=main", other},
		{"suite=noble&component=universe", other},
		{"suite=jammy&component=main", epoch},
	} {
		if rr := e.put(c.query, c.deb); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), poolRule) {
			t.Errorf("PUT ?%s = %d %s, want 409 with %q", c.query, rr.Code, rr.Body, poolRule)
		}
	}
	if n := len(e.prov.PushReferences()); n != pushes {
		t.Errorf("%d manifests pushed by refused uploads", n-pushes)
	}
}

// TestUploadRefuses checks the 400s of the upload route, before anything is published: suite
// or component missing or not of the grammar of Build, named; a body that is not a .deb, empty,
// or shorter than its Content-Length, named after the client's file name when it sends one.
func TestUploadRefuses(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	for _, c := range []struct {
		query string
		body  []byte
		hdr   []string
		want  string
	}{
		{"component=main", hello, nil, "parameter suite is missing"},
		{"suite=jammy", hello, nil, "parameter component is missing"},
		{"", hello, nil, "parameter suite is missing"},
		{"suite=../x&component=main", hello, nil, `parameter suite "../x" is not lowercase letters`},
		{"suite=jammy&component=Main", hello, nil, `parameter component "Main" is not lowercase letters`},
		{"suite=jammy&component=main&arch=arm64", hello, nil, `the upload is for architecture "arm64", the control file`},
		{"suite=jammy&component=main&arch=AMD64", hello, nil, `the upload is for architecture "AMD64"`},
		{"suite=jammy&component=main", []byte("not a package"), nil, `deb: "upload.deb": format: invalid: not an ar archive`},
		{"suite=jammy&component=main", nil, nil, `deb: "upload.deb": format: invalid: not an ar archive`},
		{"suite=jammy&component=main", []byte("not a package"), []string{"Content-Disposition", `attachment; filename="../x/hello.deb"`},
			`deb: "hello.deb": format: invalid`},
		{"suite=jammy&component=main", hello[:100], nil, `format: invalid`},
	} {
		rr := e.put(c.query, c.body, c.hdr...)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("PUT ?%s = %d %s, want 400 with %q", c.query, rr.Code, rr.Body, c.want)
		}
	}
	req := httptest.NewRequest(http.MethodPut, "/apt/ubuntu/upload?suite=jammy&component=main", bytes.NewReader(hello[:100]))
	req.ContentLength = int64(len(hello))
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "Content-Length says") {
		t.Errorf("PUT of a body shorter than its Content-Length = %d %s", rr.Code, rr.Body)
	}
	if n := len(e.prov.Pushes()) + len(e.prov.PushReferences()); n != 0 {
		t.Errorf("refused uploads pushed %d blobs and manifests", n)
	}
}

// TestUploadMaxFileSize checks policy.maxFileSize through format.LimitBody: above the limit 413
// naming it, with a declared length and without one, nothing published; a policy that cannot
// be read is 500.
func TestUploadMaxFileSize(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	repo := e.repo
	repo.Policy = []byte(`{"maxFileSize": 100}`)
	must(t, e.st.UpdateRepo(t.Context(), &repo))
	for _, body := range []io.Reader{bytes.NewReader(hello), io.MultiReader(bytes.NewReader(hello))} {
		rr := e.send(http.MethodPut, "upload?suite=jammy&component=main", body)
		if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "larger than the maxFileSize of apt/ubuntu, 100 bytes") {
			t.Errorf("PUT of %d bytes = %d %s", len(hello), rr.Code, rr.Body)
		}
	}
	repo.Policy = []byte(`{"maxFileSize": -1}`)
	must(t, e.st.UpdateRepo(t.Context(), &repo))
	if rr := e.put("suite=jammy&component=main", hello); rr.Code != http.StatusInternalServerError {
		t.Errorf("PUT with an unreadable policy = %d", rr.Code)
	}
	if n := len(e.prov.Pushes()) + len(e.prov.PushReferences()); n != 0 {
		t.Errorf("refused uploads pushed %d blobs and manifests", n)
	}
}

// writers lets alice write apt/ubuntu, bob read it, and nobody else anything.
type writers struct{}

func (writers) Allowed(_ context.Context, p auth.Principal, a auth.Action, f, r string) (bool, error) {
	if f != Name || r != "ubuntu" {
		return false, nil
	}
	return p.Name == "alice" || (p.Name == "bob" && a == auth.ActionRead), nil
}

// TestUploadAuthorize checks that the upload needs write: the anonymous principal gets 401
// with a Basic challenge, bob, a reader, 403, carol 404, and alice 201 with uploaded-by
// user:alice in the manifest; the pool needs read.
func TestUploadAuthorize(t *testing.T) {
	e := newAptEnv(t, envOptions{authz: writers{}})
	hello, info := testutil.Deb(testutil.DebOptions{})
	for _, c := range []struct {
		user string
		code int
	}{{"", http.StatusUnauthorized}, {"bob", http.StatusForbidden}, {"carol", http.StatusNotFound}} {
		rr := e.send(http.MethodPut, "upload?suite=jammy&component=main", bytes.NewReader(hello), "X-Test-User", c.user)
		if rr.Code != c.code {
			t.Errorf("PUT as %q = %d, want %d", c.user, rr.Code, c.code)
		}
	}
	resp := uploaded(t, e.put("suite=jammy&component=main", hello))
	m := e.manifest(t, resp.Digest)
	if m.Annotations["lajstrom.uploaded-by"] != "user:alice" {
		t.Errorf("lajstrom.uploaded-by = %q", m.Annotations["lajstrom.uploaded-by"])
	}
	path := "pool/main/h/hello/" + info.Filename
	if rr := e.do(http.MethodGet, path); rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET of the pool = %d", rr.Code)
	}
	if rr := e.do(http.MethodGet, path, "X-Test-User", "bob"); rr.Code != http.StatusOK {
		t.Errorf("bob's GET of the pool = %d", rr.Code)
	}
}

// manifest returns the manifest d of the hello repository in the fake registry.
func (e *aptEnv) manifest(t *testing.T, d string) struct {
	Annotations map[string]string `json:"annotations"`
} {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), "apt/ubuntu/hello")
	must(t, err)
	desc, err := rr.Resolve(t.Context(), d)
	must(t, err)
	rc, err := rr.Fetch(t.Context(), desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	var m struct {
		Annotations map[string]string `json:"annotations"`
	}
	must(t, json.NewDecoder(rc).Decode(&m))
	return m
}

// TestUploadFailures checks the 500s: no publisher, FindFiles failing; and checkPoolName on a
// file that cannot be opened, or read for its digest.
func TestUploadFailures(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	hello, _ := testutil.Deb(testutil.DebOptions{})
	deps := format.Deps{Index: e.st, Providers: provider.Map{"local": e.prov}, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/apt/ubuntu/upload?suite=jammy&component=main", bytes.NewReader(hello)))
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("PUT without a publisher = %d", rr.Code)
	}

	h := &handlers{deps: format.Deps{Index: findFailing{e.st}, Log: slog.New(slog.DiscardHandler)}}
	if _, err := h.checkPoolName(t.Context(), e.repo, debFile(hello), "jammy", "main"); !errors.Is(err, errBoom) {
		t.Errorf("checkPoolName with FindFiles failing = %v", err)
	}

	h = &handlers{deps: format.Deps{Index: e.st, Log: slog.New(slog.DiscardHandler)}}
	unopenable := format.UploadedFile{Name: "x.deb", Size: -1, Open: func() (io.ReadCloser, error) { return nil, errBoom }}
	if _, err := h.checkPoolName(t.Context(), e.repo, unopenable, "jammy", "main"); !errors.Is(err, errBoom) {
		t.Errorf("checkPoolName of a file that cannot be opened = %v", err)
	}
	e.upload(t, hello, "jammy")
	other, _ := testutil.Deb(testutil.DebOptions{Description: "another greeting"})
	opens := 0
	once := format.UploadedFile{Name: "x.deb", Size: int64(len(other)), Open: func() (io.ReadCloser, error) {
		opens++
		if opens > 1 {
			return io.NopCloser(failAfter(other[:10])), nil
		}
		return io.NopCloser(bytes.NewReader(other)), nil
	}}
	if _, err := h.checkPoolName(t.Context(), e.repo, once, "noble", "main"); !errors.Is(err, errBoom) {
		t.Errorf("checkPoolName of a file that cannot be read for its digest = %v", err)
	}
	opens = 0
	once.Open = func() (io.ReadCloser, error) {
		opens++
		if opens > 1 {
			return nil, errBoom
		}
		return io.NopCloser(bytes.NewReader(other)), nil
	}
	if _, err := h.checkPoolName(t.Context(), e.repo, once, "noble", "main"); !errors.Is(err, errBoom) {
		t.Errorf("checkPoolName of a file that cannot be opened for its digest = %v", err)
	}
	if d, err := digestOf(debFile(hello)); err != nil || d != digest.FromBytes(hello) {
		t.Errorf("digestOf = %s, %v", d, err)
	}
}

// TestUploadName checks the file name of an upload from Content-Disposition.
func TestUploadName(t *testing.T) {
	for cd, want := range map[string]string{
		"":           defaultUploadName,
		"attachment": defaultUploadName,
		`attachment; filename="hello_1.0_all.deb"`: "hello_1.0_all.deb",
		`attachment; filename="../../etc/passwd"`:  "passwd",
		`attachment; filename="C:\\x\\a.deb"`:      "a.deb",
		`attachment; filename=".."`:                defaultUploadName,
		`attachment; filename="/"`:                 defaultUploadName,
		`attachment; filename=`:                    defaultUploadName,
		"not a disposition;;":                      defaultUploadName,
	} {
		h := http.Header{}
		if cd != "" {
			h.Set("Content-Disposition", cd)
		}
		if got := uploadName(h); got != want {
			t.Errorf("uploadName(%q) = %q, want %q", cd, got, want)
		}
	}
}
