package deb

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// at sends a request with body, which may be nil, to /apt/<repo>/<path> as alice.
func (e *aptEnv) at(repo, method, path string, body io.Reader) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/apt/"+repo+"/"+path, body)
	req.Header.Set("X-Test-User", "alice")
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

// getAt sends a GET of /apt/<repo>/<path>, which must answer 200, and returns the body.
func (e *aptEnv) getAt(repo, path string) []byte {
	e.t.Helper()
	rr := e.at(repo, http.MethodGet, path, nil)
	if rr.Code != http.StatusOK {
		e.t.Fatalf("GET /apt/%s/%s = %d %s", repo, path, rr.Code, rr.Body)
	}
	return rr.Body.Bytes()
}

// addRepo creates the hosted apt repo name in the store of the pipeline.
func (e *aptEnv) addRepo(name string) index.Repo {
	e.t.Helper()
	r := index.Repo{Format: Name, Name: name, Registry: "local", Prefix: "apt/" + name, Kind: index.KindHosted}
	must(e.t, e.st.CreateRepo(e.t.Context(), &r))
	return r
}

// addGroup creates the apt group name of members, in order.
func (e *aptEnv) addGroup(name string, members ...index.Repo) index.Repo {
	e.t.Helper()
	g := index.Repo{Format: Name, Name: name, Prefix: "apt/" + name, Kind: index.KindGroup}
	must(e.t, e.st.CreateRepo(e.t.Context(), &g))
	ids := make([]int64, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	must(e.t, e.st.SetGroupMembers(e.t.Context(), g.ID, ids))
	return g
}

// uploadIn uploads files to suite/main of repo as alice.
func (e *aptEnv) uploadIn(repo index.Repo, suite string, files ...format.UploadedFile) {
	e.t.Helper()
	if _, err := e.pipe.Upload(e.t.Context(), repo, upload(files, ParamSuite, suite, ParamComponent, "main"), alice); err != nil {
		e.t.Fatalf("upload to apt/%s %s: %v", repo.Name, suite, err)
	}
}

// groupDebs are the packages of newGroupEnv.
type groupDebs struct {
	first, other, newer, world []byte
}

// newGroupEnv returns a signed aptEnv with the hosted repos ubuntu and extra, the group all of
// both and the group solo of extra alone, and the packages it uploaded: first (hello 1.0.0-1) to
// jammy of ubuntu and of extra, newer (hello 2.0-1) to jammy of extra, other (hello 1.0.0-1 with
// other bytes) and world to noble of extra, and the source package world to jammy of extra.
func newGroupEnv(t *testing.T) (*aptEnv, index.Repo, groupDebs) {
	t.Helper()
	e, _ := newSignedEnv(t, envOptions{})
	extra := e.addRepo("extra")
	e.addGroup("all", e.repo, extra)
	e.addGroup("solo", extra)
	var d groupDebs
	d.first, _ = testutil.Deb(testutil.DebOptions{})
	d.other, _ = testutil.Deb(testutil.DebOptions{Files: map[string]string{"usr/share/hello/other": "other bytes\n"}})
	d.newer, _ = testutil.Deb(testutil.DebOptions{Version: "2.0-1"})
	d.world, _ = testutil.Deb(testutil.DebOptions{Name: "world"})
	e.upload(t, d.first, "jammy")
	e.uploadIn(extra, "jammy", debFile(d.first)) // the same bytes: the first member's paragraph
	e.uploadIn(extra, "jammy", debFile(d.newer))
	e.uploadIn(extra, "noble", debFile(d.other)) // the file name of the first member's, other bytes
	e.uploadIn(extra, "noble", debFile(d.world))
	e.uploadIn(extra, "jammy", sourceFiles(testutil.Dsc(testutil.DscOptions{Source: "world"}))...)
	return e, extra, d
}

// TestGroupIndexes: an apt group serves the suites of its hosted members merged, the first
// member's version of a package, version and architecture, without a later member's version
// whose pool file name an earlier member holds with other bytes, in a Release of its own that its
// own key signs, by name and by hash (docs/spec/http-routing.md, apt, Group).
func TestGroupIndexes(t *testing.T) {
	e, _, d := newGroupEnv(t)
	packages := string(e.getAt("all", "dists/jammy/main/binary-amd64/Packages"))
	for _, want := range []string{"Package: hello\nVersion: 1.0.0-1\n", "SHA256: " + sha256Hex(d.first) + "\n",
		"Package: hello\nVersion: 2.0-1\n", "SHA256: " + sha256Hex(d.newer) + "\n"} {
		if !strings.Contains(packages, want) {
			t.Errorf("the group's Packages of jammy lacks %q:\n%s", want, packages)
		}
	}
	if strings.Count(packages, "Package: hello\nVersion: 1.0.0-1\n") != 1 {
		t.Errorf("the group's Packages lists a version of two members twice:\n%s", packages)
	}
	if sources := string(e.getAt("all", "dists/jammy/main/source/Sources")); !strings.Contains(sources, "Package: world\n") {
		t.Errorf("the group's Sources of jammy lacks the member's source package:\n%s", sources)
	}
	noble := string(e.getAt("all", "dists/noble/main/binary-amd64/Packages"))
	if !strings.Contains(noble, "Package: world\n") || strings.Contains(noble, "Package: hello\n") {
		t.Errorf("the group's Packages of noble, a suite of its second member only, without the .deb whose file name "+
			"the first member holds with other bytes:\n%s", noble)
	}
	if rr := e.at("all", http.MethodGet, "dists/focal/Release", nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET of a suite no member has = %d, want 404", rr.Code)
	}

	release := e.getAt("all", "dists/jammy/Release")
	if !bytes.Contains(release, []byte("Label: all\n")) || !bytes.Contains(release, []byte("Description: apt/all jammy\n")) {
		t.Errorf("the group's Release is not the group's:\n%s", release)
	}
	sum := listed(release)["main/binary-amd64/Packages.xz"]
	if got := e.getAt("all", "dists/jammy/main/binary-amd64/by-hash/SHA256/"+sum); sha256Hex(got) != sum {
		t.Errorf("by-hash of the group's Packages.xz served another document")
	}
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(e.getAt("all", "key.asc")))
	must(t, err)
	if fps := fingerprintsOf(keyring); len(fps) != 1 || slices.Equal(fps, fingerprintsOf(e.keyring())) {
		t.Errorf("the group's key %v is not its own, beside its member's %v", fps, fingerprintsOf(e.keyring()))
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(keyring, bytes.NewReader(release),
		bytes.NewReader(e.getAt("all", "dists/jammy/Release.gpg")), nil); err != nil {
		t.Errorf("the group's Release.gpg does not verify with the group's key: %v", err)
	}
}

// TestGroupPool: a pool file of a group is the first member's that has it, and a member's
// publish is in the group's indexes on the next request (docs/spec/proxy-and-group.md §8.6).
func TestGroupPool(t *testing.T) {
	e, extra, d := newGroupEnv(t)
	if got := e.getAt("all", "pool/main/h/hello/hello_1.0.0-1_amd64.deb"); !bytes.Equal(got, d.first) {
		t.Errorf("the group's pool file of two members is not the first member's")
	}
	if got := e.getAt("all", "pool/main/h/hello/hello_2.0-1_amd64.deb"); !bytes.Equal(got, d.newer) {
		t.Errorf("the group's pool file of the second member is not its file")
	}
	if rr := e.at("all", http.MethodGet, "pool/main/n/nothing/nothing_1_amd64.deb", nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET of a pool file no member has = %d, want 404", rr.Code)
	}
	e.getAt("all", "dists/jammy/Release")
	late, _ := testutil.Deb(testutil.DebOptions{Name: "late"})
	e.uploadIn(extra, "jammy", debFile(late))
	if packages := string(e.getAt("all", "dists/jammy/main/binary-amd64/Packages")); !strings.Contains(packages, "Package: late\n") {
		t.Errorf("the group's Packages after a member's publish lacks it:\n%s", packages)
	}
}

// TestGroupUpload: an upload through a group goes to its one member, and is 405 through a group
// of several (docs/spec/proxy-and-group.md §8.4).
func TestGroupUpload(t *testing.T) {
	e, _, _ := newGroupEnv(t)
	put, _ := testutil.Deb(testutil.DebOptions{Name: "put"})
	if rr := e.at("all", http.MethodPut, "upload?suite=jammy&component=main", bytes.NewReader(put)); rr.Code != http.StatusMethodNotAllowed ||
		rr.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("PUT through a group of two members = %d %v %s, want 405 with Allow", rr.Code, rr.Header(), rr.Body)
	}
	rr := e.at("solo", http.MethodPut, "upload?suite=jammy&component=main", bytes.NewReader(put))
	if rr.Code != http.StatusCreated || rr.Header().Get("Location") != "/apt/extra/pool/main/p/put/put_1.0.0-1_amd64.deb" {
		t.Errorf("PUT through a group of one member = %d %v %s, want 201 in the member", rr.Code, rr.Header(), rr.Body)
	}
	if got := e.getAt("extra", "pool/main/p/put/put_1.0.0-1_amd64.deb"); !bytes.Equal(got, put) {
		t.Errorf("the upload through the group is not in its member")
	}
}
