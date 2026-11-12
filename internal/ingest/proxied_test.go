package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// origin is the Origin of the fetcher's uploads in these tests.
var origin = &format.Origin{Upstream: "upstream", URL: "https://up.example/tool/-/tool-1.0.0.tgz", Checksum: "sha512:abcd"}

// TestUploadWithOrigin: the proxy fetcher's upload gets the origin annotations, not
// lajstrom.uploaded-by, in the registry and in versions.attrs, and an audit row proxy.fetch by
// the reader whose miss caused it, with the upstream, URL, checksum and size
// (docs/spec/manifest-profile.md §3.1, rbac.md §7).
func TestUploadWithOrigin(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	build := e.plugin.BuildFunc
	e.plugin.BuildFunc = func(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
		b, err := build(ctx, repo, up)
		if err == nil {
			b.Manifest.Annotations[profile.AnnotationUploadedBy] = "user:someone" // a plugin that sets it anyway
		}
		return b, err
	}
	up := upload("tool", "1.0.0", file{"tool.tgz", "bytes"}, file{"tool.sig", "sig"})
	up.Origin = origin
	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)

	rr, err := e.prov.Repository(ctx, pub.RepoPath)
	must(t, err)
	rc, err := rr.Fetch(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: pub.Digest})
	must(t, err)
	var m ocispec.Manifest
	must(t, json.NewDecoder(rc).Decode(&m))
	_ = rc.Close()
	if m.Annotations[profile.AnnotationOriginURL] != origin.URL || m.Annotations[profile.AnnotationOriginChecksum] != origin.Checksum {
		t.Errorf("manifest annotations %v, want the origin's", m.Annotations)
	}
	if _, ok := m.Annotations[profile.AnnotationUploadedBy]; ok {
		t.Errorf("manifest has %s: %v", profile.AnnotationUploadedBy, m.Annotations)
	}

	r := e.rows(t)
	if a := r.Versions[0].Attrs; a[profile.AnnotationOriginURL] != origin.URL || a[profile.AnnotationOriginChecksum] != origin.Checksum {
		t.Errorf("versions.attrs %v, want the origin's", a)
	}
	if len(r.Audit) != 1 {
		t.Fatalf("%d audit rows, want 1", len(r.Audit))
	}
	ev := r.Audit[0]
	if ev.Action != "proxy.fetch" || ev.Principal != alice.String() || ev.Digest != pub.Digest || ev.Subject != "tool 1.0.0" {
		t.Errorf("audit row %+v, want proxy.fetch of tool 1.0.0 by %s", ev, alice)
	}
	var details map[string]any
	must(t, json.Unmarshal(ev.Details, &details))
	same(t, "audit details", map[string]any{"upstream": "upstream", "url": origin.URL, "checksum": origin.Checksum, "size": float64(8)}, details)
}

// TestAuditVia: an upload, a fetch and a delete made through a group (format.WithVia) name the
// group in details.via of their audit rows (docs/spec/rbac.md §7); nothing else changes.
func TestAuditVia(t *testing.T) {
	e := newEnv(t)
	ctx := format.WithVia(t.Context(), index.Repo{Format: "raw", Name: "all", Kind: index.KindGroup})
	pub, err := e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool.tgz", "bytes"}), alice)
	must(t, err)
	up := upload("tool", "2.0.0", file{"tool.tgz", "other"})
	up.Origin = origin
	_, err = e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	_, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "tool", Reference: pub.Tag}, alice)
	must(t, err)
	want := map[string]map[string]any{
		"push":        {"via": "raw/all"},
		"proxy.fetch": {"via": "raw/all", "upstream": "upstream", "url": origin.URL, "checksum": origin.Checksum, "size": float64(5)},
		"delete":      {"via": "raw/all", "referrers": float64(0)},
	}
	rows := e.rows(t)
	if len(rows.Audit) != len(want) {
		t.Fatalf("%d audit rows, want %d", len(rows.Audit), len(want))
	}
	for _, ev := range rows.Audit {
		var details map[string]any
		must(t, json.Unmarshal(ev.Details, &details))
		same(t, "details of "+ev.Action, want[ev.Action], details)
	}
}

// TestOriginRefused: a lajstrom.origin.* key that does not come from the fetcher is 400, on an
// upload and on a push through the facade; Inspect reads it as any annotation.
func TestOriginRefused(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	build := e.plugin.BuildFunc
	e.plugin.BuildFunc = func(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
		b, err := build(ctx, repo, up)
		if err == nil {
			b.Manifest.Annotations[profile.AnnotationOriginURL] = "https://elsewhere.example/tool.tgz"
		}
		return b, err
	}
	up := upload("tool", "1.0.0", file{"tool.tgz", "bytes"})
	if _, err := e.pipe.Upload(ctx, e.repo, up, alice); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Upload with an origin key and no Origin: %v, want ErrInvalid", err)
	}
	built, raw, pkgPath := e.prepare(t, up)
	if _, err := e.ingest(t, built, raw, pkgPath); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Ingest with an origin key: %v, want ErrInvalid", err)
	}
	rows, err := e.pipe.Inspect(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, ingest.InspectOptions{})
	must(t, err)
	if rows.Version.Attrs[profile.AnnotationOriginURL] == "" {
		t.Errorf("Inspect rows %+v, want the origin key in the attrs", rows.Version)
	}
	if r := e.rows(t); len(r.Versions) != 0 || len(r.Audit) != 0 {
		t.Errorf("rows after the refusals: %+v", r)
	}
}

// TestUploadWithOriginPublished: the upstream's publish time of a proxied artifact is
// org.opencontainers.image.created of its manifest, in whole seconds, and the version's
// CreatedAt (docs/spec/manifest-profile.md §3.1); without one the plugin's time stays.
func TestUploadWithOriginPublished(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	o := *origin
	o.Published = time.Date(2024, 3, 1, 8, 30, 15, 700_000_000, time.FixedZone("CET", 3600))
	up := upload("tool", "1.0.0", file{"tool.tgz", "bytes"})
	up.Origin = &o
	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	rr, err := e.prov.Repository(ctx, pub.RepoPath)
	must(t, err)
	rc, err := rr.Fetch(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: pub.Digest})
	must(t, err)
	var m ocispec.Manifest
	must(t, json.NewDecoder(rc).Decode(&m))
	_ = rc.Close()
	want := time.Date(2024, 3, 1, 7, 30, 15, 0, time.UTC)
	if got := m.Annotations[ocispec.AnnotationCreated]; got != want.Format(time.RFC3339) {
		t.Errorf("created %q, want %s", got, want.Format(time.RFC3339))
	}
	if got := e.rows(t).Versions[0].CreatedAt; !got.Equal(want) {
		t.Errorf("CreatedAt %s, want %s", got, want)
	}

	up = upload("tool", "2.0.0", file{"tool.tgz", "other"})
	up.Origin = origin
	_, err = e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	for _, v := range e.rows(t).Versions {
		if v.Version == "2.0.0" && v.CreatedAt.Equal(want) {
			t.Errorf("2.0.0 without a publish time: CreatedAt %s, want the plugin's", v.CreatedAt)
		}
	}
}
