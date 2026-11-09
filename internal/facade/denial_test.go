package facade

import (
	"io"
	"net/http"
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
