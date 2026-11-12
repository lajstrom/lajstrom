package proxy_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/proxy"
)

// TestOCITee: the request that holds a blob's claim reads the upstream's bytes as they go into
// the backend, before the copy ends; a request that joins the fetch waits for it and is served
// from the backend (docs/spec/proxy-and-group.md §11.8).
func TestOCITee(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	layer := img.blobs[0][1]
	gate := make(chan struct{})
	e.hub.gates[layer.Digest] = gate

	desc, rc, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("OCIBlob with stream: %v, %v; want the tee", rc, err)
	}
	defer func() { _ = rc.Close() }()
	half := make([]byte, layer.Size/2)
	if _, err := io.ReadFull(rc, half); err != nil {
		t.Fatalf("the first half while the upstream holds the rest: %v", err)
	}
	joined := make(chan error, 1)
	go func() {
		_, rc, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, true)
		if rc != nil {
			t.Error("a request that joined the fetch got a tee")
		}
		joined <- err
	}()
	select {
	case err := <-joined:
		t.Fatalf("the joined request ended before the copy: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	rest, err := io.ReadAll(rc)
	if err != nil || digest.FromBytes(append(half, rest...)) != layer.Digest || int64(len(half)+len(rest)) != desc.Size {
		t.Fatalf("the tee: %d bytes, %v; want the blob", len(half)+len(rest), err)
	}
	must(t, <-joined)
	if ok, err := e.hub.blobHeld(t, e, "library/nginx", layer.Digest); !ok || err != nil {
		t.Errorf("the backend does not hold the blob after the tee: %v", err)
	}
}

// TestOCITeeRefused: bytes that are not the blob reach the claim holder as an error, never with
// their last byte, and the backend holds nothing.
func TestOCITeeRefused(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	layer := img.blobs[0][1]
	e.hub.mu.Lock()
	b := e.hub.blobs["library/nginx"][layer.Digest]
	e.hub.blobs["library/nginx"][layer.Digest] = append(bytes.Clone(b[:len(b)-1]), 'X')
	e.hub.mu.Unlock()
	_, rc, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("OCIBlob with stream: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err == nil || int64(len(got)) >= layer.Size {
		t.Fatalf("the tee of other bytes: %d of %d bytes, %v; want an error before the last byte", len(got), layer.Size, err)
	}
	if ok, _ := e.hub.blobHeld(t, e, "library/nginx", layer.Digest); ok {
		t.Error("the backend holds the refused bytes")
	}
}

// TestOCITeeClientLeaves: a claim holder that leaves halfway does not stop the copy: the backend
// holds the blob once the upstream sent it.
func TestOCITeeClientLeaves(t *testing.T) {
	e := newOCIEnv(t, ociSettings)
	img := e.hub.image(t, "library/nginx", "1.27", "amd64")
	c := e.core("a", proxy.Options{})
	_, _, err := c.OCIManifest(t.Context(), e.repo, "library/nginx", img.manifests[0].Digest.String())
	must(t, err)
	layer := img.blobs[0][1]
	gate := make(chan struct{})
	e.hub.gates[layer.Digest] = gate
	_, rc, err := c.OCIBlob(t.Context(), e.repo, "library/nginx", layer.Digest, true, true)
	if err != nil || rc == nil {
		t.Fatalf("OCIBlob with stream: %v", err)
	}
	if _, err := io.ReadFull(rc, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	close(gate)
	for range 100 {
		if ok, _ := e.hub.blobHeld(t, e, "library/nginx", layer.Digest); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the backend does not hold the blob after the claim holder left")
}

// blobHeld reports whether the env's backend holds d at path.
func (h *hub) blobHeld(t *testing.T, e *ociEnv, path string, d digest.Digest) (bool, error) {
	t.Helper()
	_, err := e.reg.StatBlob(t.Context(), "oci/hub/"+path, d)
	return err == nil, err
}
