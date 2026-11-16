package facade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// plaintextMarker is in every file of these tests, and never in the registry.
const plaintextMarker = "the plaintext marker 4b1d"

// uploadEncrypted uploads content to name as oras does, POST then PUT with the body, or in
// chunks of chunk bytes (PATCHes, then an empty PUT) when chunk > 0, and returns its digest.
func (e *env) uploadEncrypted(name string, content []byte, chunk int) digest.Digest {
	e.t.Helper()
	d := digest.FromBytes(content)
	rr := e.do(e.h, http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, principalHeader, "alice")
	wantStatus(e.t, "POST", rr, http.StatusAccepted)
	loc := rr.Header().Get("Location")
	if chunk > 0 {
		for off := 0; off < len(content); off += chunk {
			part := content[off:min(off+chunk, len(content))]
			rr = e.do(e.h, http.MethodPatch, loc, bytes.NewReader(part), principalHeader, "alice",
				"Content-Range", fmt.Sprintf("%d-%d", off, off+len(part)-1))
			wantStatus(e.t, "PATCH", rr, http.StatusAccepted)
		}
		content = nil
	}
	rr = e.do(e.h, http.MethodPut, loc+"?digest="+d.String(), bytes.NewReader(content), principalHeader, "alice")
	wantStatus(e.t, "PUT", rr, http.StatusCreated)
	wantHeader(e.t, "PUT", rr, "Docker-Content-Digest", d.String())
	return d
}

// checkCiphertext checks that the backend holds the blob of the plaintext digest d at name only
// as ciphertext, under the digest blob_map maps it to.
func (e *env) checkCiphertext(name, path string, d digest.Digest) *index.BlobMap {
	e.t.Helper()
	ctx := e.t.Context()
	rows, err := e.st.FindBlobMap(ctx, e.raw.ID, path, d, index.BlobMapBlob)
	must(e.t, err)
	if len(rows) == 0 {
		e.t.Fatalf("no row of blob_map for %s at %s", d, path)
	}
	rr, err := e.prov.Repository(ctx, name)
	must(e.t, err)
	if ok, _ := rr.Exists(ctx, ocispec.Descriptor{Digest: d, Size: rows[0].Size}); ok {
		e.t.Errorf("the backend holds the plaintext %s", d)
	}
	rc, err := rr.Fetch(ctx, ocispec.Descriptor{Digest: rows[0].Stored, Size: rows[0].Size})
	must(e.t, err)
	ct, err := io.ReadAll(rc)
	_ = rc.Close()
	must(e.t, err)
	if bytes.Contains(ct, []byte(plaintextMarker)) {
		e.t.Errorf("the ciphertext of %s holds the plaintext", d)
	}
	return &rows[0]
}

// TestEncryptedUploads: a blob uploaded into an encrypted repo in one request, in chunks (the
// HMAC read back), or in a POST with its digest is ciphertext in the backend; HEAD and GET by the
// plaintext digest answer it decrypted, never redirected; the ciphertext's digest answers it as
// the backend has it.
func TestEncryptedUploads(t *testing.T) {
	e := newEnv(t, envOptions{encrypted: true})
	e.prov.RedirectURL = func(_ string, d digest.Digest) (string, bool) { return "https://objects.test/" + d.Encoded(), true }
	name := "raw/assets/dist/app.txt"
	contents := map[string][]byte{
		"one request": []byte(plaintextMarker + strings.Repeat(" one", 1000)),
		"chunks":      []byte(plaintextMarker + strings.Repeat(" two", 1000)),
	}
	ds := map[string]digest.Digest{
		"one request": e.uploadEncrypted(name, contents["one request"], 0),
		"chunks":      e.uploadEncrypted(name, contents["chunks"], 700),
	}
	mono := []byte(plaintextMarker + " monolithic")
	md := digest.FromBytes(mono)
	rr := e.do(e.h, http.MethodPost, "/v2/"+name+"/blobs/uploads/?digest="+md.String(), bytes.NewReader(mono), principalHeader, "alice")
	wantStatus(t, "POST ?digest=", rr, http.StatusCreated)
	contents["monolithic"], ds["monolithic"] = mono, md

	for what, d := range ds {
		row := e.checkCiphertext(name, "dist/app.txt", d)
		got := e.do(e.h, http.MethodGet, "/v2/"+name+"/blobs/"+d.String(), nil, principalHeader, "alice")
		if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), contents[what]) {
			t.Errorf("%s: GET by the plaintext digest = %d, %d bytes", what, got.Code, got.Body.Len())
		}
		wantHeader(t, what, got, "Docker-Content-Digest", d.String())
		wantHeader(t, what, got, "Location", "")
		head := e.do(e.h, http.MethodHead, "/v2/"+name+"/blobs/"+d.String(), nil, principalHeader, "alice")
		wantStatus(t, what+": HEAD", head, http.StatusOK)
		wantHeader(t, what+": HEAD", head, "Content-Length", fmt.Sprint(len(contents[what])))
		stored := e.do(e.h, http.MethodGet, "/v2/"+name+"/blobs/"+row.Stored.String(), nil, principalHeader, "alice")
		if stored.Code != http.StatusTemporaryRedirect {
			t.Errorf("%s: GET by the ciphertext's digest = %d, want the backend's redirect", what, stored.Code)
		}
	}
}

// TestEncryptedManifest: oras push of a raw file to an encrypted repo stores the manifest
// changed (+encrypted layers, the wrapped key), answers the client's digest, and answers the
// manifest in the client's form by tag and by that digest, as stored by its own digest; the
// version deletes by the client's digest.
func TestEncryptedManifest(t *testing.T) {
	e := newEnv(t, envOptions{encrypted: true})
	f := rawFileOf("dist/app.txt", plaintextMarker+" in a raw file").at(e.clock.Now())
	e.uploadEncrypted("raw/assets/dist/app.txt", f.content, 0)
	body, d := f.body(t)
	rr := e.put(f, f.tag(), body)
	wantStatus(t, "PUT manifest", rr, http.StatusCreated)
	wantHeader(t, "PUT manifest", rr, "Docker-Content-Digest", d.String())

	got := e.do(e.h, http.MethodGet, "/v2/raw/assets/dist/app.txt/manifests/"+f.tag(), nil, principalHeader, "alice")
	if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), body) {
		t.Fatalf("GET by tag = %d %s, want the client's manifest", got.Code, got.Body)
	}
	wantHeader(t, "GET by tag", got, "Docker-Content-Digest", d.String())
	byDigest := e.do(e.h, http.MethodGet, "/v2/raw/assets/dist/app.txt/manifests/"+d.String(), nil, principalHeader, "alice")
	if byDigest.Code != http.StatusOK || !bytes.Equal(byDigest.Body.Bytes(), body) {
		t.Errorf("GET by the client's digest = %d", byDigest.Code)
	}
	rows, err := e.st.FindBlobMap(t.Context(), e.raw.ID, "dist/app.txt", d, index.BlobMapManifest)
	must(t, err)
	if len(rows) != 1 {
		t.Fatalf("manifest rows %+v", rows)
	}
	stored := e.do(e.h, http.MethodGet, "/v2/raw/assets/dist/app.txt/manifests/"+rows[0].Stored.String(), nil, principalHeader, "alice")
	var m ocispec.Manifest
	must(t, json.Unmarshal(stored.Body.Bytes(), &m))
	if stored.Code != http.StatusOK || len(m.Layers) != 1 || !blobcrypt.Encrypted(m.Layers[0].MediaType) ||
		m.Layers[0].Annotations[blobcrypt.AnnotationJWE] == "" {
		t.Errorf("GET by the stored digest = %d %s, want the stored manifest", stored.Code, stored.Body)
	}

	other := rawFileOf("dist/other.txt", "not uploaded encrypted").at(e.clock.Now())
	e.pushBlob("raw/assets/dist/other.txt", other.content)
	ob, _ := other.body(t)
	wantError(t, "a layer uploaded in the clear", e.put(other, other.tag(), ob), http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN")

	del := e.do(e.h, http.MethodDelete, "/v2/raw/assets/dist/app.txt/manifests/"+d.String(), nil, principalHeader, "alice")
	wantStatus(t, "DELETE by the client's digest", del, http.StatusAccepted)
	if rows, err := e.st.FindBlobMap(t.Context(), e.raw.ID, "dist/app.txt", d, index.BlobMapManifest); err != nil || len(rows) != 0 {
		t.Errorf("the manifest's row after the delete: %+v, %v", rows, err)
	}
}

// TestEncryptedReferrer: a referrer pushed by digest is stored with its subject's stored digest
// and listed under the client's digests; one under cosign's legacy tag is refused.
func TestEncryptedReferrer(t *testing.T) {
	e := newEnv(t, envOptions{encrypted: true})
	f := rawFileOf("dist/app.txt", plaintextMarker+" signed").at(e.clock.Now())
	e.uploadEncrypted("raw/assets/dist/app.txt", f.content, 0)
	body, subject := f.body(t)
	wantStatus(t, "PUT manifest", e.put(f, f.tag(), body), http.StatusCreated)

	sig := []byte(`{"critical":{"image":{"docker-manifest-digest":"` + subject.String() + `"}}}`)
	sd := e.uploadEncrypted("raw/assets/dist/app.txt", sig, 0)
	ref := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: "application/vnd.dev.cosign.artifact.sig.v1+json", Config: ocispec.DescriptorEmptyJSON,
		Layers:  []ocispec.Descriptor{{MediaType: "application/vnd.dev.cosign.simplesigning.v1+json", Digest: sd, Size: int64(len(sig))}},
		Subject: &ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: subject, Size: int64(len(body))}}
	rb, err := json.Marshal(ref)
	must(t, err)
	rd := digest.FromBytes(rb)
	rr := e.do(e.h, http.MethodPut, "/v2/raw/assets/dist/app.txt/manifests/"+rd.String(), bytes.NewReader(rb),
		"Content-Type", ocispecManifest, principalHeader, "alice")
	wantStatus(t, "PUT referrer", rr, http.StatusCreated)
	wantHeader(t, "PUT referrer", rr, "OCI-Subject", subject.String())
	list := e.do(e.h, http.MethodGet, "/v2/raw/assets/dist/app.txt/referrers/"+subject.String(), nil, principalHeader, "alice")
	var idx ocispec.Index
	must(t, json.Unmarshal(list.Body.Bytes(), &idx))
	if len(idx.Manifests) != 1 || idx.Manifests[0].Digest != rd || idx.Manifests[0].Size != int64(len(rb)) {
		t.Errorf("referrers = %+v, want the client's %s", idx.Manifests, rd)
	}
	legacy := e.do(e.h, http.MethodPut, "/v2/raw/assets/dist/app.txt/manifests/sha256-"+subject.Encoded()+".sig", bytes.NewReader(rb),
		"Content-Type", ocispecManifest, principalHeader, "alice")
	wantError(t, "a legacy cosign tag", legacy, http.StatusBadRequest, "MANIFEST_INVALID")
}

// TestEncryptedWithoutKeys: an encrypted repo on a server without the master key takes no upload.
func TestEncryptedWithoutKeys(t *testing.T) {
	e := newEnv(t, envOptions{encrypted: true})
	e.enc = nil
	e.h = e.replica()
	rr := e.do(e.h, http.MethodPost, "/v2/raw/assets/dist/app.txt/blobs/uploads/", nil, principalHeader, "alice")
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("POST without the keys = %d %s, want 500", rr.Code, rr.Body)
	}
}

// cipherStream is the fake that serves the blob stored as the ciphertext of a pattern of size
// bytes, encrypted with opts: the download of a large blob the fake discarded on upload.
type cipherStream struct {
	*providertest.Fake
	mu     sync.Mutex
	stored digest.Digest
	opts   blobcrypt.Private
	size   int64
}

func (c *cipherStream) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	c.mu.Lock()
	stored, opts := c.stored, c.opts
	c.mu.Unlock()
	if d != stored {
		return c.Fake.OpenBlob(ctx, name, d, rangeHeader, redirect)
	}
	s, err := blobcrypt.At(opts, 0)
	if err != nil {
		return provider.Blob{}, err
	}
	return provider.Blob{Status: http.StatusOK, Body: io.NopCloser(blobcrypt.EncryptReader(newPattern(c.size), s)), Size: c.size, Digest: d}, nil
}

// TestEncryptedStreamingMemory uploads a 1 GiB blob into an encrypted repo through POST and PUT
// and downloads it decrypted through GET, over real connections, and checks that the heap in use
// stays under heapLimit: the cipher streams both ways and buffers nothing.
func TestEncryptedStreamingMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 2 GiB through the cipher; skipped under -short")
	}
	const size = 1 << 30
	d := patternDigest(size)
	var cs *cipherStream
	e := newEnv(t, envOptions{encrypted: true, registry: func(f *providertest.Fake) provider.Provider {
		cs = &cipherStream{Fake: f, size: size}
		return cs
	}})
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	peak := sampleHeap(t)
	ctx := t.Context()
	base := srv.URL + "/v2/raw/assets/big/model.bin/blobs"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/uploads/", nil)
	req.Header.Set(principalHeader, "alice")
	hdr := send(t, req, http.StatusAccepted)
	req, _ = http.NewRequestWithContext(ctx, http.MethodPut, srv.URL+hdr.Get("Location")+"?digest="+d.String(), newPattern(size))
	req.ContentLength = size
	req.Header.Set(principalHeader, "alice")
	send(t, req, http.StatusCreated)

	rows, err := e.st.FindBlobMap(ctx, e.raw.ID, "big/model.bin", d, index.BlobMapBlob)
	must(t, err)
	if len(rows) != 1 || rows[0].Size != size {
		t.Fatalf("blob_map %+v", rows)
	}
	opts, err := e.enc.Unwrap(ctx, e.raw, rows[0].KeyFingerprint, rows[0].Wrapped)
	must(t, err)
	cs.mu.Lock()
	cs.stored, cs.opts = rows[0].Stored, opts
	cs.mu.Unlock()

	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+d.String(), nil)
	req.Header.Set(principalHeader, "alice")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != size {
		t.Fatalf("GET: status %d, Content-Length %d", resp.StatusCode, resp.ContentLength)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	must(t, err)
	if n != size || digest.NewDigest(digest.SHA256, h) != d {
		t.Errorf("downloaded %d bytes with digest %s, want %d with %s", n, digest.NewDigest(digest.SHA256, h), size, d)
	}
	top := peak()
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > heapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, heapLimit>>20)
	}
}
