package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// refuseOne is the fake registry that fails the delete of one manifest with err.
type refuseOne struct {
	*providertest.Fake
	digest digest.Digest
	err    error
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
		return fmt.Errorf("fake: delete %s: %w", desc.Digest, r.f.err)
	}
	return r.Repository.Delete(ctx, desc)
}

// TestUnpublishPartial (#553): both unpublishes that delete several versions, of the whole
// package and the packument update that leaves versions out, answer a failure of the second
// version's delete with its status and name the first, which they deleted and which stays
// deleted (INV-13, #476); the other versions and the journal stay. A 5xx names it too. Both
// delete 1.0.0 first: the package's versions in publish order, the update's sorted.
func TestUnpublishPartial(t *testing.T) {
	tests := []struct {
		name   string
		err    error // the registry's answer to the delete of 1.1.0
		status int
		send   func(t *testing.T, h http.Handler) (int, string) // the unpublish: status and body
	}{
		{name: "the package, a registry error", err: errors.New("connection reset"), status: http.StatusInternalServerError,
			send: func(t *testing.T, h http.Handler) (int, string) {
				rr := do(h, http.MethodDelete, widgetsURL+"/-rev/"+forWrite(t, h)["_rev"].(string), nil, false)
				return rr.Code, rr.Body.String()
			}},
		{name: "an update without 1.0.0 and 1.1.0, a refusal", err: provider.ErrConflict, status: http.StatusConflict,
			send: func(t *testing.T, h http.Handler) (int, string) {
				doc := forWrite(t, h)
				delete(doc["versions"].(map[string]any), "1.0.0")
				delete(doc["versions"].(map[string]any), "1.1.0")
				r := putDoc(t, h, widgetsURL+"/-rev/"+doc["_rev"].(string), doc)
				return r.code, r.body
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			p := newPipeline(t)
			reg := &refuseOne{Fake: p.fake, err: tt.err}
			p.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": reg}, Index: p.st, Log: slog.New(slog.DiscardHandler),
				Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
			h := p.install("https://lajstrom.example", nil)
			digests := map[string]digest.Digest{}
			for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
				publish(t, h, v, nil)
				pkg, err := p.st.GetPackage(ctx, p.repo.ID, "@acme/widgets")
				must(t, err)
				row, err := p.st.GetVersion(ctx, pkg.ID, v, "")
				must(t, err)
				digests[v] = row.Digest
			}
			reg.digest = digests["1.1.0"]
			entries := p.seqOf(t)

			status, body := tt.send(t, h)
			var answer struct{ Error string }
			must(t, json.Unmarshal([]byte(body), &answer))
			want := "; deleted before the failure, and still deleted: 1.0.0 (" + digests["1.0.0"].String() + ")"
			if status != tt.status || !strings.HasSuffix(answer.Error, want) {
				t.Errorf("unpublish = %d %s, want %d ending in %q", status, body, tt.status, want)
			}
			left := map[string]string{"1.1.0": "", "1.2.0": ""}
			if d := cmp.Diff(left, versionsOf(forWrite(t, h))); d != "" {
				t.Errorf("versions left (-want +got)\n%s", d)
			}
			if n := p.seqOf(t); n != entries {
				t.Errorf("%d journal entries after the failure, want the %d before it", n, entries)
			}
		})
	}
}
