package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// registryFixture is the API over a fresh index and a Fake registry "local", with the manifests of
// requests 1.0 in both: a wheel and a degraded sdist (no metadata layer the index could read),
// and a signature that refers to the wheel.
type registryFixture struct {
	*fixture
	fake      *providertest.Fake
	raw       map[string][]byte        // the manifests by variant
	digests   map[string]digest.Digest // their digests by variant
	signature ocispec.Descriptor
}

// newRegistryFixture returns a registryFixture whose handler is New(o) with the Fake as its
// Providers.
func newRegistryFixture(t *testing.T, o Options) *registryFixture {
	t.Helper()
	ctx := context.Background()
	f := &registryFixture{fake: &providertest.Fake{RegistryName: "local"}, raw: map[string][]byte{}, digests: map[string]digest.Digest{}}
	o.Providers = provider.Map{"local": f.fake}
	f.fixture = newFixture(t, o)
	repo := f.create(t, "pypi", "libs")
	rr, err := f.fake.Repository(ctx, "pypi/libs/requests")
	must(t, err)
	push := func(m ocispec.Manifest) (ocispec.Descriptor, []byte) {
		t.Helper()
		m.Versioned, m.MediaType = specs.Versioned{SchemaVersion: 2}, ocispec.MediaTypeImageManifest
		raw, err := json.Marshal(m)
		must(t, err)
		desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
		must(t, rr.Push(ctx, desc, bytes.NewReader(raw)))
		return desc, raw
	}
	pkg := index.Package{RepoID: repo.ID, Name: "requests", NameOCI: "requests"}
	must(t, f.st.UpsertPackage(ctx, &pkg))
	for _, variant := range []string{"sdist", "py3-none-any"} {
		desc, raw := push(ocispec.Manifest{ArtifactType: "application/vnd.lajstrom.pypi.v1", Config: ocispec.DescriptorEmptyJSON,
			Layers:      []ocispec.Descriptor{{MediaType: "application/vnd.lajstrom.pypi.wheel.v1+zip", Digest: digest.FromString(variant), Size: 7}},
			Annotations: map[string]string{"lajstrom.version": "1.0", "lajstrom.variant": variant}})
		f.raw[variant], f.digests[variant] = raw, desc.Digest
		must(t, f.st.UpsertVersion(ctx, &index.Version{PackageID: pkg.ID, Version: "1.0", Variant: variant, Tag: "1.0__" + variant,
			Digest: desc.Digest, ArtifactType: "application/vnd.lajstrom.pypi.v1", Degraded: variant == "sdist", CreatedAt: testNow}))
	}
	wheel := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: f.digests["py3-none-any"], Size: int64(len(f.raw["py3-none-any"]))}
	f.signature, _ = push(ocispec.Manifest{ArtifactType: "application/vnd.dev.sigstore.bundle.v0.3+json", Config: ocispec.DescriptorEmptyJSON,
		Layers:  []ocispec.Descriptor{{MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json", Digest: digest.FromString("bundle"), Size: 9}},
		Subject: &wheel, Annotations: map[string]string{"org.opencontainers.image.created": "2026-01-02T12:00:00Z"}})
	return f
}

// manifestEntry is the JSON of the entry of variant in an answer of GET …/manifest.
func (f *registryFixture) manifestEntry(variant string) string {
	return `{"variant": "` + variant + `", "digest": "` + f.digests[variant].String() + `", "mediaType": "` + ocispec.MediaTypeImageManifest +
		`", "size": ` + strconv.Itoa(len(f.raw[variant])) + `, "manifest": ` + string(f.raw[variant]) + `}`
}

// TestGetManifests: the manifests of every variant in variant order, or of one, read from the
// registry by the index's digest, the degraded sdist's too; one variant has its digest as ETag,
// which If-None-Match answers 304 without asking the registry; every answer says no-cache.
func TestGetManifests(t *testing.T) {
	f := newRegistryFixture(t, Options{})
	target := "/api/v1/repos/pypi/libs/packages/requests/versions/1.0/manifest"
	for _, tt := range []struct{ query, want, etag string }{
		{"", `{"manifests": [` + f.manifestEntry("py3-none-any") + `, ` + f.manifestEntry("sdist") + `]}`, ""},
		{"?variant=sdist", `{"manifests": [` + f.manifestEntry("sdist") + `]}`, `"` + f.digests["sdist"].String() + `"`},
	} {
		rr := f.do(http.MethodGet, target+tt.query, "", admin)
		var got any
		decodeJSON(t, rr, http.StatusOK, &got)
		wantJSON(t, "manifests"+tt.query, got, tt.want)
		if e, c := rr.Header().Get("ETag"), rr.Header().Get("Cache-Control"); e != tt.etag || c != "no-cache" {
			t.Errorf("%s: ETag %q, Cache-Control %q; want %q, no-cache", tt.query, e, c, tt.etag)
		}
	}

	f.fake.FailRequests = provider.ErrUnavailable // a registry read is 503 from here on
	etag := `"` + f.digests["py3-none-any"].String() + `"`
	for _, inm := range []string{etag, "W/" + etag, `"sha256:0", ` + etag, "*"} {
		rr := f.doWith(http.MethodGet, target+"?variant=py3-none-any", http.Header{"If-None-Match": {inm}}, admin)
		if rr.Code != http.StatusNotModified || rr.Header().Get("ETag") != etag || rr.Body.Len() != 0 {
			t.Errorf("If-None-Match %s: %d, ETag %q, body %q; want 304 with the ETag", inm, rr.Code, rr.Header().Get("ETag"), rr.Body)
		}
	}
	other := http.Header{"If-None-Match": {`"` + f.digests["sdist"].String() + `"`}}
	wantError(t, f.doWith(http.MethodGet, target+"?variant=py3-none-any", other, admin), http.StatusServiceUnavailable, codeUnavailable)
	wantError(t, f.do(http.MethodGet, target, "", admin), http.StatusServiceUnavailable, codeUnavailable) // two variants: no ETag
}

// TestGetManifestsErrors: a version or a variant the index does not have is 404, and so is a
// manifest the registry does not have, naming its digest; without Providers the route is 501.
func TestGetManifestsErrors(t *testing.T) {
	f := newRegistryFixture(t, Options{})
	base := "/api/v1/repos/pypi/libs/packages/requests/versions/"
	wantError(t, f.do(http.MethodGet, base+"2.0/manifest", "", admin), http.StatusNotFound, codeNotFound, `has no version "2.0"`)
	wantError(t, f.do(http.MethodGet, base+"1.0/manifest?variant=cp311", "", admin), http.StatusNotFound, codeNotFound, `with the variant "cp311"`)
	rr, err := f.fake.Repository(context.Background(), "pypi/libs/requests")
	must(t, err)
	must(t, rr.Delete(context.Background(), ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: f.digests["sdist"]}))
	wantError(t, f.do(http.MethodGet, base+"1.0/manifest", "", admin), http.StatusNotFound, codeNotFound,
		"the registry does not have the manifest "+f.digests["sdist"].String()+` of version 1.0 sdist of "requests" in pypi/libs`)

	g := newFixture(t, Options{})
	publishDetailed(t, g.st, g.create(t, "pypi", "libs").ID)
	for _, route := range []string{"manifest", "referrers"} {
		wantError(t, g.do(http.MethodGet, base+"1.0/"+route, "", admin), http.StatusNotImplemented, codeUnavailable, "not enabled")
	}
}

// TestListReferrers: the referrers of every variant, or of one, as the registry lists them,
// with the variant and its digest as their subject; ?artifactType= keeps those of the type; a
// variant without referrers, or whose OCI repository the registry does not have, has none.
func TestListReferrers(t *testing.T) {
	f := newRegistryFixture(t, Options{})
	target := "/api/v1/repos/pypi/libs/packages/requests/versions/1.0/referrers"
	sig := `{"variant": "py3-none-any", "subject": "` + f.digests["py3-none-any"].String() + `", "digest": "` + f.signature.Digest.String() +
		`", "mediaType": "` + ocispec.MediaTypeImageManifest + `", "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json", "size": ` +
		strconv.FormatInt(f.signature.Size, 10) + `, "annotations": {"org.opencontainers.image.created": "2026-01-02T12:00:00Z"}}`
	for _, tt := range []struct{ query, want string }{
		{"", `{"referrers": [` + sig + `]}`},
		{"?variant=py3-none-any&artifactType=application/vnd.dev.sigstore.bundle.v0.3%2Bjson", `{"referrers": [` + sig + `]}`},
		{"?artifactType=application/spdx%2Bjson", `{"referrers": []}`},
		{"?variant=sdist", `{"referrers": []}`},
	} {
		var got any
		decodeJSON(t, f.do(http.MethodGet, target+tt.query, "", admin), http.StatusOK, &got)
		wantJSON(t, "referrers"+tt.query, got, tt.want)
	}

	must(t, f.fake.DeleteRepository(context.Background(), "pypi/libs/requests"))
	var got any
	decodeJSON(t, f.do(http.MethodGet, target, "", admin), http.StatusOK, &got)
	wantJSON(t, "referrers without the repository", got, `{"referrers": []}`)
	f.fake.FailRequests = provider.ErrUnavailable
	wantError(t, f.do(http.MethodGet, target, "", admin), http.StatusServiceUnavailable, codeUnavailable)
}

// TestManifestsAccess: the manifests and the referrers of a version need read, as the version
// detail does (docs/spec/rbac.md §3: 401 for anonymous, 404 for a principal that cannot read the
// repo), decided before the index and the registry are asked.
func TestManifestsAccess(t *testing.T) {
	f := newRegistryFixture(t, Options{Auth: fakeAuth{grants: map[string]bool{"user:alice read pypi/libs": true}}})
	base := "/api/v1/repos/pypi/libs/packages/requests/versions/1.0/"
	for _, route := range []string{"manifest", "referrers"} {
		f.fake.FailRequests = nil
		wantStatus(t, f.do(http.MethodGet, base+route, "", alice), http.StatusOK)
		f.fake.FailRequests = provider.ErrUnavailable // a denial asks the registry nothing
		wantError(t, f.do(http.MethodGet, base+route, "", bob), http.StatusNotFound, codeNotFound)
		wantError(t, f.do(http.MethodGet, base+route, "", auth.Anonymous), http.StatusUnauthorized, codeUnauthenticated)
		wantError(t, f.do(http.MethodGet, "/api/v1/repos/pypi/libs/packages/flask/versions/1.0/"+route, "", auth.Anonymous),
			http.StatusUnauthorized, codeUnauthenticated) // before the package is looked up
	}
}
