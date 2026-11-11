package providertest

import (
	"bytes"
	"context"
	"io"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// Corrupting returns f as a registry whose blob chunks arrive with their first byte changed: it
// stores other bytes than the uploader hashed, which the commit of the upload session finds
// (its digest is refused). Its OpenBlobWriter sends chunks of 64 bytes through that change.
func Corrupting(f *Fake) provider.Provider { return corrupting{f} }

type corrupting struct{ *Fake }

func (c corrupting) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := c.Fake.Repository(ctx, name)
	return corruptingRepo{rr}, err
}

type corruptingRepo struct{ provider.Repository }

func (r corruptingRepo) AppendBlobChunk(ctx context.Context, u provider.Upload, rd io.Reader, size int64) (provider.Upload, error) {
	b, err := io.ReadAll(rd)
	if err != nil || len(b) == 0 {
		return u, err
	}
	b[0] ^= 0xff
	return r.Repository.AppendBlobChunk(ctx, u, bytes.NewReader(b), size)
}

func (r corruptingRepo) OpenBlobWriter(ctx context.Context) (provider.BlobWriter, error) {
	return provider.NewChunkedWriter(ctx, r, 64), nil
}
