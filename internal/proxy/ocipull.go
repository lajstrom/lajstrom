package proxy

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The completion of a platform's manifest that a client got (docs/spec/proxy-and-group.md
// §11.5, step 5). containerd asks only for the layers it does not hold, and keeps them by digest
// across images: the layers an image shares with another one are never asked for at its path,
// and without the completion it would never be held whole in the backend (#876).

const (
	// defaultCompleteQuiet is Options.CompleteQuiet when it is zero.
	defaultCompleteQuiet = 10 * time.Second
	// ociCompleteQuietMost is the longest a completion waits for the quiet of a path, from the
	// GET of the manifest; then it fetches while clients still pull.
	ociCompleteQuietMost = 5 * time.Minute
	// ociCompleteFetches is how many blobs the completions of a replica fetch at once.
	ociCompleteFetches = 4
)

// fetchCompleted is the result of lajstrom_proxy_fetches_total of a blob that no client asked
// for at the path, fetched by the completion of a manifest a client got.
const fetchCompleted = "completed"

// imageManifestTypes are the media types of a platform's manifest, the only documents that are
// completed: an index's other platforms are not.
var imageManifestTypes = map[string]bool{
	ocispec.MediaTypeImageManifest: true, "application/vnd.docker.distribution.manifest.v2+json": true,
}

// OCIComplete starts the completion of the platform's manifest desc, which a client got at path
// in the oci proxy repo repo from its cached row, and returns at once (§11.5, step 5): in the
// background, on a context of its own, each blob it lists that the backend does not hold at the
// path is mounted from another path of the repo that holds it, offline too; then, unless the repo
// is offline, each one still missing and within the repo's maxFileSize and the registry's
// maxBlobSize is fetched under its claim, as a client's miss is, once no client of this replica
// asked for a blob of the path for Options.CompleteQuiet, and at most ociCompleteFetches at once
// on the replica. Each blob held pushes what became complete. An index, a HEAD (the caller does
// not call it) and a manifest the backend holds start nothing; concurrent calls for one manifest
// share one completion on a replica.
func (c *Core) OCIComplete(ctx context.Context, repo index.Repo, path string, desc ocispec.Descriptor) {
	if c.o.Mirror == nil || !imageManifestTypes[desc.MediaType] {
		return
	}
	key := pullKey(repo, path)
	c.pulls.touch(key) // the client's blob requests follow its manifest's
	start := time.Now()
	c.flights.join(fmt.Sprintf("ocicomplete:%s:%s", key, desc.Digest), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		p, err := c.resolveOCI(fctx, repo, path)
		if err != nil {
			return false, err
		}
		c.ociCompleteImage(fctx, p, desc.Digest, start)
		return false, nil
	})
}

// OCIPulling records a client's GET of a blob at path in the oci proxy repo repo until done is
// called: the completions of this replica fetch no blob of the path while one runs, nor until
// Options.CompleteQuiet after the last, so that a client's own requests hold their blobs' claims
// and get the tee (§11.8).
func (c *Core) OCIPulling(repo index.Repo, path string) (done func()) {
	return c.pulls.begin(pullKey(repo, path))
}

// ociCompleteImage completes the manifest m at p's path, cached as a row, from the GET of it
// at start: what the backend holds at another path is mounted, then, unless offline, what is
// still missing is fetched.
func (c *Core) ociCompleteImage(ctx context.Context, p *ociProxied, m digest.Digest, start time.Time) {
	listed := c.ociToComplete(ctx, p, m)
	if len(listed) == 0 {
		return
	}
	limit, bound := c.ociLimit(ctx, p)
	var missing []ocispec.Descriptor
	for _, l := range listed {
		switch {
		case limit >= 0 && l.Size > limit:
			c.o.Log.InfoContext(ctx, "proxy: a blob above the bound is left out of a completion; the manifest stays incomplete", "repo", p.id(),
				"path", p.path, "manifest", m.String(), "digest", l.Digest.String(), "size", l.Size, "bound", bound, "limit", limit)
		case !c.ociHeldAt(ctx, p, l) && !c.ociMount(ctx, p, l):
			missing = append(missing, l)
		}
	}
	c.ociPushUp(ctx, p, m)
	if len(missing) > 0 && p.up != nil {
		c.ociFetchMissing(ctx, p, m, missing, start)
	}
}

// ociToComplete returns the blobs the manifest m at p's path lists, from its cached row; none
// when the backend holds m, when there is no row, or when m is an index after all, whose
// platforms are not completed.
func (c *Core) ociToComplete(ctx context.Context, p *ociProxied, m digest.Digest) []ocispec.Descriptor {
	if held, err := c.isHeld(ctx, p, m); held || err != nil {
		return nil
	}
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, metaKey(kindOCIManifest, m))
	if err != nil || row == nil || row.State != index.ProxyMetaPresent {
		return nil
	}
	listed, err := listedBy(row.Content)
	if err != nil || slices.ContainsFunc(listed, func(l ocispec.Descriptor) bool { return manifestTypes[l.MediaType] }) {
		return nil
	}
	return listed
}

// ociFetchMissing fetches the blobs missing of the manifest m at p's path for its completion,
// each once the path is quiet (ociQuiet) and within the replica's bound of such fetches.
func (c *Core) ociFetchMissing(ctx context.Context, p *ociProxied, m digest.Digest, missing []ocispec.Descriptor, start time.Time) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, l := range missing {
		if !c.ociQuiet(ctx, p, start) {
			return
		}
		select {
		case c.completing <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Go(func() {
			defer func() { <-c.completing }()
			c.ociCompleteFetch(ctx, p, m, l)
		})
	}
}

// ociHeldAt reports whether the backend holds the blob l at p's path: its oci.held row, else a
// HEAD of it there, which writes the row (a purge of the cache drops rows, not blobs).
func (c *Core) ociHeldAt(ctx context.Context, p *ociProxied, l ocispec.Descriptor) bool {
	if held, err := c.isHeld(ctx, p, l.Digest); held || err != nil {
		return held
	}
	if ok, err := c.ociHasBlob(ctx, p, l); !ok || err != nil {
		return false
	}
	if err := c.ociHeldBlob(ctx, p, l.Digest); err != nil {
		c.o.Log.WarnContext(ctx, "proxy: a blob the backend holds not recorded as held", "repo", p.id(), "path", p.path,
			"digest", l.Digest.String(), "error", err.Error())
	}
	return true
}

// ociPushUp pushes the manifest m at p's path when the backend holds every blob it lists, and
// then what became complete above it, the indexes that list it.
func (c *Core) ociPushUp(ctx context.Context, p *ociProxied, m digest.Digest) {
	pushed, err := c.ociPushComplete(ctx, p, m)
	switch {
	case err != nil:
		c.o.Log.WarnContext(ctx, "proxy: a complete manifest not pushed; the next read of it tries again", "repo", p.id(),
			"path", p.path, "digest", m.String(), "error", err.Error())
	case pushed:
		c.ociComplete(ctx, p, m)
	}
}

// ociCompleteFetch makes the backend hold the blob l of the manifest m at p's path for a
// completion, once the path is quiet: held already, or mounted, else fetched under the repo's
// claim of the digest and then in the flight and under the claim a client's miss takes; a
// fetch counts as the result completed.
func (c *Core) ociCompleteFetch(ctx context.Context, p *ociProxied, m digest.Digest, l ocispec.Descriptor) {
	// Since the GET of the manifest, a client or the completion at another path may have made
	// the backend hold the blob: then it is held here, or mounted.
	got := func(ctx context.Context) (bool, error) { return c.ociHeldAt(ctx, p, l) || c.ociMount(ctx, p, l), nil }
	if ok, _ := got(ctx); ok {
		return
	}
	// The repo's claim of the digest: the completions at two paths that share the blob fetch it
	// once, and the one that waits mounts it from the other's path once it is held there.
	release, shared, err := c.ociClaimAt(ctx, p, "", l.Digest.String(), got)
	switch {
	case err != nil:
		c.o.Log.WarnContext(ctx, "proxy: a blob of a completion not fetched; the next GET of the manifest tries again", "repo", p.id(),
			"path", p.path, "manifest", m.String(), "digest", l.Digest.String(), "error", err.Error())
		return
	case shared:
		return
	}
	defer release()
	if ok, _ := got(ctx); ok { // held at another path before the claim was taken
		return
	}
	fl, joined := c.flights.join(blobFlight(p, l.Digest), func() (bool, error) {
		return c.ociFetchBlob(ctx, p, l, nil)
	})
	select {
	case <-fl.done:
	case <-ctx.Done():
		return
	}
	switch {
	case fl.err != nil:
		c.o.Log.WarnContext(ctx, "proxy: a blob of a completion not fetched; the next GET of the manifest tries again", "repo", p.id(),
			"path", p.path, "manifest", m.String(), "digest", l.Digest.String(), "error", fl.err.Error())
	case !joined && !fl.shared:
		ociCountFetch(p, fetchCompleted)
	}
}

// ociQuiet waits until no client of this replica asked for a blob at p's path for
// Options.CompleteQuiet, or until ociCompleteQuietMost after start; false when ctx ended first.
func (c *Core) ociQuiet(ctx context.Context, p *ociProxied, start time.Time) bool {
	quiet := c.o.CompleteQuiet
	if quiet < 0 {
		return ctx.Err() == nil
	}
	key := pullKey(p.repo, p.path)
	for {
		idle := c.pulls.idle(key)
		if idle >= quiet || time.Since(start) >= ociCompleteQuietMost {
			return ctx.Err() == nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(quiet - idle):
		}
	}
}

// pullKey is the key of a path of a repo in Core.pulls and in the flights.
func pullKey(repo index.Repo, path string) string { return strconv.FormatInt(repo.ID, 10) + ":" + path }

// pulls are the blob GETs of clients that run on this replica, by repo and path, with when the
// last one began or ended: what a completion waits on before it fetches.
type pulls struct {
	mu sync.Mutex
	m  map[string]*pathPulls
}

type pathPulls struct {
	running int
	last    time.Time
}

// begin records a GET at key until end is called.
func (q *pulls) begin(key string) (end func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	pp := q.at(key)
	pp.running++
	pp.last = time.Now()
	return func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		pp.running--
		pp.last = time.Now()
	}
}

// touch records a request at key that ended now.
func (q *pulls) touch(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.at(key).last = time.Now()
}

// idle is how long no GET ran at key: 0 while one runs. A key idle longer than
// ociCompleteQuietMost is forgotten.
func (q *pulls) idle(key string) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	pp, ok := q.m[key]
	switch {
	case !ok:
		return ociCompleteQuietMost
	case pp.running > 0:
		return 0
	}
	idle := time.Since(pp.last)
	if idle >= ociCompleteQuietMost {
		delete(q.m, key)
	}
	return idle
}

// at returns the entry of key, made when there is none; q.mu is held.
func (q *pulls) at(key string) *pathPulls {
	if q.m == nil {
		q.m = map[string]*pathPulls{}
	}
	pp := q.m[key]
	if pp == nil {
		pp = &pathPulls{}
		q.m[key] = pp
	}
	return pp
}
