package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// stubProxier is a format.Proxier of a format with proxy repos: the API only asks whether a
// format has one, and its Key.
type stubProxier struct{}

func (stubProxier) Key(name string) (string, error) { return name, nil }
func (stubProxier) MetadataRequest(string, string) (format.UpstreamRequest, error) {
	return format.UpstreamRequest{}, format.ErrUnsupported
}
func (stubProxier) ParseMetadata(string, string, []byte) (*format.UpstreamIndex, error) {
	return nil, format.ErrUnsupported
}
func (stubProxier) FileRequest(string) (format.ArtifactRef, error) {
	return format.ArtifactRef{}, format.ErrUnsupported
}
func (stubProxier) RenderProxy(context.Context, index.Repo, string, string, []byte, index.Reader, string) (format.Document, error) {
	return format.Document{}, format.ErrUnsupported
}

// newProxyFixture is newFixture with the format npm, which has proxy repos, and the upstreams
// npmjs (npm) and pypi (pypi).
func newProxyFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, Options{})
	f.repos.Formats = oneOf("raw", "pypi", "npm")
	f.repos.Proxiers = func(name string) (format.Proxier, bool) { return stubProxier{}, name == "npm" }
	f.repos.Upstreams = func(name string) (string, bool) {
		formatName, ok := map[string]string{"npmjs": "npm", "pypi": "pypi"}[name]
		return formatName, ok
	}
	return f
}

// TestCreateProxy: a proxy repo is created with its settings, every key written out with its
// default, and the resource shows them; a hosted repo's resource has no upstream.
func TestCreateProxy(t *testing.T) {
	f := newProxyFixture(t)
	body := `{"format": "npm", "name": "npmjs", "registry": "local", "kind": "proxy", "upstream": {"name": "npmjs", "deny": ["@acme/*"]}}`
	var got Repo
	decodeJSON(t, f.do(http.MethodPost, "/api/v1/repos", body, auth.Principal{}), http.StatusCreated, &got)
	want := `{"format": "npm", "name": "npmjs", "registry": "local", "kind": "proxy", "anonymousRead": false, "policy": null,
		"upstream": {"name": "npmjs", "metadataTTL": "10m", "negativeTTL": "2m", "offline": false, "allow": [], "deny": ["@acme/*"]},
		"createdAt": "2026-01-02T12:00:00Z", "updatedAt": "2026-01-02T12:00:00Z"}`
	wantJSON(t, "the created proxy", got, want)
	decodeJSON(t, f.do(http.MethodGet, "/api/v1/repos/npm/npmjs", "", auth.Principal{}), http.StatusOK, &got)
	wantJSON(t, "the proxy read back", got, want)
	ev := oneAudit(t, f.st, "repo.create")
	if up, _ := ev.details["repo"].(map[string]any)["upstream"].(map[string]any); up["name"] != "npmjs" {
		t.Errorf("audit details %v, want the upstream in the resource", ev.details)
	}

	f.create(t, "raw", "scratch")
	rr := f.do(http.MethodGet, "/api/v1/repos/raw/scratch", "", auth.Principal{})
	var hosted map[string]json.RawMessage
	decodeJSON(t, rr, http.StatusOK, &hosted)
	if _, ok := hosted["upstream"]; ok || string(hosted["kind"]) != `"hosted"` {
		t.Errorf("a hosted repo = %s, want kind hosted and no upstream", rr.Body)
	}
}

func TestCreateProxyInvalid(t *testing.T) {
	const proxy = `{"format": "npm", "name": "npmjs", "registry": "local", "kind": "proxy"`
	tests := []struct{ name, body, want string }{
		{"no upstream", proxy + `}`, "upstream: a proxy repo must name one of upstreams[].name"},
		{"null upstream", proxy + `, "upstream": null}`, "upstream: a proxy repo must name one"},
		{"no upstream name", proxy + `, "upstream": {"deny": ["@acme/*"]}}`, "upstream.name: must be one of upstreams[].name"},
		{"unknown upstream", proxy + `, "upstream": {"name": "verdaccio"}}`, `upstream.name: "verdaccio" is not a configured upstream`},
		{"upstream of another format", proxy + `, "upstream": {"name": "pypi"}}`, `upstream.name: "pypi" serves pypi, not npm`},
		{"a URL", proxy + `, "upstream": {"name": "npmjs", "url": "http://169.254.169.254/"}}`, `upstream: "url" is not a key`},
		{"a TTL out of range", proxy + `, "upstream": {"name": "npmjs", "negativeTTL": "25h"}}`,
			`upstream.negativeTTL: "25h" is not a duration from 0s to 24h`},
		{"a pattern with a space", proxy + `, "upstream": {"name": "npmjs", "allow": ["a b"]}}`, "upstream.allow[0]: "},
		{"a format without proxy repos", `{"format": "pypi", "name": "pypi", "registry": "local", "kind": "proxy", "upstream": {"name": "pypi"}}`,
			"kind: format pypi has no proxy repos"},
		{"oci", `{"format": "oci", "name": "hub", "registry": "local", "kind": "proxy", "upstream": {"name": "npmjs"}}`,
			"kind: format oci has no proxy repos"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newProxyFixture(t)
			wantError(t, f.do(http.MethodPost, "/api/v1/repos", tt.body, auth.Principal{}), http.StatusBadRequest, codeInvalid, tt.want)
		})
	}
}

// TestBootstrapProxy: the bootstrap creates a configured proxy as the API does, and a proxy
// whose settings differ from the configuration's, defaults filled in, is a warning.
func TestBootstrapProxy(t *testing.T) {
	f := newProxyFixture(t)
	ctx := context.Background()
	cfg := []config.Repo{{Name: "npmjs", Format: "npm", Registry: "local", Kind: config.KindProxy,
		Upstream: map[string]any{"name": "npmjs", "deny": []any{"@acme/*"}}}}
	if created, err := f.repos.Bootstrap(ctx, cfg); err != nil || created != 1 {
		t.Fatalf("Bootstrap = %d, %v; want 1 created", created, err)
	}
	r, err := f.repos.Get(ctx, "npm", "npmjs")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != index.KindProxy || !samePolicy(r.Upstream, json.RawMessage(
		`{"name":"npmjs","metadataTTL":"10m","negativeTTL":"2m","offline":false,"allow":[],"deny":["@acme/*"]}`)) {
		t.Errorf("bootstrapped repo %+v, upstream %s", r, r.Upstream)
	}
	if created, err := f.repos.Bootstrap(ctx, cfg); err != nil || created != 0 {
		t.Fatalf("second Bootstrap = %d, %v; want none created", created, err)
	}
	if strings.Contains(f.logs.String(), "differs") {
		t.Errorf("a warning for the same settings, defaults left out:\n%s", f.logs)
	}
	f.logs.Reset()
	cfg[0].Upstream = map[string]any{"name": "npmjs", "metadataTTL": "1h"}
	if _, err := f.repos.Bootstrap(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	wantLogs(t, f.logs, `"repo":"npm/npmjs","field":"upstream"`)
	f.logs.Reset()
	cfg[0].Kind, cfg[0].Upstream = "", nil
	if _, err := f.repos.Bootstrap(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	wantLogs(t, f.logs, `"field":"upstream","config":""`, `"field":"kind","config":"hosted","controlPlane":"proxy"`)

	bad := []config.Repo{{Name: "other", Format: "npm", Registry: "local", Kind: config.KindProxy,
		Upstream: map[string]any{"name": "npmjs", "offline": "yes"}}}
	if _, err := f.repos.Bootstrap(ctx, bad); err == nil || err.Error() != "repos[0]: upstream.offline: must not be a JSON string" {
		t.Errorf("Bootstrap of bad settings = %v", err)
	}
}
