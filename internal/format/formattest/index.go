package formattest

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Index is a partial index.Store for tests: GetRepo over Repos, and index_cache in memory
// (GetIndexCache, PutIndexCache, InvalidateIndexCache). Tx runs fn with the Index itself, so a
// rollback keeps what fn wrote. Every other method panics through the nil embedded Store; add
// methods only when a test needs them. Use it as a pointer and set the fields before the first
// call.
type Index struct {
	// Store is nil; the methods Index does not implement panic on it.
	index.Store

	// Repos are the repos GetRepo finds, by Format and Name.
	Repos []index.Repo
	// GetRepoErr, when set, is what GetRepo returns for every repo.
	GetRepoErr error
	// GetRepoCalls counts the calls of GetRepo, failed ones included.
	GetRepoCalls atomic.Int64

	mu    sync.Mutex
	cache map[cacheKey]index.Cache
}

// cacheKey is the unique key of index_cache.
type cacheKey struct {
	repoID      int64
	scope, kind string
}

// GetRepo returns a copy of the repo of Repos with format and name, GetRepoErr when it is set,
// or an error wrapping index.ErrNotFound. Every call counts in GetRepoCalls.
func (i *Index) GetRepo(_ context.Context, format, name string) (*index.Repo, error) {
	i.GetRepoCalls.Add(1)
	if i.GetRepoErr != nil {
		return nil, i.GetRepoErr
	}
	for _, r := range i.Repos {
		if r.Format == format && r.Name == name {
			return &r, nil
		}
	}
	return nil, fmt.Errorf("%w: repo %s/%s", index.ErrNotFound, format, name)
}

// GetIndexCache returns a copy of the cached document, or an error wrapping index.ErrNotFound.
func (i *Index) GetIndexCache(_ context.Context, repoID int64, scope, kind string) (*index.Cache, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	c, ok := i.cache[cacheKey{repoID, scope, kind}]
	if !ok {
		return nil, fmt.Errorf("%w: index_cache %d %q %q", index.ErrNotFound, repoID, scope, kind)
	}
	return &c, nil
}

// PutIndexCache stores a copy of c, Content included, replacing the row with the same key.
func (i *Index) PutIndexCache(_ context.Context, c *index.Cache) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cache == nil {
		i.cache = map[cacheKey]index.Cache{}
	}
	stored := *c
	stored.Content = bytes.Clone(c.Content)
	i.cache[cacheKey{c.RepoID, c.Scope, c.Kind}] = stored
	return nil
}

// InvalidateIndexCache removes the documents of a scope of a repo, or of every scope when scope
// is "".
func (i *Index) InvalidateIndexCache(_ context.Context, repoID int64, scope string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	for k := range i.cache {
		if k.repoID == repoID && (scope == "" || k.scope == scope) {
			delete(i.cache, k)
		}
	}
	return nil
}

// Tx calls fn with i and returns its error. Nothing is rolled back.
func (i *Index) Tx(_ context.Context, fn func(index.Writer) error) error { return fn(i) }
