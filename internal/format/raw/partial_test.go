package raw

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// refuseOne is the fake registry that refuses to delete one manifest, as Zot keeps a manifest
// that an image index lists.
type refuseOne struct {
	*providertest.Fake
	digest digest.Digest
}

func (f *refuseOne) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return refuseOneRepo{Repository: rr, f: f}, err
}

type refuseOneRepo struct {
	provider.Repository
	f *refuseOne
}

func (r refuseOneRepo) Delete(ctx context.Context, desc ocispec.Descriptor) error {
	if desc.Digest == r.f.digest {
		return fmt.Errorf("fake: delete %s: %w", desc.Digest, provider.ErrConflict)
	}
	return r.Repository.Delete(ctx, desc)
}

// TestDeletePartial: a DELETE of a path whose second version the registry refuses answers the
// refusal's status and names the first version, which it deleted and which stays deleted
// (INV-13, #476).
func TestDeletePartial(t *testing.T) {
	reg := &refuseOne{}
	e := newEnv(t, envOptions{registry: reg})
	reg.Fake = e.prov
	v1, v2 := e.put(fileTxt, "one"), e.put(fileTxt, "two")
	reg.digest = digest.Digest(v2.Digest)
	rr := e.do(http.MethodDelete, target(fileTxt), nil)
	want := "deleted before the failure, and still deleted: " + v1.Version + " (" + v1.Digest + ")"
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), want) {
		t.Errorf("DELETE = %d %q, want 409 naming %s", rr.Code, rr.Body, want)
	}
	if vs := e.versions(fileTxt); len(vs) != 1 || vs[0].Digest.String() != v2.Digest {
		t.Errorf("versions left %+v, want %s", vs, v2.Digest)
	}
}
