package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// OCIRoute routes as the core does, with the stub's library/ rule and the member's lists.
func (s *stubProxy) OCIRoute(group index.Repo, members []index.Repo, path string) (index.Repo, string, bool) {
	name, ok := proxy.Route(proxy.StoredPaths(group.Group), path)
	i := slices.IndexFunc(members, func(m index.Repo) bool { return m.Name == name })
	if !ok || i < 0 {
		return index.Repo{}, "", false
	}
	m, at := members[i], path
	if m.Kind == index.KindProxy {
		at = s.OCIPath(m, path)
		if st, err := proxy.ParseSettings(m.Upstream); err != nil || func() bool { _, r := st.Refuses(at); return r }() {
			return index.Repo{}, "", false
		}
	}
	return m, at, true
}

// via is what the stub records of the group ctx came through: " via <format>/<group>", or "".
func via(ctx context.Context) string {
	if g, ok := format.ViaFrom(ctx); ok {
		return " via " + g.Format + "/" + g.Name
	}
	return ""
}

// ociGroupEnv is proxyEnv with an authorizer, the proxy oci/hub denying private/*, and the group
// oci/all of the hosted oci/images, which serves acme/*, and of hub, which serves the rest; and
// oci/narrow, whose one route is acme/* to images. bob may read oci/all only, carol oci/images
// only, dave oci/all and every write on it.
func ociGroupEnv(t *testing.T) (*env, *stubProxy) {
	t.Helper()
	s := &stubProxy{manifests: map[string]stubDoc{}, blobs: map[digest.Digest][]byte{}}
	all := []auth.Action{auth.ActionRead, auth.ActionWrite, auth.ActionDelete, auth.ActionManage}
	e := newEnv(t, envOptions{ociProxy: s, authz: authorizer{
		"bob": {"oci/all": {auth.ActionRead}, "oci/narrow": {auth.ActionRead}}, "carol": {"oci/images": {auth.ActionRead}},
		"dave": {"oci/all": all, "oci/images": all}}})
	s.reg = e.prov
	ctx := t.Context()
	hub := index.Repo{Format: "oci", Name: "hub", Registry: "local", Prefix: "oci/hub", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "dockerhub", "deny": ["private/*"]}`)}
	must(t, e.st.CreateRepo(ctx, &hub))
	for _, g := range []struct {
		name, paths string
		members     []int64
	}{
		{"all", `{"paths": {"images": ["acme/*"], "hub": ["*"]}}`, []int64{e.oci.ID, hub.ID}},
		{"narrow", `{"paths": {"images": ["acme/*"]}}`, []int64{e.oci.ID}},
	} {
		r := index.Repo{Format: "oci", Name: g.name, Prefix: "oci/" + g.name, Kind: index.KindGroup, Group: json.RawMessage(g.paths)}
		must(t, e.st.CreateRepo(ctx, &r))
		must(t, e.st.SetGroupMembers(ctx, r.ID, g.members))
	}
	return e, s
}

// TestOCIGroupPull: a group's path is answered by its routed member, as at the member's own URL:
// a hosted member's manifest, tags and referrers from its backend, a proxy member through the
// core with the group in its context, Docker Hub's library/ after the route and ?ns= ignored; a
// path no route selects, or one the proxy's lists refuse, is NAME_UNKNOWN
// (docs/spec/proxy-and-group.md §12.2, §12.3).
func TestOCIGroupPull(t *testing.T) {
	e, s := ociGroupEnv(t)
	body, d := manifest(t, []byte("api"), "api")
	wantStatus(t, "PUT to the hosted member", e.do(e.h, http.MethodPut, "/v2/oci/images/acme/api/manifests/1.4", bytes.NewReader(body),
		"Content-Type", ocispecManifest, principalHeader, "dave"), http.StatusCreated)

	rr := e.do(e.h, http.MethodGet, "/v2/oci/all/acme/api/manifests/1.4", nil, principalHeader, "bob")
	wantStatus(t, "GET of the hosted member's tag", rr, http.StatusOK)
	wantHeader(t, "GET of the hosted member's tag", rr, "Docker-Content-Digest", d.String())
	if !bytes.Equal(rr.Body.Bytes(), body) {
		t.Errorf("body = %s", rr.Body)
	}
	wantStatus(t, "GET by digest", e.do(e.h, http.MethodGet, "/v2/oci/all/acme/api/manifests/"+d.String(), nil, principalHeader, "bob"), http.StatusOK)
	rr = e.do(e.h, http.MethodGet, "/v2/oci/all/acme/api/tags/list?n=1", nil, principalHeader, "bob")
	wantStatus(t, "tags/list", rr, http.StatusOK)
	if rr.Body.String() != `{"name":"oci/all/acme/api","tags":["1.4"]}`+"\n" && rr.Body.String() != `{"name":"oci/all/acme/api","tags":["1.4"]}` {
		t.Errorf("tags/list = %s, want the group's name", rr.Body)
	}
	rr = e.do(e.h, http.MethodGet, "/v2/oci/all/acme/api/referrers/"+d.String(), nil, principalHeader, "bob")
	wantStatus(t, "referrers", rr, http.StatusOK)
	wantHeader(t, "referrers", rr, "Content-Type", ocispec.MediaTypeImageIndex)

	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	s.manifests["library/nginx@1.27"] = stubDoc{desc: desc, raw: raw}
	layer := []byte("layer bytes")
	ld := digest.FromBytes(layer)
	s.blobs[ld] = layer
	wantStatus(t, "GET of a proxy member's tag", e.do(e.h, http.MethodGet, "/v2/oci/all/nginx/manifests/1.27?ns=docker.io", nil,
		principalHeader, "bob"), http.StatusOK)
	wantStatus(t, "HEAD of library/nginx", e.do(e.h, http.MethodHead, "/v2/oci/all/library/nginx/manifests/1.27", nil, principalHeader, "bob"), http.StatusOK)
	rr = e.do(e.h, http.MethodGet, "/v2/oci/all/nginx/blobs/"+ld.String(), nil, principalHeader, "bob")
	wantStatus(t, "GET of a blob", rr, http.StatusOK)
	if !bytes.Equal(rr.Body.Bytes(), layer) {
		t.Errorf("blob = %q", rr.Body)
	}
	want := []string{"manifest library/nginx@1.27 via oci/all", "complete library/nginx@" + desc.Digest.String() + " via oci/all",
		"manifest library/nginx@1.27 via oci/all", "pulling library/nginx via oci/all", "blob library/nginx true via oci/all",
		"pulled library/nginx"}
	if got := strings.Join(s.calls, "|"); got != strings.Join(want, "|") {
		t.Errorf("the core was asked\n%q\nwant\n%q", s.calls, want)
	}

	for _, tt := range []struct{ what, target string }{
		{"a path the proxy's lists refuse", "/v2/oci/all/private/x/manifests/1"},
		{"a path no route selects", "/v2/oci/narrow/library/nginx/manifests/1.27"},
		{"a blob of a path no route selects", "/v2/oci/narrow/other/blobs/" + ld.String()},
	} {
		msg := wantError(t, tt.what, e.do(e.h, http.MethodGet, tt.target, nil, principalHeader, "bob"), http.StatusNotFound, codeNameUnknown)
		if !strings.Contains(msg, "no route of the group") {
			t.Errorf("%s: message %q", tt.what, msg)
		}
	}
	if !strings.Contains(e.logs.String(), "member=oci/hub path=library/nginx") {
		t.Errorf("no debug line of the routed member:\n%s", e.logs)
	}
}

// TestOCIGroupReadPermission: read on the group is the whole permission: bob reads the hosted
// member's path through the group, not at its own URL; carol, who reads the member, gets the
// group's denial, and anonymous a 401 (rbac.md §7).
func TestOCIGroupReadPermission(t *testing.T) {
	e, _ := ociGroupEnv(t)
	body, _ := manifest(t, []byte("api"), "api")
	wantStatus(t, "PUT", e.do(e.h, http.MethodPut, "/v2/oci/images/acme/api/manifests/1.4", bytes.NewReader(body),
		"Content-Type", ocispecManifest, principalHeader, "dave"), http.StatusCreated)
	wantStatus(t, "bob through the group", e.do(e.h, http.MethodGet, "/v2/oci/all/acme/api/manifests/1.4", nil, principalHeader, "bob"), http.StatusOK)
	wantError(t, "bob at the member's URL", e.do(e.h, http.MethodGet, "/v2/oci/images/acme/api/manifests/1.4", nil, principalHeader, "bob"),
		http.StatusNotFound, codeNameUnknown)
	wantStatus(t, "carol at the member's URL", e.do(e.h, http.MethodGet, "/v2/oci/images/acme/api/manifests/1.4", nil, principalHeader, "carol"), http.StatusOK)
	wantError(t, "carol through the group", e.do(e.h, http.MethodGet, "/v2/oci/all/acme/api/manifests/1.4", nil, principalHeader, "carol"),
		http.StatusNotFound, codeNameUnknown)
	wantError(t, "carol, a path no route selects", e.do(e.h, http.MethodGet, "/v2/oci/narrow/x/manifests/1", nil, principalHeader, "carol"),
		http.StatusNotFound, codeNameUnknown)
	rr := e.do(e.h, http.MethodGet, "/v2/oci/all/acme/api/manifests/1.4", nil)
	wantError(t, "anonymous through the group", rr, http.StatusUnauthorized, codeUnauthorized)
	wantHeader(t, "anonymous", rr, "WWW-Authenticate", `Bearer realm="http://example.com/v2/token",service="example.com",scope="repository:oci/all/acme/api:pull"`)

	tok := e.getToken(e.h, "bob", "repository:oci/all/acme/api:pull")
	wantStatus(t, "bob with a token", e.do(e.h, http.MethodGet, "/v2/oci/all/acme/api/manifests/1.4", nil, "Authorization", "Bearer "+tok), http.StatusOK)
}

// TestOCIGroupWritesRefused: every write through the group is 405 UNSUPPORTED with Allow: GET,
// HEAD once the principal may read the group, whatever the path and whatever it may do on the
// members; the group's denial otherwise; nothing reaches the backend (§12.3).
func TestOCIGroupWritesRefused(t *testing.T) {
	e, _ := ociGroupEnv(t)
	body, d := manifest(t, []byte("api"), "api")
	writes := []struct{ method, target string }{
		{http.MethodPut, "/v2/oci/all/acme/api/manifests/1.4"},
		{http.MethodPut, "/v2/oci/all/library/nginx/manifests/" + d.String()},
		{http.MethodDelete, "/v2/oci/all/acme/api/manifests/1.4"},
		{http.MethodDelete, "/v2/oci/all/acme/api/blobs/" + d.String()},
		{http.MethodPost, "/v2/oci/all/acme/api/blobs/uploads/"},
		{http.MethodPost, "/v2/oci/all/acme/api/blobs/uploads/?mount=" + d.String() + "&from=oci/images/acme/api"},
		{http.MethodPatch, "/v2/oci/all/acme/api/blobs/uploads/0123456789abcdef0123456789abcdef"},
		{http.MethodGet, "/v2/oci/all/acme/api/blobs/uploads/0123456789abcdef0123456789abcdef"},
		{http.MethodPut, "/v2/oci/narrow/nowhere/manifests/1"},
	}
	for _, w := range writes {
		for _, user := range []string{"dave", "bob"} {
			what := fmt.Sprintf("%s %s by %s", w.method, w.target, user)
			rr := e.do(e.h, w.method, w.target, bytes.NewReader(body), "Content-Type", ocispecManifest, principalHeader, user)
			if user == "bob" && strings.Contains(w.target, "oci/all") {
				wantError(t, what, rr, http.StatusMethodNotAllowed, codeUnsupported)
				wantHeader(t, what, rr, "Allow", "GET, HEAD")
				continue
			}
			if user == "bob" { // narrow: bob reads it
				wantError(t, what, rr, http.StatusMethodNotAllowed, codeUnsupported)
				continue
			}
			if strings.Contains(w.target, "oci/narrow") { // dave cannot read narrow
				wantError(t, what, rr, http.StatusNotFound, codeNameUnknown)
				continue
			}
			wantError(t, what, rr, http.StatusMethodNotAllowed, codeUnsupported)
		}
		wantError(t, "carol: "+w.method+" "+w.target, e.do(e.h, w.method, w.target, bytes.NewReader(body), "Content-Type", ocispecManifest,
			principalHeader, "carol"), http.StatusNotFound, codeNameUnknown)
	}
	if n := len(e.prov.PushReferences()) + len(e.prov.Pushes()); n != 0 {
		t.Errorf("%d pushes, want none", n)
	}
}

// TestOCIGroupToken: the token endpoint grants pull at most on a group's path, though dave may
// write the group (rbac.md §4).
func TestOCIGroupToken(t *testing.T) {
	e, _ := ociGroupEnv(t)
	access := payload(t, e.getToken(e.h, "dave", "repository:oci/all/acme/api:pull,push,delete", "repository:oci/images/acme/api:pull,push"))["access"]
	if got := fmt.Sprint(access); got != "[map[actions:[pull] name:oci/all/acme/api type:repository] map[actions:[pull push] name:oci/images/acme/api type:repository]]" {
		t.Errorf("access = %s", got)
	}
}

// TestOCIGroupCatalog: the catalog lists to a reader of the group each member's repositories that
// the routes send to that member, under the group; a repository a member holds at a path routed
// elsewhere is not the group's (§12.3).
func TestOCIGroupCatalog(t *testing.T) {
	e, _ := ociGroupEnv(t)
	for _, target := range []string{"/v2/oci/images/acme/api/manifests/1", "/v2/oci/images/other/tool/manifests/1"} {
		body, _ := manifest(t, []byte(target), target)
		wantStatus(t, "PUT "+target, e.do(e.h, http.MethodPut, target, bytes.NewReader(body), "Content-Type", ocispecManifest, principalHeader, "dave"),
			http.StatusCreated)
	}
	hub, err := e.st.GetRepo(t.Context(), "oci", "hub")
	must(t, err)
	for _, p := range []string{"oci/hub/library/nginx", "oci/hub/private/x"} {
		must(t, e.st.UpsertOCIRepo(t.Context(), &index.OCIRepo{RepoID: hub.ID, Path: p, SeenAt: e.clock.Now()}))
	}
	names := slices.Concat(e.listing(e.h, "/v2/_catalog", "repositories", principalHeader, "bob")...)
	if want := []string{"oci/all/acme/api", "oci/all/library/nginx", "oci/narrow/acme/api"}; !slices.Equal(names, want) {
		t.Errorf("bob's catalog %q, want %q", names, want)
	}
	names = slices.Concat(e.listing(e.h, "/v2/_catalog", "repositories", principalHeader, "carol")...)
	if want := []string{"oci/images/acme/api", "oci/images/other/tool"}; !slices.Equal(names, want) {
		t.Errorf("carol's catalog %q, want %q", names, want)
	}
}
