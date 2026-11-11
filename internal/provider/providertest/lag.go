package providertest

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
)

var _ provider.Provider = (*Lag)(nil)

// Lag is an eventually consistent registry over a Fake, as ECR is for a while after a push
// (docs/registries/README.md, Consistency): after each write, the next reads of what it wrote
// miss it. Manifests is how many reads of a manifest pushed miss it, by digest (Exists, Resolve,
// Fetch) and by its tag (Resolve); Blobs the same for a blob pushed (Exists, Fetch, StatBlob,
// OpenBlob). A negative count misses every read: a write the registry does not serve. A miss
// is false from Exists and provider.ErrNotFound from the others. Capabilities is the Fake's
// with Consistency eventual. Set the counts before the writes; Serve makes every write so far
// visible, and every write after it. It is safe for concurrent use.
type Lag struct {
	*Fake
	Manifests, Blobs int

	mu     sync.Mutex
	left   map[string]int // reads left that miss, by name@digest and name:tag
	misses int
}

// Capabilities are the Fake's, eventually consistent.
func (l *Lag) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := l.Fake.Capabilities(ctx)
	c.Consistency = provider.ConsistencyEventual
	return c, err
}

// Serve makes the registry strong from now on: every write so far, and every write to come, is
// visible to every read.
func (l *Lag) Serve() {
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.left)
	l.Manifests, l.Blobs = 0, 0
}

// Misses returns how many reads missed a write so far.
func (l *Lag) Misses() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.misses
}

// wrote starts the misses of the key of a write.
func (l *Lag) wrote(key string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.left == nil {
		l.left = map[string]int{}
	}
	l.left[key] = n
}

// miss reports whether a read of key misses it, and counts it.
func (l *Lag) miss(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.left[key]
	if !ok || n == 0 {
		return false
	}
	if n > 0 {
		l.left[key] = n - 1
	}
	l.misses++
	return true
}

// count is how many reads of a write of desc miss it.
func (l *Lag) count(desc ocispec.Descriptor) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if isManifest(desc.MediaType) {
		return l.Manifests
	}
	return l.Blobs
}

func lagKey(name string, d digest.Digest) string { return name + "@" + d.String() }

// Repository returns the Fake's repository name, lagging.
func (l *Lag) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := l.Fake.Repository(ctx, name)
	if err != nil {
		return nil, err
	}
	return &lagRepository{Repository: rr, l: l, name: name}, nil
}

// StatBlob is the Fake's, unless the blob's write is not seen yet.
func (l *Lag) StatBlob(ctx context.Context, name string, d digest.Digest) (provider.Blob, error) {
	if l.miss(lagKey(name, d)) {
		return provider.Blob{}, fmt.Errorf("providertest: stat blob %s@%s, lagging: %w", name, d, provider.ErrNotFound)
	}
	return l.Fake.StatBlob(ctx, name, d)
}

// OpenBlob is the Fake's, unless the blob's write is not seen yet.
func (l *Lag) OpenBlob(ctx context.Context, name string, d digest.Digest, rangeHeader string, redirect bool) (provider.Blob, error) {
	if l.miss(lagKey(name, d)) {
		return provider.Blob{}, fmt.Errorf("providertest: open blob %s@%s, lagging: %w", name, d, provider.ErrNotFound)
	}
	return l.Fake.OpenBlob(ctx, name, d, rangeHeader, redirect)
}

// lagRepository is a repository of a Lag.
type lagRepository struct {
	provider.Repository
	l    *Lag
	name string
}

func (r *lagRepository) Push(ctx context.Context, expected ocispec.Descriptor, content io.Reader) error {
	err := r.Repository.Push(ctx, expected, content)
	if err == nil {
		r.l.wrote(lagKey(r.name, expected.Digest), r.l.count(expected))
	}
	return err
}

func (r *lagRepository) PushReference(ctx context.Context, expected ocispec.Descriptor, content io.Reader, reference string) error {
	err := r.Repository.PushReference(ctx, expected, content, reference)
	if err == nil {
		n := r.l.count(expected)
		r.l.wrote(lagKey(r.name, expected.Digest), n)
		r.l.wrote(r.name+":"+reference, n)
	}
	return err
}

func (r *lagRepository) Exists(ctx context.Context, target ocispec.Descriptor) (bool, error) {
	if r.l.miss(lagKey(r.name, target.Digest)) {
		return false, nil
	}
	return r.Repository.Exists(ctx, target)
}

func (r *lagRepository) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	if r.l.miss(lagKey(r.name, target.Digest)) {
		return nil, fmt.Errorf("providertest: fetch %s@%s, lagging: %w", r.name, target.Digest, provider.ErrNotFound)
	}
	return r.Repository.Fetch(ctx, target)
}

func (r *lagRepository) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	key := r.name + ":" + reference
	if d, err := digest.Parse(reference); err == nil {
		key = lagKey(r.name, d)
	}
	if r.l.miss(key) {
		return ocispec.Descriptor{}, fmt.Errorf("providertest: resolve %s@%s, lagging: %w", r.name, reference, provider.ErrNotFound)
	}
	return r.Repository.Resolve(ctx, reference)
}
