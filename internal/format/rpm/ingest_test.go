package rpm

import (
	"bytes"
	"testing"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/testutil"
)

// TestIngest publishes packages through the real pipeline, Build then Validate, and checks the
// rows: the package by its NAME at its package path, the version as [E:]V-R with the arch as its
// variant, the tag, the file by its file name with the layer digest, and the metadata layer in
// the index. The same bytes again are the idempotent repeat; a delete takes the version.
func TestIngest(t *testing.T) {
	p := newPipeline(t)
	ctx := t.Context()
	hello, info := testutil.RPM(testutil.RPMOptions{Name: "libstdc++", Epoch: "3"})
	pub := p.upload(hello)
	if pub.Tag != "3_3a1.0.0-1.el9__x86_5f64" || pub.RepoPath != "rpm/el9/libstdc_2b_2b" || pub.Existed {
		t.Errorf("published %+v", pub)
	}
	pkg, err := p.st.GetPackage(ctx, p.repo.ID, "libstdc++")
	must(t, err)
	if pkg.NameOCI != "libstdc_2b_2b" {
		t.Errorf("package path %q", pkg.NameOCI)
	}
	versions, err := p.st.ListVersions(ctx, pkg.ID)
	must(t, err)
	if len(versions) != 1 || versions[0].Version != "3:1.0.0-1.el9" || versions[0].Variant != "x86_64" || versions[0].Digest != pub.Digest {
		t.Fatalf("versions %+v", versions)
	}
	checkIndexed(t, p, hello, info, versions[0].Metadata)
	if again := p.upload(hello); !again.Existed || again.Digest != pub.Digest {
		t.Errorf("the repeat = %+v", again)
	}
	if _, err := p.pipe.Delete(ctx, p.repo, format.Deletion{Path: pkg.NameOCI, Reference: pub.Tag}, alice); err != nil {
		t.Fatal(err)
	}
	if files, err := p.st.FindFiles(ctx, p.repo.ID, info.Filename); err != nil || len(files) != 0 {
		t.Errorf("after the delete, files called %s: %+v, %v", info.Filename, files, err)
	}
}

// checkIndexed checks that the index has the metadata md of the package b, and its file by the
// file name of info.
func checkIndexed(t *testing.T, p *pipeline, b []byte, info testutil.RPMInfo, md []byte) {
	t.Helper()
	parsed, err := Parse(bytes.NewReader(b))
	must(t, err)
	want, err := parsed.Metadata()
	must(t, err)
	if !bytes.Equal(md, want) {
		t.Errorf("the index has the metadata\n%s\nwant\n%s", md, want)
	}
	files, err := p.st.FindFiles(t.Context(), p.repo.ID, info.Filename)
	must(t, err)
	if len(files) != 1 || files[0].File.Digest.Encoded() != info.SHA256 || files[0].File.Size != info.Size {
		t.Errorf("files called %s: %+v", info.Filename, files)
	}
}
