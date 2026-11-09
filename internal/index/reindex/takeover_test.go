package reindex

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/sqlite"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// TestTakenOver checks that a run whose row a later start closed as abandoned stops at its next
// heartbeat, returns errTakenOver, and leaves the row as the later start wrote it.
func TestTakenOver(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(ctx, "file:reindex-takeover?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := index.Repo{Format: "raw", Name: "slow", Registry: "local", Prefix: "raw/slow", Kind: "hosted"}
	if err := st.CreateRepo(ctx, &repo); err != nil {
		t.Fatal(err)
	}
	fake := &providertest.Fake{RegistryName: "local"}
	rr, err := fake.Repository(ctx, "raw/slow/pkg")
	if err != nil {
		t.Fatal(err)
	}
	b := []byte(`{"schemaVersion":2}`)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(b), Size: int64(len(b))}
	if err := rr.PushReference(ctx, desc, bytes.NewReader(b), "1"); err != nil {
		t.Fatal(err)
	}
	w := New(Options{Index: st, Providers: provider.Map{"local": blocking{fake}}, Log: slog.New(slog.DiscardHandler)})
	w.heartbeat = 10 * time.Millisecond

	req := Request{Repo: &repo, Source: "cli", Principal: "user:admin"}
	run, err := w.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	later := index.ReindexRun{RepoID: &repo.ID, Source: "api", Principal: "user:other", Replica: "b"}
	if err := st.StartReindexRun(ctx, &later, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.Run(ctx, run, req)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errTakenOver) {
			t.Errorf("Run: %v, want errTakenOver", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not stop")
	}
	got, err := st.GetReindexRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != index.ReindexAbandoned {
		t.Errorf("the taken-over run: state %q, want abandoned", got.State)
	}
}

// blocking is a registry whose tag lists never answer before the context ends.
type blocking struct{ *providertest.Fake }

func (b blocking) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := b.Fake.Repository(ctx, name)
	return blockingRepo{rr}, err
}

type blockingRepo struct{ provider.Repository }

func (blockingRepo) Tags(ctx context.Context, _ string, _ func([]string) error) error {
	<-ctx.Done()
	return ctx.Err()
}
