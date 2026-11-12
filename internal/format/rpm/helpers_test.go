package rpm

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// alice is the principal of the uploads.
var alice = auth.Principal{Kind: auth.KindUser, Name: "alice"}

// pipeline is the real ingest pipeline over the real SQLite store in memory and the fake
// registry "local", with the rpm repo el9.
type pipeline struct {
	t    *testing.T
	st   *sqlite.Store
	prov *providertest.Fake
	pipe *ingest.Pipeline
	repo index.Repo
}

func newPipeline(t *testing.T) *pipeline {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:rpm-"+t.Name()+"?mode=memory&cache=shared")
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	must(t, st.Migrate(ctx))
	p := &pipeline{t: t, st: st, prov: &providertest.Fake{RegistryName: "local"},
		repo: index.Repo{Format: Name, Name: "el9", Registry: "local", Prefix: "rpm/el9", Kind: "hosted"}}
	must(t, st.CreateRepo(ctx, &p.repo))
	p.pipe = ingest.New(ingest.Options{Providers: provider.Map{"local": p.prov}, Index: st, Log: slog.New(slog.DiscardHandler),
		Plugins: func(name string) (format.Plugin, bool) { return Plugin{}, name == Name }})
	return p
}

// upload publishes b through the pipeline as alice.
func (p *pipeline) upload(b []byte) *format.Published {
	p.t.Helper()
	pub, err := p.pipe.Upload(p.t.Context(), p.repo, format.Upload{Files: []format.UploadedFile{rpmFile(b)}}, alice)
	if err != nil {
		p.t.Fatalf("upload: %v", err)
	}
	return pub
}

// rpmFile is an UploadedFile of b that Build reads with Open.
func rpmFile(b []byte) format.UploadedFile {
	return format.UploadedFile{Name: "x.rpm", Size: int64(len(b)), Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
