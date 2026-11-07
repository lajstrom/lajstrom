package raw

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

const fileTxt = "docs/file.txt"

// tagOf is the tag, and version, of content.
func tagOf(content string) string { return digest.FromString(content).Encoded()[:16] }

// TestPutGetHead is R-01: PUT answers 201 with Location, Docker-Content-Digest and the JSON
// body; GET returns the bytes with Content-Length, Content-Type from the metadata, ETag (the
// blob digest), Last-Modified and Accept-Ranges; HEAD has the same headers and no body;
// If-None-Match with the ETag is 304.
func TestPutGetHead(t *testing.T) {
	e := newEnv(t, envOptions{})
	const content = "hello raw\n"
	rr := e.do(http.MethodPut, target(fileTxt), strings.NewReader(content), "Content-Type", "text/plain")
	if rr.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body)
	}
	tag := tagOf(content)
	manifest, err := e.prov.Repository(t.Context(), "raw/scratch/docs/file.txt")
	must(t, err)
	desc, err := manifest.Resolve(t.Context(), tag)
	must(t, err)
	header(t, rr, "Location", "/raw/scratch/docs/file.txt")
	header(t, rr, "Docker-Content-Digest", desc.Digest.String())
	header(t, rr, "Content-Type", "application/json")
	var resp putResponse
	must(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	created, err := time.Parse(time.RFC3339, resp.Created)
	if err != nil || resp != (putResponse{Path: fileTxt, Version: tag, Digest: desc.Digest.String(), Size: 10, Created: resp.Created}) {
		t.Errorf("PUT body = %+v (%v)", resp, err)
	}

	want := map[string]string{
		"Content-Length": "10", "Content-Type": "text/plain", "ETag": `"` + digest.FromString(content).String() + `"`,
		"Last-Modified": created.Format(http.TimeFormat), "Accept-Ranges": "bytes", "Docker-Content-Digest": desc.Digest.String(),
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rr := e.do(method, target(fileTxt), nil)
		wantBody := content
		if method == http.MethodHead {
			wantBody = ""
		}
		if rr.Code != http.StatusOK || rr.Body.String() != wantBody {
			t.Errorf("%s = %d %q", method, rr.Code, rr.Body)
		}
		for name, value := range want {
			header(t, rr, name, value)
		}
	}
	if rr := e.do(http.MethodGet, target(fileTxt), nil, "If-None-Match", want["ETag"]); rr.Code != http.StatusNotModified {
		t.Errorf("GET with If-None-Match = %d", rr.Code)
	}
	if a := e.manifest("raw/scratch/docs/file.txt", desc.Digest); a[profile.AnnotationUploadedBy] != "anonymous" || a[profile.AnnotationRawPath] != fileTxt {
		t.Errorf("manifest annotations = %v", a)
	}
}

// TestPutVersions is R-02 and R-03: the same bytes again are 201 with the same digest and one
// version; new bytes are a new version that GET returns, ?digest= pins the old one, the digest
// of another path is 404 there, and a pin that is not a digest is 400.
func TestPutVersions(t *testing.T) {
	e := newEnv(t, envOptions{})
	first := e.put(fileTxt, "first")
	again := e.put(fileTxt, "first")
	if again.Digest != first.Digest || len(e.versions(fileTxt)) != 1 || len(e.prov.PushReferences()) != 1 {
		t.Errorf("repeat = %+v after %+v: %d versions, %d manifest pushes", again, first, len(e.versions(fileTxt)), len(e.prov.PushReferences()))
	}
	second := e.put(fileTxt, "second")
	if second.Digest == first.Digest || second.Version != tagOf("second") || len(e.versions(fileTxt)) != 2 {
		t.Errorf("new bytes = %+v: %d versions", second, len(e.versions(fileTxt)))
	}
	other := e.put("docs/other.txt", "other")
	tests := []struct {
		query  string
		status int
		body   string
	}{
		{"", http.StatusOK, "second"},
		{"?digest=" + first.Digest, http.StatusOK, "first"},
		{"?digest=" + second.Digest, http.StatusOK, "second"},
		{"?digest=" + other.Digest, http.StatusNotFound, ""},
		{"?digest=" + digest.FromString("unknown").String(), http.StatusNotFound, ""},
		{"?digest=latest", http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		rr := e.do(http.MethodGet, target(fileTxt)+tt.query, nil)
		if rr.Code != tt.status || (tt.body != "" && rr.Body.String() != tt.body) {
			t.Errorf("GET%s = %d %q, want %d %q", tt.query, rr.Code, rr.Body, tt.status, tt.body)
		}
	}
}

// TestRange checks R-06 as far as the frontend goes: a single range is 206 with those bytes,
// several ranges are the whole file with 200, a range past the end is 416.
func TestRange(t *testing.T) {
	e := newEnv(t, envOptions{})
	var b strings.Builder
	for i := range 4000 {
		b.WriteByte(byte('a' + i%26))
	}
	content := b.String()
	e.put("big.bin", content)
	rr := e.do(http.MethodGet, target("big.bin"), nil, "Range", "bytes=1000-1999")
	if rr.Code != http.StatusPartialContent || rr.Body.String() != content[1000:2000] {
		t.Errorf("single range = %d, %d bytes", rr.Code, rr.Body.Len())
	}
	header(t, rr, "Content-Range", "bytes 1000-1999/4000")
	header(t, rr, "Content-Length", "1000")
	if rr := e.do(http.MethodGet, target("big.bin"), nil, "Range", "bytes=0-9,20-29"); rr.Code != http.StatusOK || rr.Body.String() != content {
		t.Errorf("multi-range = %d, %d bytes; want 200 and the whole file", rr.Code, rr.Body.Len())
	}
	if rr := e.do(http.MethodGet, target("big.bin"), nil, "Range", "bytes=5000-"); rr.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("range past the end = %d", rr.Code)
	}
}

// TestRedirect checks that GET and HEAD answer 307 to the URL the provider gives.
func TestRedirect(t *testing.T) {
	e := newEnv(t, envOptions{})
	resp := e.put(fileTxt, "redirected")
	e.prov.RedirectURL = func(name string, d digest.Digest) (string, bool) {
		return "https://blobs.test/" + name + "?d=" + d.Encoded(), true
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rr := e.do(method, target(fileTxt), nil)
		if rr.Code != http.StatusTemporaryRedirect {
			t.Errorf("%s = %d, want 307", method, rr.Code)
		}
		header(t, rr, "Location", "https://blobs.test/raw/scratch/docs/file.txt?d="+digest.FromString("redirected").Encoded())
		header(t, rr, "Docker-Content-Digest", resp.Digest)
	}
}

// listingOf returns the paths and digests of a listing response.
func listingOf(t *testing.T, body []byte) listing {
	t.Helper()
	var l listing
	must(t, json.Unmarshal(body, &l))
	return l
}

// TestListing is R-04: the listing of the root and of a prefix, recursive, both versions of a
// path, sorted, with the manifest digests and the sizes; served with ETag (304 on a match) and
// rendered again after a publish; a prefix without paths is 404.
func TestListing(t *testing.T) {
	e := newEnv(t, envOptions{})
	if rr := e.do(http.MethodGet, "/raw/scratch/", nil); rr.Code != http.StatusOK || rr.Body.String() != `{"entries":[]}` {
		t.Errorf("listing of an empty repo = %d %s", rr.Code, rr.Body)
	}
	b1 := e.put("docs/b.txt", "b one")
	b2 := e.put("docs/b.txt", "b two!")
	a := e.put("docs/a.txt", "a")
	top := e.put("top.txt", "top")
	entry := func(r putResponse) listingEntry {
		return listingEntry{Path: r.Path, Size: r.Size, Digest: r.Digest, Created: r.Created}
	}
	docs := []listingEntry{entry(a), entry(b1), entry(b2)}
	rr := e.do(http.MethodGet, "/raw/scratch/", nil)
	header(t, rr, "Content-Type", "application/json")
	if got := listingOf(t, rr.Body.Bytes()); rr.Code != http.StatusOK || !equalEntries(got.Entries, append(docs, entry(top))) {
		t.Errorf("root listing = %d %s", rr.Code, rr.Body)
	}
	if rr := e.do(http.MethodGet, "/raw/scratch/docs/", nil); !equalEntries(listingOf(t, rr.Body.Bytes()).Entries, docs) {
		t.Errorf("docs/ listing = %d %s", rr.Code, rr.Body)
	}
	etag := rr.Header().Get("ETag")
	if rr := e.do(http.MethodGet, "/raw/scratch/", nil, "If-None-Match", etag); rr.Code != http.StatusNotModified {
		t.Errorf("listing with If-None-Match = %d", rr.Code)
	}
	if rr := e.do(http.MethodHead, "/raw/scratch/", nil); rr.Code != http.StatusOK || rr.Body.Len() != 0 {
		t.Errorf("HEAD of the listing = %d %q", rr.Code, rr.Body)
	}
	e.put("docs/c.txt", "c")
	if rr := e.do(http.MethodGet, "/raw/scratch/", nil, "If-None-Match", etag); rr.Code != http.StatusOK || len(listingOf(t, rr.Body.Bytes()).Entries) != 5 {
		t.Errorf("listing after a publish = %d %s", rr.Code, rr.Body)
	}
	for _, p := range []string{"/raw/scratch/nothing/", "/raw/scratch/doc/"} {
		if rr := e.do(http.MethodGet, p, nil); rr.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rr.Code)
		}
	}
}

func equalEntries(a, b []listingEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDelete is R-07 on a backend that deletes: 204, then GET is 404, the manifests are gone
// from the registry, one audit row per version with the principal and the address, the
// listing forgets the path, other paths stay; a second DELETE and a path that never was are 404.
func TestDelete(t *testing.T) {
	e := newEnv(t, envOptions{})
	v1, v2 := e.put(fileTxt, "one"), e.put(fileTxt, "two")
	kept := e.put("docs/kept.txt", "kept")
	if rr := e.do(http.MethodGet, "/raw/scratch/", nil); len(listingOf(t, rr.Body.Bytes()).Entries) != 3 {
		t.Fatalf("listing before the delete: %s", rr.Body)
	}
	if rr := e.do(http.MethodDelete, target(fileTxt), nil); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", rr.Code, rr.Body)
	}
	if rr := e.do(http.MethodGet, target(fileTxt), nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d", rr.Code)
	}
	for _, v := range []putResponse{v1, v2} {
		if a := e.manifest("raw/scratch/docs/file.txt", digest.Digest(v.Digest)); a != nil {
			t.Errorf("manifest %s still in the registry", v.Digest)
		}
	}
	if _, err := e.st.GetPackage(t.Context(), e.repo.ID, fileTxt); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("package after DELETE: %v", err)
	}
	checkDeleteAudit(t, e.audit("delete"), v1, v2)
	rr := e.do(http.MethodGet, "/raw/scratch/", nil)
	if got := listingOf(t, rr.Body.Bytes()).Entries; len(got) != 1 || got[0].Digest != kept.Digest {
		t.Errorf("listing after DELETE = %s", rr.Body)
	}
	for _, p := range []string{fileTxt, "never.txt", "docs/"} {
		if rr := e.do(http.MethodDelete, target(p), nil); rr.Code != http.StatusNotFound {
			t.Errorf("DELETE %s = %d, want 404", p, rr.Code)
		}
	}
}

// checkDeleteAudit checks that evs are one delete row per version, in order, by anonymous from
// the test client's address.
func checkDeleteAudit(t *testing.T, evs []index.AuditEvent, versions ...putResponse) {
	t.Helper()
	type row struct{ Principal, Repo, Subject, Digest, IP string }
	var got, want []row
	for _, ev := range evs {
		got = append(got, row{ev.Principal, ev.Repo, ev.Subject, ev.Digest.String(), ev.IP})
	}
	for _, v := range versions {
		want = append(want, row{"anonymous", "raw/scratch", fileTxt + " " + v.Version, v.Digest, "192.0.2.1"})
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("delete audit rows (-want +got):\n%s", d)
	}
}

// TestDeleteUnsupported is R-07 on a backend that cannot delete manifests: 501, and nothing
// changes in the registry or the index. format.WriteError answers a 5xx with its status text,
// so the reason is in the log.
func TestDeleteUnsupported(t *testing.T) {
	e := newEnv(t, envOptions{})
	v := e.put(fileTxt, "one")
	e.prov.NoDeleteManifest = true
	rr := e.do(http.MethodDelete, target(fileTxt), nil)
	if rr.Code != http.StatusNotImplemented || !strings.Contains(e.logs.String(), "the backend registry cannot delete manifests; nothing was deleted") {
		t.Errorf("DELETE = %d %s; log %s", rr.Code, rr.Body, e.logs)
	}
	if rr := e.do(http.MethodGet, target(fileTxt), nil); rr.Code != http.StatusOK || len(e.versions(fileTxt)) != 1 || len(e.audit("delete")) != 0 {
		t.Errorf("after the refused DELETE: GET %d, %d versions, %d audit rows", rr.Code, len(e.versions(fileTxt)), len(e.audit("delete")))
	}
	if a := e.manifest("raw/scratch/docs/file.txt", digest.Digest(v.Digest)); a == nil {
		t.Error("the manifest is gone after a refused DELETE")
	}
}

// TestDeleteGoneAndAuditWriter checks that a manifest already gone from the registry does not
// stop the delete of its rows, and that the audit rows go through Deps.Audit when it is set.
func TestDeleteGoneAndAuditWriter(t *testing.T) {
	w := &auditWriter{}
	e := newEnv(t, envOptions{audit: w})
	v := e.put(fileTxt, "one")
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/docs/file.txt")
	must(t, err)
	must(t, rr.Delete(t.Context(), ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest(v.Digest)}))
	if resp := e.do(http.MethodDelete, target(fileTxt), nil, principalHeader, "alice"); resp.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", resp.Code, resp.Body)
	}
	if len(e.versions(fileTxt)) != 0 || len(e.audit("delete")) != 0 {
		t.Errorf("after DELETE: %d versions, %d audit rows in the index", len(e.versions(fileTxt)), len(e.audit("delete")))
	}
	if len(w.events) != 1 || w.events[0].Action != "delete" || w.events[0].Principal != "user:alice" {
		t.Errorf("audit writer events = %+v", w.events)
	}
}

// TestPutMaxFileSize checks policy.maxFileSize: up to the limit 201, above it 413 with a declared
// length and without one, nothing published; a policy that cannot be read is 500.
func TestPutMaxFileSize(t *testing.T) {
	e := newEnv(t, envOptions{policy: `{"maxFileSize": 10}`})
	e.put("ten.txt", "0123456789")
	for _, body := range []io.Reader{strings.NewReader("0123456789a"), io.MultiReader(strings.NewReader("0123456789a"))} {
		rr := e.do(http.MethodPut, target("eleven.txt"), body)
		if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "maxFileSize of raw/scratch, 10 bytes") {
			t.Errorf("PUT of 11 bytes = %d %s", rr.Code, rr.Body)
		}
	}
	if len(e.versions("eleven.txt")) != 0 || len(e.prov.PushReferences()) != 1 {
		t.Errorf("a refused upload was published: %d manifest pushes", len(e.prov.PushReferences()))
	}
	repo := e.repo
	repo.Policy = []byte(`{"maxFileSize": "10"}`)
	must(t, e.st.UpdateRepo(t.Context(), &repo))
	if rr := e.do(http.MethodPut, target("x.txt"), strings.NewReader("x")); rr.Code != http.StatusInternalServerError {
		t.Errorf("PUT with an unreadable policy = %d", rr.Code)
	}
}

// TestEmptyFile is R-10.
func TestEmptyFile(t *testing.T) {
	e := newEnv(t, envOptions{})
	if resp := e.put("empty", ""); resp.Size != 0 || resp.Version != tagOf("") {
		t.Errorf("PUT of an empty file = %+v", resp)
	}
	rr := e.do(http.MethodGet, target("empty"), nil)
	if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
		t.Errorf("GET = %d %q", rr.Code, rr.Body)
	}
	header(t, rr, "Content-Length", "0")
	header(t, rr, "Content-Type", "application/octet-stream")
}

// TestBadPaths checks that a path the rules refuse is 400 on PUT, whatever encoding got it past
// the ServeMux's cleaning, and that nothing is published.
func TestBadPaths(t *testing.T) {
	e := newEnv(t, envOptions{})
	for _, p := range []string{
		"/raw/scratch/a/%2e%2e/b", "/raw/scratch/%2e%2e", "/raw/scratch/a%2F.%2Fb", "/raw/scratch/a%2F%2Fb",
		"/raw/scratch/a%00b", "/raw/scratch/%ff", "/raw/scratch/dir/", "/raw/scratch/",
	} {
		if rr := e.do(http.MethodPut, p, strings.NewReader("x")); rr.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", p, rr.Code, rr.Body)
		}
	}
	if n := len(e.prov.PushReferences()); n != 0 {
		t.Errorf("%d manifests pushed for refused paths", n)
	}
	if rr := e.do(http.MethodGet, "/raw/scratch/a%00b", nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET of a path that cannot exist = %d, want 404", rr.Code)
	}
}

// TestR09 is R-09: four paths that differ in case, in script and in length are four versions
// of four packages; the long one has a hashed component in the registry; the listing shows the
// original paths.
func TestR09(t *testing.T) {
	e := newEnv(t, envOptions{})
	long := "long/" + strings.Repeat("x", 300) + ".txt"
	paths := []string{"Docs/Readme.MD", "docs/readme.md", "с пробелом/файл.txt", long}
	for i, p := range paths {
		e.put(p, "content "+strconv.Itoa(i))
	}
	rr := e.do(http.MethodGet, "/raw/scratch/", nil)
	var got []string
	for _, en := range listingOf(t, rr.Body.Bytes()).Entries {
		got = append(got, en.Path)
	}
	want := []string{"Docs/Readme.MD", "docs/readme.md", long, "с пробелом/файл.txt"} // byte order
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("listing paths = %q, want %q", got, want)
	}
	names := map[string]bool{}
	for _, r := range e.prov.PushReferences() {
		names[r.Name] = true
	}
	if !names["raw/scratch/docs/readme.md"] || !names["raw/scratch/long/"+hashed(strings.Repeat("x", 300)+".txt")] || len(names) != 3 {
		t.Errorf("OCI repositories = %v", names)
	}
	for i, p := range paths {
		if rr := e.do(http.MethodGet, target(p), nil); rr.Body.String() != "content "+strconv.Itoa(i) {
			t.Errorf("GET %s = %d %q", p, rr.Code, rr.Body)
		}
	}
}

// TestAuthorization checks the routes through an Authorizer: anonymous is 401 with the Basic
// challenge, a principal without read 404, read without write 403 on PUT, write without delete
// 403 on DELETE; uploaded-by names the principal.
func TestAuthorization(t *testing.T) {
	read, write, del := auth.ActionRead, auth.ActionWrite, auth.ActionDelete
	e := newEnv(t, envOptions{authz: authorizer{"carol": {read}, "alice": {read, write}, "dave": {read, write, del}}})
	resp := e.put(fileTxt, "secret", principalHeader, "alice")
	if a := e.manifest("raw/scratch/docs/file.txt", digest.Digest(resp.Digest)); a[profile.AnnotationUploadedBy] != "user:alice" {
		t.Errorf("uploaded-by = %q", a[profile.AnnotationUploadedBy])
	}
	tests := []struct {
		method, user string
		status       int
	}{
		{http.MethodGet, "", http.StatusUnauthorized},
		{http.MethodPut, "", http.StatusUnauthorized},
		{http.MethodGet, "bob", http.StatusNotFound},
		{http.MethodPut, "bob", http.StatusNotFound},
		{http.MethodGet, "carol", http.StatusOK},
		{http.MethodPut, "carol", http.StatusForbidden},
		{http.MethodDelete, "carol", http.StatusForbidden},
		{http.MethodDelete, "alice", http.StatusForbidden},
		{http.MethodDelete, "dave", http.StatusNoContent},
	}
	for _, tt := range tests {
		rr := e.do(tt.method, target(fileTxt), strings.NewReader("other"), principalHeader, tt.user)
		if rr.Code != tt.status {
			t.Errorf("%s as %q = %d %s, want %d", tt.method, tt.user, rr.Code, rr.Body, tt.status)
		}
		if tt.status == http.StatusUnauthorized {
			header(t, rr, "WWW-Authenticate", `Basic realm="lajstrom"`)
		}
	}
	if rr := e.do(http.MethodGet, "/raw/scratch/", nil, principalHeader, "bob"); rr.Code != http.StatusNotFound {
		t.Errorf("listing as bob = %d", rr.Code)
	}
}

// TestServerErrors checks that a repo whose registry is not configured answers 500 on GET and
// DELETE, and a version with other than one file in the index 500 on GET.
func TestServerErrors(t *testing.T) {
	e := newEnv(t, envOptions{})
	resp := e.put(fileTxt, "one")
	v, err := e.st.GetVersionByDigest(t.Context(), e.repo.ID, digest.Digest(resp.Digest))
	must(t, err)
	repo := e.repo
	repo.Registry = "gone"
	e.repo = repo
	ctx := t.Context()
	other, err := e.st.GetRepo(ctx, "raw", "scratch")
	must(t, err)
	other.ID = 0
	other.Name, other.Registry, other.Prefix = "orphan", "gone", "raw/orphan"
	must(t, e.st.CreateRepo(ctx, other))
	pkg := index.Package{RepoID: other.ID, Name: fileTxt, NameOCI: fileTxt}
	must(t, e.st.UpsertPackage(ctx, &pkg))
	ov := index.Version{PackageID: pkg.ID, Version: v.Version, Tag: v.Tag, Digest: v.Digest, ArtifactType: v.ArtifactType, CreatedAt: v.CreatedAt}
	must(t, e.st.UpsertVersion(ctx, &ov))
	must(t, e.st.ReplaceFiles(ctx, ov.ID, []index.File{{Name: fileTxt, MediaType: profile.MediaTypeRawFile, Digest: digest.FromString("one"), Size: 3}}))
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if rr := e.do(method, "/raw/orphan/"+fileTxt, nil); rr.Code != http.StatusInternalServerError {
			t.Errorf("%s on a repo without its registry = %d", method, rr.Code)
		}
	}
	must(t, e.st.ReplaceFiles(ctx, v.ID, nil))
	if rr := e.do(http.MethodGet, target(fileTxt), nil); rr.Code != http.StatusInternalServerError {
		t.Errorf("GET of a version without files = %d", rr.Code)
	}
}
