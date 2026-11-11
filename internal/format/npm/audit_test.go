package npm

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"slices"
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
		rr := as(p.h, alice, http.MethodPost, "/npm/npmjs/-/"+auditBulk, []byte(`{"lodash":["4.17.15"]}`))
		if rr.Code != want || strings.Contains(rr.Body.String(), "own message") || !strings.Contains(rr.Body.String(), `"error"`) {
			t.Errorf("upstream %d: %d %s, want %d", upstream, rr.Code, rr.Body, want)
		}
	}
	rr := as(p.h, alice, http.MethodPost, "/npm/npmjs/-/"+auditBulk, []byte(`{}`), "Content-Encoding", "gzip")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a body that is not gzip as it says: %d %s", rr.Code, rr.Body)
	}
}

// TestProxyAuditDeny: a proxy repo with deny: ["@acme/*"], its upstream a listener, leaves the
// names its deny list refuses out of the bulk audit, in any case, and asks the upstream nothing
// when no name is left; npm 6's tree that names a denied package, below another or as the
// project's own name, is 501 and goes nowhere, and one that names none goes whole (#514).
func TestProxyAuditDeny(t *testing.T) {
	p := newProxyRepo(t, "https://registry.example")
	p.repo.Upstream = json.RawMessage(`{"name": "npmjs", "deny": ["@acme/*"]}`)
	must(t, p.st.UpdateRepo(t.Context(), &p.repo))
	seen := func() []seenAudit {
		p.up.mu.Lock()
		defer p.up.mu.Unlock()
		return slices.Clone(p.up.audits)
	}
	rr := as(p.h, alice, http.MethodPost, "/npm/npmjs/-/"+auditBulk,
		[]byte(`{"@acme/widgets":["1.1.0"],"@ACME/Gadgets":["1.0.0"],"lodash":["4.17.15"]}`))
	if rr.Code != http.StatusOK || rr.Body.String() != upstreamAdvisories {
		t.Fatalf("the bulk audit: %d %s", rr.Code, rr.Body)
	}
	if got := seen(); len(got) != 1 || got[0].body != `{"lodash":["4.17.15"]}` {
		t.Fatalf("the upstream got %+v, want lodash alone", got)
	}
	rr = as(p.h, alice, http.MethodPost, "/npm/npmjs/-/"+auditBulk, []byte(`{"@acme/widgets":["1.1.0"]}`))
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "{}" || len(seen()) != 1 {
		t.Errorf("a bulk audit of denied names only: %d %s, %d upstream requests", rr.Code, rr.Body, len(seen()))
	}

	public := `{"name":"app","version":"1.0.0","requires":{"lodash":"4.17.15"},"dependencies":{"lodash":{"version":"4.17.15"}}}`
	for _, tc := range []struct {
		what, tree string
		want       int
	}{
		{"a denied package below another", `{"name":"app","version":"1.0.0","requires":{"lodash":"4.17.15"},` +
			`"dependencies":{"lodash":{"version":"4.17.15","dependencies":{"@acme/widgets":{"version":"1.1.0"}}}}}`, http.StatusNotImplemented},
		{"a denied project", `{"name":"@acme/app","version":"1.0.0","requires":{},"dependencies":{}}`, http.StatusNotImplemented},
		{"a tree that does not read", `{"name":"app","dependencies":[]}`, http.StatusBadRequest},
		{"no denied name", public, http.StatusOK},
	} {
		before := len(seen())
		rr := as(p.h, alice, http.MethodPost, "/npm/npmjs/-/"+auditQuick, []byte(tc.tree))
		if rr.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.what, rr.Code, rr.Body, tc.want)
		}
		got := seen()
		switch {
		case tc.want != http.StatusOK && len(got) != before:
			t.Errorf("%s: the upstream got %q", tc.what, got[len(got)-1].body)
		case tc.want == http.StatusOK && (len(got) != before+1 || got[len(got)-1].body != tc.tree):
			t.Errorf("%s: the upstream got %+v, want the tree whole", tc.what, got[before:])
		}
	}
}
