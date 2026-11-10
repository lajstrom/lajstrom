package npm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The routes of a proxy repo of npm are the hosted repo's (docs/spec/http-routing.md §6): its
// packuments and its tarballs come from the core of proxy repos, Deps.Proxy, which asks the
// upstream and fetches a tarball on its first request; a write is 405.

// proxyCore returns the core of proxy repos, or an error wrapping format.ErrUnsupported when the
// server has none.
func (h *handlers) proxyCore(repo index.Repo) (format.Proxy, error) {
	if h.deps.Proxy == nil {
		return nil, fmt.Errorf("%w: npm/%s is a proxy repo, and this server serves none", format.ErrUnsupported, repo.Name)
	}
	return h.deps.Proxy, nil
}

// proxyDocument returns the packument of kind of name of a proxy repo, as the core renders and
// caches it. Its links are under server.externalURL; without one the core writes them from
// /npm/<repo>/ on, and they are made absolute here with the request's host, as a hosted repo's
// packument is rendered for the request (document).
func (h *handlers) proxyDocument(r *http.Request, repo index.Repo, name, kind string) (*index.Cache, error) {
	core, err := h.proxyCore(repo)
	if err != nil {
		return nil, err
	}
	c, err := core.Document(r.Context(), repo, name, kind)
	if err != nil || h.plugin.ExternalURL != "" {
		return c, err
	}
	out := *c
	out.Content = bytes.ReplaceAll(c.Content, []byte(`"tarball":"/`+Name+`/`), []byte(`"tarball":"`+requestBase(r)+`/`+Name+`/`))
	sum := sha256.Sum256(out.Content)
	out.SHA256 = hex.EncodeToString(sum[:])
	return &out, nil
}

// proxyTarball returns the tarball file of a proxy repo: held, or fetched from the upstream,
// checked and ingested by the core on a miss, by the principal of the request.
func (h *handlers) proxyTarball(ctx context.Context, repo index.Repo, name, file string) (*index.FileMatch, error) {
	core, err := h.proxyCore(repo)
	if err != nil {
		return nil, err
	}
	return core.File(ctx, repo, name+"/-/"+file)
}

// refuseWrite returns nil for a repo that takes the writes of npm, and for a proxy repo an
// error wrapping format.ErrMethodNotAllowed, once the principal may read it: a principal that
// may not is answered as for any request (rbac.md §3).
func (h *handlers) refuseWrite(ctx context.Context, repo index.Repo) error {
	if repo.Kind != index.KindProxy {
		return nil
	}
	if err := format.Authorize(ctx, h.deps, auth.ActionRead, repo); err != nil {
		return err
	}
	return fmt.Errorf("%w: npm/%s is a proxy of an upstream and takes no writes", format.ErrMethodNotAllowed, repo.Name)
}
