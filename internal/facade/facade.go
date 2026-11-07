package facade

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Options configures the handler New returns.
type Options struct {
	// Index is the index database: the repos, the upload sessions and oci_repos; required.
	Index index.Store
	// Providers are the backend registries by name; a repo's registry selects one.
	Providers provider.Set
	// Auth decides what the request's principal may do on a repo. nil allows everything: the
	// facade is open until the bearer tokens of #26 and RBAC (#45) set it.
	Auth auth.Authorizer
	// Ingest stores a manifest pushed to a repo Lajstrom manages (every format but oci): the
	// ingest pipeline. Required when such a repo is served; without it their manifest PUT is a
	// 500.
	Ingest Ingester
	// MaxManifestSize is the largest manifest a PUT may carry, in bytes (server.maxManifestSize);
	// zero or negative means 4 MiB, the least the distribution spec asks a registry to take.
	MaxManifestSize int64
	// Log receives the causes of the 5xx answers, with the request id; nil means
	// slog.Default().
	Log *slog.Logger
	// Now is the clock of the upload sessions; nil means time.Now.
	Now func() time.Time
}

// Ingester stores a manifest that a client pushed to a managed repo: *ingest.Pipeline. It is
// the facade's own interface because format.Publisher, the frontends' view of the pipeline, has
// Upload only.
type Ingester interface {
	Ingest(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, principal auth.Principal) (*ingest.Result, error)
}

var _ Ingester = (*ingest.Pipeline)(nil)

// defaultMaxManifestSize is Options.MaxManifestSize when it is zero: a manifest is read into
// memory, and the spec asks registries to take at least 4 MiB.
const defaultMaxManifestSize = 4 << 20

// ociFormat is the reserved format of the pass-through repos (format.Reserved): /v2 stores
// what it gets there, indexes nothing, and no frontend serves them.
const ociFormat = "oci"

// apiVersionHeader is on every answer: docker reads it on GET /v2/ to tell a registry of the
// distribution API.
const apiVersionHeader = "Docker-Distribution-API-Version"

// New returns the handler of the OCI distribution API, which the server mounts at /v2/
// (server.Options.Facade). Its endpoints (docs/spec/http-routing.md §2):
//
//	GET  /v2/                                   200 {} (the challenge of rbac.md §4 comes with #26)
//	GET  /v2/_catalog?n=&last=                  OCI names of the repos the principal may read
//	HEAD|GET /v2/<name>/manifests/<ref>         proxied to the backend
//	PUT  /v2/<name>/manifests/<ref>             pass-through on an oci repo; the hook into ingest elsewhere
//	HEAD|GET /v2/<name>/blobs/<digest>          streamed from the backend (Range and 307: #27)
//	POST /v2/<name>/blobs/uploads/              a session, a monolithic upload (?digest=) or a mount
//	PATCH|PUT|GET|DELETE /v2/<name>/blobs/uploads/<id>   the session, from any replica
//	GET  /v2/<name>/tags/list?n=&last=          from the index; from the backend on an oci repo
//	GET  /v2/<name>/referrers/<digest>          501 until #28
//	DELETE manifests and blobs                  405 until #61
//
// <name> is <format>/<repo>/<path>: the repo must exist (NAME_UNKNOWN), and the backend gets
// the OCI name <repo prefix>/<path>. Errors are the JSON of the distribution spec. The handler
// keeps no state between requests: an upload session is a row of the index, so any replica
// continues it. New panics when o.Index is nil.
func New(o Options) http.Handler {
	if o.Index == nil {
		panic("facade: New: nil Options.Index")
	}
	if o.Providers == nil {
		o.Providers = provider.Map{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxManifestSize <= 0 {
		o.MaxManifestSize = defaultMaxManifestSize
	}
	h := &handler{o: o, deps: format.Deps{Auth: o.Auth}}
	h.endpoints = map[routeKind]map[string]endpoint{
		routeManifest: {
			http.MethodGet: h.getManifest, http.MethodHead: h.getManifest,
			http.MethodPut: h.putManifest, http.MethodDelete: h.deleteManifest,
		},
		routeBlob: {
			http.MethodGet: h.getBlob, http.MethodHead: h.getBlob, http.MethodDelete: h.deleteBlob,
		},
		routeUploads: {http.MethodPost: h.startUpload},
		routeUpload: {
			http.MethodPatch: h.patchUpload, http.MethodPut: h.putUpload,
			http.MethodGet: h.uploadStatus, http.MethodDelete: h.cancelUpload,
		},
		routeTags:      {http.MethodGet: h.tags},
		routeReferrers: {http.MethodGet: h.referrers},
	}
	return h
}

// handler serves /v2/.
type handler struct {
	o         Options
	deps      format.Deps // Auth only, for format.Authorize
	endpoints map[routeKind]map[string]endpoint
}

// endpoint serves one method of a route on the repository t; arg is the reference, the digest
// or the upload id of the path. An error is answered by fail.
type endpoint func(w http.ResponseWriter, r *http.Request, t *target, arg string) error

// ServeHTTP dispatches /v2/, /v2/_catalog and the routes below /v2/<name>/.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(apiVersionHeader, "registry/2.0")
	rest, _ := strings.CutPrefix(r.URL.Path, "/v2/")
	var err error
	switch rest {
	case "", "/v2":
		err = h.base(w, r)
	case "_catalog":
		err = h.catalog(w, r)
	default:
		err = h.repository(w, r, rest)
	}
	if err != nil {
		h.fail(w, r, err)
	}
}

// base answers GET /v2/: 200 with {}. TODO(#26): 401 with the bearer challenge of rbac.md §4
// to a request without a token, once the token endpoint exists.
func (h *handler) base(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return methodNotAllowed(r)
	}
	writeJSON(w, http.StatusOK, struct{}{})
	return nil
}

// repository serves a route below /v2/<name>/: it parses the route, resolves the name to its
// repo and calls the endpoint of the method.
func (h *handler) repository(w http.ResponseWriter, r *http.Request, path string) error {
	rt, ok := parseRoute(path)
	if !ok {
		return &apiError{status: http.StatusNotFound, code: codeNameUnknown,
			msg: fmt.Sprintf("%s is not an endpoint of the distribution API", r.URL.Path)}
	}
	ep, ok := h.endpoints[rt.kind][r.Method]
	if !ok {
		return methodNotAllowed(r)
	}
	t, err := h.resolve(r.Context(), rt.name)
	if err != nil {
		return err
	}
	return ep(w, r, t, rt.arg)
}

// target is the repository a request addresses.
type target struct {
	repo index.Repo
	name string // <format>/<repo>/<path>, as the client addresses it
	path string // <path>, the package path below the repo
	oci  string // the OCI name the provider gets: the repo's prefix and <path>
}

// maxNameLen bounds <name>: clients limit host, port and name together to 255 bytes.
const maxNameLen = 255

// nameGrammar is the grammar of <name> (distribution spec, "Pulling manifests").
var nameGrammar = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)

// resolve returns the target of name: NAME_INVALID when it is not the grammar, NAME_UNKNOWN
// when it is not <format>/<repo>/<path> of a repo of the control plane (401 to the anonymous
// principal when an Authorizer is set, as the frontends answer: a denial does not tell
// whether the repo exists, rbac.md §3). One GetRepo per request.
func (h *handler) resolve(ctx context.Context, name string) (*target, error) {
	if len(name) > maxNameLen || !nameGrammar.MatchString(name) {
		return nil, &apiError{status: http.StatusBadRequest, code: codeNameInvalid,
			msg: fmt.Sprintf("invalid repository name %q", name)}
	}
	formatName, rest, _ := strings.Cut(name, "/")
	repoName, path, ok := strings.Cut(rest, "/")
	unknown := &apiError{status: http.StatusNotFound, code: codeNameUnknown,
		msg: fmt.Sprintf("repository name %s not known to registry: a name is <format>/<repo>/<path> of an existing repo", name)}
	if !ok {
		return nil, unknown
	}
	repo, err := h.o.Index.GetRepo(ctx, formatName, repoName)
	switch {
	case errors.Is(err, index.ErrNotFound) && h.o.Auth != nil && auth.PrincipalFrom(ctx).Kind == auth.KindAnonymous:
		return nil, &apiError{err: fmt.Errorf("%w: %s", format.ErrUnauthenticated, name)}
	case errors.Is(err, index.ErrNotFound):
		return nil, unknown
	case err != nil:
		return nil, fmt.Errorf("facade: repo of %s: %w", name, err)
	}
	return &target{repo: *repo, name: name, path: path, oci: ociName(*repo, path)}, nil
}

// repoBase and ociName are the OCI repository of a package as the provider names it, as
// internal/ingest computes it (pipeline.go, copied: the facade does not import ingest for two
// lines).
func repoBase(repo index.Repo) string {
	if base := strings.Trim(repo.Prefix, "/"); base != "" {
		return base
	}
	return repo.Format + "/" + repo.Name
}

func ociName(repo index.Repo, pkgPath string) string { return repoBase(repo) + "/" + pkgPath }

// authorize asks the Authorizer whether the request's principal may do a on the repo
// (format.Authorize): a principal that may not read it gets NAME_UNKNOWN, as for a repo that
// does not exist.
func (h *handler) authorize(ctx context.Context, a auth.Action, repo index.Repo) error {
	err := format.Authorize(ctx, h.deps, a, repo)
	if errors.Is(err, index.ErrNotFound) {
		return &apiError{code: codeNameUnknown, msg: fmt.Sprintf("repository %s/%s not known to registry", repo.Format, repo.Name), err: err}
	}
	if err != nil {
		return &apiError{err: err}
	}
	return nil
}

// backend returns the provider's repository of t.
func (h *handler) backend(ctx context.Context, t *target) (provider.Repository, error) {
	p, ok := h.o.Providers.Provider(t.repo.Registry)
	if !ok || p == nil {
		return nil, fmt.Errorf("facade: repo %s/%s: registry %q is not configured", t.repo.Format, t.repo.Name, t.repo.Registry)
	}
	rr, err := p.Repository(ctx, t.oci)
	if err != nil {
		return nil, fmt.Errorf("facade: repository %s: %w", t.name, err)
	}
	return rr, nil
}

// referrers answers GET …/referrers/<digest>: 501 until #28.
func (h *handler) referrers(_ http.ResponseWriter, r *http.Request, t *target, _ string) error {
	if err := h.authorize(r.Context(), auth.ActionRead, t.repo); err != nil {
		return err
	}
	return &apiError{status: http.StatusNotImplemented, code: codeUnsupported, msg: "the referrers API comes with #28"}
}
