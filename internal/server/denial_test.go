package server_test

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	_ "github.com/lajstrom/lajstrom/internal/format/deb" // the plugins register themselves
	_ "github.com/lajstrom/lajstrom/internal/format/maven"
	"github.com/lajstrom/lajstrom/internal/format/npm"
	_ "github.com/lajstrom/lajstrom/internal/format/pypi"
	_ "github.com/lajstrom/lajstrom/internal/format/raw"
	_ "github.com/lajstrom/lajstrom/internal/format/rpm"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/server"
)

var denialDBs atomic.Int64

// carolPassword is the password of carol, a user with no binding: she may read no repo.
const carolPassword = "carol-password-1"

// accounts are the authenticator and the tokens service as one npm.Accounts, as serve gives them.
type accounts struct {
	*auth.Authenticator
	*auth.Tokens
}

// denialServer is a server with every registered plugin, the RBAC evaluator and the
// authenticator over a real index, which holds the hosted repo <format>/private of every
// format, without anonymous read, the npm group npm/grouped of npm/private, and the user carol.
func denialServer(t *testing.T) http.Handler {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, fmt.Sprintf("file:denial-%d?mode=memory&cache=shared", denialDBs.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(carolPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(ctx, &index.User{Name: "carol", Kind: auth.KindLocal, PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn := auth.NewAuthenticator(auth.Options{Index: st, Log: log})
	plugins := format.All()
	for i, p := range plugins {
		repo := index.Repo{Format: p.Name(), Name: "private", Registry: "local", Prefix: p.Name() + "/private", Kind: index.KindHosted}
		if err := st.CreateRepo(ctx, &repo); err != nil {
			t.Fatal(err)
		}
		if p.Name() != npm.Name {
			continue
		}
		plugins[i] = npm.Plugin{ExternalURL: "http://registry.example", Accounts: accounts{authn, &auth.Tokens{Index: st, Log: log}}}
		group := index.Repo{Format: npm.Name, Name: "grouped", Kind: index.KindGroup}
		if err := st.CreateRepo(ctx, &group); err != nil {
			t.Fatal(err)
		}
		if err := st.SetGroupMembers(ctx, group.ID, []int64{repo.ID}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := server.New(server.Options{
		Listen:        "127.0.0.1:0",
		Log:           log,
		Plugins:       plugins,
		Deps:          format.Deps{Index: st, Auth: auth.NewRBAC(auth.RBACOptions{Index: st, AnonymousRead: true, Log: log}), Log: log},
		Authenticator: authn,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

// denialRequest is a request below /<format>/<repo>/.
type denialRequest struct {
	method, format, path, body string
	header                     []string // name, value, …
	// status is the answer to both principals; 0 means the denial of docs/spec/rbac.md §3, 401
	// to the anonymous principal and 404 to a user without access.
	status int
}

// The requests a client could try a repo name with: every route of every frontend, reads and
// writes, with input that is well formed and input that is not, so that no check that comes
// before the permission tells that the repo exists.
var denialRequests = []denialRequest{
	{method: http.MethodGet, format: "raw", path: "file.txt"},
	{method: http.MethodHead, format: "raw", path: "file.txt"},
	{method: http.MethodGet, format: "raw", path: "dir/"},
	{method: http.MethodGet, format: "raw", path: "file.txt?digest=sha256:short"},
	{method: http.MethodPut, format: "raw", path: "file.txt", body: "hello"},
	{method: http.MethodPut, format: "raw", path: "dir/", body: "hello"},
	{method: http.MethodDelete, format: "raw", path: "file.txt"},
	{method: http.MethodDelete, format: "raw", path: "file.txt?digest=sha256:short"},

	{method: http.MethodGet, format: "pypi", path: "simple/"},
	{method: http.MethodGet, format: "pypi", path: "simple"},
	{method: http.MethodGet, format: "pypi", path: "simple/Foo.Bar/"},
	{method: http.MethodGet, format: "pypi", path: "simple/foo"},
	{method: http.MethodGet, format: "pypi", path: "simple/foo/", header: []string{"Accept", "application/vnd.pypi.simple.v1+json"}},
	{method: http.MethodGet, format: "pypi", path: "simple/foo/", header: []string{"Accept", "image/png"}},
	{method: http.MethodGet, format: "pypi", path: "files/foo/1.0/foo-1.0.tar.gz"},
	{method: http.MethodPost, format: "pypi", path: "", body: "--x--\r\n", header: []string{"Content-Type", "multipart/form-data; boundary=x"}},
	{method: http.MethodPost, format: "pypi", path: "", body: "not a form"},

	{method: http.MethodGet, format: "maven", path: "com/acme/lib/1.0/lib-1.0.jar"},
	{method: http.MethodGet, format: "maven", path: "com/acme/lib/1.0/lib-1.0.jar.sha1"},
	{method: http.MethodGet, format: "maven", path: "com/acme/lib/maven-metadata.xml"},
	{method: http.MethodGet, format: "maven", path: "x"},
	{method: http.MethodPut, format: "maven", path: "com/acme/lib/1.0/lib-1.0.jar", body: "jar"},
	{method: http.MethodPut, format: "maven", path: "com/acme/lib/1.0/lib-1.0.jar.sha1", body: "0000"},
	{method: http.MethodPut, format: "maven", path: "com/acme/lib/maven-metadata.xml", body: "<metadata/>"},
	{method: http.MethodPut, format: "maven", path: "x", body: "x"},

	{method: http.MethodGet, format: "apt", path: "dists/stable/Release"},
	{method: http.MethodGet, format: "apt", path: "dists/stable/InRelease"},
	{method: http.MethodGet, format: "apt", path: "dists/stable/nothing"},
	{method: http.MethodGet, format: "apt", path: "dists/stable/main/binary-amd64/Packages"},
	{method: http.MethodGet, format: "apt", path: "dists/stable/main/binary-amd64/by-hash/SHA256/" + strings.Repeat("0", 64)},
	{method: http.MethodGet, format: "apt", path: "dists/stable/main/binary-amd64/by-hash/SHA256/short"},
	{method: http.MethodGet, format: "apt", path: "pool/main/f/foo/foo_1.0_amd64.deb"},
	{method: http.MethodGet, format: "apt", path: "key.asc"},
	{method: http.MethodPut, format: "apt", path: "upload?suite=stable&component=main", body: "not a deb"},
	{method: http.MethodPut, format: "apt", path: "upload", body: "not a deb"},

	{method: http.MethodGet, format: "rpm", path: "repodata/repomd.xml"},
	{method: http.MethodGet, format: "rpm", path: "repodata/" + strings.Repeat("0", 64) + "-primary.xml.gz"},
	{method: http.MethodGet, format: "rpm", path: "key.asc"},
	{method: http.MethodGet, format: "rpm", path: "Packages/foo-1.0-1.x86_64.rpm"},
	{method: http.MethodPut, format: "rpm", path: "upload", body: "not an rpm"},

	{method: http.MethodGet, format: "npm", path: "lodash"},
	{method: http.MethodHead, format: "npm", path: "lodash"},
	{method: http.MethodGet, format: "npm", path: "lodash", header: []string{"Accept", "application/vnd.npm.install-v1+json"}},
	{method: http.MethodGet, format: "npm", path: "@acme/widgets"},
	{method: http.MethodGet, format: "npm", path: "@acme%2fwidgets"},
	{method: http.MethodGet, format: "npm", path: "lodash/1.0.0"},
	{method: http.MethodGet, format: "npm", path: "lodash/-/lodash-1.0.0.tgz"},
	{method: http.MethodGet, format: "npm", path: ""},
	{method: http.MethodPut, format: "npm", path: "lodash", body: "{}"},
	{method: http.MethodPut, format: "npm", path: "lodash", body: "not json"},
	{method: http.MethodPut, format: "npm", path: "lodash/-rev/1-abc", body: "{}"},
	{method: http.MethodPut, format: "npm", path: "", body: "{}"},
	{method: http.MethodPut, format: "npm", path: "lodash/x/y", body: "{}"},
	{method: http.MethodPost, format: "npm", path: "lodash", body: "{}"},
	{method: http.MethodDelete, format: "npm", path: "lodash/-rev/1-abc"},
	{method: http.MethodDelete, format: "npm", path: "lodash/-/lodash-1.0.0.tgz/-rev/1-abc"},
	{method: http.MethodDelete, format: "npm", path: "lodash"},
	{method: http.MethodDelete, format: "npm", path: "lodash/x"},
	{method: http.MethodGet, format: "npm", path: "-/package/lodash/dist-tags"},
	{method: http.MethodPut, format: "npm", path: "-/package/lodash/dist-tags/beta", body: `"1.0.0"`},
	{method: http.MethodDelete, format: "npm", path: "-/package/lodash/dist-tags/beta"},
	{method: http.MethodGet, format: "npm", path: "-/package/lodash/dist-tags/beta"},
	{method: http.MethodPut, format: "npm", path: "-/package/lodash/dist-tags", body: `"1.0.0"`},
	{method: http.MethodPost, format: "npm", path: "-/package/lodash/dist-tags", body: `"1.0.0"`},
	{method: http.MethodGet, format: "npm", path: "-/package"},
	{method: http.MethodGet, format: "npm", path: "-/ping"},
	{method: http.MethodGet, format: "npm", path: "-/whoami"},
	{method: http.MethodGet, format: "npm", path: "-/v1/search?text=lodash"},
	{method: http.MethodPost, format: "npm", path: "-/npm/v1/security/advisories/bulk", body: `{"lodash":["1.0.0"]}`},
	{method: http.MethodPost, format: "npm", path: "-/npm/v1/security/audits/quick", body: "not json"},
	{method: http.MethodGet, format: "npm", path: "-/npm/v1/user"},
	{method: http.MethodGet, format: "npm", path: "-/user/org.couchdb.user:carol"},
	// The routes whose answer does not depend on the repo: the web login, 404 everywhere; the
	// legacy login, decided as the user of its body, whatever the request's credentials; the
	// logout, by the request's own token. The wrong password fails six logins of carol (two
	// principals, three repos), below auth.LoginFailures: no answer is the throttle's 429.
	{method: http.MethodPost, format: "npm", path: "-/v1/login", body: "{}", status: http.StatusNotFound},
	{method: http.MethodPut, format: "npm", path: "-/user/org.couchdb.user:carol", body: `{"name":"carol","password":"` + carolPassword + `"}`,
		status: http.StatusNotFound},
	{method: http.MethodPut, format: "npm", path: "-/user/org.couchdb.user:carol", body: `{"name":"carol","password":"wrong"}`,
		status: http.StatusUnauthorized},
	{method: http.MethodPut, format: "npm", path: "-/user/org.couchdb.user:carol", body: `{"name":"dave","password":"wrong"}`,
		status: http.StatusBadRequest},
	{method: http.MethodPut, format: "npm", path: "-/user/org.couchdb.user:carol", body: "not json", status: http.StatusBadRequest},
	{method: http.MethodPut, format: "npm", path: "-/user/carol", body: "{}", status: http.StatusNotFound},
	{method: http.MethodDelete, format: "npm", path: "-/user/token/" + auth.PATPrefix + strings.Repeat("0", 43), status: http.StatusNotFound},
}

// TestUnknownRepoAnswersAsUnreadable: on every frontend, a repo that does not exist and a repo
// that the caller may not read get the same answer, header for header and byte for byte but for
// the repo's name, from the anonymous principal (401) and from a user without access (404), so
// that the answer does not tell whether the repo exists (docs/spec/rbac.md §3, #1091).
func TestUnknownRepoAnswersAsUnreadable(t *testing.T) {
	for _, p := range format.All() {
		if !slices.ContainsFunc(denialRequests, func(rq denialRequest) bool { return rq.format == p.Name() }) {
			t.Errorf("no request of the format %s: add its routes to denialRequests", p.Name())
		}
	}
	h := denialServer(t)
	carol := "Basic " + base64.StdEncoding.EncodeToString([]byte("carol:"+carolPassword))
	for _, who := range []struct {
		name, authorization string
		status              int
	}{{"anonymous", "", http.StatusUnauthorized}, {"carol", carol, http.StatusNotFound}} {
		for _, rq := range denialRequests {
			t.Run(who.name+" "+rq.method+" "+rq.format+" "+rq.path, func(t *testing.T) {
				// Names of one length, so that Content-Length is compared too.
				unknown, _ := denialAnswer(t, h, rq, "missing", who.authorization)
				unreadable, status := denialAnswer(t, h, rq, "private", who.authorization)
				if unreadable != unknown {
					t.Errorf("the repo that may not be read answers\n%s\nthe repo that does not exist\n%s", unreadable, unknown)
				}
				if rq.format == npm.Name {
					if group, _ := denialAnswer(t, h, rq, "grouped", who.authorization); group != unknown {
						t.Errorf("the group that may not be read answers\n%s\nthe repo that does not exist\n%s", group, unknown)
					}
				}
				if want := cmp.Or(rq.status, who.status); status != want {
					t.Errorf("status %d, want %d:\n%s", status, want, unreadable)
				}
			})
		}
	}
}

// denialAnswer is the answer to rq on the repo called repo, as text: the status, the headers but
// those that differ from one request to the next, and the body, each with the repo's name as
// <repo>; and its status.
func denialAnswer(t *testing.T, h http.Handler, rq denialRequest, repo, authorization string) (string, int) {
	t.Helper()
	var body io.Reader
	if rq.body != "" {
		body = strings.NewReader(rq.body)
	}
	req := httptest.NewRequest(rq.method, "/"+rq.format+"/"+repo+"/"+rq.path, body)
	for i := 0; i+1 < len(rq.header); i += 2 {
		req.Header.Set(rq.header[i], rq.header[i+1])
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	norm := strings.NewReplacer(rq.format+"/"+repo, rq.format+"/<repo>", "/"+repo+"/", "/<repo>/", `"`+repo+`"`, `"<repo>"`).Replace
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", rr.Code)
	names := make([]string, 0, len(rr.Header()))
	for name := range rr.Header() {
		if name != "Date" && name != "X-Request-Id" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&b, "%s: %s\n", name, norm(strings.Join(rr.Header().Values(name), ", ")))
	}
	fmt.Fprintf(&b, "\n%s", norm(rr.Body.String()))
	return b.String(), rr.Code
}
