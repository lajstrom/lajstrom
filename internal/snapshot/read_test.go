package snapshot

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/format/raw"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestRestoreRoundTrip is the disaster recovery of docs/operations.md: a control plane with a
// published file is snapshotted into the registry; the database is lost; a new one is migrated,
// the latest snapshot restored into it and reindex run. Then the file is indexed again, and the
// user's password, her PAT and the deploy token authenticate as the same principals, with the
// same bindings; the repo's signing key is the same.
func TestRestoreRoundTrip(t *testing.T) {
	ctx := t.Context()
	m := master(t, testMasterHex)
	fake := &providertest.Fake{RegistryName: "local"}
	providers := provider.Map{"local": fake}
	src := newStore(t)
	s := filled(t, src, m)
	repo := index.Repo{Format: "raw", Name: "assets", Registry: "local", Prefix: "raw/assets", Kind: index.KindHosted}
	must(t, src.CreateRepo(ctx, &repo))
	plugins := func(name string) (format.Plugin, bool) { return raw.Plugin{}, name == raw.Name }
	pipe := ingest.New(ingest.Options{Providers: providers, Index: src, Plugins: plugins, Log: quiet})
	upload := format.Upload{Params: map[string]string{raw.ParamPath: "docs/readme.md"}, Files: []format.UploadedFile{{
		Name: "readme.md", Size: 5, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("hello")), nil }}}}
	if _, err := pipe.Upload(ctx, repo, upload, auth.Anonymous); err != nil {
		t.Fatal(err)
	}
	before := principals(t, src, s)
	keysBefore, err := src.ListKeys(ctx, nil)
	must(t, err)
	cp, err := src.ExportControlPlane(ctx, index.ExportOptions{})
	must(t, err)
	reg := Registry{Provider: fake, Master: m}
	written, err := reg.Write(ctx, cp, true, "test")
	must(t, err)

	dst := newStore(t)
	got, info, err := reg.Read(ctx, Latest)
	must(t, err)
	if info != written {
		t.Errorf("Read(latest) = %+v, want the snapshot written, %+v", info, written)
	}
	must(t, dst.ImportControlPlane(ctx, got, index.ImportOptions{}))
	w := reindex.New(reindex.Options{Index: dst, Providers: providers, Inspector: ingest.New(ingest.Options{Providers: providers,
		Index: dst, Plugins: plugins, Log: quiet}), Log: quiet})
	req := reindex.Request{Prune: true, Source: "cli", Principal: "anonymous"}
	run, err := w.Start(ctx, req)
	must(t, err)
	if _, err := w.Run(ctx, run, req); err != nil {
		t.Fatal(err)
	}

	pkg, err := dst.GetPackage(ctx, repo.ID, "docs/readme.md")
	must(t, err)
	if versions, err := dst.ListVersions(ctx, pkg.ID); err != nil || len(versions) != 1 {
		t.Errorf("the versions of docs/readme.md after reindex: %v, %v", versions, err)
	}
	if after := principals(t, dst, s); after != before {
		t.Errorf("the principals after the restore: %v, want %v", after, before)
	}
	keysAfter, err := dst.ListKeys(ctx, nil)
	must(t, err)
	if len(keysAfter) != len(keysBefore) || keysAfter[0].Fingerprint != keysBefore[0].Fingerprint {
		t.Errorf("the server keys after the restore: %v, want %v", keysAfter, keysBefore)
	}
}

// principals returns who the secrets of s authenticate as on st, and the bindings of each, as a
// string to compare: the password, the PAT, the deploy token.
func principals(t *testing.T, st index.Store, s secrets) string {
	t.Helper()
	ctx := t.Context()
	a := auth.NewAuthenticator(auth.Options{Index: st, Log: quiet})
	var out []string
	pw, _, err := a.AuthenticatePassword(ctx, "alice", string(s["password"]))
	must(t, err)
	for _, secret := range []string{string(s["pat"]), string(s["deploy token"])} {
		p, _, err := a.Authenticate(ctx, "alice", secret)
		must(t, err)
		out = append(out, p.String())
	}
	out = append(out, pw.String())
	bindings, err := st.ListBindings(ctx, pw.String())
	must(t, err)
	for _, b := range bindings {
		out = append(out, b.Role+" "+b.RepoMask)
	}
	return strings.Join(out, ", ")
}

// TestReadRefuses: a reference the registry does not have, a sealed snapshot without the master
// key or with another, a document of a newer version and one whose schema is not its manifest's
// are refused.
func TestReadRefuses(t *testing.T) {
	ctx := t.Context()
	fake := &providertest.Fake{RegistryName: "local"}
	m := master(t, testMasterHex)
	if _, _, err := (Registry{Provider: fake}).Read(ctx, Latest); !errors.Is(err, ErrNotFound) {
		t.Errorf("latest of a registry without snapshots: %v, want ErrNotFound", err)
	}
	cp := &index.ControlPlane{Schema: 21, ReadAt: time.Now().UTC()}
	sealed, err := (Registry{Provider: fake, Master: m}).Write(ctx, cp, true, "v1")
	must(t, err)
	for _, tt := range []struct {
		name string
		reg  Registry
		ref  string
		want error
		text string
	}{
		{"a tag it does not have", Registry{Provider: fake, Master: m}, "20200101T000000Z-000000000000", ErrNotFound, "no such snapshot"},
		{"not a reference", Registry{Provider: fake, Master: m}, "v1.2", ErrNotFound, "neither a snapshot's tag"},
		{"a digest it does not have", Registry{Provider: fake, Master: m}, digest.FromString("x").String(), ErrNotFound, ""},
		{"sealed, no master key", Registry{Provider: fake}, sealed.Tag, ErrNoMasterKey, ""},
		{"sealed, another master key", Registry{Provider: fake, Master: master(t, otherMasterHex)}, sealed.Tag, nil,
			"does not open with the master key"},
	} {
		_, _, err := tt.reg.Read(ctx, tt.ref)
		if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) || !strings.Contains(err.Error(), tt.text) {
			t.Errorf("%s: %v, want %v %q", tt.name, err, tt.want, tt.text)
		}
	}
	if _, info, err := (Registry{Provider: fake, Master: m}).Read(ctx, sealed.Digest.String()); err != nil || info.Tag != "" ||
		info.Digest != sealed.Digest {
		t.Errorf("Read by digest: %+v, %v", info, err)
	}

	push := func(version, schema int, doc string) string {
		t.Helper()
		gz := gzipped(t, doc)
		layer := ocispec.Descriptor{MediaType: profile.MediaTypeSnapshotDocument, Digest: digest.FromBytes(gz), Size: int64(len(gz))}
		mf, err := profile.BuildSnapshot(profile.Snapshot{Version: version, Schema: schema, Created: time.Now(), Layer: layer})
		must(t, err)
		raw, d, err := profile.Encode(mf)
		must(t, err)
		rr, err := fake.Repository(ctx, profile.SnapshotRepository)
		must(t, err)
		must(t, rr.Push(ctx, mf.Layers[0], bytes.NewReader(gz)))
		tag := profile.SnapshotTag(time.Now(), d)
		must(t, rr.PushReference(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(raw))},
			bytes.NewReader(raw), tag))
		return tag
	}
	reg := Registry{Provider: fake}
	if _, _, err := reg.Read(ctx, push(2, 21, `{"version": 2, "schema": 21}`)); !errors.Is(err, ErrVersion) {
		t.Errorf("a snapshot of version 2: %v, want ErrVersion", err)
	}
	if _, _, err := reg.Read(ctx, push(1, 22, `{"version": 1, "schema": 21}`)); err == nil || !strings.Contains(err.Error(), "its manifest says 22") {
		t.Errorf("a document of another schema than its manifest's: %v", err)
	}
	if _, _, err := reg.Read(ctx, push(1, 21, `{"version": 1, "schema": 21, "repos": [{"id": 1, "format": "raw"}]}`)); err != nil {
		t.Errorf("a well-formed document pushed by hand: %v", err)
	}
}

// gzipped returns doc gzip'd.
func gzipped(t *testing.T, doc string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(doc)); err != nil {
		t.Fatal(err)
	}
	must(t, zw.Close())
	return buf.Bytes()
}
