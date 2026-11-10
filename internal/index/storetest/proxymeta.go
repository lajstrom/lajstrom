package storetest

import (
	"encoding/json"
	"testing"

	"github.com/lajstrom/lajstrom/internal/index"
)

// testProxyMeta: proxy_meta, the cache of an upstream's documents (ADR 0013). A row is replaced
// in place, touched by a 304, deleted by name or by repo, swept when absent and old, kept by
// ResetDerived and dropped with its repo; a repo's upstream settings round-trip.
func testProxyMeta(t *testing.T, s index.Store) {
	ctx := t.Context()
	proxy := index.Repo{Format: "npm", Name: "npmjs", Registry: "local", Prefix: "npm/npmjs", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "npmjs", "metadataTTL": "10m"}`)}
	must(t, s.CreateRepo(ctx, &proxy))
	got, err := s.GetRepoByID(ctx, proxy.ID)
	must(t, err)
	same(t, "proxy repo", proxy, *got)
	other := newRepo(t, s, "npm", "other")

	present := index.ProxyMeta{RepoID: proxy.ID, Name: "left-pad", Kind: "packument", State: index.ProxyMetaPresent,
		Content: []byte(`{"name":"left-pad"}`), ContentType: "application/json", SHA256: "aa", ETag: `"v1"`,
		LastModified: "Mon, 09 Nov 2026 10:00:00 GMT", CheckedAt: at(1)}
	must(t, s.PutProxyMeta(ctx, &present))
	if present.ID == 0 {
		t.Fatal("PutProxyMeta did not fill ID")
	}
	row, err := s.GetProxyMeta(ctx, proxy.ID, "left-pad", "packument")
	must(t, err)
	same(t, "present row", present, *row)

	replaced := present
	replaced.ID, replaced.Content, replaced.SHA256, replaced.ETag, replaced.LastModified, replaced.CheckedAt =
		0, []byte(`{"name":"left-pad","v":2}`), "bb", "", "", at(2)
	must(t, s.PutProxyMeta(ctx, &replaced))
	if replaced.ID != present.ID {
		t.Errorf("PutProxyMeta of the same key: id %d, want the row's %d", replaced.ID, present.ID)
	}
	must(t, s.TouchProxyMeta(ctx, replaced.ID, at(3)))
	replaced.CheckedAt = at(3)
	row, err = s.GetProxyMeta(ctx, proxy.ID, "left-pad", "packument")
	must(t, err)
	same(t, "replaced and touched row", replaced, *row)

	rows := []index.ProxyMeta{
		{RepoID: proxy.ID, Name: "left-pad", Kind: "packument-abbreviated", State: index.ProxyMetaPresent, Content: []byte("{}"), CheckedAt: at(1)},
		{RepoID: proxy.ID, Name: "gone", Kind: "packument", State: index.ProxyMetaAbsent, CheckedAt: at(1)},
		{RepoID: proxy.ID, Name: "gone-later", Kind: "packument", State: index.ProxyMetaAbsent, CheckedAt: at(5)},
		{RepoID: other.ID, Name: "gone", Kind: "packument", State: index.ProxyMetaAbsent, CheckedAt: at(1)},
	}
	for i := range rows {
		must(t, s.PutProxyMeta(ctx, &rows[i]))
	}
	row, err = s.GetProxyMeta(ctx, proxy.ID, "gone", "packument")
	must(t, err)
	same(t, "absent row", rows[1], *row)

	// The sweep takes the old absent rows of one repo: not the present one checked as long
	// ago, not the newer absent one, not the other repo's.
	n, err := s.ExpireProxyMeta(ctx, proxy.ID, at(4))
	must(t, err)
	same(t, "ExpireProxyMeta", 1, n)
	_, err = s.GetProxyMeta(ctx, proxy.ID, "gone", "packument")
	wantErr(t, "GetProxyMeta of a swept row", err, index.ErrNotFound)
	for _, k := range []struct {
		repo       int64
		name, kind string
	}{{proxy.ID, "left-pad", "packument-abbreviated"}, {proxy.ID, "gone-later", "packument"}, {other.ID, "gone", "packument"}} {
		_, err := s.GetProxyMeta(ctx, k.repo, k.name, k.kind)
		must(t, err)
	}

	must(t, s.ResetDerived(ctx, 0))
	_, err = s.GetProxyMeta(ctx, proxy.ID, "left-pad", "packument")
	must(t, err) // a cache of the upstream, not of the registry

	n, err = s.DeleteProxyMeta(ctx, proxy.ID, "left-pad")
	must(t, err)
	same(t, "DeleteProxyMeta of a name, every kind", 2, n)
	n, err = s.DeleteProxyMeta(ctx, proxy.ID, "")
	must(t, err)
	same(t, "DeleteProxyMeta of the repo", 1, n)

	must(t, s.DeleteRepo(ctx, other.ID))
	_, err = s.GetProxyMeta(ctx, other.ID, "gone", "packument")
	wantErr(t, "GetProxyMeta after DeleteRepo", err, index.ErrNotFound)
	wantErr(t, "TouchProxyMeta of a missing row", s.TouchProxyMeta(ctx, present.ID, at(9)), index.ErrNotFound)
	wantErr(t, "PutProxyMeta of a missing repo",
		s.PutProxyMeta(ctx, &index.ProxyMeta{RepoID: other.ID, Name: "x", Kind: "packument", State: index.ProxyMetaAbsent}),
		index.ErrNotFound)
}
