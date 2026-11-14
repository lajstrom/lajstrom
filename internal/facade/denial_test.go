package facade

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestDenialBeforeInput: a principal that may not read a repo gets, for any input, the answer
// of a repo that does not exist, word for word but for the name (docs/spec/rbac.md §3): the
// permission is decided before the digest, the tag or the upload is looked at, so that a
// malformed one does not tell that the repo exists (#63, R-2 of the threat model).
func TestDenialBeforeInput(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{"carol": {"raw/other": {auth.ActionRead, auth.ActionWrite}}}})
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	requests := []struct {
		name, method, suffix, body string
	}{
		{"a blob GET with a malformed digest", http.MethodGet, "/blobs/sha256:short", ""},
		{"a blob HEAD with a malformed digest", http.MethodHead, "/blobs/nodigest", ""},
		{"a manifest GET with a malformed tag", http.MethodGet, "/manifests/-bad", ""},
		{"a manifest GET with a malformed digest", http.MethodGet, "/manifests/sha256:short", ""},
		{"a manifest PUT with a malformed tag", http.MethodPut, "/manifests/-bad", manifest},
		{"an upload PUT with a malformed digest", http.MethodPut, "/blobs/uploads/0123456789abcdef0123456789abcdef?digest=bad", ""},
	}
	for _, who := range []struct {
		name   string
		hdr    []string
		status int
	}{
		{"a user without access", []string{principalHeader, "carol"}, http.StatusNotFound},
		{"anonymous", nil, http.StatusUnauthorized},
	} {
		for _, rq := range requests {
			what := who.name + ", " + rq.name
			answer := func(repo string) (int, string, string) {
				var body io.Reader
				hdr := who.hdr
				if rq.body != "" {
					body = strings.NewReader(rq.body)
					hdr = append(append([]string{}, hdr...), "Content-Type", "application/vnd.oci.image.manifest.v1+json")
				}
				rr := e.do(e.h, rq.method, "/v2/"+repo+"/pkg"+rq.suffix, body, hdr...)
				norm := func(s string) string { return strings.ReplaceAll(s, repo, "<repo>") }
				return rr.Code, norm(rr.Body.String()), norm(rr.Header().Get("WWW-Authenticate"))
			}
			status, body, challenge := answer("raw/assets")
			wantStatus, wantBody, wantChallenge := answer("raw/none")
			if status != who.status || status != wantStatus || body != wantBody || challenge != wantChallenge {
				t.Errorf("%s: an existing repo answers %d %q %q, a missing one %d %q %q; want both %d and alike",
					what, status, body, challenge, wantStatus, wantBody, wantChallenge, who.status)
			}
		}
	}
}

// TestSubjectDigestChecked: a manifest whose subject's digest is not a digest is 400
// MANIFEST_INVALID, before anything is pushed, so that the digest never reaches the
// OCI-Subject header of the answer (#63, M-3 of the threat model).
func TestSubjectDigestChecked(t *testing.T) {
	e := newEnv(t, envOptions{})
	body := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},` +
		`"layers":[],"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"x\r\nX-Injected: 1","size":2}}`
	for _, repo := range []string{"oci/images", "raw/assets"} {
		rr := e.do(e.h, http.MethodPut, "/v2/"+repo+"/app/manifests/v1", strings.NewReader(body),
			"Content-Type", "application/vnd.oci.image.manifest.v1+json")
		wantError(t, repo, rr, http.StatusBadRequest, codeManifestInvalid)
		if v := rr.Header().Values("OCI-Subject"); len(v) != 0 {
			t.Errorf("%s: OCI-Subject = %q, want none", repo, v)
		}
	}
	if n := len(e.prov.Pushes()) + len(e.prov.PushReferences()); n != 0 {
		t.Errorf("the registry got %d pushes, want none", n)
	}
}

// TestUnknownRepoAnswersAsUnreadable: every route of /v2 below a repository answers on a repo
// that the principal may not read as on a repo that does not exist, header for header and byte
// for byte but for the repo's name, to the anonymous principal (401), to a user without access
// (404) and to a bearer token that names the repository with no action, on a managed repo and on
// an oci one (docs/spec/rbac.md §3 and §4, #1091).
func TestUnknownRepoAnswersAsUnreadable(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{"carol": {"raw/other": {auth.ActionRead}}}})
	const (
		zero     = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		upload   = "/blobs/uploads/0123456789abcdef0123456789abcdef"
		manifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	)
	requests := []struct {
		method, suffix, body string
	}{
		{http.MethodGet, "/tags/list", ""},
		{http.MethodGet, "/tags/list?n=1&last=a", ""},
		{http.MethodGet, "/manifests/v1", ""},
		{http.MethodHead, "/manifests/v1", ""},
		{http.MethodGet, "/manifests/" + zero, ""},
		{http.MethodPut, "/manifests/v1", manifest},
		{http.MethodDelete, "/manifests/" + zero, ""},
		{http.MethodGet, "/blobs/" + zero, ""},
		{http.MethodHead, "/blobs/" + zero, ""},
		{http.MethodDelete, "/blobs/" + zero, ""},
		{http.MethodGet, "/referrers/" + zero, ""},
		{http.MethodPost, "/blobs/uploads/", ""},
		{http.MethodPost, "/blobs/uploads/?digest=" + zero, "x"},
		{http.MethodPost, "/blobs/uploads/?mount=" + zero + "&from=raw/other/pkg", ""},
		{http.MethodGet, upload, ""},
		{http.MethodPatch, upload, "x"},
		{http.MethodPut, upload + "?digest=" + zero, "x"},
		{http.MethodDelete, upload, ""},
	}
	who := []struct {
		name   string
		user   string
		bearer bool // a token that names the repository, from /v2/token
		status int
	}{
		{"anonymous", "", false, http.StatusUnauthorized},
		{"a user without access", "carol", false, http.StatusNotFound},
		{"anonymous with a token for it", "", true, http.StatusUnauthorized},
		{"a user without access with a token for it", "carol", true, http.StatusNotFound},
	}
	for _, w := range who {
		// Names of one length, so that Content-Length is compared too.
		for _, repos := range [][2]string{{"raw/assets", "raw/absent"}, {"oci/images", "oci/absent"}} {
			for _, rq := range requests {
				t.Run(w.name+" "+rq.method+" "+repos[0]+rq.suffix, func(t *testing.T) {
					answer := func(repo string) (string, int) {
						var hdr []string
						if w.user != "" {
							hdr = []string{principalHeader, w.user}
						}
						if w.bearer {
							tok := e.getToken(e.h, w.user, "repository:"+repo+"/pkg:pull,push,delete")
							hdr = []string{"Authorization", "Bearer " + tok}
						}
						var body io.Reader
						if rq.body != "" {
							body = strings.NewReader(rq.body)
							hdr = append(hdr, "Content-Type", "application/vnd.oci.image.manifest.v1+json")
						}
						rr := e.do(e.h, rq.method, "/v2/"+repo+"/pkg"+rq.suffix, body, hdr...)
						return fullAnswer(rr, repo), rr.Code
					}
					unreadable, status := answer(repos[0])
					unknown, _ := answer(repos[1])
					if unreadable != unknown {
						t.Errorf("the repo that may not be read answers\n%s\nthe repo that does not exist\n%s", unreadable, unknown)
					}
					if status != w.status {
						t.Errorf("status %d, want %d:\n%s", status, w.status, unreadable)
					}
				})
			}
		}
	}
}

// fullAnswer is rr as text: the status, the headers but Date, and the body, each with repo as
// <repo>.
func fullAnswer(rr *httptest.ResponseRecorder, repo string) string {
	norm := strings.NewReplacer(repo, "<repo>").Replace
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", rr.Code)
	names := make([]string, 0, len(rr.Header()))
	for name := range rr.Header() {
		if name != "Date" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&b, "%s: %s\n", name, norm(strings.Join(rr.Header().Values(name), ", ")))
	}
	fmt.Fprintf(&b, "\n%s", norm(rr.Body.String()))
	return b.String()
}
