package facade

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// recordingIngester is the pipeline with the referrers it got recorded, so that a test sees
// what the facade passes and that the facade pushes nothing itself.
type recordingIngester struct {
	Ingester
	got []ingest.Referrer
}

func (r *recordingIngester) PutReferrer(_ context.Context, _ index.Repo, ref ingest.Referrer, _ auth.Principal) (digest.Digest, error) {
	r.got = append(r.got, ref)
	return ref.Desc.Digest, nil
}

// PutReferrer is the stub's answer to a referrer: its error.
func (s stubIngester) PutReferrer(context.Context, index.Repo, ingest.Referrer, auth.Principal) (digest.Digest, error) {
	return "", s.err
}

// TestReferrerHandOff: the facade hands a referrer to ingest with what the request says (the
// package path, the tag or none, the descriptor, the bytes, the subject of the manifest or of
// cosign's tag), on a managed repo and on an oci repo, and pushes no manifest itself.
func TestReferrerHandOff(t *testing.T) {
	rec := &recordingIngester{}
	e := newEnv(t, envOptions{})
	rec.Ingester, e.ingest = e.ingest, rec
	e.h = e.replica()
	_, subject := e.signedFile("docs/handoff.txt")
	sig, sigDigest := referrerOf(t, subject, cosignBundle, "signature")
	legacy := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":` +
		`{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	cosignTag := "sha256-" + subject.Encoded() + ".sig"
	e.pushBlob("oci/images/app", ocispec.DescriptorEmptyJSON.Data)
	e.pushBlob("oci/images/app", []byte("signature"))
	pushes := len(e.prov.PushReferences())
	for _, tt := range []struct {
		target string
		body   []byte
		want   ingest.Referrer
	}{
		{"/v2/raw/assets/docs/handoff.txt/manifests/" + sigDigest.String(), sig, ingest.Referrer{Path: "docs/handoff.txt", Subject: subject}},
		{"/v2/raw/assets/docs/handoff.txt/manifests/" + cosignTag, legacy, ingest.Referrer{Path: "docs/handoff.txt", Tag: cosignTag, Subject: subject}},
		{"/v2/oci/images/app/manifests/" + sigDigest.String(), sig, ingest.Referrer{Path: "app", Subject: subject}},
		{"/v2/oci/images/app/manifests/signed", sig, ingest.Referrer{Path: "app", Tag: "signed", Subject: subject}},
	} {
		rec.got = nil
		rr := e.do(e.h, http.MethodPut, tt.target, bytes.NewReader(tt.body), "Content-Type", ocispecManifest, principalHeader, "alice")
		wantStatus(t, tt.target, rr, http.StatusCreated)
		tt.want.Desc = ocispec.Descriptor{MediaType: ocispecManifest, Digest: digest.FromBytes(tt.body), Size: int64(len(tt.body))}
		tt.want.Manifest = tt.body
		if len(rec.got) != 1 || !reflect.DeepEqual(rec.got[0], tt.want) {
			t.Errorf("%s: ingest got %+v, want %+v", tt.target, rec.got, tt.want)
		}
	}
	if got := len(e.prov.PushReferences()); got != pushes {
		t.Errorf("the facade pushed %d manifests itself", got-pushes)
	}
}

// TestReferrerErrors is the error table of ingestReferrer.
func TestReferrerErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"invalid", fmt.Errorf("ingest: %w: the subject is not a manifest", ingest.ErrInvalid), http.StatusBadRequest, codeManifestInvalid},
		{"conflict", fmt.Errorf("ingest: %w", ingest.ErrConflict), http.StatusConflict, codeTagInvalid},
		{"backend credentials", fmt.Errorf("ingest: %w", provider.ErrUnauthorized), http.StatusBadGateway, codeUnknown},
		{"anything else", errors.New("ingest: https://backend.example went away"), http.StatusInternalServerError, codeUnknown},
	} {
		e := newEnv(t, envOptions{ingest: stubIngester{tt.err}})
		e.pushBlob("oci/images/app", ocispec.DescriptorEmptyJSON.Data)
		e.pushBlob("oci/images/app", []byte("signature"))
		sig, d := referrerOf(t, digest.FromString("an image"), cosignBundle, "signature")
		rr := e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/"+d.String(), bytes.NewReader(sig), "Content-Type", ocispecManifest)
		if msg := wantError(t, tt.name, rr, tt.status, tt.code); strings.Contains(msg, "backend.example") || strings.Contains(msg, "format: invalid") {
			t.Errorf("%s: message %q", tt.name, msg)
		}
	}
}

// signedFile is a raw file pushed through the hook to raw/assets/<path>, and the blobs of a
// referrer of it uploaded to the backend: the subject a client signs.
func (e *env) signedFile(path string) (*rawFile, digest.Digest) {
	e.t.Helper()
	f := e.newRawFile(path, "signed "+path)
	body, d := f.body(e.t)
	wantStatus(e.t, "push of the subject", e.put(f, f.tag(), body), http.StatusCreated)
	e.pushBlob("raw/assets/"+path, ocispec.DescriptorEmptyJSON.Data)
	e.pushBlob("raw/assets/"+path, []byte("signature"))
	return f, d
}

// TestReferrerPush: a referrer of a raw file passes through the hook: stored by digest with
// OCI-Subject and not indexed, then listed by the referrers API; the same push again is 201;
// cosign's legacy signature is stored under its tag and read back by it.
func TestReferrerPush(t *testing.T) {
	e := newEnv(t, envOptions{})
	f, subject := e.signedFile("docs/signed.txt")
	e.wantIndexed(f, subject)
	name := "/v2/raw/assets/" + f.path
	sig, sigDigest := referrerOf(t, subject, cosignBundle, "signature")
	for _, what := range []string{"push", "the same push again"} {
		rr := e.put(f, sigDigest.String(), sig)
		wantStatus(t, what, rr, http.StatusCreated)
		wantHeader(t, what, rr, "Location", name+"/manifests/"+sigDigest.String())
		wantHeader(t, what, rr, "Docker-Content-Digest", sigDigest.String())
		wantHeader(t, what, rr, "OCI-Subject", subject.String())
	}
	pkg, err := e.st.GetPackage(t.Context(), e.raw.ID, f.path)
	must(t, err)
	if versions, err := e.st.ListVersions(t.Context(), pkg.ID); err != nil || len(versions) != 1 {
		t.Errorf("versions = %+v, %v: the referrer is not indexed", versions, err)
	}
	// The file's push and the referrer's two, whose subject is the file's digest.
	audit, err := e.st.ListAudit(t.Context(), index.AuditFilter{})
	must(t, err)
	if len(audit) != 3 || audit[0].Subject != subject.String() || audit[1].Subject != subject.String() {
		t.Errorf("audit %+v", audit)
	}
	rr := e.do(e.h, http.MethodGet, name+"/referrers/"+subject.String(), nil)
	if got := referrersOf(t, "referrers", rr.Body.Bytes()); len(got) != 1 || got[0] != sigDigest.String()+" "+cosignBundle {
		t.Errorf("referrers = %q, want %s", got, sigDigest)
	}

	legacy := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{{MediaType: "application/vnd.dev.cosign.simplesigning.v1+json",
			Digest: digest.FromString("signature"), Size: int64(len("signature"))}}}
	body, err := json.Marshal(legacy)
	must(t, err)
	tag := "sha256-" + subject.Encoded() + ".sig"
	rr = e.put(f, tag, body)
	wantStatus(t, "cosign's legacy signature", rr, http.StatusCreated)
	wantHeader(t, "cosign's legacy signature", rr, "OCI-Subject", "")
	get := e.do(e.h, http.MethodGet, name+"/manifests/"+tag, nil)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), body) {
		t.Errorf("GET manifests/%s = %d %s", tag, get.Code, get.Body)
	}
	tags := e.do(e.h, http.MethodGet, name+"/tags/list", nil)
	if want := `{"name":"raw/assets/` + f.path + `","tags":["` + f.tag() + `"]}` + "\n"; tags.Body.String() != want {
		t.Errorf("tags/list = %s, want %s: the tags of the index only", tags.Body, want)
	}
}

// TestReferrerPushRefused: a referrer whose subject is not a manifest of the repository, whose
// blobs were not uploaded, or that comes under a tag other than cosign's, is refused, and
// nothing reaches the backend.
func TestReferrerPushRefused(t *testing.T) {
	e := newEnv(t, envOptions{})
	f, subject := e.signedFile("docs/refused.txt")
	other := digest.FromString("no such manifest")
	ok, okDigest := referrerOf(t, subject, cosignBundle, "signature")
	unknown, unknownDigest := referrerOf(t, other, cosignBundle, "signature")
	noLayer, noLayerDigest := referrerOf(t, subject, cosignBundle, "a layer nobody uploaded")
	legacy := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":` +
		`{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	var withSubject ocispec.Manifest
	must(t, json.Unmarshal(ok, &withSubject))
	withSubject.Subject.Digest = other
	mismatch, err := json.Marshal(withSubject)
	must(t, err)
	for _, tt := range []struct {
		name, ref string
		body      []byte
		status    int
		code      string
		message   string
	}{
		{"a subject the repository does not have", unknownDigest.String(), unknown, http.StatusBadRequest, codeManifestInvalid, other.String()},
		{"a layer not uploaded", noLayerDigest.String(), noLayer, http.StatusBadRequest, codeManifestBlobUnknown, digest.FromString("a layer nobody uploaded").String()},
		{"a tag that is not cosign's", "v1", ok, http.StatusBadRequest, codeManifestInvalid, "by digest"},
		{"cosign's tag of a subject the repository does not have", "sha256-" + other.Encoded() + ".sig", legacy, http.StatusBadRequest, codeManifestInvalid, other.String()},
		{"cosign's tag of another subject than the manifest's", "sha256-" + subject.Encoded() + ".att", mismatch, http.StatusBadRequest, codeManifestInvalid, other.String()},
	} {
		msg := wantError(t, tt.name, e.put(f, tt.ref, tt.body), tt.status, tt.code)
		if !strings.Contains(msg, tt.message) {
			t.Errorf("%s: message %q does not contain %q", tt.name, msg, tt.message)
		}
	}
	for _, d := range []digest.Digest{okDigest, unknownDigest, noLayerDigest} {
		rr := e.do(e.h, http.MethodHead, "/v2/raw/assets/"+f.path+"/manifests/"+d.String(), nil)
		wantStatus(t, "HEAD "+d.String()+" after the refusals", rr, http.StatusNotFound)
	}
}

// withOrigin is the manifest body with lajstrom.origin.url added, as a client that passes its
// bytes off as fetched from an upstream would push it, and its digest.
func withOrigin(t *testing.T, body []byte) ([]byte, digest.Digest) {
	t.Helper()
	var m ocispec.Manifest
	must(t, json.Unmarshal(body, &m))
	m.Annotations[profile.AnnotationOriginURL] = "https://elsewhere.example/file"
	b, err := json.Marshal(m)
	must(t, err)
	return b, digest.FromBytes(b)
}

// TestReferrerOrigin (#762): a referrer with a lajstrom.origin.* key is 400 MANIFEST_INVALID
// naming the key on a hosted repo and on a proxy repo, and nothing reaches the backend; the same
// referrer without it is 201. An oci repo stores a manifest as it came, the key included, with
// a subject or without (manifest-profile.md §3.1).
func TestReferrerOrigin(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, hosted := e.signedFile("docs/origin.txt")
	mirror := index.Repo{Registry: "local", Kind: index.KindProxy, Format: "raw", Name: "mirror", Prefix: "raw/mirror"}
	must(t, e.st.CreateRepo(t.Context(), &mirror))
	fetched, proxied := manifest(t, []byte("fetched"), "a file the proxy holds")
	rr, err := e.prov.Repository(t.Context(), "raw/mirror/docs/origin.txt")
	must(t, err)
	must(t, rr.Push(t.Context(), ocispec.Descriptor{MediaType: ocispecManifest, Digest: proxied, Size: int64(len(fetched))}, bytes.NewReader(fetched)))
	e.pushBlob("raw/mirror/docs/origin.txt", ocispec.DescriptorEmptyJSON.Data)
	e.pushBlob("raw/mirror/docs/origin.txt", []byte("signature"))
	for _, tt := range []struct {
		repo    string
		subject digest.Digest
	}{{"raw/assets", hosted}, {"raw/mirror", proxied}} {
		name := "/v2/" + tt.repo + "/docs/origin.txt/manifests/"
		sig, sigDigest := referrerOf(t, tt.subject, cosignBundle, "signature")
		claimed, claimedDigest := withOrigin(t, sig)
		rr := e.do(e.h, http.MethodPut, name+claimedDigest.String(), bytes.NewReader(claimed), "Content-Type", ocispecManifest, principalHeader, "alice")
		if msg := wantError(t, tt.repo+": a referrer with an origin key", rr, http.StatusBadRequest, codeManifestInvalid); !strings.Contains(msg, profile.AnnotationOriginURL) {
			t.Errorf("%s: message %q does not name %s", tt.repo, msg, profile.AnnotationOriginURL)
		}
		wantStatus(t, tt.repo+": HEAD after the refusal", e.do(e.h, http.MethodHead, name+claimedDigest.String(), nil), http.StatusNotFound)
		rr = e.do(e.h, http.MethodPut, name+sigDigest.String(), bytes.NewReader(sig), "Content-Type", ocispecManifest, principalHeader, "alice")
		wantStatus(t, tt.repo+": the referrer without it", rr, http.StatusCreated)
	}

	e.pushBlob("oci/images/app", ocispec.DescriptorEmptyJSON.Data)
	e.pushBlob("oci/images/app", []byte("signature"))
	image, _ := manifest(t, []byte("layer"), "no subject")
	plain, _ := withOrigin(t, image)
	sig, _ := referrerOf(t, digest.FromString("an image"), cosignBundle, "signature")
	ref, refDigest := withOrigin(t, sig)
	for _, tt := range []struct {
		what, ref string
		body      []byte
	}{{"a manifest without a subject", "v1", plain}, {"a referrer", refDigest.String(), ref}} {
		rr := e.do(e.h, http.MethodPut, "/v2/oci/images/app/manifests/"+tt.ref, bytes.NewReader(tt.body), "Content-Type", ocispecManifest, principalHeader, "alice")
		wantStatus(t, "oci repo: "+tt.what+" with an origin key", rr, http.StatusCreated)
	}
}

// TestReferrerPushAccess: pushing a referrer needs write on the repo of the subject; a reader is
// 403, a user without access 404 as for a repo that does not exist, anonymous 401
// (docs/spec/rbac.md §3).
func TestReferrerPushAccess(t *testing.T) {
	e := newEnv(t, envOptions{authz: authorizer{
		"alice": {"raw/assets": {auth.ActionRead, auth.ActionWrite}},
		"bob":   {"raw/assets": {auth.ActionRead}},
		"carol": {"raw/other": {auth.ActionRead, auth.ActionWrite}},
	}})
	f, subject := e.signedFile("docs/access.txt")
	sig, sigDigest := referrerOf(t, subject, cosignBundle, "signature")
	target := "/v2/raw/assets/" + f.path + "/manifests/" + sigDigest.String()
	for _, tt := range []struct {
		who    string
		status int
		code   string
	}{
		{"bob", http.StatusForbidden, codeDenied},
		{"carol", http.StatusNotFound, codeNameUnknown},
		{"", http.StatusUnauthorized, codeUnauthorized},
	} {
		rr := e.do(e.h, http.MethodPut, target, bytes.NewReader(sig), "Content-Type", ocispecManifest, principalHeader, tt.who)
		wantError(t, "a referrer pushed by "+cmp.Or(tt.who, "anonymous"), rr, tt.status, tt.code)
	}
	rr := e.do(e.h, http.MethodPut, target, bytes.NewReader(sig), "Content-Type", ocispecManifest, principalHeader, "alice")
	wantStatus(t, "a referrer pushed by a writer", rr, http.StatusCreated)
}
