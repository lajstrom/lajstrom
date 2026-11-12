package facade

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// blobsLate is a registry whose manifest PUTs answer 400 MANIFEST_BLOB_UNKNOWN until refusals
// run out (negative: every PUT), as ECR does while its manifest check does not see a blob it
// took a moment before. It is eventually consistent unless strong.
type blobsLate struct {
	*providertest.Fake
	strong   bool
	refusals atomic.Int64
	puts     atomic.Int64
}

func (b *blobsLate) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := b.Fake.Capabilities(ctx)
	if !b.strong {
		c.Consistency = provider.ConsistencyEventual
	}
	return c, err
}

func (b *blobsLate) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := b.Fake.Repository(ctx, name)
	return lateRepository{Repository: rr, b: b}, err
}

// refuse counts a manifest PUT and returns the refusal while there are refusals left.
func (b *blobsLate) refuse() error {
	b.puts.Add(1)
	if b.refusals.Load() == 0 {
		return nil
	}
	b.refusals.Add(-1)
	return &provider.RejectedError{Registry: "local", Status: http.StatusBadRequest, Code: codeManifestBlobUnknown}
}

// lateRepository is a repository of blobsLate.
type lateRepository struct {
	provider.Repository
	b *blobsLate
}

func (r lateRepository) Push(ctx context.Context, desc ocispec.Descriptor, content io.Reader) error {
	if desc.MediaType == ocispecManifest {
		if err := r.b.refuse(); err != nil {
			return err
		}
	}
	return r.Repository.Push(ctx, desc, content)
}

func (r lateRepository) PushReference(ctx context.Context, desc ocispec.Descriptor, content io.Reader, ref string) error {
	if err := r.b.refuse(); err != nil {
		return err
	}
	return r.Repository.PushReference(ctx, desc, content, ref)
}

// TestPassThroughBlobUnknownRetried: a manifest of an oci repo that a registry not classified
// strong answers MANIFEST_BLOB_UNKNOWN is put again, by tag and by digest, with backoff, until
// the registry takes it or the visibility timeout ends; a strong registry's refusal is the
// answer at once.
func TestPassThroughBlobUnknownRetried(t *testing.T) {
	tests := []struct {
		name     string
		strong   bool
		byDigest bool
		refusals int64
		status   int
		puts     int64 // 0: more than one
	}{
		{name: "by tag, twice", refusals: 2, status: http.StatusCreated, puts: 3},
		{name: "by digest, once", byDigest: true, refusals: 1, status: http.StatusCreated, puts: 2},
		{name: "past the visibility timeout", refusals: -1, status: http.StatusBadRequest},
		{name: "on a strong registry", strong: true, refusals: 1, status: http.StatusBadRequest, puts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var late *blobsLate
			e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider {
				late = &blobsLate{Fake: f, strong: tt.strong}
				late.refusals.Store(tt.refusals)
				return late
			}})
			h := New(Options{Index: e.st, Providers: provider.Map{"local": e.reg}, Ingest: e.ingest, Tokens: e.tokens,
				Log: slog.New(slog.NewTextHandler(e.logs, nil)), Now: e.clock.Now, VisibilityTimeout: 300 * time.Millisecond})
			body, d := manifest(t, []byte("layer"), "lagging")
			ref := "v1"
			if tt.byDigest {
				ref = d.String()
			}
			rec := e.do(h, http.MethodPut, "/v2/oci/images/app/manifests/"+ref, bytes.NewReader(body), "Content-Type", ocispecManifest)
			if n := late.puts.Load(); rec.Code != tt.status || tt.puts != 0 && n != tt.puts || tt.puts == 0 && n < 2 {
				t.Errorf("PUT: %d after %d manifest PUTs to the registry, want %d after %d (0: more than one); %s",
					rec.Code, n, tt.status, tt.puts, rec.Body)
			}
		})
	}
}
