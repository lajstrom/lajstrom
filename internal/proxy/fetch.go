package proxy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// The results of lajstrom_proxy_fetches_total.
const (
	fetchHeld     = "held"
	fetchFetched  = "fetched"
	fetchShared   = "shared"
	fetchMissing  = "missing"
	fetchRejected = "rejected"
	fetchError    = "error"
)

// artifact is the artifact a file request names, as the index and the claims key it.
type artifact struct {
	ref     format.ArtifactRef
	key     string // the plugin's key of the name
	pkgPath string // the package path (Plugin.EscapeName)
	tag     string // profile.Tag of version and variant
}

// subject is the artifact as the audit rows name it: name, version and variant.
func (a artifact) subject() string {
	s := a.ref.Name + " " + a.ref.Version
	if a.ref.Variant != "" {
		s += " " + a.ref.Variant
	}
	return s
}

// File returns the file that path names in the proxy repo repo (format.Proxy), as docs/spec/
// proxy-and-group.md §5 says: after the allow and deny lists, the file the repo holds, or else,
// unless the repo is offline, the file fetched from the upstream into the one write path by the
// principal of ctx, once across replicas, and served from the registry like a held one. The
// fetch runs on a context of its own: a client that leaves does not cancel it for the requests
// that wait on it, the server's shutdown does. A group's file is its members' (groupFile).
func (c *Core) File(ctx context.Context, repo index.Repo, path string) (*index.FileMatch, error) {
	if repo.Kind == index.KindGroup {
		return c.groupFile(ctx, repo, path)
	}
	return c.proxyFile(ctx, repo, path)
}

// proxyFile is File of a proxy repo.
func (c *Core) proxyFile(ctx context.Context, repo index.Repo, path string) (*index.FileMatch, error) {
	p, err := c.resolve(repo)
	if err != nil {
		return nil, err
	}
	a, err := p.artifact(path)
	if err != nil {
		return nil, err
	}
	m, _, err := c.proxyArtifact(ctx, p, a, nil, nil)
	return m, err
}

// proxyArtifact returns the file of the artifact a of a proxy repo, held or fetched (§5 from
// step 3), and how it came: fetchHeld, fetchFetched or fetchShared. entry and row are the
// artifact's entry in the name's document and that document's row when the caller has read
// them (a prefetch); nil looks them up (§5, step 5) once the artifact is found missing.
func (c *Core) proxyArtifact(ctx context.Context, p *proxied, a artifact, entry *format.UpstreamEntry, row *index.ProxyMeta) (*index.FileMatch, string, error) {
	if m, err := c.held(ctx, p, a); m != nil || err != nil {
		if m != nil {
			countFetch(p, fetchHeld)
		}
		return m, fetchHeld, err
	}
	if p.up == nil {
		return nil, "", fmt.Errorf("%w: proxy: %s is offline and does not hold %s", index.ErrNotFound, p.id(), a.ref.File)
	}
	if entry == nil {
		var err error
		if entry, row, err = c.entry(ctx, p, a); err != nil {
			return nil, "", err
		}
	}
	fl, joined := c.flights.join(fmt.Sprintf("%d/%s:%s", p.repo.ID, a.pkgPath, a.tag), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		return c.fetch(fctx, p, a, entry, row)
	})
	select {
	case <-fl.done:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	if fl.err != nil {
		return nil, "", fl.err
	}
	m, err := c.held(ctx, p, a)
	switch {
	case err != nil:
		return nil, "", err
	case m == nil:
		countFetch(p, fetchError)
		return nil, "", fmt.Errorf("%w: proxy: %s: %s was fetched and is not in the index", format.ErrBadGateway, p.id(), a.subject())
	case joined || fl.shared:
		countFetch(p, fetchShared)
		return m, fetchShared, nil
	}
	countFetch(p, fetchFetched)
	return m, fetchFetched, nil
}

// artifact maps a request path to the artifact, after the lists.
func (p *proxied) artifact(path string) (artifact, error) {
	ref, err := p.hooks.FileRequest(path)
	if err != nil {
		return artifact{}, err
	}
	return p.artifactOf(ref)
}

// artifactOf is the artifact that ref names, after the lists: a request path's, or an entry of
// a name's document with its first file (a prefetch).
func (p *proxied) artifactOf(ref format.ArtifactRef) (artifact, error) {
	key, err := p.key(ref.Name)
	if err != nil {
		return artifact{}, err
	}
	pkgPath, err := p.plugin.EscapeName(ref.Name)
	if err != nil {
		return artifact{}, fmt.Errorf("%w: %w", index.ErrNotFound, err)
	}
	return artifact{ref: ref, key: key, pkgPath: pkgPath, tag: profile.Tag(ref.Version, ref.Variant)}, nil
}

// held returns the file of the artifact that the index has, nil when it does not have the
// version, and index.ErrNotFound when it has the version without the file.
func (c *Core) held(ctx context.Context, p *proxied, a artifact) (*index.FileMatch, error) {
	return heldFile(ctx, c.o.Index, p.repo, a)
}

// heldFile returns the file of the artifact that repo holds in the index r, nil when it does not
// hold the version, and index.ErrNotFound when it holds the version without the file.
func heldFile(ctx context.Context, r index.Reader, repo index.Repo, a artifact) (*index.FileMatch, error) {
	pkg, err := index.PackageAt(ctx, r, repo.ID, a.pkgPath)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := r.GetVersion(ctx, pkg.ID, a.ref.Version, a.ref.Variant)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	files, err := r.ListFiles(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.Name == a.ref.File {
			return &index.FileMatch{File: f, Version: *v, Package: *pkg}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s/%s: %s has no file %s", index.ErrNotFound, repo.Format, repo.Name, a.subject(), a.ref.File)
}

// entry returns the entry of the artifact in the name's document (§5, step 5), brought up to
// date as a read of the document is, and the row it comes from. A version the document does
// not list, or a file its entry does not have, is index.ErrNotFound.
func (c *Core) entry(ctx context.Context, p *proxied, a artifact) (*format.UpstreamEntry, *index.ProxyMeta, error) {
	upErr := c.refresh(ctx, p, a.ref.Name, a.key, a.ref.Kind)
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, a.key, a.ref.Kind)
	switch {
	case err != nil:
		return nil, nil, err
	case row == nil && upErr != nil:
		return nil, nil, upErr
	case row == nil || row.State == index.ProxyMetaAbsent:
		countFetch(p, fetchMissing)
		return nil, nil, fmt.Errorf("%w: proxy: %s: upstream %s does not have %q", index.ErrNotFound, p.id(), p.settings.Upstream, a.ref.Name)
	}
	parsed, err := p.hooks.ParseMetadata(a.ref.Name, a.ref.Kind, row.Content)
	if err != nil {
		return nil, nil, fmt.Errorf("proxy: %s: the kept document of %q does not parse: %s", p.id(), a.ref.Name, err.Error())
	}
	for i, e := range parsed.Entries {
		if e.Version != a.ref.Version || e.Variant != a.ref.Variant {
			continue
		}
		for _, f := range e.Files {
			if f.Name == a.ref.File {
				return &parsed.Entries[i], row, nil
			}
		}
	}
	countFetch(p, fetchMissing)
	return nil, nil, fmt.Errorf("%w: proxy: %s: upstream %s does not list %s", index.ErrNotFound, p.id(), p.settings.Upstream, a.ref.File)
}

// fetch takes the artifact's claim and streams its file from the upstream into the registry
// through the one write path, checked on its way (§5, steps 6 to 10), on its own context. shared
// reports that another replica fetched it while this one waited.
func (c *Core) fetch(ctx context.Context, p *proxied, a artifact, entry *format.UpstreamEntry, row *index.ProxyMeta) (shared bool, err error) {
	principal := auth.PrincipalFrom(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	held := func(ctx context.Context) (bool, error) {
		m, err := c.held(ctx, p, a)
		return m != nil, err
	}
	// A claim taken over cancels the fetch: this replica does not hold it any more.
	release, shared, err := c.claim(ctx, fetchClaim{repoID: p.repo.ID, pkg: a.pkgPath, tag: a.tag, repo: p.id(), what: a.subject(),
		attrs: []any{"artifact", a.subject()}, done: held, lost: cancel})
	if shared || err != nil {
		return shared, err
	}
	defer release()
	// Another replica's fetch may have recorded the version and released its claim between this
	// request's look-up and its claim: the version is in the index before its claim goes, so
	// nothing is downloaded twice.
	if ok, err := held(ctx); ok || err != nil {
		return ok, err
	}
	body, err := c.download(ctx, p, a, entry, row)
	if err != nil {
		return false, err
	}
	defer body.close()
	up := format.StreamUpload{Name: a.ref.Name, File: format.UploadedFile{Name: body.name, Size: -1}, Body: body,
		Params: entry.Params, Origin: body.origin}
	pub, err := c.o.Ingest.UploadStream(ctx, p.repo, up, principal)
	switch {
	case errors.Is(err, index.ErrConflict):
		// The tag holds other bytes: the repo serves what it holds (ADR 0005).
		c.o.Log.WarnContext(ctx, "proxy: the upstream serves other bytes than the repo holds under the tag; the repo's are served",
			"repo", p.id(), "upstream", p.settings.Upstream, "artifact", a.subject(), "error", err.Error())
		return false, nil
	case err != nil:
		return false, c.refused(ctx, p, a, body, err)
	}
	c.o.Log.InfoContext(ctx, "proxy: fetched from the upstream and published", "repo", p.id(), "upstream", p.settings.Upstream,
		"artifact", a.subject(), "digest", pub.Digest.String(), "principal", principal.String())
	return false, nil
}

// fetchClaim is a fetch claim to take (§5, step 6): its key, what a wait for another replica's
// fetch asks, and what logs and errors name. A file's key is the artifact's package path and
// tag; an oci document's or blob's, its path and digest (§11.4, ociClaim).
type fetchClaim struct {
	repoID   int64
	pkg, tag string
	repo     string // the repo, as logs and errors name it
	what     string // what is fetched, as errors name it: the artifact, path@digest
	attrs    []any  // what is fetched, as log lines name it
	// done reports whether another replica's fetch is there; claim asks it only while another
	// replica holds the claim.
	done func(context.Context) (bool, error)
	// lost, when not nil, is called when another replica takes the claim over: the fetch stops.
	// Without it the fetch goes on, as an oci fetch does: the backend takes the second copy of
	// the same bytes.
	lost context.CancelFunc
}

// logAttrs is the attributes of a log line about the claim, followed by more.
func (f fetchClaim) logAttrs(more ...any) []any {
	return slices.Concat([]any{"repo", f.repo}, f.attrs, more)
}

// claim takes the claim f (§5, step 6). While another replica holds a fresh claim, the request
// waits, asking f.done after WaitFirst and then twice as long each time, at most WaitLongest
// apart: shared is true when f.done reports the other replica's fetch; a claim gone without it
// is taken, once; a claim that stopped beating is taken over. The claim taken beats every
// ClaimHeartbeat until release, which removes it. Another replica's fetch may have ended
// between the caller's look and the claim: the caller looks again before it fetches.
func (c *Core) claim(ctx context.Context, f fetchClaim) (release func(), shared bool, err error) {
	cl := index.FetchClaim{RepoID: f.repoID, Package: f.pkg, Tag: f.tag, Principal: auth.PrincipalFrom(ctx).String(), Replica: c.o.Replica}
	w := claimWait{next: c.o.WaitFirst}
	for {
		now := c.o.Now()
		cl.ClaimedAt, cl.HeartbeatAt = now, now
		err := c.o.Index.ClaimFetch(ctx, &cl, now.Add(-c.o.ClaimStale))
		if err == nil {
			break
		}
		if !errors.Is(err, index.ErrConflict) {
			return nil, false, err
		}
		if shared, err := c.wait(ctx, f, &w); shared || err != nil {
			return nil, shared, err
		}
	}
	stop := c.heartbeat(ctx, f, cl.ID)
	return func() {
		stop()
		if err := c.o.Index.ReleaseFetchClaim(context.WithoutCancel(ctx), cl.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
			c.o.Log.WarnContext(ctx, "proxy: a fetch claim not released; it goes stale", f.logAttrs("error", err.Error())...)
		}
	}, false, nil
}

// claimWait is the state of a wait for another replica's claim: the next wait, and whether a
// claim gone without the fetch's result was taken once already.
type claimWait struct {
	next    time.Duration
	retried bool
}

// wait waits while another replica holds the claim f: shared when f.done reports its fetch, nil
// when the claim is gone (once) or stale and may be taken.
func (c *Core) wait(ctx context.Context, f fetchClaim, w *claimWait) (shared bool, err error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(w.next):
		}
		w.next = min(2*w.next, c.o.WaitLongest)
		if ok, err := f.done(ctx); ok || err != nil {
			return ok, err
		}
		other, err := c.o.Index.GetFetchClaim(ctx, f.repoID, f.pkg, f.tag)
		switch {
		case errors.Is(err, index.ErrNotFound) && w.retried:
			return false, fmt.Errorf("%w: proxy: %s: the fetch of %s by another replica ended without it", format.ErrBadGateway,
				f.repo, f.what)
		case errors.Is(err, index.ErrNotFound):
			w.retried = true
			return false, nil
		case err != nil:
			return false, err
		case other.HeartbeatAt.Before(c.o.Now().Add(-c.o.ClaimStale)):
			return false, nil // stale: ClaimFetch takes it over
		}
	}
}

// heartbeat beats the claim id every ClaimHeartbeat until stop is called. A claim that is gone,
// taken over by a replica that found it stale, calls f.lost when there is one.
func (c *Core) heartbeat(ctx context.Context, f fetchClaim, id int64) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(c.o.ClaimHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			err := c.o.Index.HeartbeatFetchClaim(ctx, id, c.o.Now())
			switch {
			case errors.Is(err, index.ErrNotFound) && f.lost != nil:
				c.o.Log.WarnContext(ctx, "proxy: the fetch claim was taken over; the fetch stops", f.logAttrs("claim", id)...)
				f.lost()
				return
			case errors.Is(err, index.ErrNotFound):
				c.o.Log.WarnContext(ctx, "proxy: the fetch claim was taken over by another replica; the fetch goes on", f.logAttrs("claim", id)...)
				return
			case err != nil && ctx.Err() == nil:
				c.o.Log.WarnContext(ctx, "proxy: a fetch claim did not beat", f.logAttrs("claim", id, "error", err.Error())...)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// countFetch records a file request in lajstrom_proxy_fetches_total.
func countFetch(p *proxied, result string) {
	metrics.ProxyFetches.WithLabelValues(p.settings.Upstream, result).Inc()
}
