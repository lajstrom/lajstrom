package facade

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestUploadLiveSessions: a principal holds at most UploadSessionsPerRepo live upload sessions
// of a repo on a replica, and UploadSessionsPerPrincipal in all repos; a new one beyond is 429
// TOOMANYREQUESTS with Retry-After, naming the bound and its key, and opens nothing in the
// backend (U-4, #246). DELETE, the closing PUT and an error that ends a session free its slot at
// once; a session idle for 5 minutes takes none and stays resumable; one that another replica
// closed is forgotten at the bound once the index has been asked again; a mount that mounts
// holds none.
func TestUploadLiveSessions(t *testing.T) {
	z := authorizer{
		"alice": {"oci/images": {auth.ActionRead, auth.ActionWrite}, "raw/assets": {auth.ActionWrite}},
		"bob":   {"oci/images": {auth.ActionWrite}},
	}
	e := newEnv(t, envOptions{authz: z, sessions: [2]int{3, 5}})
	alice, bob := []string{principalHeader, "alice"}, []string{principalHeader, "bob"}
	const assets = "/v2/raw/assets/x/blobs/uploads/"
	open := func(h http.Handler, target string, who []string) *httptest.ResponseRecorder {
		return e.do(h, http.MethodPost, target, nil, who...)
	}
	var ids []string
	opened := func(what string, h http.Handler, target string) {
		t.Helper()
		rr := open(h, target, alice)
		wantStatus(t, what, rr, http.StatusAccepted)
		if rr.Code == http.StatusAccepted {
			ids = append(ids, uploadID(t, rr))
		}
	}
	refused := func(what, target, bound, retry string) {
		t.Helper()
		backend := e.prov.OpenUploads()
		rr := open(e.h, target, alice)
		if msg := wantError(t, what, rr, http.StatusTooManyRequests, codeTooManyRequests); !strings.Contains(msg, bound) {
			t.Errorf("%s: message %q, want it to name %q", what, msg, bound)
		}
		wantHeader(t, what, rr, "Retry-After", retry)
		if n := e.prov.OpenUploads(); n != backend {
			t.Errorf("%s: the refused POST left a backend session: %d, want %d", what, n, backend)
		}
	}
	perRepo := "3 live upload sessions of oci/images on this replica, the bound server.uploadSessions.perRepo"
	for range 3 {
		opened("POST", e.h, uploads)
	}
	refused("one more", uploads, perRepo, "300")
	refused("a mount that would open a session", uploads+"?mount="+digest.FromString("absent").String()+"&from=oci/images/base", perRepo, "300")
	d := e.pushBlob("oci/images/base", []byte("mount me"))
	wantCreated(t, "a mount that mounts", open(e.h, uploads+"?mount="+d.String()+"&from=oci/images/base", alice), d)
	wantStatus(t, "another principal", open(e.h, uploads, bob), http.StatusAccepted)
	opened("another repo", e.h, assets)
	opened("another repo again", e.h, assets)
	refused("beyond the principal's bound", assets, "5 live upload sessions in all repos on this replica, the bound server.uploadSessions.perPrincipal", "300")

	wantStatus(t, "DELETE", e.do(e.h, http.MethodDelete, uploads+ids[0], nil, alice...), http.StatusNoContent)
	opened("POST after a DELETE", e.h, uploads)
	b := []byte("closed")
	wantStatus(t, "the closing PUT", e.do(e.h, http.MethodPut, uploads+ids[1]+"?digest="+digest.FromBytes(b).String(), bytes.NewReader(b), alice...), http.StatusCreated)
	opened("POST after a PUT", e.h, uploads)
	wantError(t, "a PUT that ends the session", e.do(e.h, http.MethodPut, uploads+ids[2]+"?digest="+digest.FromString("other").String(), bytes.NewReader(b), alice...),
		http.StatusBadRequest, codeDigestInvalid)
	opened("POST after an error that ended a session", e.h, uploads)
	e.clock.advance(2 * time.Minute)
	refused("and one more", uploads, perRepo, "180")

	e.clock.advance(3 * time.Minute)
	opened("POST once the others are idle", e.h, uploads)
	wantStatus(t, "PATCH of an idle session", e.do(e.h, http.MethodPatch, uploads+ids[5], strings.NewReader("abc"), alice...), http.StatusAccepted)
	opened("POST beside the session resumed", e.h, uploads)
	refused("the session resumed counts again", uploads, perRepo, "300")

	last := ids[len(ids)-1]
	wantStatus(t, "PUT on another replica", e.do(e.replica(), http.MethodPut, uploads+last+"?digest="+digest.FromBytes(b).String(), bytes.NewReader(b), alice...), http.StatusCreated)
	refused("within recheckAfter of the last answer of the index", uploads, perRepo, "300")
	e.clock.advance(recheckAfter)
	opened("POST once the index says the session closed elsewhere is gone", e.h, uploads)
	opened("another replica", e.replica(), uploads)
}

// TestUploadSessionsOfAnotherReplica: a session another replica opened counts on this one once
// this one answers a request on it; that request is never refused.
func TestUploadSessionsOfAnotherReplica(t *testing.T) {
	e := newEnv(t, envOptions{sessions: [2]int{2, 0}})
	other := e.replica()
	for range 2 {
		rr := e.do(other, http.MethodPost, uploads, nil)
		wantStatus(t, "POST on the other replica", rr, http.StatusAccepted)
		if rr.Code == http.StatusAccepted {
			wantStatus(t, "PATCH here", e.do(e.h, http.MethodPatch, uploads+uploadID(t, rr), strings.NewReader("abc")), http.StatusAccepted)
		}
	}
	wantError(t, "POST here", e.do(e.h, http.MethodPost, uploads, nil), http.StatusTooManyRequests, codeTooManyRequests)
}

// TestUploadSessionsOff: 0 turns a bound off, and both off count nothing.
func TestUploadSessionsOff(t *testing.T) {
	for _, bounds := range [][2]int{{0, 0}, {0, 2}, {2, 0}} {
		e := newEnv(t, envOptions{sessions: bounds})
		n, want := 0, 5
		if bounds != [2]int{} {
			want = 2
		}
		for range 5 {
			if e.do(e.h, http.MethodPost, uploads, nil).Code == http.StatusAccepted {
				n++
			}
		}
		if n != want {
			t.Errorf("bounds %v: %d sessions, want %d", bounds, n, want)
		}
	}
}

// TestLiveUploads: a session counts while a request on it runs, however long, and for
// liveWindow after its last request ended; Retry-After is the time until one of those counted
// stops counting, retryRunning while a request on one runs.
func TestLiveUploads(t *testing.T) {
	q := liveUploads{perRepo: 1}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if f := q.admit("p", 1, "a", now); f != nil {
		t.Fatalf("the first session: %+v", f)
	}
	later := now.Add(time.Hour)
	if f := q.admit("p", 1, "b", later); f == nil || !f.perRepo || f.limit != 1 || f.retry != retryRunning {
		t.Errorf("beside a request that runs: %+v, want the repo's bound and retry %s", f, retryRunning)
	}
	if f := q.admit("p", 2, "b", later); f != nil {
		t.Errorf("in another repo: %+v", f)
	}
	q.end("p", "a", later)
	if f := q.admit("p", 1, "c", later.Add(time.Minute)); f == nil || f.retry != liveWindow-time.Minute {
		t.Errorf("a minute after the request ended: %+v, want retry %s", f, liveWindow-time.Minute)
	}
	if f := q.admit("p", 1, "c", later.Add(liveWindow)); f != nil {
		t.Errorf("liveWindow after the request ended: %+v", f)
	}
	if _, ok := q.held["p"]["a"]; ok {
		t.Error("a session that stopped counting is still held")
	}
}

// TestLiveUploadsPrincipals: when maxUploadPrincipals principals are counted, a new one is
// counted once one of the others has nothing live, and is let through uncounted while none has.
func TestLiveUploadsPrincipals(t *testing.T) {
	q := liveUploads{perRepo: 1}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for i := range maxUploadPrincipals {
		if f := q.admit(strconv.Itoa(i), 1, "id", now); f != nil {
			t.Fatalf("principal %d refused", i)
		}
	}
	if len(q.held) != maxUploadPrincipals {
		t.Fatalf("%d principals counted, want %d", len(q.held), maxUploadPrincipals)
	}
	if f := q.admit("new", 1, "id", now); f != nil || q.held["new"] != nil {
		t.Errorf("a new principal while all are busy: %+v, want it let through uncounted", f)
	}
	q.release("0", "id")
	if f := q.admit("new", 1, "id", now); f != nil || q.held["new"] == nil {
		t.Errorf("a new principal after one left: %+v, want it counted", f)
	}
	for p := range q.held {
		q.end(p, "id", now)
	}
	later := now.Add(liveWindow)
	if f := q.admit("other", 1, "id", later); f != nil || len(q.held) != 1 {
		t.Errorf("after every session stopped counting: %d principals counted, want the new one alone", len(q.held))
	}
}
