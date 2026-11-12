package providertest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
)

var _ provider.Provider = (*Lag)(nil)

// Lag is an eventually consistent registry over a Fake, as ECR is for a while after a push
// (docs/registries/README.md, Consistency): after each write, the next reads of what it wrote
// miss it. Manifests is how many reads of a manifest pushed miss it, by digest (Exists, Resolve,
// Fetch, StatManifest) and by its tag (Resolve); Blobs the same for a blob pushed (Exists, Fetch,
// StatBlob, OpenBlob). Lists is how many listings miss a write: a tag pushed is left out of the
// next Lists tag lists of its repository (Tags), and a repository that a write created out of
// the next Lists catalogs that would name it (ListRepositories). Deletes is how many reads of a
// manifest deleted still find it: by digest, by a tag that named it, and its tags in the tag
// lists of its repository, each counted on its own. A negative count misses every read: a write
// the registry does not serve, a delete it never shows. A miss is false from Exists, a tag or a
// repository left out of a list, and provider.ErrNotFound from the others, with
// provider.ErrManifestUnknown too from StatManifest. Capabilities is the Fake's with Consistency
// eventual. Set the counts before the writes; Serve makes every write so far visible, and every
// write after it. It is safe for concurrent use.
type Lag struct {
	*Fake
	Manifests, Blobs, Lists, Deletes int

	mu     sync.Mutex
	left   map[string]int    // reads left that miss, by name@digest and name:tag
	listed map[string]int    // listings left that miss, by name:tag (Tags) and name (ListRepositories)
	ghosts map[string]*ghost // manifests deleted that reads still find, by name@digest and name:tag
	misses int
}

// ghost is a manifest deleted that the Lag still serves: reads by its digest or by a tag that
// named it, and the tag lists of its repository for a tag, as many each.
type ghost struct {
	desc  ocispec.Descriptor
	data  []byte
	reads int
	lists int
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
	clear(l.listed)
	clear(l.ghosts)
	l.Manifests, l.Blobs, l.Lists, l.Deletes = 0, 0, 0, 0
}

// Misses returns how many reads and listings missed a write or a delete so far.
func (l *Lag) Misses() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.misses
}

// wrote starts the misses of the key of a write; a delete of the key before it is not served
// any more.
func (l *Lag) wrote(key string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.left == nil {
		l.left = map[string]int{}
	}
	l.left[key] = n
	delete(l.ghosts, key)
}

// wroteListed starts the misses of a write in the listings that would name key.
func (l *Lag) wroteListed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Lists == 0 {
		return
	}
	if l.listed == nil {
		l.listed = map[string]int{}
	}
	l.listed[key] = l.Lists
}

// unlisted reports whether a listing that would name key leaves it out, and counts it.
func (l *Lag) unlisted(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.listed[key]
	if !ok || n == 0 {
		return false
	}
	if n > 0 {
		l.listed[key] = n - 1
	}
	l.misses++
	return true
}

// deleted keeps the manifest m, d of the repository name, readable after its delete with the
// tags that named it, Deletes reads each.
func (l *Lag) deleted(name string, d digest.Digest, m manifest, tags []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Deletes == 0 {
		return
	}
	if l.ghosts == nil {
		l.ghosts = map[string]*ghost{}
	}
	l.ghosts[lagKey(name, d)] = &ghost{desc: m.desc, data: m.data, reads: l.Deletes}
	for _, tag := range tags {
		l.ghosts[name+":"+tag] = &ghost{desc: m.desc, data: m.data, reads: l.Deletes, lists: l.Deletes}
	}
}

// ghostRead returns the manifest deleted that a read of key still finds, and counts it.
func (l *Lag) ghostRead(key string) (*ghost, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, ok := l.ghosts[key]
	if !ok || g.reads == 0 {
		return nil, false
	}
	if g.reads > 0 {
		g.reads--
	}
	l.misses++
	return g, true
}

// ghostTags returns the tags of manifests deleted that a tag list of the repository name still
// names, and counts them.
func (l *Lag) ghostTags(name string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var tags []string
	for key, g := range l.ghosts {
		tag, ok := strings.CutPrefix(key, name+":")
		if !ok || g.lists == 0 {
			continue
		}
		if g.lists > 0 {
			g.lists--
		}
		l.misses++
		tags = append(tags, tag)
	}
	return tags
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

// ListRepositories is the Fake's, without the repositories whose creating write it does not
// list yet.
func (l *Lag) ListRepositories(ctx context.Context, prefix string) ([]string, error) {
	names, err := l.Fake.ListRepositories(ctx, prefix)
	return slices.DeleteFunc(names, l.unlisted), err
}

// StatManifest is the Fake's, unless the manifest's write is not seen yet (MANIFEST_UNKNOWN) or
// its delete is not.
func (l *Lag) StatManifest(ctx context.Context, name string, d digest.Digest) (ocispec.Descriptor, error) {
	key := lagKey(name, d)
	if l.miss(key) {
		return ocispec.Descriptor{}, fmt.Errorf("providertest: stat manifest %s@%s, lagging: %w: %w", name, d,
			provider.ErrManifestUnknown, provider.ErrNotFound)
	}
	if g, ok := l.ghostRead(key); ok {
		return ocispec.Descriptor{MediaType: g.desc.MediaType, Digest: g.desc.Digest, Size: g.desc.Size}, nil
	}
	return l.Fake.StatManifest(ctx, name, d)
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
	created := !r.l.hasRepo(r.name)
	err := r.Repository.Push(ctx, expected, content)
	if err == nil {
		r.l.wrote(lagKey(r.name, expected.Digest), r.l.count(expected))
		if created {
			r.l.wroteListed(r.name)
		}
	}
	return err
}

func (r *lagRepository) PushReference(ctx context.Context, expected ocispec.Descriptor, content io.Reader, reference string) error {
	created := !r.l.hasRepo(r.name)
	err := r.Repository.PushReference(ctx, expected, content, reference)
	if err == nil {
		n := r.l.count(expected)
		r.l.wrote(lagKey(r.name, expected.Digest), n)
		r.l.wrote(r.name+":"+reference, n)
		r.l.wroteListed(r.name + ":" + reference)
		if created {
			r.l.wroteListed(r.name)
		}
	}
	return err
}

// Delete is the Fake's; a manifest deleted stays readable, Deletes reads.
func (r *lagRepository) Delete(ctx context.Context, target ocispec.Descriptor) error {
	m, tags, isManifest := r.l.manifestOf(r.name, target.Digest)
	err := r.Repository.Delete(ctx, target)
	if err == nil && isManifest {
		r.l.deleted(r.name, target.Digest, m, tags)
	}
	return err
}

// Tags calls fn once with the Fake's tags after last, sorted, without those whose writes it does
// not list yet and with those of manifests deleted that it still lists.
func (r *lagRepository) Tags(ctx context.Context, last string, fn func(tags []string) error) error {
	var tags []string
	err := r.Repository.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	})
	ghosts := r.l.ghostTags(r.name)
	if err != nil && (!errors.Is(err, provider.ErrNotFound) || len(ghosts) == 0) {
		return err
	}
	tags = slices.DeleteFunc(tags, func(tag string) bool { return r.l.unlisted(r.name + ":" + tag) })
	tags = append(tags, ghosts...)
	slices.Sort(tags)
	tags = slices.Compact(tags)
	return fn(slices.DeleteFunc(tags, func(tag string) bool { return tag <= last }))
}

func (r *lagRepository) Exists(ctx context.Context, target ocispec.Descriptor) (bool, error) {
	key := lagKey(r.name, target.Digest)
	if r.l.miss(key) {
		return false, nil
	}
	if _, ok := r.l.ghostRead(key); ok {
		return true, nil
	}
	return r.Repository.Exists(ctx, target)
}

func (r *lagRepository) Fetch(ctx context.Context, target ocispec.Descriptor) (io.ReadCloser, error) {
	key := lagKey(r.name, target.Digest)
	if r.l.miss(key) {
		return nil, fmt.Errorf("providertest: fetch %s@%s, lagging: %w", r.name, target.Digest, provider.ErrNotFound)
	}
	if g, ok := r.l.ghostRead(key); ok {
		return seekNopCloser{bytes.NewReader(g.data)}, nil
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
	if g, ok := r.l.ghostRead(key); ok {
		return g.desc, nil
	}
	return r.Repository.Resolve(ctx, reference)
}

// hasRepo reports whether the Fake has the repository name.
func (l *Lag) hasRepo(name string) bool {
	l.Fake.mu.Lock()
	defer l.Fake.mu.Unlock()
	return l.repo(name, false) != nil
}

// manifestOf returns the manifest d of the repository name and the tags that name it; false when
// the Fake has no such manifest.
func (l *Lag) manifestOf(name string, d digest.Digest) (manifest, []string, bool) {
	l.Fake.mu.Lock()
	defer l.Fake.mu.Unlock()
	rd := l.repo(name, false)
	if rd == nil {
		return manifest{}, nil, false
	}
	m, ok := rd.manifests[d]
	if !ok {
		return manifest{}, nil, false
	}
	var tags []string
	for tag, td := range rd.tags {
		if td == d {
			tags = append(tags, tag)
		}
	}
	return m, tags, true
}
