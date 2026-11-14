package npm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The routes of a proxy repo and of a group of npm are the hosted repo's (docs/spec/
// http-routing.md §6): their packuments and their tarballs come from the core of proxy and group
// repos, Deps.Proxy, which asks the upstream and fetches a tarball on its first request, and
// merges a group's members; a write to a proxy is 405, a write to a group its hosted member's.

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
	out.Content = bytes.ReplaceAll(c.Content, []byte(tarballLink+`/`+Name+`/`), []byte(tarballLink+requestBase(r)+`/`+Name+`/`))
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

// proxyAudit forwards an audit request on route to the upstream of a proxy repo, without the
// names that the repo's deny list refuses, which never leave the server (http-routing.md, npm,
// Audit): the bulk request loses them (forwardBulk); npm 6's tree, the two other routes, cannot
// lose a package without what hangs below it, and is forwarded whole when it names none and
// 501 when it names one.
func (h *handlers) proxyAudit(w http.ResponseWriter, r *http.Request, repo index.Repo, route string, body io.Reader) {
	core, err := h.proxyCore(repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if route == auditBulk {
		names, err := bulkNames(body)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		h.forwardBulk(w, r, core, repo, names)
		return
	}
	tree, err := io.ReadAll(body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	name, pattern, err := deniedInTree(core, repo, tree)
	switch {
	case err != nil:
		h.fail(w, r, err)
		return
	case name != "":
		h.fail(w, r, fmt.Errorf("%w: npm 6's audit through npm/%s names %q, which deny: %s keeps on the server: "+
			"its tree would carry the name or lose what hangs below it", format.ErrUnsupported, repo.Name, name, pattern))
		return
	}
	h.forwardAudit(w, r, core, repo, route, bytes.NewReader(tree))
}

// forwardBulk forwards the bulk audit names to the upstream of the proxy repo without the names
// its deny list refuses; {} without a request when no name is left. The answer says nothing of
// the names left out.
func (h *handlers) forwardBulk(w http.ResponseWriter, r *http.Request, core format.Proxy, repo index.Repo, names map[string]json.RawMessage) {
	for name := range names {
		_, denied, err := core.Denies(repo, name)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if denied {
			delete(names, name)
		}
	}
	if len(names) == 0 {
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	h.forwardAudit(w, r, core, repo, auditBulk, bytes.NewReader(mustJSON(names)))
}

// bulkNames reads the bulk audit body, a JSON object of names: anything else wraps
// format.ErrInvalid, and a body past the bound is the *http.MaxBytesError of auditBody (413).
func bulkNames(body io.Reader) (map[string]json.RawMessage, error) {
	var names map[string]json.RawMessage
	if err := json.NewDecoder(body).Decode(&names); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: the audit body is not a JSON object of names: %w", format.ErrInvalid, err)
	}
	return names, nil
}

// auditNode is a node of npm 6's audit tree as far as its names go: the root, with the
// project's name, and every dependency, with the names it requires and those below it.
type auditNode struct {
	Name         string                     `json:"name"`
	Requires     map[string]json.RawMessage `json:"requires"`
	Dependencies map[string]auditNode       `json:"dependencies"`
}

// deniedInTree returns the first name, in order, of npm 6's audit tree that the proxy repo's
// deny list refuses, and the pattern; "" when it names none. A tree that does not read wraps
// format.ErrInvalid.
func deniedInTree(core format.Proxy, repo index.Repo, tree []byte) (name, pattern string, err error) {
	var root auditNode
	if err := json.Unmarshal(tree, &root); err != nil {
		return "", "", fmt.Errorf("%w: the audit body is not npm 6's dependency tree: %w", format.ErrInvalid, err)
	}
	names := treeNames(root, []string{root.Name})
	slices.Sort(names)
	for _, n := range slices.Compact(names) {
		if n == "" {
			continue
		}
		pattern, denied, err := core.Denies(repo, n)
		if err != nil || denied {
			return n, pattern, err
		}
	}
	return "", "", nil
}

// treeNames appends to names those that n requires and those of its dependencies, below it too.
func treeNames(n auditNode, names []string) []string {
	for req := range n.Requires {
		names = append(names, req)
	}
	for dep, below := range n.Dependencies {
		names = treeNames(below, append(names, dep))
	}
	return names
}

// forwardAudit sends an audit request on route to the upstream of a proxy repo through the
// core (format.Proxy.Forward): a POST of the same route below the upstream's URL, the body
// decoded. The upstream's answer is the answer, its status and its body as it sent them, with
// Content-Type application/json and its npm-notice, which npm prints, and no other header of
// the upstream's (http-routing.md, npm). Nothing is kept.
func (h *handlers) forwardAudit(w http.ResponseWriter, r *http.Request, core format.Proxy, repo index.Repo, route string, body io.Reader) {
	resp, err := core.Forward(r.Context(), repo, format.ForwardRequest{Method: http.MethodPost, Path: "-/" + route,
		ContentType: "application/json", Accept: "application/json", Body: body})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if notice := resp.Header.Get("npm-notice"); notice != "" {
		w.Header().Set("npm-notice", notice)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		h.deps.Log.WarnContext(r.Context(), "npm: the upstream's audit answer broke off", "repo", repo.Name, "error", err.Error())
	}
}

// writeTarget returns the request and the repo that serve a write route sent to repo
// (format.WriteTarget, docs/spec/proxy-and-group.md §8.4): a hosted repo itself; a group's
// hosted member, once the principal may read the group, with the group in the request's context
// for the audit rows and the denials; for a proxy, and a group without a hosted member, an
// error wrapping format.ErrMethodNotAllowed once the principal may read it.
func (h *handlers) writeTarget(r *http.Request, repo index.Repo) (*http.Request, index.Repo, error) {
	ctx, target, err := format.WriteTarget(r.Context(), h.deps, repo)
	if err != nil {
		return r, repo, err
	}
	return r.WithContext(ctx), target, nil
}

// writeDocument answers ?write=true on a group, the read that npm deprecate and unpublish send
// back changed: the hosted member's own full packument, its links under the group (§8.4).
func (h *handlers) writeDocument(r *http.Request, group index.Repo, name, kind string) (*index.Cache, error) {
	r, target, err := h.writeTarget(r, group)
	if err != nil {
		return nil, err
	}
	c, err := h.document(r, target, name, kind)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(h.plugin.ExternalURL, "/")
	if base == "" {
		base = requestBase(r)
	}
	d, err := h.plugin.MergeGroup(r.Context(), group, name, kind, []format.MemberDocument{{Repo: target, Content: c.Content}},
		base+"/"+Name+"/"+group.Name+"/")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(d.Content)
	return &index.Cache{RepoID: group.ID, Scope: name, Kind: kind, Content: d.Content, SHA256: hex.EncodeToString(sum[:]),
		RenderedAt: c.RenderedAt}, nil
}
