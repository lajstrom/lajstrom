package probe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
)

// emptyRegistry answers the generic provider's probe and lists names in its catalog; every
// tag list is 404.
func emptyRegistry(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			_, _ = io.WriteString(w, "{}")
		case "/v2/_catalog":
			_, _ = io.WriteString(w, `{"repositories":["`+strings.Join(names, `","`)+`"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestRunRefusesAScratchPrefixThatHoldsSomething(t *testing.T) {
	srv := emptyRegistry(t, "art/lajstrom-probe-taken/create", "other")
	cfg := config.Registry{Name: "r", Type: "generic", URL: srv.URL, Prefix: "art/"}
	_, err := Run(context.Background(), cfg, Options{Scratch: "lajstrom-probe-taken", Log: quiet()})
	if !errors.Is(err, ErrNotEmpty) || !strings.Contains(err.Error(), "art/lajstrom-probe-taken/create") {
		t.Errorf("Run = %v, want ErrNotEmpty naming the repository", err)
	}
}

// TestRunWhereNothingIsCreated runs against a registry that refuses every upload session, as
// ECR without a repository: every cell says what it met, and nothing is left.
func TestRunWhereNothingIsCreated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			_, _ = io.WriteString(w, "{}")
		case "/v2/_catalog":
			_, _ = io.WriteString(w, `{"repositories":[]}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known to registry"}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	r, err := Run(context.Background(), config.Registry{Name: "r", URL: srv.URL}, Options{Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Cell("createOnPush").Value; v != "no: 404 `NAME_UNKNOWN` to the first upload session" {
		t.Errorf("createOnPush = %q", v)
	}
	for _, c := range r.Cells {
		switch c.Key {
		case "createOnPush", "probeFinds", "overrides":
		default:
			if c.Measured {
				t.Errorf("%s = %q, measured", c.Key, c.Value)
			}
		}
	}
	for _, key := range []string{"where", "deleteRepository"} {
		if v := r.Cell(key).Value; !strings.HasPrefix(v, "*not measured*: ") {
			t.Errorf("%s = %q, want why it is not measured", key, v)
		}
	}
	if v := r.Cell("probeFinds").Value; v != "`referrers=false catalog=true`" {
		t.Errorf("probeFinds = %q", v)
	}
	if len(r.LeftBehind) > 0 || len(r.Cell("createOnPush").Exchanges) == 0 {
		t.Errorf("left %v; create exchanges %d", r.LeftBehind, len(r.Cell("createOnPush").Exchanges))
	}
}

func TestRunRefuses(t *testing.T) {
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	srv := emptyRegistry(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	cases := []struct {
		name string
		cfg  config.Registry
		o    Options
		want string
	}{
		{"another type", config.Registry{Name: "r", Type: "gar", URL: srv.URL}, Options{}, "only types generic and ecr"},
		{"ecr without a region", config.Registry{Name: "r", Type: "ecr", URL: srv.URL}, Options{}, "no AWS region"},
		{"scratch outside the grammar", config.Registry{Name: "r", URL: srv.URL}, Options{Scratch: "Upper"}, "not a repository name"},
		{"url with a path", config.Registry{Name: "r", URL: "ftp://x"}, Options{}, "absolute http(s) URL"},
		{"the provider's probe fails", config.Registry{Name: "r", URL: down.URL, Retries: -1}, Options{}, "the provider's probe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.o.Log = quiet()
			if _, err := Run(context.Background(), c.cfg, c.o); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Run = %v, want an error with %q", err, c.want)
			}
		})
	}
}

func TestOverridesVerdict(t *testing.T) {
	probed := Capabilities{CreateOnPush: true, Catalog: true, DeleteManifest: true, BlobRedirect: true}
	withReferrers := probed
	withReferrers.Referrers = true
	quay := Capabilities{CreateOnPush: true, DeleteManifest: true, BlobRedirect: true, BuildRoutes: true}
	cases := []struct {
		name string
		caps Capabilities
		f    facts
		want string
	}{
		{"distribution", probed, facts{catalog: true}, "none"},
		{"zot", withReferrers, facts{catalog: true, referrersAPI: true, referrersKnown: true}, "none"},
		{"referrers not measured", withReferrers, facts{catalog: true}, "none"},
		{"nexus docker", probed, facts{catalog: true, tagsStay: true}, "`deleteManifest: false`, or a delete leaves the tags"},
		{"jcr before the probe fix", withReferrers, facts{referrersAPI: true, referrersKnown: true}, "`catalog: false`"},
		{"zot behind a policy", withReferrers, facts{catalog: true, referrersAPI: true, referrersKnown: true, repointRefused: true}, "`immutableTags: true`"},
		{"gitlab B", probed, facts{catalog: true, profileRefused: true}, "no override helps: **the registry refuses Lajstrom's manifests**"},
		{"ghcr", probed, facts{catalog: true, deleteRefused: true}, "`deleteManifest: false` saves the first refused delete"},
		{"the probe says referrers, wrongly", withReferrers, facts{catalog: true, referrersKnown: true}, "`referrers: false`"},
		// ECR refuses _catalog's last=<name> (405), and the provider lists with DescribeRepositories.
		{"ecr", withReferrers, facts{referrersAPI: true, referrersKnown: true, ownListing: true}, "none"},
		{"quay with 77b369a", quay, facts{buildRoutesRefused: true}, "none"},
		{"quay 3.18.2", quay, facts{buildRoutesTaken: true}, "`buildRoutes: false` lets new packages end in `build`: this registry creates such names"},
		{"build routes elsewhere", probed, facts{catalog: true, buildRoutesRefused: true},
			"`buildRoutes: true`: the registry creates no repository whose name ends in `build`"},
		{"zot, the build names taken", probed, facts{catalog: true, buildRoutesTaken: true}, "none"},
	}
	for _, c := range cases {
		if got := overridesVerdict(c.caps, c.f); got != c.want {
			t.Errorf("%s: overridesVerdict = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNextLink(t *testing.T) {
	cases := []struct{ link, want string }{
		{`</v2/_catalog?last=a&n=2>; rel="next"`, "/v2/_catalog?last=a&n=2"},
		{`<http://r.test/v2/x/tags/list?n=2&last=b>; rel=next`, "http://r.test/v2/x/tags/list?n=2&last=b"},
		{`</v2/x/tags/list?n=2&last=b>; rel="prev"`, ""},
		{`<http://r.test/v2/x/tags/list?cursor=abc>;rel="next"`, "http://r.test/v2/x/tags/list?cursor=abc"},
		{"", ""},
	}
	for _, c := range cases {
		a := ans(200, "")
		if c.link != "" {
			a.header.Set("Link", c.link)
		}
		if got := nextLink(a); got != c.want {
			t.Errorf("nextLink(%q) = %q, want %q", c.link, got, c.want)
		}
	}
}
