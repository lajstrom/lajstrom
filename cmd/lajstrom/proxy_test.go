package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// withHooks is a plugin with the proxy hooks, none of them called here.
type withHooks struct {
	formattest.Fake
	format.Proxier
}

// TestLoadUpstreams: serve reads the upstreams' credential files, and refuses an upstream of a
// format that has no proxy repos in the build.
func TestLoadUpstreams(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plugins := func(name string) (format.Plugin, bool) {
		switch name {
		case "npm":
			return withHooks{Fake: formattest.Fake{FormatName: "npm"}}, true
		case "raw":
			return formattest.Fake{FormatName: "raw"}, true
		}
		return nil, false
	}
	log := slog.New(slog.DiscardHandler)
	c := &config.Config{Upstreams: []config.Upstream{{Name: "npmjs", Format: "npm", URL: "https://registry.npmjs.org/",
		Auth: config.UpstreamAuth{TokenFile: token}}}}
	ups, err := loadUpstreams(c, plugins, log)
	if err != nil || ups["npmjs"] == nil || ups["npmjs"].Format() != "npm" {
		t.Fatalf("loadUpstreams = %v, %v", ups, err)
	}
	for _, tt := range []struct{ format, want string }{
		{"raw", `upstreams[0].format: "raw" is not a format of this build that has proxy repos`},
		{"nope", `upstreams[0].format: "nope" is not a format`},
	} {
		c.Upstreams[0].Format = tt.format
		if _, err := loadUpstreams(c, plugins, log); err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("format %s: %v, want %q", tt.format, err, tt.want)
		}
	}
	c.Upstreams[0].Format, c.Upstreams[0].Auth.TokenFile = "npm", filepath.Join(t.TempDir(), "missing")
	if _, err := loadUpstreams(c, plugins, log); err == nil || !strings.HasPrefix(err.Error(), "upstreams[0].auth.tokenFile: open ") {
		t.Errorf("a missing token file: %v", err)
	}
}

// TestCheckProxyRepos: a proxy repo whose upstream is not configured is an error in the log,
// and serve goes on; the sweep loop stops with its context.
func TestCheckProxyRepos(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	st, err := sqlite.Open(ctx, "file:"+filepath.Join(t.TempDir(), "lajstrom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range []index.Repo{
		{Format: "npm", Name: "npmjs", Registry: "local", Kind: index.KindProxy, Upstream: json.RawMessage(`{"name": "npmjs"}`)},
		{Format: "npm", Name: "old", Registry: "local", Kind: index.KindProxy, Upstream: json.RawMessage(`{"name": "verdaccio"}`)},
		{Format: "npm", Name: "internal", Registry: "local", Kind: index.KindHosted},
	} {
		if err := st.CreateRepo(ctx, &r); err != nil {
			t.Fatal(err)
		}
	}
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	ups := map[string]*proxy.Upstream{"npmjs": nil}
	if err := checkProxyRepos(ctx, st, ups, log); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); strings.Count(got, "level=ERROR") != 1 || !strings.Contains(got, "repo=npm/old upstream=verdaccio") {
		t.Errorf("log:\n%s\nwant one error for npm/old", got)
	}

	done := make(chan struct{})
	go func() {
		sweepProxyMeta(ctx, proxy.New(proxy.Options{Index: st, Log: log}), log, 5*time.Millisecond)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
}
