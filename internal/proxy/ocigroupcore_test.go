package proxy_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// TestOCIRoute: the core routes a group's path by its routes, then the member keeps it under its
// own path (Docker Hub's library/), and a path a proxy member's lists refuse is no route
// (docs/spec/proxy-and-group.md §12.2).
func TestOCIRoute(t *testing.T) {
	e := newOCIEnv(t, `{"name": "hub", "deny": ["private/*"]}`)
	c := e.core("a", proxy.Options{})
	internal := index.Repo{ID: 90, Format: "oci", Name: "internal", Kind: index.KindHosted}
	group := index.Repo{Format: "oci", Name: "all", Kind: index.KindGroup,
		Group: json.RawMessage(`{"paths": {"internal": ["acme/*", "nginx"], "hub": ["*"]}}`)}
	members := []index.Repo{internal, e.repo}
	for path, want := range map[string]string{
		"acme/api":      "internal acme/api",
		"nginx":         "internal nginx",
		"redis":         "hub library/redis",
		"library/redis": "hub library/redis",
		"ghcr/x/y":      "hub ghcr/x/y",
		"private/x":     "",
	} {
		m, at, ok := c.OCIRoute(group, members, path)
		got := ""
		if ok {
			got = m.Name + " " + at
		}
		if got != want {
			t.Errorf("OCIRoute(%q) = %q, want %q", path, got, want)
		}
	}
	if _, _, ok := c.OCIRoute(group, members[:1], "redis"); ok {
		t.Errorf("a route to a repo that is not among the members")
	}
	if _, _, ok := c.OCIRoute(index.Repo{Format: "npm", Kind: index.KindGroup, Group: group.Group}, members, "redis"); ok {
		t.Errorf("a route of a group of another format")
	}
}

// TestOCIViaGroup: a proxy member's fetch through a group writes its proxy.fetch and
// proxy.reject rows with details.via, the group (§12.4, rbac.md §7).
func TestOCIViaGroup(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	bad := e.hub.image(t, "library/redis", "7", "amd64")
	e.hub.corrupted[bad.index.Digest] = true
	c := e.core("a", proxy.Options{})
	ctx := format.WithVia(t.Context(), index.Repo{Format: "oci", Name: "all", Kind: index.KindGroup})
	if _, _, err := c.OCIManifest(ctx, e.repo, "library/redis", "7"); !errors.Is(err, format.ErrBadGateway) {
		t.Fatalf("other bytes: %v, want format.ErrBadGateway", err)
	}
	_, _, err := c.OCIManifest(ctx, e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	for _, b := range img.blobs[0] {
		if _, _, err := c.OCIBlob(ctx, e.repo, "library/nginx", b.Digest, true, false); err != nil {
			t.Fatalf("OCIBlob %s: %v", b.Digest, err)
		}
	}
	for _, action := range []string{"proxy.reject", "proxy.fetch"} {
		evs, err := e.st.ListAudit(t.Context(), index.AuditFilter{Action: action})
		must(t, err)
		if len(evs) == 0 {
			t.Fatalf("no %s row", action)
		}
		for _, ev := range evs {
			var d map[string]any
			must(t, json.Unmarshal(ev.Details, &d))
			if d["via"] != "oci/all" {
				t.Errorf("%s details %v, want via oci/all", action, d)
			}
		}
	}
}
