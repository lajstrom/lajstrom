package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// The artifact types of the referrers in these tests.
const (
	cosignBundle = "application/vnd.dev.sigstore.bundle.v0.3+json"
	spdxJSON     = "application/spdx+json"
)

// referrerOf returns a manifest of artifactType whose subject is the manifest subject, with
// the empty config and one layer of the bytes layer, and its digest.
func referrerOf(t *testing.T, subject digest.Digest, artifactType, layer string) ([]byte, digest.Digest) {
	t.Helper()
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config:       ocispec.DescriptorEmptyJSON,
		Layers:       []ocispec.Descriptor{{MediaType: artifactType, Digest: digest.FromString(layer), Size: int64(len(layer))}},
		Subject:      &ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: subject, Size: 42},
		Annotations:  map[string]string{"test": layer},
	}
	b, err := json.Marshal(m)
	must(t, err)
	return b, digest.FromBytes(b)
}

// pushReferrer stores a referrer of subject in the OCI repository name of the fake, as a client
// would through the registry itself, and returns its digest.
func (e *env) pushReferrer(name string, subject digest.Digest, artifactType, layer string) digest.Digest {
	e.t.Helper()
	b, d := referrerOf(e.t, subject, artifactType, layer)
	rr, err := e.prov.Repository(e.t.Context(), name)
	must(e.t, err)
	must(e.t, rr.Push(e.t.Context(), ocispec.Descriptor{MediaType: ocispecManifest, Digest: d, Size: int64(len(b))}, bytes.NewReader(b)))
	return d
}

// referrersOf decodes the answer of a referrers request: the digests and artifact types of
// its manifests, sorted, after checking the image index around them.
func referrersOf(t *testing.T, what string, body []byte) []string {
	t.Helper()
	var idx ocispec.Index
	if err := json.Unmarshal(body, &idx); err != nil {
		t.Fatalf("%s: the body is not an image index: %v: %s", what, err, body)
	}
	if idx.SchemaVersion != 2 || idx.MediaType != ocispec.MediaTypeImageIndex || idx.Manifests == nil {
		t.Errorf("%s: index %s, want schemaVersion 2, the index media type and a manifests array", what, body)
	}
	var got []string
	for _, m := range idx.Manifests {
		got = append(got, m.Digest.String()+" "+m.ArtifactType)
	}
	slices.Sort(got)
	return got
}

// TestReferrers lists the referrers of a manifest from the provider: all of them, those of
// one artifactType with OCI-Filters-Applied, none for a subject without referrers (200, an
// empty index, never 404), on a managed repo and on an oci repo alike.
func TestReferrers(t *testing.T) {
	e := newEnv(t, envOptions{})
	subject := digest.FromString("the subject")
	sig := e.pushReferrer("raw/assets/hello", subject, cosignBundle, "signature")
	sbom := e.pushReferrer("raw/assets/hello", subject, spdxJSON, "sbom")
	e.pushReferrer("raw/assets/hello", digest.FromString("another subject"), cosignBundle, "elsewhere")
	e.pushReferrer("raw/assets/other", subject, cosignBundle, "another repository")
	image := e.pushReferrer("oci/images/app", subject, cosignBundle, "image signature")
	ref := func(d digest.Digest, artifactType string) string { return d.String() + " " + artifactType }
	tests := []struct {
		name, target, filter string
		want                 []string
	}{
		{"all", "/v2/raw/assets/hello/referrers/" + subject.String(), "", []string{ref(sig, cosignBundle), ref(sbom, spdxJSON)}},
		{"one artifactType", "/v2/raw/assets/hello/referrers/" + subject.String() + "?artifactType=" + url.QueryEscape(spdxJSON), spdxJSON, []string{ref(sbom, spdxJSON)}},
		{"an artifactType nobody has", "/v2/raw/assets/hello/referrers/" + subject.String() + "?artifactType=application/x-none", "application/x-none", nil},
		{"a subject without referrers", "/v2/raw/assets/hello/referrers/" + string(zeroDigest), "", nil},
		{"a repository the backend does not have", "/v2/raw/assets/nothing/referrers/" + subject.String(), "", nil},
		{"an oci repo", "/v2/oci/images/app/referrers/" + subject.String(), "", []string{ref(image, cosignBundle)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := e.do(e.h, http.MethodGet, tt.target, nil)
			wantStatus(t, tt.name, rr, http.StatusOK)
			wantHeader(t, tt.name, rr, "Content-Type", ocispec.MediaTypeImageIndex)
			wantHeader(t, tt.name, rr, "Docker-Distribution-API-Version", "registry/2.0")
			wantHeader(t, tt.name, rr, "OCI-Filters-Applied", map[bool]string{true: "artifactType"}[tt.filter != ""])
			slices.Sort(tt.want)
			if got := referrersOf(t, tt.name, rr.Body.Bytes()); !slices.Equal(got, tt.want) {
				t.Errorf("referrers = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestReferrersPassThrough: an oci repo passes a referrer through and answers OCI-Subject, so
// that the client does not write the tag schema itself (the conformance suite's references
// setup wants it); the referrers API lists it.
func TestReferrersPassThrough(t *testing.T) {
	e := newEnv(t, envOptions{})
	subject := digest.FromString("an image")
	e.pushBlob("oci/images/app", ocispec.DescriptorEmptyJSON.Data)
	e.pushBlob("oci/images/app", []byte("signature"))
	sig, sigDigest := referrerOf(t, subject, cosignBundle, "signature")
	rr := e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/"+sigDigest.String(), bytes.NewReader(sig), "Content-Type", ocispecManifest)
	wantStatus(t, "a referrer", rr, http.StatusCreated)
	wantHeader(t, "a referrer", rr, "OCI-Subject", subject.String())
	m, _ := manifest(t, []byte("layer"), "no subject")
	rr = e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/v1", bytes.NewReader(m), "Content-Type", ocispecManifest)
	wantStatus(t, "a manifest without a subject", rr, http.StatusCreated)
	wantHeader(t, "a manifest without a subject", rr, "OCI-Subject", "")
	rr = e.do(e.h, http.MethodGet, "/v2/oci/images/app/referrers/"+subject.String(), nil)
	if got := referrersOf(t, "referrers", rr.Body.Bytes()); len(got) != 1 || got[0] != sigDigest.String()+" "+cosignBundle {
		t.Errorf("referrers = %q, want %s", got, sigDigest)
	}
}

// TestReferrersAccess: a referrers request needs read on the repo of the subject, and a repo
// the caller may not read answers as one that does not exist (docs/spec/rbac.md §3): 401 to
// anonymous, 404 NAME_UNKNOWN to a user without access, as for a repo that does not exist.
// A bearer is decided by its access claims.
func TestReferrersAccess(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{
		"bob":   {"raw/assets": {auth.ActionRead}},
		"carol": {"raw/other": {auth.ActionRead, auth.ActionWrite}},
	}})
	subject := digest.FromString("a signed file")
	sig := e.pushReferrer("raw/assets/hello", subject, cosignBundle, "signature")
	target := "/v2/raw/assets/hello/referrers/" + subject.String()
	for _, tt := range []struct {
		name, target string
		hdr          []string
		status       int
		code         string
	}{
		{"a reader", target, []string{principalHeader, "bob"}, http.StatusOK, ""},
		{"a user without access", target, []string{principalHeader, "carol"}, http.StatusNotFound, codeNameUnknown},
		{"a repo that does not exist", "/v2/raw/none/hello/referrers/" + subject.String(), []string{principalHeader, "carol"}, http.StatusNotFound, codeNameUnknown},
		{"anonymous", target, nil, http.StatusUnauthorized, codeUnauthorized},
		{"anonymous, a repo that does not exist", "/v2/raw/none/hello/referrers/" + subject.String(), nil, http.StatusUnauthorized, codeUnauthorized},
		{"a pull token", target, []string{"Authorization", "Bearer " + e.issue(userP("carol"), repoAccess("raw/assets/hello", "pull"))}, http.StatusOK, ""},
		{"a token of another name", target, []string{"Authorization", "Bearer " + e.issue(userP("bob"), repoAccess("raw/assets/other", "pull"))}, http.StatusUnauthorized, codeUnauthorized},
	} {
		rr := e.do(e.h, http.MethodGet, tt.target, nil, tt.hdr...)
		if tt.code != "" {
			msg := wantError(t, tt.name, rr, tt.status, tt.code)
			if strings.Contains(msg, sig.Encoded()) {
				t.Errorf("%s: the denial names the referrer: %q", tt.name, msg)
			}
			continue
		}
		wantStatus(t, tt.name, rr, tt.status)
		if got := referrersOf(t, tt.name, rr.Body.Bytes()); len(got) != 1 || !strings.HasPrefix(got[0], sig.String()) {
			t.Errorf("%s: referrers = %q, want %s", tt.name, got, sig)
		}
	}
}

// TestReferrersErrors: a digest that is not one is 400 DIGEST_INVALID before the backend is
// asked; a backend that fails is a 500 that names nothing of it, its cause logged.
func TestReferrersErrors(t *testing.T) {
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return referrersFailing{f} }})
	for _, tt := range []struct {
		name, target string
		status       int
		code         string
	}{
		{"not a digest", "/v2/raw/assets/hello/referrers/v1", http.StatusBadRequest, codeDigestInvalid},
		{"a digest of the wrong length", "/v2/raw/assets/hello/referrers/sha256:abc", http.StatusBadRequest, codeDigestInvalid},
		{"the backend fails", "/v2/raw/assets/hello/referrers/" + string(zeroDigest), http.StatusInternalServerError, codeUnknown},
	} {
		rr := e.do(e.h, http.MethodGet, tt.target, nil)
		if msg := wantError(t, tt.name, rr, tt.status, tt.code); strings.Contains(msg, "backend.example") {
			t.Errorf("%s: the message names the backend: %q", tt.name, msg)
		}
	}
	if !strings.Contains(e.logs.String(), "backend.example") {
		t.Errorf("the cause of the 500 is not logged:\n%s", e.logs)
	}
}

// referrersFailing is the fake whose repositories fail every referrers listing.
type referrersFailing struct{ *providertest.Fake }

func (f referrersFailing) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return referrersFailingRepository{rr}, err
}

type referrersFailingRepository struct{ provider.Repository }

func (referrersFailingRepository) Referrers(context.Context, ocispec.Descriptor, string, func([]ocispec.Descriptor) error) error {
	return errors.New("GET https://backend.example/v2/x/referrers: connection reset")
}
