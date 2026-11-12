package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
	"github.com/lajstrom/lajstrom/internal/server"
)

// stubProxy is the core of an oci proxy repo for the facade's tests: manifests by path and
// reference, blobs by digest, pushed into the fake registry when fetched, and an error for
// every call when err is set. It records what it was asked.
type stubProxy struct {
	reg       *providertest.Fake
	mu        sync.Mutex
	manifests map[string]stubDoc // path@ref
	blobs     map[digest.Digest][]byte
	calls     []string
	err       error
	tee       func(b []byte) io.ReadCloser // the claim holder's stream of a blob, when set
}

type stubDoc struct {
	desc ocispec.Descriptor
	raw  []byte // nil: the backend holds it
}

func (s *stubProxy) OCIPath(_ index.Repo, path string) string {
	if !strings.Contains(path, "/") {
		return "library/" + path
	}
	return path
}

func (s *stubProxy) OCIManifest(_ context.Context, _ index.Repo, path, ref string) (ocispec.Descriptor, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "manifest "+path+"@"+ref)
	if s.err != nil {
		return ocispec.Descriptor{}, nil, s.err
	}
	d, ok := s.manifests[path+"@"+ref]
	if !ok {
		return ocispec.Descriptor{}, nil, fmt.Errorf("%w: %s", index.ErrNotFound, ref)
	}
	return d.desc, d.raw, nil
}

func (s *stubProxy) OCIBlob(ctx context.Context, _ index.Repo, path string, d digest.Digest, fetch, stream bool) (ocispec.Descriptor, io.ReadCloser, error) {
	s.mu.Lock()
	s.calls = append(s.calls, fmt.Sprintf("blob %s %t", path, fetch))
	b, ok := s.blobs[d]
	err := s.err
	s.mu.Unlock()
	switch {
	case err != nil:
		return ocispec.Descriptor{}, nil, err
	case !ok:
		return ocispec.Descriptor{}, nil, fmt.Errorf("%w: %s", index.ErrNotFound, d)
	}
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: d, Size: int64(len(b))}
	if stream && s.tee != nil {
		return desc, s.tee(b), nil
	}
	if fetch {
		rr, _ := s.reg.Repository(ctx, "oci/hub/"+path)
		if err := rr.Push(ctx, desc, bytes.NewReader(b)); err != nil {
			return ocispec.Descriptor{}, nil, err
		}
	}
	return desc, nil, nil
}

func (s *stubProxy) OCIComplete(_ context.Context, _ index.Repo, path string, desc ocispec.Descriptor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "complete "+path+"@"+desc.Digest.String())
}

func (s *stubProxy) OCIPulling(_ index.Repo, path string) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "pulling "+path)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, "pulled "+path)
	}
}

// proxyEnv is an env with the oci proxy repo oci/hub served by a stubProxy.
func proxyEnv(t *testing.T) (*env, *stubProxy) {
	t.Helper()
	s := &stubProxy{manifests: map[string]stubDoc{}, blobs: map[digest.Digest][]byte{}}
	e := newEnv(t, envOptions{})
	s.reg, e.proxy = e.prov, s
	e.h = e.replica()
	repo := index.Repo{Format: "oci", Name: "hub", Registry: "local", Prefix: "oci/hub", Kind: index.KindProxy,
		Upstream: json.RawMessage(`{"name": "dockerhub"}`)}
	must(t, e.st.CreateRepo(t.Context(), &repo))
	return e, s
}

// TestOCIProxyPull: a pull of an oci proxy repo asks the core, with Docker Hub's library/ rule
// on the path and containerd's ?ns= ignored; a blob the backend does not hold is fetched on a
// GET and answered from the listing on a HEAD (docs/spec/proxy-and-group.md §11).
func TestOCIProxyPull(t *testing.T) {
	e, s := proxyEnv(t)
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	s.manifests["library/nginx@1.27"] = stubDoc{desc: desc, raw: raw}
	rr := e.do(e.h, http.MethodGet, "/v2/oci/hub/nginx/manifests/1.27?ns=docker.io", nil)
	wantStatus(t, "GET of the manifest", rr, http.StatusOK)
	wantHeader(t, "GET of the manifest", rr, "Docker-Content-Digest", desc.Digest.String())
	wantHeader(t, "GET of the manifest", rr, "Content-Type", ocispec.MediaTypeImageIndex)
	if !bytes.Equal(rr.Body.Bytes(), raw) {
		t.Errorf("body = %s", rr.Body)
	}
	rr = e.do(e.h, http.MethodHead, "/v2/oci/hub/library/nginx/manifests/1.27", nil)
	wantStatus(t, "HEAD of the manifest", rr, http.StatusOK)
	if rr.Body.Len() != 0 {
		t.Errorf("HEAD has a body")
	}

	layer := []byte("layer bytes")
	d := digest.FromBytes(layer)
	s.blobs[d] = layer
	rr = e.do(e.h, http.MethodHead, "/v2/oci/hub/nginx/blobs/"+d.String(), nil)
	wantStatus(t, "HEAD of a blob not held", rr, http.StatusOK)
	wantHeader(t, "HEAD of a blob not held", rr, "Content-Length", fmt.Sprint(len(layer)))
	rr = e.do(e.h, http.MethodGet, "/v2/oci/hub/nginx/blobs/"+d.String(), nil)
	wantStatus(t, "GET of a blob not held", rr, http.StatusOK)
	if !bytes.Equal(rr.Body.Bytes(), layer) {
		t.Errorf("blob body = %q", rr.Body)
	}
	rr = e.do(e.h, http.MethodGet, "/v2/oci/hub/nginx/blobs/"+d.String(), nil)
	wantStatus(t, "GET of a blob held", rr, http.StatusOK)
	want := []string{"manifest library/nginx@1.27", "complete library/nginx@" + desc.Digest.String(), "manifest library/nginx@1.27",
		"blob library/nginx false", "pulling library/nginx", "blob library/nginx true", "pulled library/nginx", "pulling library/nginx",
		"pulled library/nginx"}
	if strings.Join(s.calls, "|") != strings.Join(want, "|") {
		t.Errorf("the core was asked %q, want %q", s.calls, want)
	}
}

// TestOCIProxyHeld: a manifest the backend holds is read from it, checked against its digest,
// and starts no completion: it is complete.
func TestOCIProxyHeld(t *testing.T) {
	e, s := proxyEnv(t)
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	backend, err := e.prov.Repository(t.Context(), "oci/hub/library/alpine")
	must(t, err)
	must(t, backend.PushReference(t.Context(), desc, bytes.NewReader(raw), profile.KeepTag(desc.Digest)))
	must(t, backend.PushReference(t.Context(), desc, bytes.NewReader(raw), "3"))
	s.manifests["library/alpine@3"] = stubDoc{desc: desc}
	rr := e.do(e.h, http.MethodGet, "/v2/oci/hub/alpine/manifests/3", nil)
	wantStatus(t, "GET of a held manifest", rr, http.StatusOK)
	if !bytes.Equal(rr.Body.Bytes(), raw) {
		t.Errorf("body = %s", rr.Body)
	}
	if want := []string{"manifest library/alpine@3"}; !slices.Equal(s.calls, want) {
		t.Errorf("the core was asked %q, want %q", s.calls, want)
	}
	rr = e.do(e.h, http.MethodGet, "/v2/oci/hub/alpine/tags/list", nil)
	wantStatus(t, "tags/list", rr, http.StatusOK)
	var list tagList
	must(t, json.Unmarshal(rr.Body.Bytes(), &list))
	if len(list.Tags) != 1 || list.Tags[0] != "3" {
		t.Errorf("tags = %q, want 3 alone: keep-tags are hidden", list.Tags)
	}
}

// TestOCIProxyErrors: the core's errors in the distribution spec's JSON.
func TestOCIProxyErrors(t *testing.T) {
	e, s := proxyEnv(t)
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: gone", index.ErrNotFound), http.StatusNotFound, codeManifestUnknown},
		{fmt.Errorf("%w: deny: acme/*", format.ErrForbidden), http.StatusForbidden, codeDenied},
		{fmt.Errorf("%w: too deep", format.ErrInvalid), http.StatusBadRequest, codeNameInvalid},
		{fmt.Errorf("upstream: %w", &format.UnavailableError{RetryAfter: 7 * time.Second}), http.StatusServiceUnavailable, codeTooManyRequests},
		{fmt.Errorf("%w: upstream hub answered 500", format.ErrBadGateway), http.StatusBadGateway, codeUnknown},
	} {
		s.err = tt.err
		rr := e.do(e.h, http.MethodGet, "/v2/oci/hub/library/nginx/manifests/1.27", nil)
		wantError(t, tt.err.Error(), rr, tt.status, tt.code)
		if tt.status == http.StatusServiceUnavailable {
			wantHeader(t, "a 429 upstream", rr, "Retry-After", "7")
		}
	}
	s.err = fmt.Errorf("%w: not listed", index.ErrNotFound)
	rr := e.do(e.h, http.MethodGet, "/v2/oci/hub/library/nginx/blobs/"+digest.FromString("x").String(), nil)
	wantError(t, "a blob nothing lists", rr, http.StatusNotFound, codeBlobUnknown)
}

// TestOCIProxyPutRefused: a manifest that is not a referrer is 405 UNSUPPORTED on an oci proxy.
func TestOCIProxyPutRefused(t *testing.T) {
	e, _ := proxyEnv(t)
	raw := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[]}`)
	rr := e.do(e.h, http.MethodPut, "/v2/oci/hub/library/nginx/manifests/mine", bytes.NewReader(raw), "Content-Type", ocispec.MediaTypeImageManifest)
	wantError(t, "PUT of a manifest", rr, http.StatusMethodNotAllowed, codeUnsupported)
	if n := len(e.prov.PushReferences()) + len(e.prov.Pushes()); n != 0 {
		t.Errorf("%d pushes, want none", n)
	}
}

// brokenTee is a tee whose copy failed before its last byte.
type brokenTee struct{ r io.Reader }

func (b brokenTee) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		return n, errors.New("the registry refused the digest")
	}
	return n, err
}

func (brokenTee) Close() error { return nil }

// TestOCIProxyTee: the claim holder gets the blob from the core's tee with the headers of a whole
// blob; a tee that fails aborts the response, so that the client never completes the blob
// (proxy-and-group.md §11.8, rule 2).
func TestOCIProxyTee(t *testing.T) {
	e, s := proxyEnv(t)
	layer := bytes.Repeat([]byte("layer "), 1000)
	d := digest.FromBytes(layer)
	s.blobs[d] = layer
	srv, err := server.New(server.Options{Listen: "127.0.0.1:0", Facade: e.h, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	url := ts.URL + "/v2/oci/hub/library/nginx/blobs/" + d.String()

	s.tee = func(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }
	n, err := get(t, client, url)
	if err != nil || n != int64(len(layer)) {
		t.Fatalf("GET through the tee: %d bytes, %v", n, err)
	}
	s.tee = func(b []byte) io.ReadCloser { return brokenTee{bytes.NewReader(b[:len(b)-1])} }
	d2 := digest.FromString("another layer")
	s.blobs[d2] = []byte("another layer")
	n, err = get(t, client, ts.URL+"/v2/oci/hub/library/nginx/blobs/"+d2.String())
	if err == nil || n >= int64(len("another layer")) {
		t.Fatalf("GET through a tee that failed: %d bytes, %v; want a short body and an error", n, err)
	}
}
