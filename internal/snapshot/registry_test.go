package snapshot

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// otherMasterHex is a master key that is not testMasterHex.
var otherMasterHex = strings.Repeat("fedcba9876543210", 4)

// layerOf returns the bytes of the layer of the snapshot info in fake, as the registry has them.
func layerOf(t *testing.T, fake *providertest.Fake, info Info) (ocispec.Manifest, []byte) {
	t.Helper()
	ctx := t.Context()
	rr, err := fake.Repository(ctx, profile.SnapshotRepository)
	must(t, err)
	desc, err := rr.Resolve(ctx, info.Tag)
	must(t, err)
	raw, err := fetch(ctx, rr, desc)
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(raw, &m))
	layer, err := fetch(ctx, rr, m.Layers[0])
	must(t, err)
	return m, layer
}

// checkNoSecret fails the test when blob holds one of the secrets, as it is, in base64, in hex or
// in either case.
func checkNoSecret(t *testing.T, what string, blob []byte, s secrets) {
	t.Helper()
	for name, secret := range s {
		for _, form := range [][]byte{secret, []byte(base64.StdEncoding.EncodeToString(secret)),
			[]byte(base64.RawURLEncoding.EncodeToString(secret)), bytes.ToUpper(secret)} {
			if len(secret) > 0 && bytes.Contains(blob, form) {
				t.Errorf("%s holds the %s", what, name)
			}
		}
	}
}

// TestWriteHoldsNoSecret writes a control plane, unsealed and sealed, reads the layer back from
// the registry, and looks for every secret of it in the bytes and in the document: none is
// there. The document holds the hashes and the ciphertext instead, and the sealed layer opens
// with the master key and no other.
func TestWriteHoldsNoSecret(t *testing.T) {
	st, m := newStore(t), master(t, testMasterHex)
	s := filled(t, st, m)
	cp, err := st.ExportControlPlane(t.Context(), index.ExportOptions{WithAudit: true})
	must(t, err)
	fake := &providertest.Fake{RegistryName: "local"}
	reg := Registry{Provider: fake, Master: m}

	info, err := reg.Write(t.Context(), cp, false, "test")
	must(t, err)
	mf, layer := layerOf(t, fake, info)
	if mf.Layers[0].MediaType != profile.MediaTypeSnapshotDocument || info.Sealed {
		t.Errorf("an unsealed snapshot: layer %s, sealed %v", mf.Layers[0].MediaType, info.Sealed)
	}
	doc := gunzip(t, layer)
	checkNoSecret(t, "the unsealed layer", layer, s)
	checkNoSecret(t, "the unsealed document", doc, s)
	for _, want := range []string{`"passwordHash":"$argon2id$`, `"privateEnc":"`, `"hash":"`, `"uid":"`} {
		if !bytes.Contains(doc, []byte(want)) {
			t.Errorf("the document lacks %s", want)
		}
	}

	info, err = reg.Write(t.Context(), cp, true, "test")
	must(t, err)
	mf, layer = layerOf(t, fake, info)
	if mf.Layers[0].MediaType != profile.MediaTypeSnapshotSealed || !info.Sealed {
		t.Errorf("a sealed snapshot: layer %s, sealed %v", mf.Layers[0].MediaType, info.Sealed)
	}
	checkNoSecret(t, "the sealed layer", layer, s)
	if bytes.Contains(layer, []byte("argon2id")) || bytes.HasPrefix(layer, []byte{0x1f, 0x8b}) {
		t.Error("the sealed layer is readable without the master key")
	}
	plain, err := m.OpenSnapshot(layer, []byte(profile.MediaTypeSnapshotSealed))
	must(t, err)
	checkNoSecret(t, "the sealed document", gunzip(t, plain), s)
	if _, err := master(t, otherMasterHex).OpenSnapshot(layer, []byte(profile.MediaTypeSnapshotSealed)); err == nil {
		t.Error("the sealed layer opens with another master key")
	}
}

// TestWriteList: snapshots are listed the newest first, each with what its manifest says; the
// same snapshot written twice is one tag; a tag that names another manifest is a conflict, and
// nothing is moved.
func TestWriteList(t *testing.T) {
	ctx := t.Context()
	st := newStore(t)
	fake := &providertest.Fake{RegistryName: "local"}
	reg := Registry{Provider: fake}
	infos, err := reg.List(ctx)
	if err != nil || len(infos) != 0 {
		t.Fatalf("List of a registry without snapshots: %v, %v", infos, err)
	}
	cp, err := st.ExportControlPlane(ctx, index.ExportOptions{})
	must(t, err)
	first, err := reg.Write(ctx, cp, false, "v1")
	must(t, err)
	again, err := reg.Write(ctx, cp, false, "v1")
	if err != nil || again != first {
		t.Errorf("the same snapshot again: %+v, %v; want %+v", again, err, first)
	}
	later := *cp
	later.ReadAt = cp.ReadAt.Add(time.Second)
	second, err := reg.Write(ctx, &later, false, "v1")
	must(t, err)
	if !profile.IsSnapshotTag(first.Tag) || second.Tag <= first.Tag || first.Schema != cp.Schema || first.Version != Version {
		t.Errorf("the snapshots %+v and %+v", first, second)
	}
	infos, err = reg.List(ctx)
	must(t, err)
	if len(infos) != 2 || infos[0] != second || infos[1] != first {
		t.Errorf("List = %+v, want the second, then the first", infos)
	}

	if _, err := reg.Write(ctx, cp, true, "v1"); !errors.Is(err, ErrNoMasterKey) {
		t.Errorf("a sealed snapshot without the master key: %v, want ErrNoMasterKey", err)
	}
}

// TestWriteConflict: a tag that names another manifest than the snapshot's is a conflict, and
// nothing is moved.
func TestWriteConflict(t *testing.T) {
	ctx := t.Context()
	fake := &providertest.Fake{RegistryName: "local"}
	reg := Registry{Provider: fake}
	cp := &index.ControlPlane{Schema: 21, ReadAt: time.Now().UTC()}
	first, err := reg.Write(ctx, cp, false, "v1")
	must(t, err)
	later := *cp
	later.ReadAt = cp.ReadAt.Add(time.Second)
	second, err := reg.Write(ctx, &later, false, "v1")
	must(t, err)
	rr, err := fake.Repository(ctx, profile.SnapshotRepository)
	must(t, err)
	other, err := rr.Resolve(ctx, second.Tag)
	must(t, err)
	must(t, rr.Tag(ctx, other, first.Tag)) // the registry moved the tag, as Lajstrom never does
	if _, err := reg.Write(ctx, cp, false, "v1"); !errors.Is(err, ErrConflict) {
		t.Errorf("a tag that names another manifest: %v, want ErrConflict", err)
	}
	if desc, err := rr.Resolve(ctx, first.Tag); err != nil || desc.Digest != second.Digest {
		t.Errorf("after the conflict the tag names %v (%v), want it left as it was", desc.Digest, err)
	}
}

// TestWriteReadsBack: on a registry that serves a write late, Write returns once the tag reads
// back, and fails when it never does within ReadBack.
func TestWriteReadsBack(t *testing.T) {
	ctx := t.Context()
	cp := &index.ControlPlane{Schema: 21, ReadAt: time.Now().UTC()}
	lag := &providertest.Lag{Fake: &providertest.Fake{RegistryName: "local"}, Manifests: 2}
	if _, err := (Registry{Provider: lag, ReadBack: 10 * time.Second}).Write(ctx, cp, false, "v1"); err != nil {
		t.Errorf("Write on a registry that serves the tag at the third read: %v", err)
	}
	never := &providertest.Lag{Fake: &providertest.Fake{RegistryName: "local"}, Manifests: -1}
	if _, err := (Registry{Provider: never, ReadBack: 300 * time.Millisecond}).Write(ctx, cp, false, "v1"); err == nil ||
		!strings.Contains(err.Error(), "does not serve it") {
		t.Errorf("Write on a registry that never serves the tag: %v", err)
	}
}
