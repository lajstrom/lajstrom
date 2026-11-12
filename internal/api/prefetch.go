package api

import (
	"context"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// Prefetcher fetches versions of a list of names into a proxy repo before a client asks for
// them (docs/spec/proxy-and-group.md §5.1): *proxy.Core.
type Prefetcher interface {
	Prefetch(ctx context.Context, repo index.Repo, names []string, all bool) ([]proxy.PrefetchResult, error)
}

// PrefetchRequest is the body of POST …/prefetch (docs/api.md, Prefetch a proxy repo).
type PrefetchRequest struct {
	Names       []string `json:"names"`
	AllVersions bool     `json:"allVersions"`
}

// PrefetchAnswer is its answer: a result per version, and per name that failed before its
// versions, in the order of the names.
type PrefetchAnswer struct {
	Results []proxy.PrefetchResult `json:"results"`
}

// prefetch answers POST /api/v1/repos/{format}/{repo}/prefetch: the versions of 1 to 100 names
// fetched into a proxy repo as misses would be, by the caller (manage on the repo, checked
// before the body is read). 200 with a result each; 400 on a repo that is not a proxy, 409 on
// an offline one.
func (h *handler) prefetch(w http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	if h.prefetcher == nil {
		return 0, nil, &clientError{msg: "prefetch is not enabled in this server", err: format.ErrUnsupported}
	}
	var req PrefetchRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	if n := len(req.Names); n == 0 || n > proxy.MaxPrefetchNames {
		return 0, nil, invalid("names: %d of them; a prefetch takes 1 to %d", n, proxy.MaxPrefetchNames)
	}
	for i, n := range req.Names {
		if n == "" {
			return 0, nil, invalid("names[%d]: empty", i)
		}
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("prefetch into", f, name, err)
	}
	if repo.Kind != index.KindProxy {
		return 0, nil, invalid("repo %s/%s is a %s repo: only a proxy repo takes a prefetch", f, name, repo.Kind)
	}
	results, err := h.prefetcher.Prefetch(ctx, *repo, req.Names, req.AllVersions)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, PrefetchAnswer{Results: results}, nil
}
