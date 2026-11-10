package npm

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
)

// seenAudit is an audit request as the fake upstream got it.
type seenAudit struct {
	path   string
	header http.Header
	body   string
}

// upstreamAdvisories is what the fake upstream answers to every audit route, with an npm-notice
// and a cookie of its own.
const upstreamAdvisories = `{"lodash":[{"id":1106913,"url":"https://github.com/advisories/GHSA-35jh-r3h4-6jhm","severity":"high"}]}`

// serveAudit records an audit request and answers it as registry.npmjs.org does, or with the
// status auditCode; u.mu is held.
func (u *fakeNpmjs) serveAudit(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.audits = append(u.audits, seenAudit{path: r.URL.Path, header: r.Header.Clone(), body: string(b)})
	w.Header().Set("Set-Cookie", "__cf_bm=upstream")
	if code := int(u.auditCode.Load()); code != 0 {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error": "the upstream's own message"}`)
		return
	}
	w.Header().Set("npm-notice", "This endpoint is being retired.")
	w.Header().Set("Content-Type", "text/html")
	_, _ = io.WriteString(w, upstreamAdvisories)
}

// TestProxyAudit: a proxy repo forwards the three audit routes to its upstream, the body decoded
// and no header of the client's; the upstream's answer is the answer, with its npm-notice and
// no other header of the upstream's; its failures are the statuses of proxy-and-group.md §6,
// without its own message.
func TestProxyAudit(t *testing.T) {
	p := newProxyRepo(t, "https://registry.example")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"lodash":["4.17.15"]}`))
	must(t, zw.Close())
	tree := `{"name":"app","version":"1.0.0","requires":{"lodash":"4.17.15"},"dependencies":{"lodash":{"version":"4.17.15"}}}`
	for i, tc := range []struct {
		route, sent, want string
		header            []string
	}{
		{auditBulk, gz.String(), `{"lodash":["4.17.15"]}`, []string{"Content-Encoding", "gzip"}},
		{auditFull, tree, tree, nil},
		{auditQuick, tree, tree, nil},
	} {
		header := append(tc.header, "Authorization", "Bearer the-client's-token", "Cookie", "session=client", "Content-Type", "text/plain")
		rr := as(p.h, alice, http.MethodPost, "/npm/npmjs/-/"+tc.route, []byte(tc.sent), header...)
		if rr.Code != http.StatusOK || rr.Body.String() != upstreamAdvisories {
			t.Fatalf("%s: %d %s", tc.route, rr.Code, rr.Body)
		}
		for k, want := range map[string]string{"Content-Type": "application/json", "Npm-Notice": "This endpoint is being retired.", "Set-Cookie": ""} {
			if got := rr.Header().Get(k); got != want {
				t.Errorf("%s: the answer's %s %q, want %q", tc.route, k, got, want)
			}
		}
		p.up.mu.Lock()
		seen := p.up.audits[i]
		p.up.mu.Unlock()
		if seen.path != "/-/"+tc.route || seen.body != tc.want {
			t.Errorf("%s: the upstream got %s %q", tc.route, seen.path, seen.body)
		}
		for k, want := range map[string]string{"Content-Type": "application/json", "Authorization": "", "Cookie": "", "Content-Encoding": ""} {
			if got := seen.header.Get(k); got != want {
				t.Errorf("%s: the upstream's %s %q, want %q", tc.route, k, got, want)
			}
		}
	}
	for upstream, want := range map[int32]int{http.StatusBadRequest: 400, http.StatusNotFound: 404, http.StatusTooManyRequests: 503,
		http.StatusInternalServerError: 502} {
		p.up.auditCode.Store(upstream)
		rr := as(p.h, alice, http.MethodPost, "/npm/npmjs/-/"+auditBulk, []byte(`{}`))
		if rr.Code != want || strings.Contains(rr.Body.String(), "own message") || !strings.Contains(rr.Body.String(), `"error"`) {
			t.Errorf("upstream %d: %d %s, want %d", upstream, rr.Code, rr.Body, want)
		}
	}
	rr := as(p.h, alice, http.MethodPost, "/npm/npmjs/-/"+auditBulk, []byte(`{}`), "Content-Encoding", "gzip")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a body that is not gzip as it says: %d %s", rr.Code, rr.Body)
	}
}
