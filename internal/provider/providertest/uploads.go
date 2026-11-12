package providertest

import (
	"bytes"
	"context"
	"fmt"
	"hash"
	"io"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// uploadAlgorithms are the algorithms a partial blob is digested with as it arrives, so that
// a session larger than MaxKept still finishes against a digest of any of them.
var uploadAlgorithms = []digest.Algorithm{digest.SHA256, digest.SHA384, digest.SHA512}

// partial is the blob of an open upload session: its bytes while they fit in MaxKept, and the
// running digests of every byte.
type partial struct {
	name string // the repository that opened it

	mu        sync.Mutex // serialises the chunks of one session; held while a chunk is read
	size      int64
	kept      bytes.Buffer
	discarded bool // the session outgrew MaxKept: kept is empty
	hashes    map[digest.Algorithm]hash.Hash
}

// Write takes the next bytes of the session. The caller holds p.mu.
func (p *partial) Write(b []byte) (int, error) {
	if p.hashes == nil {
		p.hashes = make(map[digest.Algorithm]hash.Hash, len(uploadAlgorithms))
		for _, a := range uploadAlgorithms {
			p.hashes[a] = a.Hash()
		}
	}
	for _, h := range p.hashes {
		_, _ = h.Write(b) // a hash.Hash never fails
	}
	switch {
	case p.discarded:
	case p.size+int64(len(b)) > MaxKept:
		p.discarded = true
		p.kept = bytes.Buffer{}
	default:
		p.kept.Write(b)
	}
	p.size += int64(len(b))
	return len(b), nil
}

// append reads r to its end into the session and checks that it held size bytes when size is
// not -1. The caller holds p.mu. The bytes read before a failure stay in the session, as a
// registry keeps the part of a chunk it received.
func (p *partial) append(r io.Reader, size int64) error {
	if r == nil {
		r = bytes.NewReader(nil)
	}
	n, err := io.Copy(p, r)
	if err != nil {
		return fmt.Errorf("read chunk after %d bytes: %w", n, err)
	}
	if size >= 0 && n != size {
		return fmt.Errorf("chunk of %d bytes, the caller said %d", n, size)
	}
	return nil
}

// digest returns the digest of the session's bytes in algorithm a; false when the session
// has no digest in a. The caller holds p.mu.
func (p *partial) digest(a digest.Algorithm) (digest.Digest, bool) {
	if p.hashes == nil {
		return a.FromBytes(nil), a.Available()
	}
	h, ok := p.hashes[a]
	if !ok {
		return "", false
	}
	return digest.NewDigest(a, h), true
}

// upload returns the open session u of the repository, or provider.ErrNotFound.
func (r *repository) upload(verb string, u provider.Upload) (*partial, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	p, ok := r.f.uploads[u.ID]
	if !ok || p.name != r.name {
		return nil, fmt.Errorf("%s: %w", r.op(verb, "upload "+u.ID), provider.ErrNotFound)
	}
	return p, nil
}

// at returns the session u with the offset of p and the Fake's ChunkMinLength. The caller
// holds p.mu.
func (r *repository) at(u provider.Upload, p *partial) provider.Upload {
	return provider.Upload{Location: u.Location, ID: u.ID, Offset: p.size, ChunkMinLength: r.f.ChunkMinLength}
}

// StartBlobUpload opens a session; its ID is the key of the Fake's partial blobs.
func (r *repository) StartBlobUpload(context.Context) (provider.Upload, error) {
	if err := r.f.FailUpload; err != nil {
		return provider.Upload{}, fmt.Errorf("fake: start a blob upload in %s: %w", r.name, err)
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if r.f.uploads == nil {
		r.f.uploads = map[string]*partial{}
	}
	r.f.uploadSeq++
	id := fmt.Sprintf("upload-%d", r.f.uploadSeq)
	r.f.uploads[id] = &partial{name: r.name}
	return provider.Upload{Location: "/v2/" + r.name + "/blobs/uploads/" + id, ID: id, ChunkMinLength: r.f.ChunkMinLength}, nil
}

// MountBlob copies the blob d from the repository from when that repository has it, kept or
// discarded; otherwise, and when from is empty, it opens a session.
func (r *repository) MountBlob(ctx context.Context, d digest.Digest, from string) (bool, provider.Upload, error) {
	if err := d.Validate(); err != nil {
		return false, provider.Upload{}, fmt.Errorf("%s: %w", r.op("mount", d), err)
	}
	r.f.mu.Lock()
	var src blob
	found := false
	if rd := r.f.repo(from, false); from != "" && rd != nil {
		src, found = rd.blobs[d]
	}
	if found {
		r.f.repo(r.name, true).blobs[d] = src
	}
	r.f.mu.Unlock()
	if found {
		return true, provider.Upload{}, nil
	}
	u, err := r.StartBlobUpload(ctx)
	return false, u, err
}

// AppendBlobChunk appends r to the session u. The chunk must start at the end of the session
// (u.Offset), as a registry refuses one that does not.
func (r *repository) AppendBlobChunk(_ context.Context, u provider.Upload, rd io.Reader, size int64) (provider.Upload, error) {
	p, err := r.upload("append", u)
	if err != nil {
		return u, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if u.Offset != p.size {
		return r.at(u, p), fmt.Errorf("%s: chunk at offset %d, the session holds %d bytes", r.op("append", u.ID), u.Offset, p.size)
	}
	if err := p.append(rd, size); err != nil {
		return r.at(u, p), fmt.Errorf("%s: %w", r.op("append", u.ID), err)
	}
	return r.at(u, p), nil
}

// FinishBlobUpload appends the last chunk r, checks the session's bytes against d and stores
// them as the blob d, which ends the session. A digest they do not have wraps
// content.ErrMismatchedDigest and leaves the session open: the caller cancels it.
func (r *repository) FinishBlobUpload(_ context.Context, u provider.Upload, d digest.Digest, rd io.Reader, size int64) (ocispec.Descriptor, error) {
	op := r.op("finish", u.ID)
	if err := d.Validate(); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("%s: %w: %w", op, content.ErrMismatchedDigest, err)
	}
	p, err := r.upload("finish", u)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if rd != nil {
		if p.hashes == nil && d.Algorithm().Available() {
			// The whole blob comes now: one digest is enough, and a large test blob is
			// hashed once instead of three times.
			p.hashes = map[digest.Algorithm]hash.Hash{d.Algorithm(): d.Algorithm().Hash()}
		}
		if err := p.append(rd, size); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("%s: %w", op, err)
		}
	}
	if got, ok := p.digest(d.Algorithm()); !ok || got != d {
		return ocispec.Descriptor{}, fmt.Errorf("%s: the session's %d bytes are not %s: %w", op, p.size, d, content.ErrMismatchedDigest)
	}
	b := blob{size: p.size}
	if !p.discarded {
		b.data = append([]byte{}, p.kept.Bytes()...) // not nil: nil means discarded
	}
	r.f.mu.Lock()
	r.f.repo(r.name, true).blobs[d] = b
	delete(r.f.uploads, u.ID)
	r.f.mu.Unlock()
	return ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: p.size}, nil
}

// BlobUploadStatus returns the session with the bytes it holds.
func (r *repository) BlobUploadStatus(_ context.Context, u provider.Upload) (provider.Upload, error) {
	p, err := r.upload("status", u)
	if err != nil {
		return u, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return r.at(u, p), nil
}

// CancelBlobUpload drops the session and its bytes.
func (r *repository) CancelBlobUpload(_ context.Context, u provider.Upload) error {
	if _, err := r.upload("cancel", u); err != nil {
		return err
	}
	r.f.mu.Lock()
	delete(r.f.uploads, u.ID)
	r.f.mu.Unlock()
	return nil
}

// DefaultChunkSize is the chunk size of OpenBlobWriter when Fake.ChunkSize is zero.
const DefaultChunkSize = 64 << 10

// OpenBlobWriter is a provider.NewChunkedWriter over the Fake's sessions, with chunks of
// Fake.ChunkSize bytes.
func (r *repository) OpenBlobWriter(ctx context.Context) (provider.BlobWriter, error) {
	size := r.f.ChunkSize
	if size <= 0 {
		size = DefaultChunkSize
	}
	return provider.NewChunkedWriter(ctx, r, size), nil
}
