package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
