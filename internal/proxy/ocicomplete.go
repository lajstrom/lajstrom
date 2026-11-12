package proxy

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// ociComplete pushes what became complete when d became held at p's path
// (docs/spec/proxy-and-group.md §11.5, step 3): each manifest or index that lists d and whose
// every blob or child is held is pushed under its keep-tag, which makes it held in turn, up to
// the indexes; and with each, the upstream's tags that name it (ociPushTags). A failure is
// logged: the next read of the manifest tries again (ociDocument, ociTagPushed).
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
		if !c.ociPushTag(ctx, p, tag, desc, raw) {
			return
		}
	}
}

// ociPushTag pushes tag to desc when its row names desc and the push is not done yet, and
// marks the row pushed; it reports whether more tags of the repository may be pushed: false
// once the backend refused one.
func (c *Core) ociPushTag(ctx context.Context, p *ociProxied, tag string, desc ocispec.Descriptor, raw []byte) bool {
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, kindOCITag+tag)
	if err != nil || row == nil || row.State != index.ProxyMetaPresent {
		return true
	}
	var t ociTagRow
	if json.Unmarshal(row.Content, &t) != nil || t.Digest != desc.Digest || t.Pushed {
		return true
	}
	err = c.o.Mirror.MirrorManifest(ctx, p.repo, ingest.Mirror{Path: p.path, Desc: desc, Manifest: raw, Tag: tag, Upstream: p.upstreamName()},
		auth.PrincipalFrom(ctx))
	switch {
	case errors.Is(err, ingest.ErrConflict):
		c.fixTags(ctx, p, tag, err)
		return false
	case err != nil:
		c.o.Log.WarnContext(ctx, "proxy: an upstream tag not pushed; the next read of it tries again", "repo", p.id(), "path", p.path,
			"tag", tag, "error", err.Error())
		return true
	}
	t.Pushed = true
	content, _ := json.Marshal(t) // always marshals
	m := presentRow(p, row.Kind, content, row.ContentType, row.ETag, row.CheckedAt)
	if err := c.o.Index.PutProxyMeta(ctx, m); err != nil {
		c.o.Log.WarnContext(ctx, "proxy: a pushed tag not recorded; the next read pushes it again", "repo", p.id(), "tag", tag,
			"error", err.Error())
	}
	return true
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

// ociTagPushed pushes the tag that names desc when the backend holds desc and the tag's push is
// not done yet: the completion that would have pushed it was lost to a concurrent write, or
// failed (§11.4, step 4).
func (c *Core) ociTagPushed(ctx context.Context, p *ociProxied, tag string, desc ocispec.Descriptor) {
	if held, err := c.isHeld(ctx, p, desc.Digest); !held || err != nil || !c.liveTags(ctx, p) {
		return
	}
	row, err := metaRow(ctx, c.o.Index, p.repo.ID, p.path, metaKey(kindOCIManifest, desc.Digest))
	if err != nil || row == nil || row.State != index.ProxyMetaPresent {
		return
	}
	c.ociPushTag(ctx, p, tag, ocispec.Descriptor{MediaType: row.ContentType, Digest: desc.Digest, Size: int64(len(row.Content))}, row.Content)
}
