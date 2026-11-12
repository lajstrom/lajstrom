package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// ociComplete pushes what became complete when d became held at p's path
// (docs/spec/proxy-and-group.md §11.5, step 3): each manifest or index that lists d and whose
// every blob or child is held is pushed under its keep-tag, which makes it held in turn, up to
// the indexes; and with each, the upstream's tags that name it (ociPushTags). A failure is
// logged: the next read of the manifest tries again (ociDocument, ociTagLater).
func (c *Core) ociComplete(ctx context.Context, p *ociProxied, d digest.Digest) {
	queue, seen := c.parents(ctx, p, d), map[digest.Digest]bool{}
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		if seen[m] {
			continue
		}
		seen[m] = true
		pushed, err := c.ociPushComplete(ctx, p, m)
		if err != nil {
			c.o.Log.WarnContext(ctx, "proxy: a complete manifest not pushed; the next read of it tries again", "repo", p.id(),
				"path", p.path, "digest", m.String(), "error", err.Error())
			continue
		}
		if pushed {
			queue = append(queue, c.parents(ctx, p, m)...)
		}
	}
}

// parents are the manifests and indexes at p's path that list d, as its oci.ref row says.
func (c *Core) parents(ctx context.Context, p *ociProxied, d digest.Digest) []digest.Digest {
	ref, _ := c.ociRef(ctx, p, d)
	return ref.Parents
}

// ociRef reads the oci.ref row of d at p's path; the zero row when there is none.
func (c *Core) ociRef(ctx context.Context, p *ociProxied, d digest.Digest) (ociRefRow, error) {
	var ref ociRefRow
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, metaKey(kindOCIRef, d))
	if row != nil {
		_ = json.Unmarshal(row.Content, &ref) // a row that does not read lists nothing
	}
	return ref, err
}

// ociPushComplete pushes the cached manifest or index m at p's path when the backend does not
// hold it yet and holds everything it lists; held reports that the backend holds m now.
func (c *Core) ociPushComplete(ctx context.Context, p *ociProxied, m digest.Digest) (held bool, err error) {
	if held, err := c.isHeld(ctx, p, m); held || err != nil {
		return held, err
	}
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, metaKey(kindOCIManifest, m))
	if err != nil || row == nil || row.State != index.ProxyMetaPresent {
		return false, err
	}
	listed, err := listedBy(row.Content)
	if err != nil {
		return false, err
	}
	for _, l := range listed {
		if ok, err := c.isHeld(ctx, p, l.Digest); !ok || err != nil {
			return false, err
		}
	}
	desc := ocispec.Descriptor{MediaType: row.ContentType, Digest: m, Size: int64(len(row.Content))}
	err = c.o.Mirror.MirrorManifest(ctx, p.repo, ingest.Mirror{Path: p.path, Desc: desc, Manifest: row.Content, Upstream: p.upstreamName()},
		auth.PrincipalFrom(ctx))
	if err != nil {
		return false, err
	}
	if err := c.ociHeld(ctx, p, m); err != nil {
		return false, err
	}
	c.ociPushTags(ctx, p, desc, row.Content)
	return true, nil
}

// ociPushTags pushes, or moves, each upstream tag whose oci.tag row names desc, now held, where
// the backend lets the repository's tags move (§11.6); a refusal turns the fallback on.
func (c *Core) ociPushTags(ctx context.Context, p *ociProxied, desc ocispec.Descriptor, raw []byte) {
	ref, err := c.ociRef(ctx, p, desc.Digest)
	if err != nil || len(ref.Tags) == 0 || !c.liveTags(ctx, p) {
		return
	}
	for _, tag := range ref.Tags {
		if !c.ociPushTag(ctx, p, tag, desc, raw, false) {
			return
		}
	}
}

// ociPushTag pushes tag to desc under the tag's lock (ociTagLock), and marks its row pushed: when
// the row, read again under the lock, names desc and is not pushed; with verify, also when it is
// pushed but the backend's tag, asked with one HEAD, names something else. A row that names
// another digest by then is left to the push of that digest. It reports whether more tags of the
// repository may be pushed: false once the backend refused one.
func (c *Core) ociPushTag(ctx context.Context, p *ociProxied, tag string, desc ocispec.Descriptor, raw []byte, verify bool) (more bool) {
	more = true
	err := c.ociTagLock(ctx, p, tag, func(ctx context.Context) error {
		t, _, err := ociTagNow(ctx, c.o.Index, p, tag)
		switch {
		case err != nil || t.Digest != desc.Digest || t.Pushed && !verify:
			return err
		case t.Pushed:
			if same, err := c.backendNames(ctx, p, tag, desc.Digest); same || err != nil {
				return err
			}
			c.o.Log.WarnContext(ctx, "proxy: the backend's tag is not the one recorded as pushed; it is pushed again", "repo", p.id(),
				"path", p.path, "tag", tag, "digest", desc.Digest.String())
		}
		err = c.o.Mirror.MirrorManifest(ctx, p.repo, ingest.Mirror{Path: p.path, Desc: desc, Manifest: raw, Tag: tag, Upstream: p.upstreamName()},
			auth.PrincipalFrom(ctx))
		if errors.Is(err, ingest.ErrConflict) {
			c.fixTags(ctx, p, tag, err)
			more = false
			return nil
		}
		if err != nil {
			return err
		}
		return c.ociTagRecord(ctx, p, tag, desc.Digest)
	})
	if err != nil {
		c.o.Log.WarnContext(ctx, "proxy: an upstream tag not pushed, or not recorded; the next read of it tries again", "repo", p.id(),
			"path", p.path, "tag", tag, "error", err.Error())
	}
	return more
}

// ociTagLock calls fn holding the lock of tag at p's path, which every push of an upstream tag
// holds from the read of its row to the record of the push, so that two replicas never push one
// tag at once (#879): Store.WithReferrersLock of the backend's OCI repository of the path and of
// ociTagKey(tag), across replicas as ingest's tag locks are (index-schema.md, Coordination). The
// push under it takes the lock of its digest too, and the record a connection of its own: a
// replica holds one tag's lock at a time (tagLocks), so that its pushes of several tags cannot
// take every connection of the pool while each waits for one more.
func (c *Core) ociTagLock(ctx context.Context, p *ociProxied, tag string, fn func(context.Context) error) error {
	select {
	case c.tagLocks <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.tagLocks }()
	return c.o.Index.WithReferrersLock(ctx, p.name, ociTagKey(tag), fn)
}

// ociTagKey is the subject of the lock of an upstream tag: the digest of its row's kind,
// oci.tag:<tag>, which is no manifest's.
func ociTagKey(tag string) digest.Digest { return digest.FromString(kindOCITag + tag) }

// ociTagNow reads the oci.tag row of tag at p's path on r; the zero tag when there is none, when
// it is absent, or when it does not read.
func ociTagNow(ctx context.Context, r index.Reader, p *ociProxied, tag string) (ociTagRow, *index.ProxyMeta, error) {
	var t ociTagRow
	row, err := metaRow(ctx, r, p.repo.ID, p.path, kindOCITag+tag)
	if err != nil || row == nil || row.State != index.ProxyMetaPresent || json.Unmarshal(row.Content, &t) != nil {
		return ociTagRow{}, nil, err
	}
	return t, row, nil
}

// ociTagRecord marks the oci.tag row of tag pushed at d: the row as it is now, and only while it
// names d, never a copy read before the push, which another replica may have replaced meanwhile;
// its checked_at stays. A concurrent write of the row makes the transaction read it again.
func (c *Core) ociTagRecord(ctx context.Context, p *ociProxied, tag string, d digest.Digest) error {
	return c.o.Index.Tx(index.WithRepeatableRead(ctx), func(w index.Writer) error {
		t, row, err := ociTagNow(ctx, w, p, tag)
		if err != nil || row == nil || t.Digest != d || t.Pushed {
			return err
		}
		t.Pushed = true
		content, _ := json.Marshal(t) // always marshals
		return w.PutProxyMeta(ctx, presentRow(p, row.Kind, content, row.ContentType, row.ETag, row.CheckedAt))
	})
}

// backendNames reports whether the backend's tag at p's path names d, with one HEAD; a tag the
// backend does not have names nothing.
func (c *Core) backendNames(ctx context.Context, p *ociProxied, tag string, d digest.Digest) (bool, error) {
	desc, err := p.rr.Resolve(ctx, tag)
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("proxy: the tag %s of %s in the backend: %w", tag, p.name, err)
	}
	return desc.Digest == d, nil
}

// liveTags reports whether the upstream's tags of p's repository are pushed to the backend: not
// on a registry with Capabilities.ImmutableTags, and not once it refused to move one
// (oci.fixed-tags).
func (c *Core) liveTags(ctx context.Context, p *ociProxied) bool {
	if caps, err := p.prov.Capabilities(ctx); err != nil || caps.ImmutableTags {
		return false
	}
	fixed, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, kindOCIFixedTags)
	return err == nil && fixed == nil
}

// fixTags turns the fallback on for p's repository: the backend refused to move tag, and from
// now on its upstream tags stay in proxy_meta only, on every replica, until a purge of the cache.
func (c *Core) fixTags(ctx context.Context, p *ociProxied, tag string, cause error) {
	c.o.Log.WarnContext(ctx, "proxy: the registry refuses to move a tag of the repository; its upstream tags stay in the cache only",
		"repo", p.id(), "registry", p.repo.Registry, "path", p.path, "tag", tag, "error", cause.Error())
	if err := c.o.Index.PutProxyMeta(ctx, presentRow(p, kindOCIFixedTags, []byte{}, "", "", c.o.Now())); err != nil {
		c.o.Log.ErrorContext(ctx, "proxy: the fallback of tags that do not move not recorded", "repo", p.id(), "path", p.path,
			"error", err.Error())
	}
}

// ociVerifyEvery is the window of the check of a pushed tag against the backend's
// (ociVerifyDue): about once per window for a tag that clients read, one HEAD of the backend.
const ociVerifyEvery = 10 * time.Minute

// ociVerifyDue reports whether a revalidation at now that found the upstream's tag where row
// says, checks the backend's tag (ociSyncTag with verify): when the row is pushed and its last
// revalidation, its checked_at, was in an earlier window of ociVerifyEvery. A tag that clients
// read is so checked about once per window, by whichever replica revalidates it first, with no
// state but checked_at.
func ociVerifyDue(row *index.ProxyMeta, now time.Time) bool {
	var t ociTagRow
	return json.Unmarshal(row.Content, &t) == nil && t.Pushed && !row.CheckedAt.Truncate(ociVerifyEvery).Equal(now.Truncate(ociVerifyEvery))
}

// ociTagLater starts ociSyncTag of tag at p's path in the background, once per replica, on a
// context of its own, and returns at once: a client's request never waits for the push of a tag
// (#879), which the tag's lock, a slow backend or the read back of an eventually consistent one
// may make longer than a client waits.
func (c *Core) ociTagLater(ctx context.Context, p *ociProxied, tag string, verify bool) {
	if c.o.Mirror == nil {
		return
	}
	what := "ocitagpush"
	if verify {
		what = "ocitagverify"
	}
	c.flights.join(fmt.Sprintf("%s:%d:%s:%s", what, p.repo.ID, p.path, tag), func() (bool, error) {
		fctx, cancel := c.detach(ctx)
		defer cancel()
		c.ociSyncTag(fctx, p, tag, verify)
		return false, nil
	})
}

// ociSyncTag pushes the tag at p's path to what its row names, when the backend holds it and
// the tag's push is not done yet: the completion that would have pushed it was lost to a
// concurrent write, or failed (§11.4, step 4). With verify, a row marked pushed is checked
// against the backend's tag, which is pushed again when it names something else: a push that
// landed after its replica lost the tag's lock, or a replica that died between its push and its
// record.
func (c *Core) ociSyncTag(ctx context.Context, p *ociProxied, tag string, verify bool) {
	t, row, err := ociTagNow(ctx, c.o.Index, p, tag)
	if err != nil || row == nil || t.Pushed && !verify || !c.liveTags(ctx, p) {
		return
	}
	if held, err := c.isHeld(ctx, p, t.Digest); !held || err != nil {
		return
	}
	m, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, metaKey(kindOCIManifest, t.Digest))
	if err != nil || m == nil || m.State != index.ProxyMetaPresent {
		return
	}
	c.ociPushTag(ctx, p, tag, ocispec.Descriptor{MediaType: m.ContentType, Digest: t.Digest, Size: int64(len(m.Content))}, m.Content, verify)
}
