package npm

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// as sends a request to h as the principal p.
func as(h http.Handler, p auth.Principal, method, path string, body []byte, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestPingWhoami: npm ping and npm whoami, and the web login, which is 404 so that npm asks
// for a password.
func TestPingWhoami(t *testing.T) {
	p := newPipeline(t)
	must(t, p.st.CreateUser(t.Context(), &index.User{Name: "alice", Kind: "local"}))
	h := p.routes(nil)
	deploy := auth.Principal{Kind: auth.KindToken, Name: "7"}
	for _, tc := range []struct {
		name      string
		p         auth.Principal
		method    string
		path      string
		status    int
		body      string
		challenge bool
	}{
		{"ping", alice, http.MethodGet, "/npm/internal/-/ping?write=true", http.StatusOK, `{}`, false},
		{"ping anonymous", auth.Anonymous, http.MethodGet, "/npm/internal/-/ping", http.StatusOK, `{}`, false},
		{"whoami", alice, http.MethodGet, "/npm/internal/-/whoami", http.StatusOK, `{"username":"alice"}`, false},
		{"whoami of a deploy token", deploy, http.MethodGet, "/npm/internal/-/whoami", http.StatusOK, `{"username":"token:7"}`, false},
		{"whoami anonymous", auth.Anonymous, http.MethodGet, "/npm/internal/-/whoami", http.StatusUnauthorized, "", true},
		{"web login", alice, http.MethodPost, "/npm/internal/-/v1/login", http.StatusNotFound, "", false},
		{"npm token", alice, http.MethodGet, "/npm/internal/-/npm/v1/tokens", http.StatusNotFound, "", false},
		{"a POST of a package", alice, http.MethodPost, "/npm/internal/widgets", http.StatusNotFound, "", false},
		{"a POST of a dist-tag", alice, http.MethodPost, "/npm/internal/-/package/widgets/dist-tags/latest", http.StatusNotFound, "", false},
	} {
		rr := as(h, tc.p, tc.method, tc.path, []byte(`{}`))
		if rr.Code != tc.status || (tc.body != "" && strings.TrimSpace(rr.Body.String()) != tc.body) ||
			rr.Header().Get("Content-Type") != "application/json" || (rr.Header().Get("WWW-Authenticate") != "") != tc.challenge {
			t.Errorf("%s: %d %v %s", tc.name, rr.Code, rr.Header(), rr.Body)
		}
	}
	if rr := as(p.routes(deny{}), alice, http.MethodGet, "/npm/internal/-/ping", nil); rr.Code != http.StatusNotFound {
		t.Errorf("ping without read: %d", rr.Code)
	}
}

// TestSearch: npm search from the index, by a part of the name without regard to case, sorted
// by name and paginated, each package with its latest version and description.
func TestSearch(t *testing.T) {
	p := newPipeline(t)
	h := p.install("https://lajstrom.example", nil)
	for _, o := range []testutil.NpmOptions{
		{Name: "@acme/widgets", Version: "1.0.0", Description: "widgets of acme"},
		{Name: "@acme/widgets", Version: "1.1.0", Description: "better widgets"},
		{Name: "widget-kit", Version: "0.1.0"},
		{Name: "gadgets", Version: "2.0.0"},
	} {
		b, info := testutil.NpmTarball(o)
		if rr := do(h, http.MethodPut, "/npm/internal/"+strings.Replace(o.Name, "/", "%2f", 1), npmBody(t, b, info, nil), false); rr.Code != http.StatusCreated {
			t.Fatalf("publish %s: %d %s", o.Name, rr.Code, rr.Body)
		}
	}
	type answer struct {
		Objects []struct {
			Package struct{ Name, Version, Description, Date string }
		}
		Total int
	}
	for _, tc := range []struct {
		query string
		total int
		names []string
	}{
		{"text=WIDGET", 2, []string{"@acme/widgets", "widget-kit"}},
		{"text=widget&size=1", 2, []string{"@acme/widgets"}},
		{"text=widget&size=1&from=1", 2, []string{"widget-kit"}},
		{"text=widget&from=5", 2, nil},
		{"text=", 3, []string{"@acme/widgets", "gadgets", "widget-kit"}},
		{"text=nothing", 0, nil},
	} {
		rr := get(h, "/npm/internal/-/v1/search?"+tc.query)
		var a answer
		must(t, json.Unmarshal(rr.Body.Bytes(), &a))
		var names []string
		for _, o := range a.Objects {
			names = append(names, o.Package.Name)
		}
		if d := cmp.Diff(tc.names, names); rr.Code != http.StatusOK || a.Total != tc.total || d != "" {
			t.Errorf("%s: %d, total %d (-want +got)\n%s", tc.query, rr.Code, a.Total, d)
		}
		if tc.query == "text=WIDGET" {
			if got := a.Objects[0].Package; got.Version != "1.1.0" || got.Description != "better widgets" || got.Date == "" {
				t.Errorf("the object of @acme/widgets: %+v", got)
			}
			if !strings.Contains(rr.Body.String(), `"maintainers":[]`) {
				t.Errorf("an object without the list of maintainers npm reads: %s", rr.Body)
			}
		}
	}
}

// TestAudit: a hosted repo knows no advisory: the bulk request of npm is {}, gzipped or not,
// pnpm's two the empty report; a body above 16 MiB decoded is 413.
func TestAudit(t *testing.T) {
	p := newPipeline(t)
	h := p.routes(nil)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"widgets": ["1.0.0"]}`))
	must(t, zw.Close())
	for _, tc := range []struct {
		name, path string
		body       []byte
		header     []string
		status     int
		want       string
	}{
		{"bulk", "/npm/internal/-/npm/v1/security/advisories/bulk", []byte(`{"widgets": ["1.0.0"]}`), nil, http.StatusOK, `{}`},
		{"bulk gzipped", "/npm/internal/-/npm/v1/security/advisories/bulk", gz.Bytes(), []string{"Content-Encoding", "gzip"}, http.StatusOK, `{}`},
		{"not gzip", "/npm/internal/-/npm/v1/security/advisories/bulk", []byte(`{}`), []string{"Content-Encoding", "gzip"}, http.StatusBadRequest, ""},
		{"audits", "/npm/internal/-/npm/v1/security/audits", []byte(`{"name": "x"}`), nil, http.StatusOK, `"vulnerabilities":{"critical":0`},
		{"audits quick", "/npm/internal/-/npm/v1/security/audits/quick", []byte(`{"name": "x"}`), nil, http.StatusOK, `"advisories":{}`},
		{"too large", "/npm/internal/-/npm/v1/security/advisories/bulk", bytes.Repeat([]byte(" "), maxAuditBody+1), nil, http.StatusRequestEntityTooLarge, ""},
	} {
		rr := as(h, alice, http.MethodPost, tc.path, tc.body, tc.header...)
		if rr.Code != tc.status || !strings.Contains(rr.Body.String(), tc.want) || rr.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s", tc.name, rr.Code, rr.Body)
		}
	}
}

// TestSearchProxy: a proxy repo's search lists the packages it holds, rendered from the index,
// and never asks the upstream: the routes here have no proxy core at all.
func TestSearchProxy(t *testing.T) {
	ctx := t.Context()
	p := newPipeline(t)
	proxy := index.Repo{Format: Name, Name: "npmjs", Registry: "local", Prefix: "npm/npmjs", Kind: index.KindProxy}
	must(t, p.st.CreateRepo(ctx, &proxy))
	b, _ := testutil.NpmTarball(testutil.NpmOptions{Name: "left-pad", Version: "1.3.0", Description: "pads left"})
	if _, err := p.pipe.Upload(ctx, proxy, upload(b, ""), alice); err != nil {
		t.Fatal(err)
	}
	rr := get(p.install("", nil), "/npm/npmjs/-/v1/search?text=pad")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"name":"left-pad","version":"1.3.0","description":"pads left"`) {
		t.Errorf("search of a proxy: %d %s", rr.Code, rr.Body)
	}
}
