package snapshot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/keys/keystest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// A snapshot sealed under a data key of a KMS (#1388; manifest-profile.md §9).

// dataKeyMaster makes a data key wrapped by kms's first key, writes its row and the sealing row
// naming it into st, and returns a Master that holds it and seals with it, and the row.
func dataKeyMaster(t *testing.T, st index.Store, kms *keystest.WrapKMS) (*keys.Master, index.WrappedMasterKey, config.KMSKey) {
	t.Helper()
	ctx := t.Context()
	key, err := config.ParseMasterKMSKey(kms.URI())
	must(t, err)
	id, wrapped, err := keys.WrapMaster(ctx, kms, key)
	must(t, err)
	row := index.WrappedMasterKey{KeyID: id, KMS: kms.URI(), Wrapped: wrapped}
	must(t, st.PutWrappedMasterKey(ctx, &row, nil))
	must(t, st.PutSealingKey(ctx, &index.SealingKey{KeyID: id, Source: index.SealingRewrap, Host: "admin"}, ""))
	m, err := keys.HoldMasterKeys(nil, []string{id})
	must(t, err)
	must(t, m.Unwrap(ctx, kms, key, row))
	m.SealWith(id)
	return m, row, key
}

// TestWrappedKeyAnnotation: a sealed snapshot under a data key carries its row in
// lajstrom.snapshot.wrappedKey, the ciphertext in base64 and no key; an unsealed one, and one
// sealed under a key of a file, carry none. A restore whose master keys do not hold the key reads
// it with UnwrapKey, which gets the annotation's row, and gets the rows of wrapped_master_keys in
// the document; without UnwrapKey, or when it fails, the layer does not open.
func TestWrappedKeyAnnotation(t *testing.T) {
	ctx := t.Context()
	st, kms := newStore(t), keystest.NewWrapKMS()
	m, row, key := dataKeyMaster(t, st, kms)
	cp, err := st.ExportControlPlane(ctx, index.ExportOptions{})
	must(t, err)
	fake := &providertest.Fake{RegistryName: "local"}
	info, err := Registry{Provider: fake, Master: m}.Write(ctx, cp, true, "test")
	must(t, err)
	mf, _ := layerOf(t, fake, info)
	ann := mf.Annotations[profile.AnnotationSnapshotWrappedKey]
	want := `{"keyId":"` + row.KeyID + `","kms":"` + kms.URI() + `","wrapped":"` + base64.StdEncoding.EncodeToString(row.Wrapped) + `"}`
	if ann != want {
		t.Errorf("the annotation %s, want %s", ann, want)
	}
	for _, reg := range []Registry{{Provider: &providertest.Fake{RegistryName: "a"}, Master: m},
		{Provider: &providertest.Fake{RegistryName: "b"}, Master: master(t, testMasterHex)}} {
		sealed := reg.Master != m
		i, err := reg.Write(ctx, cp, sealed, "test")
		must(t, err)
		if mf, _ := layerOf(t, reg.Provider.(*providertest.Fake), i); mf.Annotations[profile.AnnotationSnapshotWrappedKey] != "" {
			t.Errorf("a snapshot, sealed %t under a key of a file or unsealed, carries the annotation", sealed)
		}
	}

	restore, err := keys.HoldMasterKeys(nil, []string{row.KeyID})
	must(t, err)
	if _, _, err := (Registry{Provider: fake, Master: restore}).Read(ctx, info.Tag); err == nil {
		t.Error("a sealed snapshot read without the data key and without UnwrapKey")
	}
	errBoom := errors.New("refused")
	failing := Registry{Provider: fake, Master: restore, UnwrapKey: func(context.Context, index.WrappedMasterKey) error { return errBoom }}
	if _, _, err := failing.Read(ctx, info.Tag); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), row.KeyID) {
		t.Errorf("a read whose unwrap fails: %v, want its error naming the id", err)
	}
	var asked index.WrappedMasterKey
	reg := Registry{Provider: fake, Master: restore, UnwrapKey: func(ctx context.Context, r index.WrappedMasterKey) error {
		asked = r
		return restore.Unwrap(ctx, kms, key, r)
	}}
	got, _, err := reg.Read(ctx, info.Tag)
	must(t, err)
	if asked.KeyID != row.KeyID || asked.KMS != row.KMS || !bytes.Equal(asked.Wrapped, row.Wrapped) {
		t.Errorf("UnwrapKey got %+v, want the row of the annotation", asked)
	}
	if len(got.WrappedMasterKeys) != 1 || got.WrappedMasterKeys[0].KeyID != row.KeyID || got.SealingKey.KeyID != row.KeyID {
		t.Errorf("the restored control plane: %+v, %+v; want the row and the sealing key", got.WrappedMasterKeys, got.SealingKey)
	}
	n := kms.Decrypts()
	if _, _, err := reg.Read(ctx, info.Tag); err != nil || kms.Decrypts() != n {
		t.Errorf("a second read with the key held: %v, %d unwraps; want none", err, kms.Decrypts()-n)
	}
}
