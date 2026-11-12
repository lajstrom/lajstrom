package rpm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

var el9 = index.Repo{Format: Name, Name: "el9"}

// TestBuild builds the manifest of a binary and of a source package with an epoch: the
// annotations, the tag, the repository path, the file layer titled with the file name, and the
// metadata layer, which is what Parse finds in the file.
func TestBuild(t *testing.T) {
	for _, c := range []struct {
		opts             testutil.RPMOptions
		version, variant string
		tag              string
	}{
		{testutil.RPMOptions{}, "1.0.0-1.el9", "x86_64", "1.0.0-1.el9__x86_5f64"},
		{testutil.RPMOptions{Epoch: "2", Source: true}, "2:1.0.0-1.el9", "src", "2_3a1.0.0-1.el9__src"},
		{testutil.RPMOptions{Name: "libstdc++", Arch: "noarch", Epoch: "0"}, "1.0.0-1.el9", "noarch", "1.0.0-1.el9__noarch"},
	} {
		b, info := testutil.RPM(c.opts)
		built, err := Plugin{}.Build(t.Context(), el9, format.Upload{Files: []format.UploadedFile{rpmFile(b)},
			Params: map[string]string{ParamUploadedBy: "user:alice"}})
		if err != nil {
			t.Fatal(err)
		}
		checkBuilt(t, built, info, c.version, c.variant, c.tag)
		pkg, err := Parse(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		want, err := pkg.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(built.Entry.Metadata, want) {
			t.Errorf("metadata layer\n%s\nwant\n%s", built.Entry.Metadata, want)
		}
		if _, err := (Plugin{}).Validate(t.Context(), el9, built.Manifest, fetcherOf(built, b)); err != nil {
			t.Errorf("Validate of what Build made: %v", err)
		}
	}
}

// checkBuilt checks the annotations, tag, repository path and file layer of what Build made of
// the package of info.
func checkBuilt(t *testing.T, built *format.Built, info testutil.RPMInfo, version, variant, tag string) {
	t.Helper()
	a := built.Manifest.Annotations
	switch {
	case a[profile.AnnotationName] != info.Name || a[profile.AnnotationVersion] != version || a[profile.AnnotationVariant] != variant:
		t.Errorf("annotations %v", a)
	case a[profile.AnnotationUploadedBy] != "user:alice" || a[profile.AnnotationFormat] != "rpm":
		t.Errorf("annotations %v", a)
	case built.Tag != tag || built.RepoPath != "rpm/el9/"+profile.EscapeComponent(info.Name):
		t.Errorf("tag %q, repo path %q", built.Tag, built.RepoPath)
	case built.Manifest.ArtifactType != "application/vnd.lajstrom.rpm.v1" || built.Manifest.Layers[0].MediaType != profile.MediaTypeRpmPackage:
		t.Errorf("artifactType %q, layer %q", built.Manifest.ArtifactType, built.Manifest.Layers[0].MediaType)
	case built.Manifest.Layers[0].Annotations[ocispec.AnnotationTitle] != info.Filename:
		t.Errorf("title %q, want %q", built.Manifest.Layers[0].Annotations[ocispec.AnnotationTitle], info.Filename)
	case built.Manifest.Layers[0].Digest.Encoded() != info.SHA256 || built.Manifest.Layers[0].Size != info.Size:
		t.Errorf("file layer %+v", built.Manifest.Layers[0])
	}
}

// TestBuildRefuses refuses two files, a file that is not an rpm, and a size that is not the
// file's.
func TestBuildRefuses(t *testing.T) {
	b, _ := testutil.RPM(testutil.RPMOptions{})
	short := rpmFile(b)
	short.Size--
	for name, files := range map[string][]format.UploadedFile{
		"two files": {rpmFile(b), rpmFile(b)},
		"not an rpm": {rpmFile([]byte("not a package, not even close to one, and longer than the lead of an rpm file: " +
			strings.Repeat("x", 100)))},
		"size": {short},
	} {
		if _, err := (Plugin{}).Build(t.Context(), el9, format.Upload{Files: files}); !errors.Is(err, format.ErrInvalid) {
			t.Errorf("%s: Build = %v, want ErrInvalid", name, err)
		}
	}
}

// TestValidate refuses manifests whose annotations, metadata layer and package disagree, one
// way each, with an error that wraps format.ErrInvalid and names the disagreement; a failure to
// fetch is not ErrInvalid; the file of a streamed upload is not read.
func TestValidate(t *testing.T) {
	b, _ := testutil.RPM(testutil.RPMOptions{})
	built, err := Plugin{}.Build(t.Context(), el9, format.Upload{Files: []format.UploadedFile{rpmFile(b)}})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := testutil.RPM(testutil.RPMOptions{Summary: "another summary"})
	for _, c := range []struct {
		name  string
		edit  func(m *ocispec.Manifest, md map[string]any)
		file  []byte
		want  string
		valid bool
	}{
		{name: "as built", valid: true},
		{name: "title", edit: func(m *ocispec.Manifest, _ map[string]any) {
			m.Layers[0].Annotations[ocispec.AnnotationTitle] = "x.rpm"
		}, want: "titled"},
		{name: "epoch 0", edit: func(m *ocispec.Manifest, md map[string]any) { setVersion(m, md, "0:1.0.0-1.el9") }, want: "epoch"},
		{name: "variant", edit: func(m *ocispec.Manifest, _ map[string]any) { m.Annotations[profile.AnnotationVariant] = "x86-64" }, want: "rpm arch"},
		{name: "name", edit: func(_ *ocispec.Manifest, md map[string]any) { md["name"] = "other" }, want: "metadata name"},
		{name: "unknown key", edit: func(_ *ocispec.Manifest, md map[string]any) { md["color"] = "blue" }, want: "unknown field"},
		{name: "summary", edit: func(_ *ocispec.Manifest, md map[string]any) { md["summary"] = "x" }, want: "metadata summary is not what the header"},
		{name: "files", edit: func(_ *ocispec.Manifest, md map[string]any) { md["files"] = []any{} }, want: "metadata files"},
		{name: "size", edit: func(_ *ocispec.Manifest, md map[string]any) { md["size"] = 1 }, want: "metadata size"},
		{name: "another package", file: other, want: "metadata summary is not what the header"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, md := cloneBuilt(t, built)
			if c.edit != nil {
				c.edit(&m, md)
			}
			file := b
			if c.file != nil {
				file = c.file
			}
			fetch := fetcherWith(t, &m, md, file)
			_, err := (Plugin{}).Validate(t.Context(), el9, m, fetch)
			switch {
			case c.valid && err != nil:
				t.Fatal(err)
			case !c.valid && (!errors.Is(err, format.ErrInvalid) || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("Validate = %v, want ErrInvalid with %q", err, c.want)
			}
		})
	}
	t.Run("fetch fails", func(t *testing.T) {
		boom := errors.New("boom")
		_, err := (Plugin{}).Validate(t.Context(), el9, built.Manifest, func(context.Context, digest.Digest) (io.ReadCloser, error) { return nil, boom })
		if !errors.Is(err, boom) || errors.Is(err, format.ErrInvalid) {
			t.Fatalf("Validate = %v, want boom", err)
		}
	})
	t.Run("streamed", func(t *testing.T) {
		fetch := fetcherOf(built, b)
		streamed := func(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
			if d == built.Manifest.Layers[0].Digest {
				return nil, format.ErrStreamed
			}
			return fetch(ctx, d)
		}
		if _, err := (Plugin{}).Validate(t.Context(), el9, built.Manifest, streamed); err != nil {
			t.Fatal(err)
		}
	})
}

// setVersion sets lajstrom.version and the parts of the metadata to v.
func setVersion(m *ocispec.Manifest, md map[string]any, v string) {
	m.Annotations[profile.AnnotationVersion] = v
	e, ver, rel := SplitEVR(v)
	md["epoch"], md["version"], md["release"] = e, ver, rel
}

// cloneBuilt returns a copy of the manifest of built, with copies of its annotation maps, and its
// metadata layer decoded.
func cloneBuilt(t *testing.T, built *format.Built) (ocispec.Manifest, map[string]any) {
	t.Helper()
	m := built.Manifest
	m.Annotations = clone(m.Annotations)
	m.Layers = append([]ocispec.Descriptor{}, m.Layers...)
	for i := range m.Layers {
		m.Layers[i].Annotations = clone(m.Layers[i].Annotations)
	}
	var md map[string]any
	must(t, json.Unmarshal(built.Entry.Metadata, &md))
	return m, md
}

func clone(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// fetcherWith encodes md as the metadata layer of m, the last layer, and returns a fetcher of
// it and of file under the digest of the file layer of m.
func fetcherWith(t *testing.T, m *ocispec.Manifest, md map[string]any, file []byte) format.BlobFetcher {
	t.Helper()
	b, err := json.Marshal(md)
	must(t, err)
	l := &m.Layers[len(m.Layers)-1]
	l.Digest, l.Size = digest.FromBytes(b), int64(len(b))
	blobs := map[digest.Digest][]byte{l.Digest: b, m.Layers[0].Digest: file}
	return func(_ context.Context, d digest.Digest) (io.ReadCloser, error) {
		if b, ok := blobs[d]; ok {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
		return nil, fmt.Errorf("no blob %s", d)
	}
}

// fetcherOf is a fetcher of the blobs of built, the file b among them.
func fetcherOf(built *format.Built, b []byte) format.BlobFetcher {
	return func(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
		if d == built.Manifest.Layers[0].Digest {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
		for _, s := range built.Blobs {
			if s.Descriptor.Digest == d {
				return s.Open(ctx)
			}
		}
		return nil, fmt.Errorf("no blob %s", d)
	}
}
