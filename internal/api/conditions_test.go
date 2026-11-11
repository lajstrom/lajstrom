package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// doWith is f.do with request headers.
func (f *fixture) doWith(method, target string, header http.Header, p auth.Principal) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		r.Header[k] = v
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), f.asStored(p)))
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, r)
	return rr
}

// etags is an If-Match of the digests.
func etags(ds ...digest.Digest) string {
	q := make([]string, len(ds))
	for i, d := range ds {
		q[i] = `"` + d.String() + `"`
	}
	return strings.Join(q, ", ")
}

// TestDeleteVersionConditions: If-Match must list exactly the digests of the variants the
// delete takes, If-Unmodified-Since is the cut-off, and a version a dist-tag names is refused
// unless forced; a refusal is 412 or 409 and asks nothing of the Deleter. What passes goes to
// the Deleter as the Condition of each variant, which ingest checks again under its locks;
// a refusal there keeps its status and message.
func TestDeleteVersionConditions(t *testing.T) {
	d := &recordingDeleter{}
	f := newFixture(t, Options{Deleter: d})
	repo := f.create(t, "pypi", "libs")
	published := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	pkg := publish(t, f.st, repo.ID, "requests", "1.0", published, "sdist", "py3-none-any")
	sdist, wheel := digest.FromString("requests1.0sdist"), digest.FromString("requests1.0py3-none-any")
	target := "/api/v1/repos/pypi/libs/packages/requests/versions/1.0"
	at := func(t time.Time) string { return t.Format(http.TimeFormat) }
	for _, tt := range []struct {
		name   string
		query  string
		header http.Header
		status int
		want   string
	}{
		{"one digest of two", "", http.Header{"If-Match": {etags(sdist)}}, http.StatusPreconditionFailed, "changed since it was read"},
		{"a digest it does not have", "", http.Header{"If-Match": {etags(sdist, wheel, digest.FromString("other"))}}, http.StatusPreconditionFailed, "changed"},
		{"the variant's digest is not this", "?variant=sdist", http.Header{"If-Match": {etags(wheel)}}, http.StatusPreconditionFailed, "changed"},
		{"published after the cut-off", "", http.Header{"If-Unmodified-Since": {at(published.Add(-time.Second))}}, http.StatusPreconditionFailed, "after the cut-off"},
		{"not quoted", "", http.Header{"If-Match": {sdist.String()}}, http.StatusBadRequest, "If-Match:"},
		{"not a digest", "", http.Header{"If-Match": {`"v1"`}}, http.StatusBadRequest, "If-Match:"},
		{"not an HTTP-date", "", http.Header{"If-Unmodified-Since": {"2026-11-01"}}, http.StatusBadRequest, "If-Unmodified-Since:"},
		{"force not a bool", "?force=yes", nil, http.StatusBadRequest, "force:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, f.doWith(http.MethodDelete, target+tt.query, tt.header, admin), tt.status, codeOf(tt.status), tt.want)
			if len(d.got) != 0 {
				t.Errorf("deletions %+v", d.got)
			}
		})
	}

	header := http.Header{"If-Match": {etags(wheel), etags(sdist)}, "If-Unmodified-Since": {at(published)}}
	wantStatus(t, f.doWith(http.MethodDelete, target, header, admin), http.StatusNoContent)
	if len(d.got) != 2 {
		t.Fatalf("deletions %+v", d.got)
	}
	for _, del := range d.got {
		c := del.Condition
		if c == nil || c.Version != "1.0" || (c.Variant != "sdist" && c.Variant != "py3-none-any") || !c.UnmodifiedSince.Equal(published) || c.Force {
			t.Errorf("Condition %+v", c)
		}
	}
	d.got = nil
	wantStatus(t, f.doWith(http.MethodDelete, target+"?variant=sdist", http.Header{"If-Match": {"*"}}, admin), http.StatusNoContent)
	wantDeletions(t, d, "requests", sdist)

	d.got = nil
	must(t, f.st.ReplaceJournalState(context.Background(), pkg.ID, []index.JournalState{{Kind: index.JournalDistTag, Key: "latest", Value: "1.0", Seq: 1}}))
	wantError(t, f.do(http.MethodDelete, target, "", admin), http.StatusConflict, codeConflict, "dist-tag latest", "force")
	if len(d.got) != 0 {
		t.Errorf("deletions of a protected version %+v", d.got)
	}
	wantStatus(t, f.do(http.MethodDelete, target+"?force=true", "", admin), http.StatusNoContent)
	if len(d.got) != 2 || !d.got[0].Condition.Force {
		t.Errorf("forced deletions %+v", d.got)
	}

	d.got = nil
	d.err = fmt.Errorf("ingest: %w", &format.RefusedError{Reason: format.RefusedChanged, Subject: "version 1.0 (sdist) of \"requests\" in pypi/libs",
		Digests: []digest.Digest{digest.FromString("again")}})
	wantError(t, f.do(http.MethodDelete, target+"?force=true", "", admin), http.StatusPreconditionFailed, codeConflict,
		`version 1.0 (sdist) of "requests" in pypi/libs changed since it was read`)
}
