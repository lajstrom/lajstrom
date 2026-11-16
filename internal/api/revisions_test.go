package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// revisioned is a resource that has a revision (docs/api.md, Revisions), over a fixture: its
// path, and the bodies of a PATCH that changes it, that changes it back, and that changes
// nothing.
type revisioned struct {
	name              string
	f                 *fixture
	target            string
	change, back, nop string
}

// revisionedResources returns a repo in a fixture of its own.
func revisionedResources(t *testing.T) []revisioned {
	t.Helper()
	repos := newFixture(t, Options{})
	repos.create(t, "raw", "scratch")
	return []revisioned{
		{"repo", repos, "/api/v1/repos/raw/scratch", `{"anonymousRead": true}`, `{"anonymousRead": false}`, `{"policy": null}`},
	}
}

// send sends method to target with body ("" for none) and header as the admin.
func (f *fixture) send(method, target, body string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range header {
		r.Header[k] = v
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), f.asStored(admin)))
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, r)
	return rr
}

// etagOf returns the ETag of a GET of target, which must be 200 with one.
func (f *fixture) etagOf(t *testing.T, target string) string {
	t.Helper()
	rr := f.send(http.MethodGet, target, "", nil)
	wantStatus(t, rr, http.StatusOK)
	etag := rr.Header().Get("ETag")
	if len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		t.Fatalf("GET %s: ETag %q, want a strong entity tag", target, etag)
	}
	return etag
}

// wantETag fails the test unless rr has the ETag want.
func wantETag(t *testing.T, what string, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := rr.Header().Get("ETag"); got != want {
		t.Errorf("%s: ETag %q, want %q", what, got, want)
	}
}

// ifMatch is the header If-Match: v.
func ifMatch(v string) http.Header { return http.Header{"If-Match": {v}} }

// TestRevisionGet: GET of a repo answers a strong ETag, the same while
// nothing changes; If-None-Match with it, weak or strong, or *, is 304 without a body, another
// tag 200, and a value that is no tag 400.
func TestRevisionGet(t *testing.T) {
	for _, res := range revisionedResources(t) {
		t.Run(res.name, func(t *testing.T) {
			f := res.f
			etag := f.etagOf(t, res.target)
			if again := f.etagOf(t, res.target); again != etag {
				t.Errorf("a second GET: ETag %s, want %s", again, etag)
			}
			for _, v := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
				rr := f.send(http.MethodGet, res.target, "", http.Header{"If-None-Match": {v}})
				wantStatus(t, rr, http.StatusNotModified)
				wantETag(t, "304 of "+v, rr, etag)
				if rr.Body.Len() != 0 {
					t.Errorf("304 of %s has a body: %s", v, rr.Body)
				}
			}
			rr := f.send(http.MethodGet, res.target, "", http.Header{"If-None-Match": {`"other"`}})
			wantStatus(t, rr, http.StatusOK)
			wantETag(t, "200 of another tag", rr, etag)
			wantError(t, f.send(http.MethodGet, res.target, "", http.Header{"If-None-Match": {"other"}}), http.StatusBadRequest,
				codeInvalid, "If-None-Match:")
		})
	}
}

// TestRevisionPatch: a PATCH with the ETag the client read is made and answers the new ETag; one
// with a stale ETag, or a weak one, is 412 with the ETag now, and changes nothing; one without
// If-Match, or with *, is made as before; a list passes when one of its tags is the ETag; a
// value that is no tag is 400; a PATCH that changes nothing keeps the ETag.
func TestRevisionPatch(t *testing.T) {
	for _, res := range revisionedResources(t) {
		t.Run(res.name, func(t *testing.T) {
			f := res.f
			stale := f.etagOf(t, res.target)
			rr := f.send(http.MethodPatch, res.target, res.change, ifMatch(stale))
			wantStatus(t, rr, http.StatusOK)
			fresh := rr.Header().Get("ETag")
			if fresh == stale || fresh != f.etagOf(t, res.target) {
				t.Fatalf("ETag after the change %q, before %q: want a new one, which GET answers", fresh, stale)
			}
			before := f.send(http.MethodGet, res.target, "", nil).Body.String()

			rr = f.send(http.MethodPatch, res.target, res.back, ifMatch(stale))
			wantError(t, rr, http.StatusPreconditionFailed, codeConflict, "changed since it was read", fresh)
			wantETag(t, "412", rr, fresh)
			wantError(t, f.send(http.MethodPatch, res.target, res.back, ifMatch("W/"+fresh)), http.StatusPreconditionFailed, codeConflict)
			if after := f.send(http.MethodGet, res.target, "", nil).Body.String(); after != before {
				t.Errorf("a refused PATCH changed the resource:\n%s\nwas\n%s", after, before)
			}
			wantError(t, f.send(http.MethodPatch, res.target, res.back, ifMatch(fresh[1:])), http.StatusBadRequest, codeInvalid, "If-Match:")

			rr = f.send(http.MethodPatch, res.target, res.nop, ifMatch(fresh))
			wantStatus(t, rr, http.StatusOK)
			wantETag(t, "a PATCH that changes nothing", rr, fresh)
			rr = f.send(http.MethodPatch, res.target, res.back, ifMatch(stale+", "+fresh))
			wantStatus(t, rr, http.StatusOK)
			wantStatus(t, f.send(http.MethodPatch, res.target, res.change, nil), http.StatusOK)
			rr = f.send(http.MethodPatch, res.target, res.back, ifMatch("*"))
			wantStatus(t, rr, http.StatusOK)
			wantETag(t, "PATCH with *", rr, f.etagOf(t, res.target))
		})
	}
}

// TestRevisionDelete: a DELETE with a stale ETag is 412 and deletes nothing, one with the ETag
// the client read is made; without If-Match a DELETE is made as before.
func TestRevisionDelete(t *testing.T) {
	for _, res := range revisionedResources(t) {
		t.Run(res.name, func(t *testing.T) {
			f := res.f
			stale := f.etagOf(t, res.target)
			wantStatus(t, f.send(http.MethodPatch, res.target, res.change, nil), http.StatusOK)
			fresh := f.etagOf(t, res.target)
			rr := f.send(http.MethodDelete, res.target, "", ifMatch(stale))
			wantError(t, rr, http.StatusPreconditionFailed, codeConflict, "changed since it was read")
			wantETag(t, "412", rr, fresh)
			wantStatus(t, f.send(http.MethodGet, res.target, "", nil), http.StatusOK)
			wantStatus(t, f.send(http.MethodDelete, res.target, "", ifMatch(fresh)), http.StatusNoContent)
			wantStatus(t, f.send(http.MethodGet, res.target, "", nil), http.StatusNotFound)
		})
	}
	for _, res := range revisionedResources(t) {
		t.Run(res.name+" without If-Match", func(t *testing.T) {
			wantStatus(t, res.f.send(http.MethodPatch, res.target, res.change, nil), http.StatusOK)
			wantStatus(t, res.f.send(http.MethodDelete, res.target, "", nil), http.StatusNoContent)
		})
	}
}

// TestRevisionRace: PATCHes sent at once with the same ETag: exactly one is made, the others are
// 412 with the ETag it left.
func TestRevisionRace(t *testing.T) {
	const n = 8
	for _, res := range revisionedResources(t) {
		t.Run(res.name, func(t *testing.T) {
			f := res.f
			etag := f.etagOf(t, res.target)
			answers := make([]*httptest.ResponseRecorder, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Go(func() { answers[i] = f.send(http.MethodPatch, res.target, res.change, ifMatch(etag)) })
			}
			wg.Wait()
			now, made := f.etagOf(t, res.target), 0
			for _, rr := range answers {
				switch rr.Code {
				case http.StatusOK:
					made++
					wantETag(t, "the PATCH made", rr, now)
				case http.StatusPreconditionFailed:
					wantETag(t, "a PATCH refused", rr, now)
				default:
					t.Errorf("status %d, want 200 or 412: %s", rr.Code, rr.Body)
				}
			}
			if made != 1 {
				t.Errorf("%d PATCHes made with the same ETag, want 1", made)
			}
		})
	}
}

// TestRevisionOfAnother: the ETag of one resource is not another's, nor that of a repo deleted
// and created again under the name; a POST answers the ETag of the resource it created.
func TestRevisionOfAnother(t *testing.T) {
	f := newFixture(t, Options{})
	rr := f.send(http.MethodPost, "/api/v1/repos", scratch, nil)
	wantStatus(t, rr, http.StatusCreated)
	old := rr.Header().Get("ETag")
	wantETag(t, "POST", f.send(http.MethodGet, "/api/v1/repos/raw/scratch", "", nil), old)
	f.create(t, "raw", "other")
	wantError(t, f.send(http.MethodPatch, "/api/v1/repos/raw/other", `{"anonymousRead": true}`, ifMatch(old)),
		http.StatusPreconditionFailed, codeConflict, "repo raw/other changed")
	wantStatus(t, f.send(http.MethodDelete, "/api/v1/repos/raw/scratch", "", ifMatch(old)), http.StatusNoContent)
	wantStatus(t, f.send(http.MethodPost, "/api/v1/repos", scratch, nil), http.StatusCreated)
	wantError(t, f.send(http.MethodPatch, "/api/v1/repos/raw/scratch", `{"anonymousRead": true}`, ifMatch(old)),
		http.StatusPreconditionFailed, codeConflict)
}
