package npm

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The routes of the npm CLI below /npm/<repo>/-/ that read (docs/spec/http-routing.md, npm):
// ping, whoami, search and audit, and the web login, which Lajstrom does not have.

// maxAuditBody bounds the body of an audit request, decoded.
const maxAuditBody = 16 << 20

// The bounds of a search page.
const (
	searchSize    = 20
	maxSearchSize = 250
)

// ping answers npm ping: 200 {} to a principal that may read the repo.
func (h *handlers) ping(w http.ResponseWriter, r *http.Request, repo index.Repo) {
	if err := format.Authorize(r.Context(), h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// whoami answers npm whoami: the user's name, token:<id> for a deploy token, to a principal
// that may read the repo; 401 to anonymous, also on a repo with anonymous read.
func (h *handlers) whoami(w http.ResponseWriter, r *http.Request, repo index.Repo) {
	ctx := r.Context()
	p := auth.PrincipalFrom(ctx)
	if p.Kind == auth.KindAnonymous {
		h.fail(w, r, fmt.Errorf("%w: npm whoami needs credentials", format.ErrUnauthenticated))
		return
	}
	if err := format.Authorize(ctx, h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	name := p.String()
	if p.Kind == auth.KindUser {
		u, err := auth.LookupUser(ctx, h.deps.Index, p)
		if err != nil {
			h.fail(w, r, fmt.Errorf("npm: whoami: %w", err))
			return
		}
		name = u.Name
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": name})
}

// searchObject is one package of a search answer. npm 10 and 11 read maintainers as a list and
// fail without one: it is empty, Lajstrom keeps no maintainers.
type searchObject struct {
	Package struct {
		Name        string          `json:"name"`
		Version     string          `json:"version"`
		Description string          `json:"description,omitempty"`
		Keywords    json.RawMessage `json:"keywords,omitempty"`
		Date        string          `json:"date,omitempty"`
		Maintainers []struct{}      `json:"maintainers"`
	} `json:"package"`
	Score struct {
		Final  float64            `json:"final"`
		Detail map[string]float64 `json:"detail"`
	} `json:"score"`
	SearchScore float64 `json:"searchScore"`
}

// search answers npm search from the index, never from an upstream: the repo's packages whose
// name contains text without regard to case, sorted by name, from and size (20 by default, at
// most 250) paginating them; each with the version latest names, its description and its date.
func (h *handlers) search(w http.ResponseWriter, r *http.Request, repo index.Repo) {
	ctx := r.Context()
	if err := format.Authorize(ctx, h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	text := strings.ToLower(q.Get("text"))
	size, from := queryInt(q.Get("size"), searchSize, maxSearchSize), queryInt(q.Get("from"), 0, -1)
	pkgs, err := h.deps.Index.ListPackages(ctx, repo.ID, "")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	pkgs = slices.DeleteFunc(pkgs, func(p index.Package) bool { return !strings.Contains(strings.ToLower(p.Name), text) })
	total := len(pkgs)
	objects := []searchObject{}
	for _, p := range pkgs[min(from, total):min(from+size, total)] {
		o, err := h.searchObject(r, repo, p.Name)
		if errors.Is(err, index.ErrNotFound) {
			continue // deleted since the list
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		objects = append(objects, o)
	}
	writeJSON(w, http.StatusOK, map[string]any{"objects": objects, "total": total, "time": time.Now().UTC().Format(time.RFC3339)})
}

// queryInt reads a non-negative integer parameter: def when it is missing or not one, and at most
// limit when limit is not negative.
func queryInt(v string, def, limit int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		n = def
	}
	if limit >= 0 {
		n = min(n, limit)
	}
	return n
}

// searchObject is the search answer of the package name, from its packument: a hosted repo's,
// and for a proxy repo the packument of the versions it holds, rendered from the index, so that
// a search never asks the upstream.
func (h *handlers) searchObject(r *http.Request, repo index.Repo, name string) (searchObject, error) {
	var o searchObject
	var content []byte
	if repo.Kind == index.KindProxy {
		d, err := h.plugin.renderHeld(r.Context(), repo, name, kindPackument, h.deps.Index, requestBase(r)+"/"+Name+"/"+repo.Name+"/")
		if err != nil {
			return o, err
		}
		content = d.Content
	} else {
		c, err := h.document(r, repo, name, kindPackument)
		if err != nil {
			return o, err
		}
		content = c.Content
	}
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Description string
			Keywords    json.RawMessage
		} `json:"versions"`
		Time map[string]string `json:"time"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		return o, fmt.Errorf("npm: the packument of %s: %w", name, err)
	}
	latest := doc.DistTags["latest"]
	o.Package.Name, o.Package.Version = name, latest
	o.Package.Description, o.Package.Date = doc.Versions[latest].Description, doc.Time[latest]
	o.Package.Keywords, o.Package.Maintainers = doc.Versions[latest].Keywords, []struct{}{}
	o.Score.Final, o.SearchScore = 1, 1
	o.Score.Detail = map[string]float64{"quality": 1, "popularity": 1, "maintenance": 1}
	return o, nil
}

// emptyAuditReport is the answer of npm 6's audit endpoints, which pnpm uses, for a repo that
// knows no advisory.
var emptyAuditReport = map[string]any{
	"actions": []any{}, "advisories": map[string]any{}, "muted": []any{},
	"metadata": map[string]any{
		"vulnerabilities":      map[string]int{"info": 0, "low": 0, "moderate": 0, "high": 0, "critical": 0},
		"dependencies":         0,
		"devDependencies":      0,
		"optionalDependencies": 0,
		"totalDependencies":    0,
	},
}

// audit answers npm audit and pnpm audit for a hosted repo, which knows no advisory for its
// packages: the bulk endpoint {}, the two others the empty report. The body, gzipped when
// Content-Encoding says so, is read and bounded at 16 MiB decoded (413). A proxy forwards to
// its upstream and a group the bulk request without its hosted names (ADR 0013, #310, #311):
// 501 here until then.
func (h *handlers) audit(w http.ResponseWriter, r *http.Request, repo index.Repo, bulk bool) {
	if err := format.Authorize(r.Context(), h.deps, auth.ActionRead, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	if repo.Kind != "" && repo.Kind != index.KindHosted {
		h.fail(w, r, fmt.Errorf("%w: the audit of a %s repo", format.ErrUnsupported, repo.Kind))
		return
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxAuditBody)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(body)
		if err != nil {
			h.fail(w, r, fmt.Errorf("%w: the audit body is not gzip: %w", format.ErrInvalid, err))
			return
		}
		body = zr
	}
	n, err := io.Copy(io.Discard, io.LimitReader(body, maxAuditBody+1))
	switch {
	case err != nil && errors.As(err, new(*http.MaxBytesError)):
		h.fail(w, r, err)
		return
	case err != nil:
		h.fail(w, r, fmt.Errorf("%w: the audit body: %w", format.ErrInvalid, err))
		return
	case n > maxAuditBody:
		h.fail(w, r, fmt.Errorf("the audit body is larger than %d bytes decoded: %w", maxAuditBody, &http.MaxBytesError{Limit: maxAuditBody}))
		return
	}
	if bulk {
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	writeJSON(w, http.StatusOK, emptyAuditReport)
}
