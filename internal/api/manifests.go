package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// defaultMaxManifestSize is the largest manifest read when Options.MaxManifestSize is zero:
// server.maxManifestSize's default.
const defaultMaxManifestSize = 4 << 20

// ManifestList is the answer of GET …/packages/{name}/versions/{version}/manifest (docs/api.md,
// The manifests of a version; #951).
type ManifestList struct {
	Manifests []ManifestEntry `json:"manifests"`
}

// ManifestEntry is the manifest of one variant, read from the registry.
type ManifestEntry struct {
	Variant   string          `json:"variant"`
	Digest    string          `json:"digest"`
	MediaType string          `json:"mediaType"`
	Size      int64           `json:"size"`
	Manifest  json.RawMessage `json:"manifest"`
}

// ReferrerList is the answer of GET …/packages/{name}/versions/{version}/referrers (docs/api.md,
// The referrers of a version; #951).
type ReferrerList struct {
	Referrers []ReferrerEntry `json:"referrers"`
}

// ReferrerEntry is one manifest whose subject is a variant's manifest, as the registry lists it.
type ReferrerEntry struct {
	Variant      string            `json:"variant"`
	Subject      string            `json:"subject"` // the variant's manifest digest
	Digest       string            `json:"digest"`
	MediaType    string            `json:"mediaType"`
	ArtifactType string            `json:"artifactType"`
	Size         int64             `json:"size"`
	Annotations  map[string]string `json:"annotations"`
}

// getManifests answers GET /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/manifest
// (docs/spec/http-routing.md §4, #951): read on the repo, checked first; then the manifest of each
// variant of the version, or of the one ?variant= names, read through the provider by the digest
// the index has and checked against it. An answer of one variant has its digest as ETag, and an
// If-None-Match with it is 304 before the registry is asked. A variant whose manifest the
// registry does not have is 404 naming its digest.
func (h *handler) getManifests(w http.ResponseWriter, r *http.Request) (int, any, error) {
	repo, pkg, rows, err := h.registryVariants(r, "read the manifests of a version of")
	if err != nil {
		return 0, nil, err
	}
	etag := ""
	if len(rows) == 1 {
		etag = `"` + rows[0].Digest.String() + `"`
	}
	hdr := w.Header()
	if etag != "" && noneMatch(r, etag) {
		hdr.Set("ETag", etag)
		hdr.Set("Cache-Control", "no-cache")
		return http.StatusNotModified, nil, nil
	}
	ctx := r.Context()
	out := ManifestList{Manifests: make([]ManifestEntry, 0, len(rows))}
	for _, v := range rows {
		rr, err := h.registryRepository(ctx, repo, pkg, v)
		if err != nil {
			return 0, nil, err
		}
		e, err := h.manifestOf(ctx, rr, repo, pkg, v)
		if err != nil {
			return 0, nil, err
		}
		out.Manifests = append(out.Manifests, e)
	}
	if etag != "" {
		hdr.Set("ETag", etag)
	}
	hdr.Set("Cache-Control", "no-cache")
	return http.StatusOK, out, nil
}

// listReferrers answers GET /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/referrers
// (docs/spec/http-routing.md §4, #951): read on the repo, checked first; then the referrers of
// the manifest of each variant of the version, or of the one ?variant= names, as the provider
// lists them (Repository.Referrers: the backend's referrers API or the tag schema), of
// ?artifactType when it is set. A variant whose OCI repository the registry does not have has
// none.
func (h *handler) listReferrers(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	repo, pkg, rows, err := h.registryVariants(r, "read the referrers of a version of")
	if err != nil {
		return 0, nil, err
	}
	ctx := r.Context()
	artifactType := r.URL.Query().Get("artifactType")
	out := ReferrerList{Referrers: []ReferrerEntry{}}
	for _, v := range rows {
		rr, err := h.registryRepository(ctx, repo, pkg, v)
		if err != nil {
			return 0, nil, err
		}
		subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: v.Digest}
		err = rr.Referrers(ctx, subject, artifactType, func(page []ocispec.Descriptor) error {
			for _, d := range page {
				out.Referrers = append(out.Referrers, ReferrerEntry{Variant: v.Variant, Subject: v.Digest.String(),
					Digest: d.Digest.String(), MediaType: d.MediaType, ArtifactType: d.ArtifactType, Size: d.Size,
					Annotations: orEmpty(d.Annotations)})
			}
			return nil
		})
		if err != nil && !errors.Is(err, provider.ErrNotFound) {
			return 0, nil, fmt.Errorf("api: referrers of %s of %q %s in %s/%s: %w", v.Digest, pkg.Name, v.Version, repo.Format, repo.Name, err)
		}
	}
	return http.StatusOK, out, nil
}

// registryVariants returns the repo, the package and the variants of the request's version, or
// the one ?variant= names, in variant order, for a read of the registry: read on the repo is
// checked first, then whether the server reads registries for the API (Options.Providers).
func (h *handler) registryVariants(r *http.Request, what string) (*index.Repo, *index.Package, []index.Version, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionRead, f, name); err != nil {
		return nil, nil, nil, err
	}
	if h.providers == nil {
		return nil, nil, nil, &clientError{msg: "reading the registry is not enabled in this server", err: format.ErrUnsupported}
	}
	var variant *string
	if v, ok := r.URL.Query()["variant"]; ok {
		variant = &v[0]
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return nil, nil, nil, repoError(what, f, name, err)
	}
	pkg, rows, err := h.versionOf(ctx, repo, r.PathValue("name"), r.PathValue("version"), variant)
	if err != nil {
		return nil, nil, nil, err
	}
	slices.SortFunc(rows, func(a, b index.Version) int { return strings.Compare(a.Variant, b.Variant) })
	return repo, pkg, rows, nil
}

// registryRepository returns the data path of the OCI repository that holds the manifest of the
// variant v of pkg (index.ManifestPath), below the repo's path, through the provider of the
// repo's registry.
func (h *handler) registryRepository(ctx context.Context, repo *index.Repo, pkg *index.Package, v index.Version) (provider.Repository, error) {
	p, ok := h.providers.Provider(repo.Registry)
	if !ok || p == nil {
		return nil, fmt.Errorf("api: repo %s/%s: registry %q is not configured", repo.Format, repo.Name, repo.Registry)
	}
	base := strings.Trim(repo.Prefix, "/")
	if base == "" {
		base = repo.Format + "/" + repo.Name
	}
	name := base + "/" + index.ManifestPath(*pkg, v)
	rr, err := p.Repository(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("api: repository %s: %w", name, err)
	}
	return rr, nil
}

// manifestOf reads the manifest of the variant v from rr by its digest, at most h.maxManifest
// bytes, and checks the bytes against the digest. A manifest the registry does not have is 404.
func (h *handler) manifestOf(ctx context.Context, rr provider.Repository, repo *index.Repo, pkg *index.Package, v index.Version) (ManifestEntry, error) {
	what := fmt.Sprintf("the manifest %s of version %s", v.Digest, v.Version)
	if v.Variant != "" {
		what += " " + v.Variant
	}
	what += fmt.Sprintf(" of %q in %s/%s", pkg.Name, repo.Format, repo.Name)
	desc, err := rr.Resolve(ctx, v.Digest.String())
	var rc io.ReadCloser
	if err == nil {
		if desc.Size > h.maxManifest {
			return ManifestEntry{}, fmt.Errorf("api: %s is %d bytes, more than %d", what, desc.Size, h.maxManifest)
		}
		rc, err = rr.Fetch(ctx, desc)
	}
	if errors.Is(err, provider.ErrNotFound) {
		return ManifestEntry{}, &clientError{msg: "the registry does not have " + what, err: err}
	} else if err != nil {
		return ManifestEntry{}, fmt.Errorf("api: %s: %w", what, err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, h.maxManifest+1))
	switch {
	case err != nil:
		return ManifestEntry{}, fmt.Errorf("api: %s: %w", what, err)
	case int64(len(raw)) > h.maxManifest:
		return ManifestEntry{}, fmt.Errorf("api: %s is more than %d bytes", what, h.maxManifest)
	case v.Digest.Algorithm().FromBytes(raw) != v.Digest:
		return ManifestEntry{}, fmt.Errorf("api: %s: the bytes from registry %s do not have the digest", what, repo.Registry)
	case !json.Valid(raw):
		return ManifestEntry{}, fmt.Errorf("api: %s is not JSON", what)
	}
	return ManifestEntry{Variant: v.Variant, Digest: v.Digest.String(), MediaType: desc.MediaType, Size: int64(len(raw)), Manifest: raw}, nil
}

// noneMatch reports whether the If-None-Match of r names etag, by the weak comparison of RFC
// 9110 §13.1.2 (W/"x" matches "x"), or is *.
func noneMatch(r *http.Request, etag string) bool {
	for _, v := range r.Header.Values("If-None-Match") {
		for _, tag := range strings.Split(v, ",") {
			tag = strings.TrimSpace(tag)
			if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
				return true
			}
		}
	}
	return false
}
