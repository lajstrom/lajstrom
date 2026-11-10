package npm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// routes returns the npm routes over the pipeline, as the server mounts them; authz decides,
// nil allows everything.
func (p *pipeline) routes(authz auth.Authorizer) http.Handler {
	deps := format.Deps{Index: p.st, Ingest: p.pipe, Providers: provider.Map{"local": p.fake}, Auth: authz, Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	Plugin{}.Routes(format.NewRouter(mux, Name, deps), deps)
	return mux
}

// do sends a request to h as alice, or as anonymous when anonymous is set.
func do(h http.Handler, method, path string, body []byte, anonymous bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if !anonymous {
		req = req.WithContext(auth.WithPrincipal(req.Context(), alice))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// npmBody returns the body npm publish sends for a tarball: its version document, the dist-tag
// latest, the attachment; change edits it before it is encoded.
func npmBody(t *testing.T, b []byte, info testutil.NpmInfo, change func(body, doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	must(t, json.Unmarshal([]byte(document(t, info, nil)), &doc))
	body := map[string]any{"_id": info.Name, "name": info.Name, "dist-tags": map[string]any{"latest": info.Version},
		"versions": map[string]any{info.Version: doc}, "access": nil,
		"_attachments": map[string]any{info.Name + "-" + info.Version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(b), "length": len(b)}}}
	if change != nil {
		change(body, doc)
	}
	out, err := json.Marshal(body)
	must(t, err)
	return out
}

// TestPublishCaptured publishes the bodies captured from npm 10 and 11, pnpm 10 and yarn 4, at
// the path each client used (%2f in a scoped name): 201 {"ok": true}, the version indexed with its
// document; the same body again is 201 too.
func TestPublishCaptured(t *testing.T) {
	p := newPipeline(t)
	h := p.routes(nil)
	files, err := filepath.Glob("testdata/publish/*.json")
	must(t, err)
	for _, f := range files {
		body, err := os.ReadFile(f) //nolint:gosec // a captured body of this package's testdata
		must(t, err)
		var b struct{ Name string }
		must(t, json.Unmarshal(body, &b))
		path := "/npm/internal/" + strings.Replace(b.Name, "/", "%2f", 1)
		for range 2 {
			if rr := do(h, http.MethodPut, path, body, false); rr.Code != http.StatusCreated || strings.TrimSpace(rr.Body.String()) != `{"ok":true}` {
				t.Errorf("%s: %d %s", f, rr.Code, rr.Body)
			}
		}
		pkg, err := p.st.GetPackage(t.Context(), p.repo.ID, b.Name)
		must(t, err)
		v, err := p.st.GetVersion(t.Context(), pkg.ID, "1.0.0", "")
		must(t, err)
		var md metadata
		must(t, json.Unmarshal(v.Metadata, &md))
		if md.Name != b.Name || !strings.Contains(md.Readme, "A package for the capture.") || bytes.Contains(md.Manifest, []byte(`"_id"`)) {
			t.Errorf("%s: metadata %s", f, v.Metadata)
		}
	}
}

// TestPublish publishes a scoped package as npm does, at @acme/widgets in two segments, and
// another tarball under its version: 409. A packument update is not implemented yet: 501.
func TestPublish(t *testing.T) {
	p := newPipeline(t)
	h := p.routes(nil)
	b, info := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Dependencies: map[string]string{"widgets": "^1.0.0"}})
	if rr := do(h, http.MethodPut, "/npm/internal/@acme/widgets", npmBody(t, b, info, nil), false); rr.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rr.Code, rr.Body)
	}
	other, oi := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Description: "other bytes"})
	rr := do(h, http.MethodPut, "/npm/internal/@acme%2Fwidgets", npmBody(t, other, oi, nil), false)
	if rr.Code != http.StatusConflict || rr.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(rr.Body.String(), `"error":"`) || !strings.Contains(rr.Body.String(), "@acme/widgets@1.0.0") {
		t.Errorf("another tarball: %d %s", rr.Code, rr.Body)
	}
	for _, tc := range []struct{ path, body string }{
		{"/npm/internal/@acme/widgets", `{"name": "@acme/widgets", "versions": {}}`},
		{"/npm/internal/@acme/widgets/-rev/0-x", `{}`},
	} {
		if rr := do(h, http.MethodPut, tc.path, []byte(tc.body), false); rr.Code != http.StatusNotImplemented {
			t.Errorf("%s: %d %s", tc.path, rr.Code, rr.Body)
		}
	}
	if rr := do(h, http.MethodPut, "/npm/internal/-/whatever", nil, false); rr.Code != http.StatusNotFound {
		t.Errorf("below /-/: %d", rr.Code)
	}
}

// TestPublishRefuses is the 400 rules of the publish (docs/spec/http-routing.md, npm), and 413
// over maxFileSize and the bound of the metadata; each answer is JSON, and nothing is published.
func TestPublishRefuses(t *testing.T) {
	p := newPipeline(t)
	p.repo.Policy = json.RawMessage(`{"maxFileSize": 4096}`)
	must(t, p.st.UpdateRepo(t.Context(), &p.repo))
	h := p.routes(nil)
	b, info := testutil.NpmTarball(testutil.NpmOptions{Dependencies: map[string]string{"lodash": "^4.0.0"}})
	type edit = func(body, doc map[string]any)
	for _, tc := range []struct {
		name, path, want string
		status           int
		edit             edit
	}{
		{"another name in the body", "", "of the package", 400, func(body, _ map[string]any) { body["name"] = "other" }},
		{"an upper-case name", "/npm/internal/Widgets", "upper-case", 400, func(body, _ map[string]any) { body["name"] = "Widgets" }},
		{"two versions", "", "one version", 400, func(body, doc map[string]any) { body["versions"].(map[string]any)["2.0.0"] = doc }},
		{"no attachment", "", "one attachment", 400, func(body, _ map[string]any) { body["_attachments"] = map[string]any{} }},
		{"build metadata", "", "build metadata", 400, func(body, doc map[string]any) {
			body["versions"] = map[string]any{"1.0.0+b": doc}
		}},
		{"a document of another version", "", "is of widgets@2.0.0", 400, func(_, doc map[string]any) { doc["version"] = "2.0.0" }},
		{"another length", "", "a length of 3", 400, func(body, _ map[string]any) {
			body["_attachments"].(map[string]any)["widgets-1.0.0.tgz"].(map[string]any)["length"] = 3
		}},
		{"another integrity", "", "dist.integrity", 400, func(_, doc map[string]any) {
			doc["dist"].(map[string]any)["integrity"] = "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
		}},
		{"another shasum", "", "dist.shasum", 400, func(_, doc map[string]any) { doc["dist"].(map[string]any)["shasum"] = strings.Repeat("0", 40) }},
		{"a malformed dist-tag", "", "dist-tag \"1x\"", 400, func(body, _ map[string]any) { body["dist-tags"] = map[string]any{"1x": "1.0.0"} }},
		{"a dist-tag of another version", "", "names 0.9.0", 400, func(body, _ map[string]any) { body["dist-tags"] = map[string]any{"beta": "0.9.0"} }},
		{"a dependency the tarball has not", "", "differ in dependencies", 400, func(_, doc map[string]any) {
			doc["dependencies"] = map[string]any{"lodash": "^4.0.0", "evil": "*"}
		}},
		{"a readme of 3 MiB", "", "metadata", 413, func(_, doc map[string]any) { doc["readme"] = strings.Repeat("x", 3<<20) }},
		{"a tarball over maxFileSize", "", "maxFileSize of npm/internal", 413, func(body, _ map[string]any) {
			body["_attachments"].(map[string]any)["widgets-1.0.0.tgz"].(map[string]any)["data"] = base64.StdEncoding.EncodeToString(make([]byte, 5000))
		}},
	} {
		path := tc.path
		if path == "" {
			path = "/npm/internal/widgets"
		}
		rr := do(h, http.MethodPut, path, npmBody(t, b, info, tc.edit), false)
		var e struct{ Error string }
		if rr.Code != tc.status || json.Unmarshal(rr.Body.Bytes(), &e) != nil || !strings.Contains(e.Error, tc.want) {
			t.Errorf("%s: %d %s, want %d with %q", tc.name, rr.Code, rr.Body, tc.status, tc.want)
		}
	}
	if pkgs, err := p.st.ListPackages(t.Context(), p.repo.ID, ""); err != nil || len(pkgs) != 0 {
		t.Errorf("packages after the refusals: %+v, %v", pkgs, err)
	}
}

// deny lets nobody do anything.
type deny struct{}

func (deny) Allowed(context.Context, auth.Principal, auth.Action, string, string) (bool, error) {
	return false, nil
}

// TestPublishDenied: anonymous is 401 with the Basic challenge, as JSON, before the body is read.
func TestPublishDenied(t *testing.T) {
	p := newPipeline(t)
	rr := do(p.routes(deny{}), http.MethodPut, "/npm/internal/widgets", []byte("not even JSON"), true)
	if rr.Code != http.StatusUnauthorized || rr.Header().Get("WWW-Authenticate") != `Basic realm="lajstrom"` || !strings.Contains(rr.Body.String(), `"error"`) {
		t.Errorf("anonymous: %d %v %s", rr.Code, rr.Header(), rr.Body)
	}
}
