package proxy

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The mount of an oci proxy's blob from another path of the repo where the backend holds it
// (docs/spec/proxy-and-group.md §11.5, step 2): containerd keeps layers by digest across images
// and asks only for those it does not hold, so a layer that two images share is asked for at one
// path only (#876).

// ociHoldersRow is the content of an oci.holders row, the repo's (its name is ""): the paths of
// the repo where the backend holds a blob, the newest last, at most ociHoldersMost. A write reads
// the row and replaces it, so two replicas may each lose the other's path: that costs a fetch,
// never a wrong answer, since a blob is mounted only from a path that answers its HEAD.
type ociHoldersRow struct {
	Paths []string `json:"paths"`
}

// ociHoldersMost is how many paths an oci.holders row keeps; one that holds the blob is enough.
const ociHoldersMost = 8

// fetchMounted is the result of lajstrom_proxy_fetches_total of a miss that the backend mounted
// from another path of the repo, with no request to the upstream.
const fetchMounted = "mounted"

// ociHeldBlob records that the backend holds the blob d at p's path: its oci.held row, and the
// path in the repo's oci.holders row of d, in one transaction.
func (c *Core) ociHeldBlob(ctx context.Context, p *ociProxied, d digest.Digest) error {
	now := c.o.Now()
	return c.o.Index.Tx(ctx, func(w index.Writer) error {
		if err := w.PutProxyMeta(ctx, presentRow(p, metaKey(kindOCIHeld, d), []byte{}, "", "", now)); err != nil {
			return err
		}
		holders, err := c.ociHolders(ctx, w, p, d)
		if err != nil || slices.Contains(holders.Paths, p.path) {
			return err
		}
		paths := append(holders.Paths, p.path)
		content, _ := json.Marshal(ociHoldersRow{Paths: paths[max(0, len(paths)-ociHoldersMost):]}) // always marshals
		row := presentRow(p, metaKey(kindOCIHolders, d), content, "application/json", "", now)
		row.Name = ""
		return w.PutProxyMeta(ctx, row)
	})
}

// ociHolders reads the repo's oci.holders row of d on r; no paths when there is none, or when it
// does not read.
func (c *Core) ociHolders(ctx context.Context, r index.Reader, p *ociProxied, d digest.Digest) (ociHoldersRow, error) {
	var holders ociHoldersRow
	row, err := metaRow(ctx, r, p.repo.ID, "", metaKey(kindOCIHolders, d))
	if row != nil {
		_ = json.Unmarshal(row.Content, &holders) // a row that does not read is replaced
	}
	return holders, err
}

// ociHolder returns a path of the repo other than p's whose backend repository holds the blob
// desc: of those its oci.holders row lists, the newest first, the first that answers a HEAD of
// it; "" for none.
func (c *Core) ociHolder(ctx context.Context, p *ociProxied, desc ocispec.Descriptor) string {
	holders, err := c.ociHolders(ctx, c.o.Index, p, desc.Digest)
	if err != nil {
		c.o.Log.WarnContext(ctx, "proxy: where the backend holds a blob not read; it is fetched", "repo", p.id(), "digest",
			desc.Digest.String(), "error", err.Error())
		return ""
	}
	for _, from := range slices.Backward(holders.Paths) {
		if from == p.path {
			continue
		}
		rr, err := p.prov.Repository(ctx, ociName(p.repo, from))
		if err != nil {
			continue
		}
		if ok, err := rr.Exists(ctx, desc); ok && err == nil {
			return from
		}
	}
	return ""
}

// ociHeldOrMounted reports whether the backend holds the blob desc at p's path, already or once
// mounted from another path of the repo.
func (c *Core) ociHeldOrMounted(ctx context.Context, p *ociProxied, desc ocispec.Descriptor) (bool, error) {
	if held, err := c.ociHasBlob(ctx, p, desc); held || err != nil {
		return held, err
	}
	return c.ociMount(ctx, p, desc), nil
}

// ociMount makes the backend hold the blob desc at p's path by a mount from another path of the
// repo where it holds it (§11.5, step 2), on a context of its own, counts it as the result
// mounted, and pushes what became complete; it reports whether the backend mounted it. Nothing
// is asked of the upstream, so an offline repo mounts too. A backend that does not mount, or
// fails, leaves the blob to the caller: a fetch from the upstream, or a 404 offline.
func (c *Core) ociMount(ctx context.Context, p *ociProxied, desc ocispec.Descriptor) bool {
	if c.o.Mirror == nil {
		return false
	}
	fctx, cancel := c.detach(ctx)
	defer cancel()
	from := c.ociHolder(fctx, p, desc)
	if from == "" {
		return false
	}
	mounted, err := c.o.Mirror.MirrorMount(fctx, p.repo, p.path, from, desc)
	switch {
	case err != nil:
		c.o.Log.WarnContext(ctx, "proxy: a blob not mounted from another path of the repo; it is fetched", "repo", p.id(), "path", p.path,
			"from", from, "digest", desc.Digest.String(), "error", err.Error())
		return false
	case !mounted:
		c.o.Log.InfoContext(ctx, "proxy: the registry opened a session instead of a mount; the blob is fetched", "repo", p.id(),
			"registry", p.repo.Registry, "path", p.path, "from", from, "digest", desc.Digest.String())
		return false
	}
	ociCountFetch(p, fetchMounted)
	c.o.Log.InfoContext(ctx, "proxy: a blob mounted from another path of the repo", "repo", p.id(), "path", p.path, "from", from,
		"digest", desc.Digest.String(), "size", desc.Size, "principal", auth.PrincipalFrom(ctx).String())
	if err := c.ociHeldBlob(fctx, p, desc.Digest); err != nil {
		c.o.Log.WarnContext(ctx, "proxy: a mounted blob not recorded as held", "repo", p.id(), "path", p.path, "digest",
			desc.Digest.String(), "error", err.Error())
		return true
	}
	c.ociComplete(fctx, p, desc.Digest)
	return true
}
