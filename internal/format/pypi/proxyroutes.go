package pypi

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The routes of a proxy repo of pypi are the hosted repo's (docs/spec/http-routing.md §6, pypi,
// Proxy): a project's page and its files come from the core of proxy and group repos,
// Deps.Proxy, which asks the upstream and fetches a file on its first request; an upload to a
// proxy is 405 (upload.go).

// proxied reports whether the core of proxy and group repos serves the repo's pages and files.
func proxied(repo index.Repo) bool { return repo.Kind == index.KindProxy }

// proxyCore returns the core of proxy and group repos, or an error wrapping
// format.ErrUnsupported when the server has none.
func (h *handlers) proxyCore(repo index.Repo) (format.Proxy, error) {
	if h.deps.Proxy == nil {
		return nil, fmt.Errorf("%w: pypi/%s is a %s repo, and this server serves none", format.ErrUnsupported, repo.Name, repo.Kind)
	}
	return h.deps.Proxy, nil
}

// proxyPage returns the page of a project of a proxy in kind: the JSON page as the core renders
// and caches it, and for the HTML form that page converted, with the sha256 of its bytes for its
// ETag.
func (h *handlers) proxyPage(ctx context.Context, repo index.Repo, project, kind string) (*index.Cache, error) {
	core, err := h.proxyCore(repo)
	if err != nil {
		return nil, err
	}
	c, err := core.Document(ctx, repo, project, kindJSON)
	if err != nil || kind == kindJSON {
		return c, err
	}
	content, err := htmlOfJSON(c.Content)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	return &index.Cache{RepoID: c.RepoID, Scope: c.Scope, Kind: kindHTML, Content: content, SHA256: hex.EncodeToString(sum[:]),
		RenderedAt: c.RenderedAt}, nil
}

// htmlOfJSON converts a JSON page of a project, as RenderIndex and RenderProxy write it, to the
// HTML form of PEP 503.
func htmlOfJSON(content []byte) ([]byte, error) {
	var page struct {
		Meta  map[string]string `json:"meta"`
		Name  string            `json:"name"`
		Files []struct {
			Filename       string            `json:"filename"`
			URL            string            `json:"url"`
			Hashes         map[string]string `json:"hashes"`
			RequiresPython string            `json:"requires-python"`
			CoreMetadata   json.RawMessage   `json:"core-metadata"`
			Yanked         json.RawMessage   `json:"yanked"`
		} `json:"files"`
	}
	if err := json.Unmarshal(content, &page); err != nil {
		return nil, fmt.Errorf("pypi: the JSON page: %w", err)
	}
	files := make([]projectFile, 0, len(page.Files))
	for _, f := range page.Files {
		files = append(files, projectFile{Filename: f.Filename, URL: f.URL, Hashes: f.Hashes, RequiresPython: f.RequiresPython,
			coreSHA256: coreHash(f.CoreMetadata, nil), yanked: yankReason(f.Yanked)})
	}
	return pageHTML(page.Name, cmp.Or(page.Meta["api-version"], apiVersion), files), nil
}

// proxyProjects returns the project list of a proxy, the projects it holds, in kind: rendered
// for the request and kept nowhere.
func (h *handlers) proxyProjects(ctx context.Context, repo index.Repo, kind string) (*index.Cache, error) {
	repos := []index.Repo{repo}
	byName := map[string]index.Package{}
	for _, r := range repos {
		pkgs, err := h.deps.Index.ListPackages(ctx, r.ID, "")
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			if _, ok := byName[p.Name]; !ok {
				byName[p.Name] = p
			}
		}
	}
	pkgs := make([]index.Package, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		pkgs = append(pkgs, byName[name])
	}
	docs, err := projectList(pkgs)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d.Kind == kind {
			sum := sha256.Sum256(d.Content)
			return &index.Cache{RepoID: repo.ID, Scope: scopeProjects, Kind: kind, Content: d.Content, SHA256: hex.EncodeToString(sum[:]),
				RenderedAt: time.Now().UTC().Truncate(time.Second)}, nil
		}
	}
	return nil, fmt.Errorf("pypi: no project list of kind %s", kind)
}

// proxyFile returns a file of a proxy, held or fetched by the core on a miss by the principal of
// ctx, and the repo that holds it.
func (h *handlers) proxyFile(ctx context.Context, repo index.Repo, project, version, filename string) (*index.FileMatch, index.Repo, error) {
	core, err := h.proxyCore(repo)
	if err != nil {
		return nil, repo, err
	}
	m, err := core.File(ctx, repo, filePath(project, version, filename))
	if err != nil {
		return nil, repo, err
	}
	if m.Package.RepoID != repo.ID {
		member, err := h.deps.Index.GetRepoByID(ctx, m.Package.RepoID)
		if err != nil {
			return nil, repo, fmt.Errorf("pypi: the member of %s/%s that holds %s: %w", repo.Format, repo.Name, filename, err)
		}
		repo = *member
	}
	return m, repo, nil
}
