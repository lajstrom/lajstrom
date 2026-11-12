package formattest

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Rebuilding is a Streamer whose every Build writes an org.opencontainers.image.created a second
// after the one before: a client's retry builds the manifest of the same files again, at a later
// second, and so with another digest. Without it a test's retry gets that digest only when the
// clock crosses a second between the two builds.
type Rebuilding struct {
	format.Streamer
	builds atomic.Int64
}

// Build builds with the Streamer and adds one second per Build so far to the created time.
func (p *Rebuilding) Build(ctx context.Context, repo index.Repo, in format.Upload) (*format.Built, error) {
	b, err := p.Streamer.Build(ctx, repo, in)
	if err != nil {
		return nil, err
	}
	created, err := time.Parse(time.RFC3339, b.Manifest.Annotations[ocispec.AnnotationCreated])
	if err != nil {
		return nil, fmt.Errorf("formattest: %s of the built manifest: %w", ocispec.AnnotationCreated, err)
	}
	later := created.Add(time.Duration(p.builds.Add(1)) * time.Second)
	b.Manifest.Annotations[ocispec.AnnotationCreated] = later.UTC().Format(time.RFC3339)
	return b, nil
}
