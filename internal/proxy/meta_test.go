package proxy_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// listing decodes a rendered document of the plugin: version → "<link> <sha256>".
func listing(t *testing.T, c *index.Cache) map[string]string {
	t.Helper()
	var m map[string]string
	must(t, json.Unmarshal(c.Content, &m))
	return m
}

// statusOf is the status a frontend answers err with, and its Retry-After.
func statusOf(err error) (int, string) {
	rr := httptest.NewRecorder()
	format.WriteError(rr, err)
	return rr.Code, rr.Header().Get("Retry-After")
}

// TestDocumentCache: a miss asks the upstream and keeps its document, a fresh row is used as it
// is, an expired one is revalidated with its ETag (304, nothing rendered again), and a changed
// document replaces the row and the rendered one (docs/spec/proxy-and-group.md §4.2).
func TestDocumentCache(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, `{"name": "up", "metadataTTL": "10m"}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	c := e.core(t, "r1", proxy.Options{})

	doc, err := c.Document(ctx, e.repo, "tool", docKind)
	must(t, err)
	if l := listing(t, doc); len(l) != 1 || !strings.HasPrefix(l["1.0.0"], "https://art.test/raw/up/tool/-/tool-1.0.0.tgz ") {
		t.Errorf("document %s, want 1.0.0 linked below the repo", doc.Content)
	}
	row, err := e.st.GetProxyMeta(ctx, e.repo.ID, "tool", docKind)
	must(t, err)
	if row.State != index.ProxyMetaPresent || row.ETag == "" || !row.CheckedAt.Equal(e.clock()) {
		t.Errorf("row %+v, want present with the upstream's ETag, checked now", row)
	}

	e.advance(5 * time.Minute)
	again, err := c.Document(ctx, e.repo, "TOOL", docKind) // the key is the name in lower case
	must(t, err)
	if e.up.metaCalls.Load() != 1 || again.ID != doc.ID {
		t.Errorf("a fresh row: %d upstream requests, document %d; want 1 and the cached %d", e.up.metaCalls.Load(), again.ID, doc.ID)
	}

	e.advance(6 * time.Minute)
	again, err = c.Document(ctx, e.repo, "tool", docKind)
	must(t, err)
	row, err = e.st.GetProxyMeta(ctx, e.repo.ID, "tool", docKind)
	must(t, err)
	if e.up.metaCalls.Load() != 2 || again.ID != doc.ID || !row.CheckedAt.Equal(e.clock()) {
		t.Errorf("revalidation: %d requests, document %d (want %d), checked %v", e.up.metaCalls.Load(), again.ID, doc.ID, row.CheckedAt)
	}

	e.up.publish("tool", "2.0.0", "two", nil)
	e.advance(11 * time.Minute)
	doc, err = c.Document(ctx, e.repo, "tool", docKind)
	must(t, err)
	if l := listing(t, doc); len(l) != 2 {
		t.Errorf("after the upstream changed: %s, want two versions", doc.Content)
	}
}

// TestDocumentNegative: a name the upstream does not have is 404, remembered for negativeTTL
// and swept after it; a negativeTTL of 0s remembers nothing.
func TestDocumentNegative(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, `{"name": "up", "negativeTTL": "2m"}`)
	c := e.core(t, "r1", proxy.Options{})
	for range 2 {
		if _, err := c.Document(ctx, e.repo, "nope", docKind); !errors.Is(err, index.ErrNotFound) {
			t.Fatalf("a name the upstream does not have: %v, want ErrNotFound", err)
		}
	}
	if n := e.up.metaCalls.Load(); n != 1 {
		t.Errorf("%d upstream requests, want 1: the negative entry answers the second", n)
	}
	if n, err := c.Sweep(ctx); err != nil || n != 0 {
		t.Errorf("Sweep of a fresh negative entry = %d, %v", n, err)
	}
	e.advance(3 * time.Minute)
	if n, err := c.Sweep(ctx); err != nil || n != 1 {
		t.Errorf("Sweep of an expired negative entry = %d, %v; want 1", n, err)
	}

	e.repo.Upstream = json.RawMessage(`{"name": "up", "negativeTTL": "0s"}`)
	if _, err := c.Document(ctx, e.repo, "nope", docKind); !errors.Is(err, index.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := e.st.GetProxyMeta(ctx, e.repo.ID, "nope", docKind); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("negativeTTL 0s left a row: %v", err)
	}
}

// TestDocumentUpstreamDown: when the upstream fails, the cached row is used however old;
// without one the read fails with the status of the failure (§6), and offline asks nothing.
func TestDocumentUpstreamDown(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, `{"name": "up", "metadataTTL": "0s"}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	c := e.core(t, "r1", proxy.Options{})
	if _, err := c.Document(ctx, e.repo, "tool", docKind); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		status     int32
		want       int
		retryAfter string
	}{
		{http.StatusInternalServerError, http.StatusBadGateway, ""},
		{http.StatusForbidden, http.StatusBadGateway, ""},
		{http.StatusTooManyRequests, http.StatusServiceUnavailable, "7"},
	} {
		e.up.status.Store(tt.status)
		doc, err := c.Document(ctx, e.repo, "tool", docKind)
		if err != nil || len(listing(t, doc)) != 1 {
			t.Errorf("upstream %d with a row: %v, want the cached document", tt.status, err)
		}
		_, err = c.Document(ctx, e.repo, "other", docKind)
		if got, ra := statusOf(err); got != tt.want || ra != tt.retryAfter {
			t.Errorf("upstream %d without a row: %v, %d Retry-After %q; want %d %q", tt.status, err, got, ra, tt.want, tt.retryAfter)
		}
	}

	calls := e.up.metaCalls.Load()
	e.up.status.Store(0)
	e.repo.Upstream = json.RawMessage(`{"name": "up", "metadataTTL": "0s", "offline": true}`)
	if _, err := c.Document(ctx, e.repo, "tool", docKind); err != nil {
		t.Errorf("offline with a row: %v", err)
	}
	if _, err := c.Document(ctx, e.repo, "other", docKind); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("offline without a row: %v, want ErrNotFound", err)
	}
	e.repo.Upstream = json.RawMessage(`{"name": "gone"}`) // not configured: offline
	if _, err := c.Document(ctx, e.repo, "tool", docKind); err != nil {
		t.Errorf("an upstream not configured, with a row: %v", err)
	}
	if n := e.up.metaCalls.Load(); n != calls {
		t.Errorf("%d upstream requests offline, want none", n-calls)
	}
}

// TestDocumentLimits: a document above maxMetadataSize or one the plugin cannot read is not
// cached, and is 502 without a row.
func TestDocumentLimits(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, `{"name": "up"}`)
	e.up.publish("big", "1.0.0", "one", func(v *upstreamVersion) { v.URL = strings.Repeat("x", 2048) })
	small := e.core(t, "r1", proxy.Options{Upstreams: map[string]*proxy.Upstream{"up": e.upstream(t, 1024)}})
	if _, err := small.Document(ctx, e.repo, "big", docKind); !errors.Is(err, format.ErrBadGateway) {
		t.Errorf("a document above maxMetadataSize: %v, want ErrBadGateway", err)
	}
	if _, err := e.st.GetProxyMeta(ctx, e.repo.ID, "big", docKind); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("a document above maxMetadataSize was cached: %v", err)
	}
	e.up.mu.Lock()
	e.up.docs["broken"] = &upstreamDoc{Name: "broken"}
	e.up.mu.Unlock()
	e.plugin.broken = true
	if _, err := e.core(t, "r1", proxy.Options{}).Document(ctx, e.repo, "broken", docKind); !errors.Is(err, format.ErrBadGateway) {
		t.Errorf("a document that does not parse: %v, want ErrBadGateway", err)
	}
}

// TestDocumentLists: a name outside allow, or matched by deny, is 403 naming the list and the
// pattern, and nothing is asked of the upstream (§3).
func TestDocumentLists(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, `{"name": "up", "allow": ["tool*", "acme-*"], "deny": ["acme-*"]}`)
	e.up.publish("tool", "1.0.0", "one", nil)
	c := e.core(t, "r1", proxy.Options{})
	for _, tt := range []struct{ name, want string }{
		{"acme-billing", `raw/up does not mirror "acme-billing": deny: acme-*`},
		{"react", `raw/up does not mirror "react": allow`},
	} {
		_, err := c.Document(ctx, e.repo, tt.name, docKind)
		if status, _ := statusOf(err); status != http.StatusForbidden || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v (%d), want 403 %q", tt.name, err, status, tt.want)
		}
	}
	if n := e.up.metaCalls.Load(); n != 0 {
		t.Errorf("%d upstream requests for refused names", n)
	}
	if _, err := c.Document(ctx, e.repo, "Tool", docKind); err != nil {
		t.Errorf("an allowed name, ASCII case aside: %v", err)
	}
}

// TestDocumentKinds: Document serves proxy repos only.
func TestDocumentKinds(t *testing.T) {
	e := newEnv(t, `{"name": "up"}`)
	hosted := index.Repo{ID: e.repo.ID, Format: "raw", Name: "up", Kind: index.KindHosted}
	if _, err := e.core(t, "r1", proxy.Options{}).Document(t.Context(), hosted, "tool", docKind); !errors.Is(err, format.ErrUnsupported) {
		t.Errorf("a hosted repo: %v, want ErrUnsupported", err)
	}
}
