package terraform

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format/formattest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// largeModule writes to w a module archive with one file of size bytes of formattest.Pattern,
// gzip-compressed without compression, so that it is fast to make and as large as it says.
func largeModule(w io.WriteCloser, size int64) error {
	gz, err := gzip.NewWriterLevel(w, gzip.NoCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "data.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: size}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, io.LimitReader(&formattest.Pattern{}, size)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return w.Close()
}

// TestPublishStreams publishes a module archive of 256 MiB through the publish route over a real
// connection, without a temporary directory, and checks that the heap in use stays under
// formattest.HeapLimit: the inspector reads the archive as it passes, and the provider holds one
// chunk (ADR 0014, INV-25).
func TestPublishStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 256 MiB; skipped under -short")
	}
	formattest.NoTempDir(t)
	e := newEnv(t)
	e.prov.ChunkSize = 8 << 20
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(largeModule(pw, 256<<20)) }()
	peak := formattest.SampleHeap(t)
	h := sha256.New()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/terraform/infra/big/aws/1.0.0", io.TeeReader(pr, h))
	must(t, err)
	req.Header.Set("X-Test-User", "alice")
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT = %d %s", resp.StatusCode, b)
	}
	top := peak()
	files, err := e.st.FindFiles(t.Context(), e.repo.ID, "big-aws-1.0.0.tar.gz")
	must(t, err)
	if d := digest.NewDigest(digest.SHA256, h); len(files) != 1 || files[0].File.Digest != d {
		t.Errorf("files %+v, want one with %s", files, d)
	}
	t.Logf("peak heap in use: %d MiB", top>>20)
	if top > formattest.HeapLimit {
		t.Errorf("peak heap in use %d MiB, want under %d MiB", top>>20, formattest.HeapLimit>>20)
	}
}

// TestPublishRefusedStoresNothing: a publish whose archive the inspector refuses, or whose tag
// the registry holds with another manifest, leaves no blob, no open session, no manifest and no
// row.
func TestPublishRefusedStoresNothing(t *testing.T) {
	module, _ := testutil.TerraformModule(testutil.TerraformOptions{})
	escaping := handTarGz(t, tarEntry{name: "main.tf", typ: tar.TypeReg, body: strings.Repeat("x", 4000)},
		tarEntry{name: "../evil.tf", typ: tar.TypeReg, body: "x"})
	for _, tt := range []struct {
		name   string
		body   []byte
		status int
	}{
		{"not gzip", []byte(strings.Repeat("not gzip ", 200)), http.StatusBadRequest},
		{"an entry out of the archive", escaping, http.StatusBadRequest},
		{"cut short", module[:len(module)-8], http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.prov.ChunkSize = 64
			if rr := e.publish("vpc/aws", "1.0.0", tt.body); rr.Code != tt.status {
				t.Fatalf("PUT = %d %s, want %d", rr.Code, rr.Body, tt.status)
			}
			rr, err := e.prov.Repository(t.Context(), "terraform/infra/vpc/aws")
			must(t, err)
			stored, err := rr.Exists(t.Context(), ocispec.Descriptor{Digest: digest.FromBytes(tt.body), Size: int64(len(tt.body))})
			must(t, err)
			config, err := rr.Exists(t.Context(), profile.EmptyConfig)
			must(t, err)
			pkgs, err := e.st.ListPackages(t.Context(), e.repo.ID, "")
			must(t, err)
			if stored || config || e.prov.OpenUploads() != 0 || len(e.prov.PushReferences()) != 0 || len(pkgs) != 0 {
				t.Errorf("archive stored %v, config %v, %d sessions open, %d manifests pushed, %d packages; want nothing",
					stored, config, e.prov.OpenUploads(), len(e.prov.PushReferences()), len(pkgs))
			}
		})
	}
}
