package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// countingIngester is the pipeline with its Ingest calls counted, so that a test sees that the
// facade refused a push before ingest saw it.
type countingIngester struct {
	Ingester
	ingests int
}

func (c *countingIngester) Ingest(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, p auth.Principal) (*ingest.Result, error) {
	c.ingests++
	return c.Ingester.Ingest(ctx, repo, pkgPath, tag, m, raw, p)
}

// kindEnv is an env with the repo raw/<kind> of that kind on the registry "local" and its Ingest
// calls counted. A hosted or proxy repo holds the version f, stored as the proxy's fetcher
// stores what it fetched: by the pipeline, not through the facade. The facade reads the kind
// alone, so raw stands in for npm, the format that has the kinds in M4.
func kindEnv(t *testing.T, kind string) (*env, *countingIngester, index.Repo, *rawFile) {
	t.Helper()
	e := newEnv(t, envOptions{})
	counted := &countingIngester{Ingester: e.ingest}
	e.ingest = counted
	e.h = e.replica()
	repo := index.Repo{Format: "raw", Name: kind, Prefix: "raw/" + kind, Kind: kind}
	if kind != index.KindGroup {
		repo.Registry = "local"
	}
	must(t, e.st.CreateRepo(t.Context(), &repo))
	f := rawFileOf("docs/held.txt", "held by raw/"+kind)
	f.m.Annotations[profile.AnnotationRepo] = kind
	if kind != index.KindGroup {
		e.pushBlob(repo.Prefix+"/"+f.path, f.content)
		body, _ := f.body(t)
		_, err := counted.Ingester.Ingest(t.Context(), repo, f.path, f.tag(), f.m, body, userP("alice"))
		must(t, err)
	}
	return e, counted, repo, f
}

// pushTo sends method with body to /v2/raw/<repo>/<path>/<route> by alice, with the Content-Type
// of an OCI manifest, which a blob upload ignores.
func (e *env) pushTo(repo index.Repo, path, method, route string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(e.h, method, "/v2/raw/"+repo.Name+"/"+path+"/"+route, bytes.NewReader(body),
		"Content-Type", ocispecManifest, principalHeader, "alice")
}

// TestPushByKind is the /v2 row of each kind (docs/spec/http-routing.md §6): a hosted repo takes
// a blob, an artifact and a referrer; a proxy repo the blob and the referrer of a version it
// holds, as a hosted repo does, and any other manifest is 405 UNSUPPORTED before ingest sees it;
// a group has no OCI repository, NAME_UNKNOWN to all three.
func TestPushByKind(t *testing.T) {
	for _, tt := range []struct {
		kind                     string
		blob, artifact, referrer int
		code                     string // the error of the artifact's push, "" for none
	}{
		{index.KindHosted, http.StatusCreated, http.StatusCreated, http.StatusCreated, ""},
		{index.KindProxy, http.StatusCreated, http.StatusMethodNotAllowed, http.StatusCreated, codeUnsupported},
		{index.KindGroup, http.StatusNotFound, http.StatusNotFound, http.StatusNotFound, codeNameUnknown},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			e, counted, repo, held := kindEnv(t, tt.kind)
			before := counted.ingests
			f := rawFileOf("docs/new.txt", "pushed to raw/"+tt.kind)
			f.m.Annotations[profile.AnnotationRepo] = tt.kind
			for _, b := range [][]byte{f.content, ocispec.DescriptorEmptyJSON.Data, []byte("signature")} {
				path := f.path
				if !bytes.Equal(b, f.content) {
					path = held.path
				}
				rr := e.pushTo(repo, path, http.MethodPost, "blobs/uploads/?digest="+digest.FromBytes(b).String(), b)
				wantStatus(t, "blob upload", rr, tt.blob)
			}

			body, _ := f.body(t)
			rr := e.pushTo(repo, f.path, http.MethodPut, "manifests/"+f.tag(), body)
			if tt.code == "" {
				wantStatus(t, "artifact", rr, tt.artifact)
			} else {
				entry := wantEntry(t, "artifact", rr, tt.artifact, tt.code)
				if !strings.Contains(entry.Message, "raw/"+tt.kind) {
					t.Errorf("message %q does not name the repo", entry.Message)
				}
			}
			if refused := tt.code != ""; refused && counted.ingests != before {
				t.Errorf("ingest got %d manifests after the refusal", counted.ingests-before)
			}

			_, subject := held.body(t)
			sig, sigDigest := referrerOf(t, subject, cosignBundle, "signature")
			rr = e.pushTo(repo, held.path, http.MethodPut, "manifests/"+sigDigest.String(), sig)
			wantStatus(t, "referrer", rr, tt.referrer)
			if tt.referrer == http.StatusCreated {
				wantHeader(t, "referrer", rr, "OCI-Subject", subject.String())
			}
		})
	}
}

// TestProxyRepoRefusals: on a proxy repo every manifest that is not a referrer is 405
// UNSUPPORTED, by tag or by digest, whatever it is; nothing reaches ingest or the backend, and
// the repo still serves, lists and deletes what it holds as a hosted repo does.
func TestProxyRepoRefusals(t *testing.T) {
	e, counted, repo, held := kindEnv(t, index.KindProxy)
	before, pushes := counted.ingests, len(e.prov.PushReferences())
	_, subject := held.body(t)

	f := rawFileOf("docs/new.txt", "not from the upstream")
	f.m.Annotations[profile.AnnotationRepo] = repo.Name
	e.pushBlob(repo.Prefix+"/"+f.path, f.content)
	artifact, artifactDigest := f.body(t)
	withSubject := rawFileOf("docs/new.txt", "not from the upstream")
	withSubject.m.Subject = &ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: subject, Size: 42}
	attached, _ := withSubject.body(t)
	imageIndex := []byte(`{"schemaVersion":2,"mediaType":"` + ocispec.MediaTypeImageIndex + `","manifests":[]}`)
	heldBody, _ := held.body(t)
	for _, tt := range []struct {
		name, path, ref, mediaType string
		body                       []byte
	}{
		{"an artifact by tag", f.path, f.tag(), ocispecManifest, artifact},
		{"an artifact by digest", f.path, artifactDigest.String(), ocispecManifest, artifact},
		{"a Lajstrom artifact with a subject", f.path, f.tag(), ocispecManifest, attached},
		{"an image index", f.path, "v1", ocispec.MediaTypeImageIndex, imageIndex},
		{"the manifest the repo holds, again", held.path, held.tag(), ocispecManifest, heldBody},
	} {
		rr := e.do(e.h, http.MethodPut, "/v2/raw/proxy/"+tt.path+"/manifests/"+tt.ref, bytes.NewReader(tt.body),
			"Content-Type", tt.mediaType, principalHeader, "alice")
		entry := wantEntry(t, tt.name, rr, http.StatusMethodNotAllowed, codeUnsupported)
		if !strings.Contains(entry.Message, "raw/proxy is a proxy repo") || !strings.Contains(entry.Message, "referrers") {
			t.Errorf("%s: message %q", tt.name, entry.Message)
		}
		wantHeader(t, tt.name, rr, "Allow", "")
	}
	if counted.ingests != before || len(e.prov.PushReferences()) != pushes {
		t.Errorf("after the refusals ingest got %d manifests, the backend %d", counted.ingests-before, len(e.prov.PushReferences())-pushes)
	}
	if _, err := e.st.GetPackage(t.Context(), repo.ID, f.path); err == nil {
		t.Error("a refused push is in the index")
	}

	// What the repo holds is a hosted repo's to read and to delete.
	name := "/v2/raw/proxy/" + held.path
	wantStatus(t, "GET of the held manifest", e.do(e.h, http.MethodGet, name+"/manifests/"+held.tag(), nil, principalHeader, "alice"), http.StatusOK)
	tags := e.do(e.h, http.MethodGet, name+"/tags/list", nil, principalHeader, "alice")
	if wantStatus(t, "tags/list", tags, http.StatusOK); !strings.Contains(tags.Body.String(), held.tag()) {
		t.Errorf("tags/list %s does not list %s", tags.Body, held.tag())
	}
	wantStatus(t, "DELETE of the held version", e.do(e.h, http.MethodDelete, name+"/manifests/"+subject.String(), nil, principalHeader, "alice"),
		http.StatusAccepted)
}

// TestProxyRepoLegacySignature: cosign's legacy signature, a manifest under the tag
// sha256-<hex>.sig and without a subject, is a referrer, which a proxy repo takes.
func TestProxyRepoLegacySignature(t *testing.T) {
	e, _, repo, held := kindEnv(t, index.KindProxy)
	_, subject := held.body(t)
	e.pushBlob(repo.Prefix+"/"+held.path, ocispec.DescriptorEmptyJSON.Data)
	legacy, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.DescriptorEmptyJSON, Layers: []ocispec.Descriptor{}})
	must(t, err)
	rr := e.pushTo(repo, held.path, http.MethodPut, "manifests/sha256-"+subject.Encoded()+".sig", legacy)
	wantStatus(t, "legacy signature", rr, http.StatusCreated)
}
