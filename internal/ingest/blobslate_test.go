package ingest_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// blobsLate is a registry whose manifest PUTs answer 400 with code until refusals run out
// (negative: every PUT), as ECR answers MANIFEST_BLOB_UNKNOWN while its manifest check does not
// see a blob it took a moment before. It is eventually consistent unless strong.
type blobsLate struct {
	*providertest.Fake
	strong   bool
	code     string
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

// lateRepository is a repository of blobsLate.
type lateRepository struct {
	provider.Repository
	b *blobsLate
}

func (r lateRepository) PushReference(ctx context.Context, desc ocispec.Descriptor, content io.Reader, ref string) error {
	r.b.puts.Add(1)
	if n := r.b.refusals.Load(); n != 0 {
		r.b.refusals.Add(-1)
		return &provider.RejectedError{Registry: "local", Status: http.StatusBadRequest, Code: r.b.code}
	}
	return r.Repository.PushReference(ctx, desc, content, ref)
}

// TestPushManifestBlobUnknownRetried: a manifest PUT that a registry not classified strong
// answers MANIFEST_BLOB_UNKNOWN is sent again, with backoff, until it passes or the visibility
// timeout ends; a strong registry's refusal, and any other refusal, is the answer at once.
func TestPushManifestBlobUnknownRetried(t *testing.T) {
	tests := []struct {
		name     string
		strong   bool
		code     string
		refusals int64
		ok       bool
		puts     int64 // 0: more than one
	}{
		{name: "twice, then taken", code: "MANIFEST_BLOB_UNKNOWN", refusals: 2, ok: true, puts: 3},
		{name: "past the visibility timeout", code: "MANIFEST_BLOB_UNKNOWN", refusals: -1},
		{name: "on a strong registry", strong: true, code: "MANIFEST_BLOB_UNKNOWN", refusals: 1, puts: 1},
		{name: "another refusal", code: "MANIFEST_INVALID", refusals: 1, puts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			late := &blobsLate{Fake: e.prov, strong: tt.strong, code: tt.code}
			late.refusals.Store(tt.refusals)
			o := e.options(late)
			o.VisibilityTimeout = 300 * time.Millisecond
			e.pipe = ingest.New(o)
			pub, err := e.pipe.Upload(t.Context(), e.repo, upload("tool", "1.0.0", file{"tool", "linux binary"}), alice)
			var rej *provider.RejectedError
			switch {
			case tt.ok && err != nil:
				t.Fatalf("Upload: %v", err)
			case tt.ok:
				if r := e.rows(t); len(r.Versions) != 1 || r.Versions[0].Digest != pub.Digest {
					t.Errorf("versions %+v, want the one published", r.Versions)
				}
			case !errors.As(err, &rej) || rej.Code != tt.code:
				t.Errorf("Upload: %v, want the registry's %s", err, tt.code)
			}
			if n := late.puts.Load(); tt.puts != 0 && n != tt.puts || tt.puts == 0 && n < 2 {
				t.Errorf("%d manifest PUTs, want %d (0: more than one)", n, tt.puts)
			}
			e.noClaim(t, "tool", "1.0.0")
		})
	}
}
