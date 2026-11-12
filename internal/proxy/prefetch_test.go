package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// at sets the upstream's publish time of a version.
func at(t string) func(*upstreamVersion) { return func(v *upstreamVersion) { v.Time = t } }

// statuses are the results of a prefetch without their messages.
func statuses(results []proxy.PrefetchResult) []proxy.PrefetchResult {
	out := make([]proxy.PrefetchResult, len(results))
	for i, r := range results {
		r.Message = ""
		out[i] = r
	}
	return out
}

// TestPrefetch: a prefetch fetches the versions the upstream's pointers name, or the newest
// when they name none, every version with all, through the fetch of a miss with its audit rows;
// what the repo holds is held, and a name or an entry that fails is failed with the status of a
// request, while the others go on (docs/spec/proxy-and-group.md §5.1).
func TestPrefetch(t *testing.T) {
	e := newEnv(t, `{"name": "up", "deny": ["secret*"]}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	e.up.publish("tool", "1.1.0", "one-one", nil)
	e.up.publish("tool", "2.0.0-rc.1", "two", nil)
	e.up.tag("tool", "latest", "1.1.0")
	e.up.tag("tool", "next", "2.0.0-rc.1")
	e.up.publish("lib", "0.9.0", "old", at("2026-01-01T00:00:00Z"))
	e.up.publish("lib", "0.10.0", "new", at("2026-02-01T00:00:00Z"))
	e.up.publish("lib", "0.8.0", "older", at("2025-12-01T00:00:00Z"))
	e.up.publish("bad", "1.0.0", "bytes", func(v *upstreamVersion) { v.SHA512 = fmt.Sprintf("%0128x", 1) })
	c := e.core(t, "r1", proxy.Options{})

	names := []string{"tool", "lib", "gone", "secret-sauce", "bad"}
	got, err := c.Prefetch(asAlice(t), e.repo, names, false)
	must(t, err)
	want := []proxy.PrefetchResult{
		{Name: "tool", Version: "1.1.0", Status: "fetched"},
		{Name: "tool", Version: "2.0.0-rc.1", Status: "fetched"},
		{Name: "lib", Version: "0.10.0", Status: "fetched"},
		{Name: "gone", Status: proxy.PrefetchFailed, HTTPStatus: http.StatusNotFound},
		{Name: "secret-sauce", Status: proxy.PrefetchFailed, HTTPStatus: http.StatusForbidden},
		{Name: "bad", Version: "1.0.0", Status: proxy.PrefetchFailed, HTTPStatus: http.StatusBadGateway},
	}
	if d := cmp.Diff(want, statuses(got)); d != "" {
		t.Fatalf("the first prefetch (-want +got)\n%s", d)
	}
	for _, r := range got {
		if r.Status == proxy.PrefetchFailed && r.Message == "" {
			t.Errorf("%s %s failed without a message", r.Name, r.Version)
		}
	}
	if evs, _ := audits(t, e, "proxy.fetch"); len(evs) != 3 || evs[0].Principal != alice.String() {
		t.Errorf("proxy.fetch rows %+v, want 3 by alice", evs)
	}
	if evs, details := audits(t, e, "proxy.reject"); len(evs) != 1 || evs[0].Subject != "bad 1.0.0" || details[0]["reason"] != "checksum" {
		t.Errorf("proxy.reject rows %+v %v, want the checksum of bad 1.0.0", evs, details)
	}

	downloads := e.up.fileCalls.Load()
	got, err = c.Prefetch(asAlice(t), e.repo, []string{"tool", "lib"}, false)
	must(t, err)
	want = []proxy.PrefetchResult{
		{Name: "tool", Version: "1.1.0", Status: "held"},
		{Name: "tool", Version: "2.0.0-rc.1", Status: "held"},
		{Name: "lib", Version: "0.10.0", Status: "held"},
	}
	if d := cmp.Diff(want, got); d != "" || e.up.fileCalls.Load() != downloads {
		t.Errorf("the second prefetch (-want +got)\n%s\ndownloads %d, want %d", d, e.up.fileCalls.Load(), downloads)
	}

	got, err = c.Prefetch(asAlice(t), e.repo, []string{"tool"}, true)
	must(t, err)
	want = []proxy.PrefetchResult{
		{Name: "tool", Version: "1.0.0", Status: "fetched"},
		{Name: "tool", Version: "1.1.0", Status: "held"},
		{Name: "tool", Version: "2.0.0-rc.1", Status: "held"},
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("every version (-want +got)\n%s", d)
	}
}

// TestPrefetchRefused: a repo that is not a proxy, a format without the hook, too few or too
// many names, and an offline repo or one whose upstream is not configured are refused before
// anything is asked of the upstream; a request that ended attempts nothing.
func TestPrefetchRefused(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	c := e.core(t, "r1", proxy.Options{})
	hosted := e.repo
	hosted.Kind = index.KindHosted
	many := make([]string, proxy.MaxPrefetchNames+1)
	for i := range many {
		many[i] = fmt.Sprintf("n%d", i)
	}
	offline := e.repo
	offline.Upstream = json.RawMessage(`{"name": "up", "offline": true}`)
	unknown := e.repo
	unknown.Upstream = json.RawMessage(`{"name": "elsewhere"}`)
	hookless := e.core(t, "r2", proxy.Options{Plugins: func(string) (format.Plugin, bool) { return proxierOnly{e.plugin.Fake, e.plugin}, true }})
	for _, tc := range []struct {
		name  string
		c     *proxy.Core
		repo  index.Repo
		names []string
		want  error
	}{
		{"hosted", c, hosted, []string{"tool"}, format.ErrInvalid},
		{"no names", c, e.repo, nil, format.ErrInvalid},
		{"too many", c, e.repo, many, format.ErrInvalid},
		{"no hook", hookless, e.repo, []string{"tool"}, format.ErrInvalid},
		{"offline", c, offline, []string{"tool"}, index.ErrConflict},
		{"not configured", c, unknown, []string{"tool"}, index.ErrConflict},
	} {
		if _, err := tc.c.Prefetch(asAlice(t), tc.repo, tc.names, false); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if n := e.up.metaCalls.Load() + e.up.fileCalls.Load(); n != 0 {
		t.Errorf("%d requests to the upstream, want none", n)
	}

	ended, cancel := context.WithCancel(asAlice(t))
	cancel()
	got, err := c.Prefetch(ended, e.repo, []string{"tool"}, false)
	if err != nil || len(got) != 1 || got[0].Status != proxy.PrefetchFailed || e.up.fileCalls.Load() != 0 {
		t.Errorf("an ended request: %+v, %v, %d downloads; want tool failed and nothing fetched", got, err, e.up.fileCalls.Load())
	}
}

// proxierOnly is the plugin of the tests without format.Prefetcher.
type proxierOnly struct {
	formattest.Fake
	format.Proxier
}

// TestPrefetchTwoReplicas: two replicas that prefetch the same names at once download each
// version once, through the fetch claims; each version ends fetched on one replica and held or
// shared on the other.
func TestPrefetchTwoReplicas(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		e.up.publish("tool", v, "tool "+v, nil)
	}
	e.up.fileDelay.Store(int64(100 * time.Millisecond))
	knobs := proxy.Options{WaitFirst: 10 * time.Millisecond, WaitLongest: 40 * time.Millisecond}
	replicas := []*proxy.Core{e.core(t, "r1:1", knobs), e.core(t, "r2:1", knobs)}
	var wg sync.WaitGroup
	got := make([][]proxy.PrefetchResult, len(replicas))
	errs := make([]error, len(replicas))
	for i, c := range replicas {
		wg.Go(func() { got[i], errs[i] = c.Prefetch(asAlice(t), e.repo, []string{"tool"}, true) })
	}
	wg.Wait()
	fetched := 0
	for i := range replicas {
		if errs[i] != nil || len(got[i]) != 3 {
			t.Fatalf("replica %d: %+v, %v", i, got[i], errs[i])
		}
		for _, r := range got[i] {
			switch r.Status {
			case "fetched":
				fetched++
			case "held", "shared":
			default:
				t.Errorf("replica %d: %+v", i, r)
			}
		}
	}
	if n := e.up.fileCalls.Load(); n != 3 || fetched != 3 {
		t.Errorf("%d downloads and %d fetched across two replicas, want 3 and 3", n, fetched)
	}
	if evs, _ := audits(t, e, "proxy.fetch"); len(evs) != 3 {
		t.Errorf("%d proxy.fetch rows, want 3", len(evs))
	}
}
