package facade

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// referrers answers GET …/referrers/<digest> (distribution spec, "Listing Referrers"): the
// manifests whose subject is the manifest <digest>, as an image index of their descriptors,
// from the provider, which asks the backend's referrers API or reads the sha256-<hex> tag
// schema where the backend has none (Capabilities.Referrers, docs/spec/manifest-profile.md
// §6), or both with referrers.alwaysTag. ?artifactType= keeps the referrers of that type, and the answer says so with
// OCI-Filters-Applied: the provider filters what a backend sent unfiltered. A subject without
// referrers, in a repository the backend does not have too, is an empty index and never a 404,
// which tells a client that the registry has no referrers API: it would then write the tag
// schema itself, behind the facade's back. It needs read on the repo, decided before the
// digest is looked at; a digest that is not one is 400 DIGEST_INVALID.
func (h *handler) referrers(w http.ResponseWriter, r *http.Request, t *target, arg string) error {
	ctx := r.Context()
	if err := h.authorize(ctx, auth.ActionRead, t); err != nil {
		return err
	}
	d, err := parseDigest(arg)
	if err != nil {
		return err
	}
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	artifactType := r.URL.Query().Get("artifactType")
	refs := []ocispec.Descriptor{}
	subject := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d}
	err = rr.Referrers(ctx, subject, artifactType, func(page []ocispec.Descriptor) error {
		refs = append(refs, page...)
		return nil
	})
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		return fmt.Errorf("facade: referrers of %s@%s: %w", t.name, d, err)
	}
	body, err := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex, Manifests: refs})
	if err != nil {
		return fmt.Errorf("facade: referrers of %s@%s: %w", t.name, d, err)
	}
	hdr := w.Header()
	hdr.Set("Content-Type", ocispec.MediaTypeImageIndex)
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	if artifactType != "" {
		hdr.Set("OCI-Filters-Applied", "artifactType")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return nil
}

// created answers the push of the manifest d to t: 201 with Location, Docker-Content-Digest and,
// when the manifest has a subject, OCI-Subject. The facade sends OCI-Subject whatever the
// backend has, since it serves the referrers API (referrers): a client that gets none writes
// the sha256-<hex> tag schema itself, which the provider writes where the backend needs it.
func created(w http.ResponseWriter, t *target, d digest.Digest, subject *ocispec.Descriptor) {
	hdr := w.Header()
	hdr.Set("Location", "/v2/"+t.name+"/manifests/"+d.String())
	hdr.Set("Docker-Content-Digest", d.String())
	if subject != nil {
		hdr.Set("OCI-Subject", subject.Digest.String())
	}
	hdr.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// subjectOf returns the subject of the manifest body, nil when it has none or does not parse.
func subjectOf(body []byte) *ocispec.Descriptor {
	var m struct {
		Subject *ocispec.Descriptor `json:"subject"`
	}
	_ = json.Unmarshal(body, &m) // readManifest has checked that it is JSON
	return m.Subject
}
