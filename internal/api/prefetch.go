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
// them (docs/spec/proxy-and-group.md §5.1), and images into an oci proxy repo before a client
// pulls them (§11.10): *proxy.Core.
type Prefetcher interface {
	Prefetch(ctx context.Context, repo index.Repo, names []string, all bool) ([]proxy.PrefetchResult, error)
	PrefetchOCI(ctx context.Context, repo index.Repo, images, platforms []string) ([]proxy.PrefetchResult, error)
}

// PrefetchRequest is the body of POST …/prefetch (docs/api.md, Prefetch a proxy repo): names
// and allVersions of a format's proxy repo, images and platforms of an oci one.
type PrefetchRequest struct {
	Names       []string `json:"names,omitempty"`
	AllVersions bool     `json:"allVersions,omitempty"`
	Images      []string `json:"images,omitempty"`
	Platforms   []string `json:"platforms,omitempty"`
}

// PrefetchAnswer is its answer: a result per version, and per name that failed before its
// versions, in the order of the names; of an oci repo, per image, platform and blob not held.
type PrefetchAnswer struct {
	Results []proxy.PrefetchResult `json:"results"`
}

// prefetch answers POST /api/v1/repos/{format}/{repo}/prefetch: the versions of 1 to 100 names
// fetched into a proxy repo as misses would be, or the images of 1 to 100 references into an oci
// proxy repo as pulls would be, by the caller (manage on the repo, checked before the body is
// read). 200 with the results; 400 on a repo that is not a proxy or the other format's body, 409
// on an offline one.
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
	if err := req.check(f, name); err != nil {
		return 0, nil, err
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("prefetch into", f, name, err)
	}
	if repo.Kind != index.KindProxy {
		return 0, nil, invalid("repo %s/%s is a %s repo: only a proxy repo takes a prefetch", f, name, repo.Kind)
	}
	var results []proxy.PrefetchResult
	if f == formatOCI {
		results, err = h.prefetcher.PrefetchOCI(ctx, *repo, req.Images, req.Platforms)
	} else {
		results, err = h.prefetcher.Prefetch(ctx, *repo, req.Names, req.AllVersions)
	}
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, PrefetchAnswer{Results: results}, nil
}

// check refuses a body of the other format's keys, and a list of names or images that is not 1
// to proxy.MaxPrefetchNames non-empty strings: the keys of an oci repo are images and platforms,
// those of the others names and allVersions.
func (req *PrefetchRequest) check(f, name string) error {
	key, list := "names", req.Names
	switch {
	case f == formatOCI && (len(req.Names) > 0 || req.AllVersions):
		return invalid("an oci proxy repo takes images and platforms, not names or allVersions")
	case f == formatOCI:
		key, list = "images", req.Images
	case len(req.Images) > 0 || len(req.Platforms) > 0:
		return invalid("images and platforms are an oci proxy repo's; %s/%s takes names", f, name)
	}
	if n := len(list); n == 0 || n > proxy.MaxPrefetchNames {
		return invalid("%s: %d of them; a prefetch takes 1 to %d", key, n, proxy.MaxPrefetchNames)
	}
	for i, n := range list {
		if n == "" {
			return invalid("%s[%d]: empty", key, i)
		}
	}
	return nil
}
