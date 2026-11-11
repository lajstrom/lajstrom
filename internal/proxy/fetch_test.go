package proxy_test

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// alice is the reader whose requests miss.
var alice = auth.Principal{Kind: auth.KindUser, Name: "alice"}

// asAlice is a request context of alice.
func asAlice(t *testing.T) context.Context { return auth.WithPrincipal(t.Context(), alice) }

// fileOf is the request path of version of name.
func fileOf(name, version string) string { return name + "/-/" + name + "-" + version + ".tgz" }

// audits returns the audit rows of action, newest first, with their details.
func audits(t *testing.T, e *env, action string) ([]index.AuditEvent, []map[string]any) {
	t.Helper()
	evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: action})
	must(t, err)
	details := make([]map[string]any, len(evs))
	for i, ev := range evs {
		must(t, json.Unmarshal(ev.Details, &details[i]))
	}
	return evs, details
}

// TestFileMiss: the first request of a file fetches it, checks it and publishes it through
// ingest with its origin, by the reader who asked, before it is answered
// (docs/spec/proxy-and-group.md §5).
func TestFileMiss(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "hello", nil)
	c := e.core(t, "r1", proxy.Options{})

	m, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0"))
	must(t, err)
	sum := sha512.Sum512([]byte("hello"))
	origin := map[string]string{profile.AnnotationOriginURL: e.up.srv.URL + "/files/tool-1.0.0.tgz",
		profile.AnnotationOriginChecksum: "sha512:" + hex.EncodeToString(sum[:])}
	if m.Version.Version != "1.0.0" || m.File.Name != "tool-1.0.0.tgz" || m.File.Size != 5 ||
		m.Version.Attrs[profile.AnnotationOriginURL] != origin[profile.AnnotationOriginURL] ||
		m.Version.Attrs[profile.AnnotationOriginChecksum] != origin[profile.AnnotationOriginChecksum] {
		t.Errorf("file %+v, version attrs %v; want the origin %v", m.File, m.Version.Attrs, origin)
	}
	if pushes := e.reg.PushReferences(); len(pushes) != 1 || pushes[0].Name != "raw/up/tool" || pushes[0].Tag != "1.0.0" {
		t.Errorf("manifests pushed: %+v, want raw/up/tool:1.0.0", pushes)
	}
	evs, details := audits(t, e, "proxy.fetch")
	if len(evs) != 1 || evs[0].Principal != alice.String() || evs[0].Repo != "raw/up" || evs[0].Subject != "tool 1.0.0" {
		t.Fatalf("proxy.fetch rows %+v, want one by alice of tool 1.0.0", evs)
	}
	want := map[string]any{"upstream": "up", "url": origin[profile.AnnotationOriginURL],
		"checksum": origin[profile.AnnotationOriginChecksum], "size": float64(5)}
	if d := cmp.Diff(want, details[0]); d != "" {
		t.Errorf("proxy.fetch details (-want +got)\n%s", d)
	}
	if _, err := e.st.GetFetchClaim(t.Context(), e.repo.ID, "tool", "1.0.0"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("the fetch claim after the fetch: %v, want it released", err)
	}
}

// TestFileHit: a file the repo holds is served without the upstream; a version the upstream
// does not list and a path that names no file are 404.
func TestFileHit(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "hello", nil)
	c := e.core(t, "r1", proxy.Options{})
	m, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0"))
	must(t, err)
	again, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0"))
	must(t, err)
	if again.File.Digest != m.File.Digest || e.up.fileCalls.Load() != 1 {
		t.Errorf("a hit: %v, %d downloads; want the held file and 1 download", again.File.Digest, e.up.fileCalls.Load())
	}
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "9.9.9")); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a version the upstream does not list: %v, want ErrNotFound", err)
	}
	if _, err := c.File(asAlice(t), e.repo, "tool/latest"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a path that names no file: %v, want ErrNotFound", err)
	}
}

// TestFileOneFetch: the requests of one replica that miss one artifact at once share one fetch.
func TestFileOneFetch(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "hello", nil)
	e.up.fileDelay.Store(int64(100 * time.Millisecond))
	c := e.core(t, "r1", proxy.Options{})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { _, errs[i] = c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d: %v", i, err)
		}
	}
	if n := e.up.fileCalls.Load(); n != 1 {
		t.Errorf("%d downloads, want 1", n)
	}
}

// TestFileTwoReplicas: two replicas that miss one artifact at once download it once: the fetch
// claim in the index, the real store's lock, holds the second off until the version is there.
func TestFileTwoReplicas(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "hello", nil)
	e.up.fileDelay.Store(int64(300 * time.Millisecond))
	knobs := proxy.Options{WaitFirst: 10 * time.Millisecond, WaitLongest: 40 * time.Millisecond}
	replicas := []*proxy.Core{e.core(t, "r1:1", knobs), e.core(t, "r2:1", knobs)}
	var wg sync.WaitGroup
	got := make([]*index.FileMatch, len(replicas))
	errs := make([]error, len(replicas))
	for i, c := range replicas {
		wg.Go(func() { got[i], errs[i] = c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")) })
	}
	wg.Wait()
	for i := range replicas {
		if errs[i] != nil || got[i] == nil || got[i].File.Digest != got[0].File.Digest {
			t.Errorf("replica %d: %v, %v", i, got[i], errs[i])
		}
	}
	if n := e.up.fileCalls.Load(); n != 1 {
		t.Errorf("%d downloads across two replicas, want 1", n)
	}
	if evs, _ := audits(t, e, "proxy.fetch"); len(evs) != 1 {
		t.Errorf("%d proxy.fetch rows, want 1", len(evs))
	}
}

// TestFileClaims: a claim that stopped beating is taken over, and a claim that goes away
// without the version is taken, once (§5, step 6).
func TestFileClaims(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	e.up.publish("tool", "2.0.0", "two", nil)
	knobs := proxy.Options{WaitFirst: 10 * time.Millisecond, WaitLongest: 20 * time.Millisecond}
	c := e.core(t, "r1:1", knobs)

	stale := index.FetchClaim{RepoID: e.repo.ID, Package: "tool", Tag: "1.0.0", Principal: "anonymous", Replica: "dead:1",
		ClaimedAt: e.clock().Add(-time.Hour), HeartbeatAt: e.clock().Add(-3 * time.Minute)}
	must(t, e.st.ClaimFetch(ctx, &stale, e.clock().Add(-time.Hour)))
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); err != nil {
		t.Errorf("over a stale claim: %v", err)
	}

	fresh := index.FetchClaim{RepoID: e.repo.ID, Package: "tool", Tag: "2.0.0", Principal: "anonymous", Replica: "other:1",
		ClaimedAt: e.clock(), HeartbeatAt: e.clock()}
	must(t, e.st.ClaimFetch(ctx, &fresh, e.clock().Add(-time.Hour)))
	time.AfterFunc(50*time.Millisecond, func() { _ = e.st.ReleaseFetchClaim(context.Background(), fresh.ID) })
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "2.0.0")); err != nil {
		t.Errorf("after a claim that went away: %v", err)
	}
	if n := e.up.fileCalls.Load(); n != 2 {
		t.Errorf("%d downloads, want 2", n)
	}
}

// TestFileChecksum: the strongest checksum the upstream gives must match the bytes; a mismatch,
// or an entry without a checksum, is refused, 502 and a proxy.reject row naming both values,
// and nothing is pushed (§5, step 8).
func TestFileChecksum(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "hello", func(v *upstreamVersion) { v.SHA512 = hex.EncodeToString(make([]byte, 64)) })
	e.up.publish("tool", "2.0.0", "hello", func(v *upstreamVersion) { v.SHA512, v.SHA256, v.SHA1 = "", "", "" })
	e.up.publish("tool", "3.0.0", "hello", func(v *upstreamVersion) { v.SHA512, v.SHA256 = "", "" })
	c := e.core(t, "r1", proxy.Options{})

	for _, version := range []string{"1.0.0", "2.0.0"} {
		_, err := c.File(asAlice(t), e.repo, fileOf("tool", version))
		if status, _ := statusOf(err); status != http.StatusBadGateway {
			t.Errorf("%s: %v (%d), want 502", version, err, status)
		}
	}
	if pushes := e.reg.PushReferences(); len(pushes) != 0 {
		t.Errorf("refused fetches pushed %+v", pushes)
	}
	evs, details := audits(t, e, "proxy.reject")
	if len(evs) != 2 || evs[1].Principal != alice.String() || details[1]["reason"] != "checksum" || details[1]["algorithm"] != "sha512" ||
		details[1]["expected"] != hex.EncodeToString(make([]byte, 64)) || details[1]["actual"] == "" || details[0]["reason"] != "checksum" {
		t.Errorf("proxy.reject rows %+v, details %v", evs, details)
	}
	m, err := c.File(asAlice(t), e.repo, fileOf("tool", "3.0.0"))
	if err != nil || m.Version.Attrs[profile.AnnotationOriginChecksum][:5] != "sha1:" {
		t.Errorf("an entry with sha1 only: %v, %v; want it checked with sha1", m, err)
	}
}

// TestFileRefused: a file above maxFileSize is 403 naming the bound, one that Validate refuses
// 502; each is a proxy.reject row, and nothing is pushed.
func TestFileRefused(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.repo.Policy = json.RawMessage(`{"maxFileSize": 8}`)
	must(t, e.st.UpdateRepo(t.Context(), &e.repo))
	e.up.publish("tool", "1.0.0", "more than eight bytes", nil)
	e.up.publish("tool", "2.0.0", "evil", nil)
	c := e.core(t, "r1", proxy.Options{})
	for _, tt := range []struct {
		version string
		status  int
		reason  string
	}{
		{"1.0.0", http.StatusForbidden, "size"},
		{"2.0.0", http.StatusBadGateway, "invalid"},
	} {
		_, err := c.File(asAlice(t), e.repo, fileOf("tool", tt.version))
		if status, _ := statusOf(err); status != tt.status {
			t.Errorf("%s: %v (%d), want %d", tt.version, err, status, tt.status)
		}
		if _, details := audits(t, e, "proxy.reject"); len(details) == 0 || details[0]["reason"] != tt.reason {
			t.Errorf("%s: proxy.reject details %v, want the reason %s", tt.version, details, tt.reason)
		}
	}
	if pushes := e.reg.PushReferences(); len(pushes) != 0 {
		t.Errorf("refused fetches pushed %+v", pushes)
	}
}

// TestFileNameDepth: a file whose OCI repository is deeper than the registry's maxNameDepth is
// 403 naming the limit and a proxy.reject row, asks the upstream for no file and pushes nothing
// (#599).
func TestFileNameDepth(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	e.reg.MaxNameDepth = 2 // raw/up/tool has 3 components
	e.up.publish("tool", "1.0.0", "one", nil)
	_, err := e.core(t, "r1", proxy.Options{}).File(asAlice(t), e.repo, fileOf("tool", "1.0.0"))
	if status, _ := statusOf(err); status != http.StatusForbidden || !strings.Contains(err.Error(), "raw/up/tool has 3 components") {
		t.Errorf("a name too deep: %v (%d), want 403 naming the limit", err, status)
	}
	if _, details := audits(t, e, "proxy.reject"); len(details) == 0 || details[0]["reason"] != "name" {
		t.Errorf("proxy.reject details %v, want the reason name", details)
	}
	if n := e.up.fileCalls.Load(); n != 0 || len(e.reg.Pushes()) != 0 {
		t.Errorf("%d files asked upstream and %d blobs pushed for a name the registry does not take", n, len(e.reg.Pushes()))
	}
}

// TestFileUpstreamDown: with the upstream down, what the repo holds is served, its document
// from the cache, and from the held versions when the cache was dropped; a file it does not
// hold is 502, and offline 404 (§4.4, §6).
func TestFileUpstreamDown(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	e.up.publish("tool", "2.0.0", "two", nil)
	c := e.core(t, "r1", proxy.Options{})
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); err != nil {
		t.Fatal(err)
	}

	e.up.status.Store(http.StatusServiceUnavailable)
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); err != nil {
		t.Errorf("a held file, the upstream down: %v", err)
	}
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "2.0.0")); !errors.Is(err, format.ErrBadGateway) {
		t.Errorf("a file not held, the upstream down: %v, want ErrBadGateway", err)
	}
	doc, err := c.Document(ctx, e.repo, "tool", docKind)
	if err != nil || len(listing(t, doc)) != 2 {
		t.Errorf("the document, the upstream down: %v, want the cached two versions", err)
	}
	_, err = e.st.DeleteProxyMeta(ctx, e.repo.ID, "")
	must(t, err)
	doc, err = c.Document(ctx, e.repo, "tool", docKind)
	if err != nil || len(listing(t, doc)) != 1 || listing(t, doc)["1.0.0"] == "" {
		t.Errorf("the document without a row, the upstream down: %v, want the held 1.0.0", err)
	}

	e.repo.Upstream = json.RawMessage(`{"name": "up", "offline": true}`)
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "2.0.0")); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("offline, a file not held: %v, want ErrNotFound", err)
	}
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); err != nil {
		t.Errorf("offline, a held file: %v", err)
	}
}

// TestFileUpstreamMissing: a file the upstream answers 404 for is 404, and makes the name's
// document stale, so that the next read asks the upstream again (§4.3).
func TestFileUpstreamMissing(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	e.up.mu.Lock()
	delete(e.up.files, "tool-1.0.0.tgz")
	e.up.mu.Unlock()
	c := e.core(t, "r1", proxy.Options{})
	if _, err := c.File(asAlice(t), e.repo, fileOf("tool", "1.0.0")); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a file the upstream does not have: %v, want ErrNotFound", err)
	}
	row, err := e.st.GetProxyMeta(ctx, e.repo.ID, "tool", docKind)
	must(t, err)
	if !row.CheckedAt.Before(e.clock().Add(-24 * time.Hour)) {
		t.Errorf("the row after a missing file is checked at %v, want stale", row.CheckedAt)
	}
}

// TestFileLists: a name outside the lists is 403 and asks the upstream nothing.
func TestFileLists(t *testing.T) {
	e := newEnv(t, `{"name": "up", "deny": ["tool"]}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	_, err := e.core(t, "r1", proxy.Options{}).File(asAlice(t), e.repo, fileOf("tool", "1.0.0"))
	if status, _ := statusOf(err); status != http.StatusForbidden || e.up.metaCalls.Load()+e.up.fileCalls.Load() != 0 {
		t.Errorf("a denied name: %v (%d), %d requests upstream", err, status, e.up.metaCalls.Load()+e.up.fileCalls.Load())
	}
}

// derivedRows are the derived rows of the env's repo, without ids and IndexedAt.
func derivedRows(t *testing.T, e *env) map[string]any {
	t.Helper()
	ctx := t.Context()
	out := map[string]any{}
	pkgs, err := e.st.ListPackages(ctx, e.repo.ID, "")
	must(t, err)
	for _, p := range pkgs {
		vs, err := e.st.ListVersions(ctx, p.ID)
		must(t, err)
		for _, v := range vs {
			files, err := e.st.ListFiles(ctx, v.ID)
			must(t, err)
			for i := range files {
				files[i].ID, files[i].VersionID = 0, 0
			}
			v.ID, v.PackageID, v.IndexedAt = 0, 0, time.Time{}
			out[p.Name+":"+v.Tag] = []any{v, files}
		}
	}
	return out
}

// TestReindexProxied: what a proxy fetched is an ordinary manifest: reindex restores its rows
// from the registry, the origin annotations in the attrs, and leaves proxy_meta alone.
func TestReindexProxied(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	e.up.publish("tool", "2.0.0", "two", nil)
	c := e.core(t, "r1", proxy.Options{})
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if _, err := c.File(asAlice(t), e.repo, fileOf("tool", v)); err != nil {
			t.Fatal(err)
		}
	}
	before := derivedRows(t, e)
	must(t, e.st.ResetDerived(ctx, e.repo.ID))
	if rows := derivedRows(t, e); len(rows) != 0 {
		t.Fatalf("rows after ResetDerived: %v", rows)
	}
	w := reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": e.reg}, Inspector: e.pipe,
		Log: slog.New(slog.DiscardHandler)})
	req := reindex.Request{Repo: &e.repo, Prune: true, Source: "cli", Principal: "user:admin"}
	run, err := w.Start(ctx, req)
	must(t, err)
	if _, err := w.Run(ctx, run, req); err != nil {
		t.Fatal(err)
	}
	after := derivedRows(t, e)
	if d := cmp.Diff(before, after); d != "" {
		t.Errorf("the rows reindex rebuilt (-before +after)\n%s", d)
	}
	if len(after) != 2 {
		t.Errorf("%d versions after reindex, want 2", len(after))
	}
	if _, err := e.st.GetProxyMeta(ctx, e.repo.ID, "tool", docKind); err != nil {
		t.Errorf("proxy_meta after reindex: %v", err)
	}
}
