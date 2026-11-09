package pypi

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// bigWheel is a wheel of about 900 KiB, its data stored, not compressed, so that its METADATA is
// at the end of a file larger than two blocks of the fetcher.
func bigWheel(t *testing.T) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "big/data.bin", Method: zip.Store})
	must(t, err)
	data := make([]byte, 900<<10)
	_, _ = rand.Read(data)
	_, err = w.Write(data)
	must(t, err)
	md, err := zw.Create("big-1.0.dist-info/METADATA")
	must(t, err)
	_, err = md.Write([]byte("Metadata-Version: 2.1\nName: big\nVersion: 1.0\nRequires-Dist: requests\n"))
	must(t, err)
	must(t, zw.Close())
	return "big-1.0-py3-none-any.whl", buf.Bytes()
}

// pushBuilt pushes the blobs of built to the env's fake registry, as a client does before PUT
// manifest, and returns the manifest's bytes.
func (e *env) pushBuilt(t *testing.T, m ocispec.Manifest, blobs []format.BlobSource) []byte {
	t.Helper()
	ctx := t.Context()
	rr, err := e.fake.Repository(ctx, "pypi/internal/big")
	must(t, err)
	for _, b := range blobs {
		if ok, _ := rr.Exists(ctx, b.Descriptor); ok {
			continue
		}
		rc, err := b.Open(ctx)
		must(t, err)
		must(t, rr.Push(ctx, b.Descriptor, rc))
		_ = rc.Close()
	}
	raw, _, err := profile.Encode(m)
	must(t, err)
	return raw
}

// lie rewrites the metadata layer of built so that it says the wheel has no dependencies: the
// fields and core_metadata agree with each other and with the annotations, and not with the
// METADATA inside the wheel.
func lie(t *testing.T, built *format.Built) {
	t.Helper()
	var md map[string]any
	must(t, json.Unmarshal(built.Entry.Metadata, &md))
	delete(md, "requires_dist")
	md[FieldCoreMetadata] = strings.Replace(md[FieldCoreMetadata].(string), "Requires-Dist: requests\n", "", 1)
	b, err := json.Marshal(md)
	must(t, err)
	desc := ocispec.Descriptor{MediaType: built.Manifest.Layers[1].MediaType, Digest: digest.FromBytes(b), Size: int64(len(b)),
		Annotations: built.Manifest.Layers[1].Annotations}
	built.Manifest.Layers[1] = desc
	built.Blobs[2] = bytesBlob(desc, b)
}

// TestPushedArchive is #161: a wheel pushed through the facade is checked against the METADATA
// inside it, read with range requests: the honest one is published and only parts of it were
// read; one whose metadata layer lies about its dependencies is ErrInvalid through the facade and
// through Inspect, the check reindex runs.
func TestPushedArchive(t *testing.T) {
	name, whl := bigWheel(t)
	t.Run("honest", func(t *testing.T) {
		ctx := t.Context()
		e := newEnv(t)
		built, err := Plugin{}.Build(ctx, e.repo, upload(name, whl))
		must(t, err)
		raw := e.pushBuilt(t, built.Manifest, built.Blobs)
		reads := len(e.fake.OpenBlobs())
		if _, err := e.pipe.Ingest(ctx, e.repo, "big", built.Tag, built.Manifest, raw, alice()); err != nil {
			t.Fatalf("the honest wheel through the facade: %v", err)
		}
		wheel := built.Manifest.Layers[0].Digest
		var ranged int64
		for _, r := range e.fake.OpenBlobs()[reads:] {
			if r.Digest != wheel {
				continue
			}
			var first, last int64
			if _, err := fmt.Sscanf(r.Range, "bytes=%d-%d", &first, &last); err != nil {
				t.Fatalf("a read of the wheel without a range: %+v", r)
			}
			ranged += last - first + 1
		}
		t.Logf("the check read %d bytes of the %d of the wheel", ranged, len(whl))
		if ranged == 0 || ranged > int64(len(whl))/4 {
			t.Errorf("the check read %d bytes of the %d of the wheel", ranged, len(whl))
		}
	})
	t.Run("lying", func(t *testing.T) {
		ctx := t.Context()
		e := newEnv(t)
		liar, err := Plugin{}.Build(ctx, e.repo, upload(name, whl))
		must(t, err)
		lie(t, liar)
		raw := e.pushBuilt(t, liar.Manifest, liar.Blobs)
		if _, err := e.pipe.Ingest(ctx, e.repo, "big", liar.Tag, liar.Manifest, raw, alice()); !errors.Is(err, format.ErrInvalid) ||
			!strings.Contains(err.Error(), "not the core metadata inside") {
			t.Errorf("a lying metadata layer through the facade: %v", err)
		}
		if _, err := e.pipe.Inspect(ctx, e.repo, "big", liar.Tag, liar.Manifest, raw, ingestInspect()); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("the same manifest met by reindex: %v", err)
		}
	})
}

// alice is the principal of the facade pushes of the tests.
func alice() auth.Principal { return auth.Principal{Kind: auth.KindUser, Name: "alice"} }

// ingestInspect are the options reindex inspects with.
func ingestInspect() ingest.InspectOptions { return ingest.InspectOptions{AllowDegraded: true} }
