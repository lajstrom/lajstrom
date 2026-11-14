package deb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/testutil"
)

// part is a file of a multipart body: its file name and bytes; form names the part, "file"
// when empty, and a part without a file name is a form field.
type part struct {
	name    string
	content []byte
	form    string
}

// partsOf are the parts of a source package: the .dsc, then the files it names.
func partsOf(d testutil.DscInfo) []part {
	parts := []part{{name: d.Filename, content: d.Dsc}}
	for _, f := range d.Files {
		parts = append(parts, part{name: f.Name, content: f.Content})
	}
	return parts
}

// multipartBody returns the multipart/form-data body of parts and its Content-Type.
func multipartBody(t *testing.T, parts []part) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, p := range parts {
		form := p.form
		if form == "" {
			form = "file"
		}
		var pw io.Writer
		var err error
		if p.name == "" {
			pw, err = w.CreateFormField(form)
		} else {
			pw, err = w.CreateFormFile(form, p.name)
		}
		must(t, err)
		_, err = pw.Write(p.content)
		must(t, err)
	}
	must(t, w.Close())
	return &b, w.FormDataContentType()
}

// post sends a source upload of parts to upload?<query> as alice.
func (e *aptEnv) post(query string, parts []part) *httptest.ResponseRecorder {
	e.t.Helper()
	body, ct := multipartBody(e.t, parts)
	return e.send(http.MethodPost, "upload?"+query, body, "X-Test-User", "alice", "Content-Type", ct)
}

// TestUploadSource checks the source upload: 201 with Location (the .dsc in the pool),
// Docker-Content-Digest and the JSON body; the .dsc and the files served from the pool with
// their content types; the same files again are the repeat; a second revision without its
// .orig.tar.gz takes it from the pool; the sessions are closed.
func TestUploadSource(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	d := testutil.Dsc(testutil.DscOptions{})
	rr := e.post("suite=jammy&component=main", partsOf(d))
	resp := uploaded(t, rr)
	want := uploadResponse{Package: "hello", Version: "1.0.0-1", Architecture: ArchSource, Suite: "jammy", Component: "main",
		Filename: d.Filename, Digest: resp.Digest, Tag: "1.0.0-1__jammy_2fmain_2fsource"}
	if resp != want {
		t.Errorf("body = %+v, want %+v", resp, want)
	}
	if loc := rr.Header().Get("Location"); loc != "/apt/ubuntu/pool/main/h/hello/"+d.Filename || rr.Header().Get("Docker-Content-Digest") != resp.Digest {
		t.Errorf("headers %v", rr.Header())
	}
	for _, f := range []struct {
		name        string
		content     []byte
		contentType string
	}{
		{d.Filename, d.Dsc, "text/plain; charset=utf-8"},
		{d.Files[0].Name, d.Files[0].Content, "application/gzip"},
		{d.Files[1].Name, d.Files[1].Content, "application/x-xz"},
	} {
		got := e.get("pool/main/h/hello/" + f.name)
		if !bytes.Equal(got.Body.Bytes(), f.content) || got.Header().Get("Content-Type") != f.contentType {
			t.Errorf("GET %s: %d bytes, %s; want %d bytes, %s", f.name, got.Body.Len(), got.Header().Get("Content-Type"), len(f.content), f.contentType)
		}
	}
	if again := uploaded(t, e.post("suite=jammy&component=main", partsOf(d))); again.Digest != resp.Digest {
		t.Errorf("the repeat: %s, want %s", again.Digest, resp.Digest)
	}
	second := testutil.Dsc(testutil.DscOptions{Version: "1.0.0-2"})
	parts := partsOf(second)
	uploaded(t, e.post("suite=jammy&component=main", []part{parts[0], parts[2]})) // the .orig.tar.gz from the pool
	sessionsClosed(t, e)
}

// sessionsClosed checks that no publication session of the repo is open.
func sessionsClosed(t *testing.T, e *aptEnv) {
	t.Helper()
	for _, key := range []string{"hello:1.0.0-1", "hello:1.0.0-2"} {
		if s, err := e.st.GetSession(t.Context(), e.repo.ID, key); err == nil {
			t.Errorf("the session %s is %s", key, s.State)
		}
	}
}

// TestUploadSourceRefuses: a source upload that is not a source package is refused with the
// status and message of each rule, publishes nothing and leaves no session open.
func TestUploadSourceRefuses(t *testing.T) {
	d := testutil.Dsc(testutil.DscOptions{})
	dsc, orig, debian := part{name: d.Filename, content: d.Dsc}, partsOf(d)[1], partsOf(d)[2]
	otherOrig := testutil.Dsc(testutil.DscOptions{Version: "1.0.0-2", Files: map[string]string{"README": "other\n"}})
	for _, tc := range []struct {
		name, query string
		parts       []part
		status      int
		want        string
	}{
		{"no suite", "component=main", partsOf(d), http.StatusBadRequest, "parameter suite is missing"},
		{"no file", "suite=jammy&component=main", nil, http.StatusBadRequest, "the body has no file"},
		{"not a .dsc first", "suite=jammy&component=main", []part{orig, dsc}, http.StatusBadRequest, "the first file of a source upload is the .dsc"},
		{"a form field", "suite=jammy&component=main", []part{{form: "suite", content: []byte("jammy")}}, http.StatusBadRequest,
			`the part "suite" of the body is not a file`},
		{"a .dsc renamed", "suite=jammy&component=main", []part{{name: "x.dsc", content: d.Dsc}}, http.StatusBadRequest,
			"the .dsc is called x.dsc"},
		{"a broken .dsc", "suite=jammy&component=main", []part{{name: d.Filename, content: []byte("Format: 3.0\n")}}, http.StatusBadRequest,
			"has no Source field"},
		{"a file it does not name", "suite=jammy&component=main", []part{dsc, {name: "hello_x.tar.gz", content: []byte("x")}},
			http.StatusBadRequest, "hello_x.tar.gz is not a file hello_1.0.0-1.dsc names"},
		{"a file twice", "suite=jammy&component=main", []part{dsc, orig, orig}, http.StatusBadRequest, "or comes twice"},
		{"other bytes", "suite=jammy&component=main", []part{dsc, {name: orig.name, content: []byte("other")}, debian},
			http.StatusBadRequest, orig.name + " has 5 bytes, the .dsc says"},
		{"a file missing", "suite=jammy&component=main", []part{dsc, debian}, http.StatusBadRequest,
			"names " + orig.name + ", which the body lacks and no version of hello in the pool holds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAptEnv(t, envOptions{})
			rr := e.post(tc.query, tc.parts)
			if rr.Code != tc.status || !strings.Contains(rr.Body.String(), tc.want) {
				t.Errorf("POST = %d %s, want %d with %q", rr.Code, rr.Body, tc.status, tc.want)
			}
			if n := len(e.prov.PushReferences()); n != 0 {
				t.Errorf("the refused upload pushed %d manifests", n)
			}
			sessionsClosed(t, e)
		})
	}
	t.Run("not multipart", func(t *testing.T) {
		e := newAptEnv(t, envOptions{})
		rr := e.send(http.MethodPost, "upload?suite=jammy&component=main", bytes.NewReader(d.Dsc), "X-Test-User", "alice")
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "multipart/form-data") {
			t.Errorf("POST of a bare .dsc = %d %s", rr.Code, rr.Body)
		}
	})
	t.Run("the pool holds another .orig", func(t *testing.T) {
		e := newAptEnv(t, envOptions{})
		uploaded(t, e.post("suite=jammy&component=main", partsOf(d)))
		rr := e.post("suite=jammy&component=main", partsOf(otherOrig))
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "pool file name "+orig.name+" is held by version 1.0.0-1 in jammy/main") {
			t.Errorf("POST of another .orig under the name = %d %s", rr.Code, rr.Body)
		}
	})
	t.Run("maxFileSize", func(t *testing.T) {
		e := newAptEnv(t, envOptions{})
		repo := e.repo
		repo.Policy = []byte(`{"maxFileSize": 100}`)
		must(t, e.st.UpdateRepo(t.Context(), &repo))
		rr := e.post("suite=jammy&component=main", partsOf(d))
		if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "larger than the maxFileSize of apt/ubuntu") {
			t.Errorf("POST over maxFileSize = %d %s", rr.Code, rr.Body)
		}
		sessionsClosed(t, e)
	})
}

// TestSourcesRoutes serves the Sources of a suite by name and by hash, as the Release lists them.
func TestSourcesRoutes(t *testing.T) {
	e := newAptEnv(t, envOptions{})
	d := testutil.Dsc(testutil.DscOptions{})
	uploaded(t, e.post("suite=jammy&component=main", partsOf(d)))
	release := e.get("dists/jammy/Release").Body.Bytes()
	sums := listed(release)
	for _, kind := range []string{"main/source/Sources", "main/source/Sources.gz", "main/source/Sources.xz"} {
		body := e.get("dists/jammy/" + kind).Body.Bytes()
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != sums[kind] {
			t.Errorf("%s is not the file the Release lists", kind)
		}
		if got := e.get("dists/jammy/main/source/by-hash/SHA256/" + sums[kind]).Body.Bytes(); !bytes.Equal(got, body) {
			t.Errorf("by-hash of %s is another file", kind)
		}
	}
	if !strings.Contains(e.get("dists/jammy/main/source/Sources").Body.String(), "Directory: pool/main/h/hello\n") {
		t.Error("Sources has no Directory")
	}
}
