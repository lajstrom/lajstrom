package index

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// byNameOCI is a Reader with packages by NameOCI only; it records the lookups.
type byNameOCI struct {
	Reader
	pkgs    map[string]*Package
	err     error
	lookups []string
}

func (r *byNameOCI) GetPackageByNameOCI(_ context.Context, _ int64, nameOCI string) (*Package, error) {
	r.lookups = append(r.lookups, nameOCI)
	if r.err != nil {
		return nil, r.err
	}
	if p, ok := r.pkgs[nameOCI]; ok {
		return p, nil
	}
	return nil, ErrNotFound
}

func TestPackageAt(t *testing.T) {
	old := &Package{Name: "@netlify/blobs", NameOCI: "netlify/blobs"}
	now := &Package{Name: "@acme/blobs", NameOCI: "acme/blob_73"}
	docs := &Package{Name: "docs", NameOCI: "docs"}
	// k8s/manifests/.gitignore: three forms, before ADR 0017, of it before its amendment, and now.
	const hash = "bc37d034bad564583790a46f19d807ab"
	v1 := &Package{Name: "k8s/manifests/.gitignore", NameOCI: "k8s/manifests/h0__" + hash}
	v2 := &Package{Name: "x/manifests/.gitignore", NameOCI: "x/manifests/h0_h" + hash}
	r := &byNameOCI{pkgs: map[string]*Package{old.NameOCI: old, now.NameOCI: now, docs.NameOCI: docs, v1.NameOCI: v1, v2.NameOCI: v2}}
	for _, tc := range []struct {
		pkgPath string
		want    *Package
		lookups []string
	}{
		{"netlify/blob_73", old, []string{"netlify/blob_73", "netlify/blobs"}}, // stored in the earlier form
		{"netlify/blobs", old, []string{"netlify/blobs"}},
		{"acme/blob_73", now, []string{"acme/blob_73"}},
		{"docs", docs, []string{"docs"}},
		{"other/blob_73", nil, []string{"other/blob_73", "other/blobs"}},
		{"other", nil, []string{"other"}},
		{"k8s/manifest_73/h0_h" + hash, v1, []string{"k8s/manifest_73/h0_h" + hash, "k8s/manifests/h0_h" + hash, v1.NameOCI}},
		{"x/manifest_73/h0_h" + hash, v2, []string{"x/manifest_73/h0_h" + hash, v2.NameOCI}},
		{"x/tag_73/list", nil, []string{"x/tag_73/list", "x/tags/list"}},
	} {
		r.lookups = nil
		got, err := PackageAt(t.Context(), r, 1, tc.pkgPath)
		switch {
		case tc.want == nil && !errors.Is(err, ErrNotFound), tc.want != nil && (err != nil || got != tc.want):
			t.Errorf("PackageAt(%q) = %+v, %v, want %+v", tc.pkgPath, got, err, tc.want)
		case !slices.Equal(r.lookups, tc.lookups):
			t.Errorf("PackageAt(%q) looked up %q, want %q", tc.pkgPath, r.lookups, tc.lookups)
		}
	}
	// Another error is returned as it is, without a second lookup.
	r.err, r.lookups = errors.New("database gone"), nil
	if _, err := PackageAt(t.Context(), r, 1, "netlify/blob_73"); !errors.Is(err, r.err) || len(r.lookups) != 1 {
		t.Errorf("PackageAt with a failing store = %v after %q", err, r.lookups)
	}
}
