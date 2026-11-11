package npm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// fetcher serves the blobs of built.
func fetcher(t *testing.T, built *format.Built) format.BlobFetcher {
	t.Helper()
	blobs := map[digest.Digest][]byte{}
	for _, b := range built.Blobs {
		rc, err := b.Open(t.Context())
		must(t, err)
		data, err := io.ReadAll(rc)
		must(t, err)
		_ = rc.Close()
		blobs[b.Descriptor.Digest] = data
	}
	return func(_ context.Context, d digest.Digest) (io.ReadCloser, error) {
		b, ok := blobs[d]
		if !ok {
			return nil, fmt.Errorf("no blob %s", d)
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

// TestBuildValidate validates the manifests of buildCases: the entry of Validate is Build's.
func TestBuildValidate(t *testing.T) {
	for _, tc := range buildCases {
		built, _, _ := tc.build(t)
		entry, err := Plugin{}.Validate(t.Context(), repo, built.Manifest, fetcher(t, built))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if d := cmp.Diff(&built.Entry, entry); d != "" {
			t.Errorf("%s: (-Build +Validate)\n%s", tc.name, d)
		}
	}
}

// streamedOf is the upload of the tarball b and the version document doc as ingest hands a
// streamed tarball to Build: written to the inspector of Inspect a few bytes at a time and
// closed, without Open, with its size and sha256.
func streamedOf(t *testing.T, b []byte, doc string) format.Upload {
	t.Helper()
	in, err := Plugin{}.Inspect(repo, format.StreamUpload{Name: "x", File: format.UploadedFile{Name: "x.tgz", Size: -1}})
	must(t, err)
	for rest := b; len(rest) > 0; rest = rest[min(len(rest), 100):] {
		_, err := in.Inspector.Write(rest[:min(len(rest), 100)])
		must(t, err)
	}
	must(t, in.Inspector.Close())
	return format.Upload{Params: map[string]string{ParamUploadedBy: "user:alice", ParamDocument: doc}, Files: []format.UploadedFile{{
		Name: "x.tgz", Size: int64(len(b)), Inspector: in.Inspector, Checksums: map[string]string{"sha256": digest.FromBytes(b).Encoded()},
	}}}
}

// TestBuildStreamed: Build takes a tarball that streamed through the inspector of Inspect,
// without Open, and makes the entry it makes of the same tarball read whole, but for the tarball
// among the blobs; Validate takes the manifest when the fetch of the tarball is
// format.ErrStreamed, as ingest's is for the file of the upload, and not when the fetch fails
// otherwise; the manifest-confusion check is Build's then: another dependency in the document is
// refused there.
func TestBuildStreamed(t *testing.T) {
	ctx := t.Context()
	b, info := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Scripts: map[string]string{"install": "node x.js"}})
	built, err := Plugin{}.Build(ctx, repo, streamedOf(t, b, document(t, info, nil)))
	must(t, err)
	whole, err := Plugin{}.Build(ctx, repo, upload(b, document(t, info, nil)))
	must(t, err)
	if d := cmp.Diff(whole.Entry, built.Entry); d != "" || len(built.Blobs) != 2 || built.Manifest.Layers[0].Digest != digest.FromBytes(b) {
		t.Fatalf("%d blobs, the tarball layer %s; the entry (-read whole +streamed)\n%s", len(built.Blobs), built.Manifest.Layers[0].Digest, d)
	}
	served := fetcher(t, built)
	for _, tt := range []struct {
		err error
		ok  bool
	}{{fmt.Errorf("ingest: committed after Validate: %w", format.ErrStreamed), true}, {errors.New("connection reset"), false}} {
		fetch := func(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
			if d == built.Manifest.Layers[0].Digest {
				return nil, tt.err
			}
			return served(ctx, d)
		}
		entry, err := Plugin{}.Validate(ctx, repo, built.Manifest, fetch)
		switch {
		case (err == nil) != tt.ok:
			t.Errorf("Validate with the fetch of the tarball %v: %v", tt.err, err)
		case err == nil:
			if d := cmp.Diff(&built.Entry, entry); d != "" {
				t.Errorf("(-Build +Validate)\n%s", d)
			}
		}
	}
	confused := document(t, info, func(doc map[string]any) { doc["dependencies"] = map[string]any{"evil": "*"} })
	if _, err := (Plugin{}).Build(ctx, repo, streamedOf(t, b, confused)); !errors.Is(err, format.ErrInvalid) {
		t.Errorf("Build of a document with another dependency: %v, want ErrInvalid", err)
	}
}

// TestValidateRefuses changes one thing of a built manifest or of its metadata layer at a time:
// each disagreement is ErrInvalid.
func TestValidateRefuses(t *testing.T) {
	ctx := t.Context()
	b, info := testutil.NpmTarball(testutil.NpmOptions{Name: "@acme/widgets", Scripts: map[string]string{"install": "node x.js"}})
	for _, tc := range []struct {
		name     string
		manifest func(m *ocispec.Manifest)
		metadata func(md map[string]any)
	}{
		{name: "a variant", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationVariant] = "linux" }},
		{name: "an npm annotation of the manifest", manifest: func(m *ocispec.Manifest) { m.Annotations["lajstrom.npm.seq"] = "1" }},
		{name: "a version with build metadata", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationVersion] = "1.0.0+x" }},
		{name: "another name", manifest: func(m *ocispec.Manifest) { m.Annotations[profile.AnnotationName] = "@acme/other" }},
		{name: "another title", manifest: func(m *ocispec.Manifest) { m.Layers[0].Annotations[ocispec.AnnotationTitle] = "x.tgz" }},
		{name: "another integrity", manifest: func(m *ocispec.Manifest) { m.Layers[0].Annotations[profile.AnnotationNpmIntegrity] = "sha512-x" }},
		{name: "no shasum", manifest: func(m *ocispec.Manifest) { delete(m.Layers[0].Annotations, profile.AnnotationNpmShasum) }},
		{name: "metadata of another name", metadata: func(md map[string]any) { md["name"] = "widgets" }},
		{name: "metadata of another size", metadata: func(md map[string]any) { md["size"] = 1 }},
		{name: "metadata of another integrity", metadata: func(md map[string]any) { md["integrity"] = "sha512-x" }},
		{name: "metadata of another sha256", metadata: func(md map[string]any) { md["sha256"] = strings.Repeat("0", 64) }},
		{name: "metadata that hides the install script", metadata: func(md map[string]any) { md["hasInstallScript"] = false }},
		{name: "metadata that says a shrinkwrap", metadata: func(md map[string]any) { md["hasShrinkwrap"] = true }},
		{name: "a manifest without the install script", metadata: func(md map[string]any) { delete(md["manifest"].(map[string]any), "scripts") }},
		{name: "a manifest of another version", metadata: func(md map[string]any) { md["manifest"].(map[string]any)["version"] = "9.0.0" }},
		{name: "no manifest", metadata: func(md map[string]any) { delete(md, "manifest") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built, err := Plugin{}.Build(ctx, repo, upload(b, document(t, info, nil)))
			must(t, err)
			if tc.metadata != nil {
				var md map[string]any
				must(t, json.Unmarshal(built.Entry.Metadata, &md))
				tc.metadata(md)
				raw, err := json.Marshal(md)
				must(t, err)
				desc := ocispec.Descriptor{MediaType: built.Manifest.Layers[1].MediaType, Digest: digest.FromBytes(raw), Size: int64(len(raw)),
					Annotations: built.Manifest.Layers[1].Annotations}
				built.Manifest.Layers[1] = desc
				built.Blobs[2] = bytesBlob(desc, raw)
			}
			if tc.manifest != nil {
				tc.manifest(&built.Manifest)
			}
			if _, err := (Plugin{}).Validate(ctx, repo, built.Manifest, fetcher(t, built)); !errors.Is(err, format.ErrInvalid) {
				t.Errorf("Validate: %v, want ErrInvalid", err)
			}
		})
	}
}
