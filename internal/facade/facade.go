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

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/metrics"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Options configures the handler New returns.
type Options struct {
	// Index is the index database: the repos, the upload sessions and oci_repos; required.
	Index index.Store
	// Providers are the backend registries by name; a repo's registry selects one.
	Providers provider.Set
	// Auth decides what the request's principal may do on a repo, for a request without a
	// bearer, and which scopes /v2/token grants: serve sets auth.RBAC. nil allows everything,
	// every requested scope included, for the tests.
	Auth auth.Authorizer
	// Tokens are the keys of the bearer tokens of /v2 (ADR 0010): keys.Tokens over the index, or
	// keys.EphemeralTokens without a master key; required.
	Tokens token.Keys
	// ExternalURL is server.externalURL: the realm of the bearer challenge is
	// <ExternalURL>/v2/token and the service its host. Empty: the request's Host over http://,
	// the dev stack (token.Realm).
	ExternalURL string
	// Ingest stores a manifest pushed to a repo Lajstrom manages (every format but oci), and a
	// referrer pushed to any repo: the ingest pipeline. Required; without it those manifest PUTs
	// are a 500.
	Ingest Ingester
	// MaxManifestSize is the largest manifest a PUT may carry, in bytes (server.maxManifestSize);
	// zero or negative means 4 MiB, the least the distribution spec asks a registry to take.
	MaxManifestSize int64
	// UploadSessionsPerRepo is how many live upload sessions one principal may hold in one repo
	// on this replica, and UploadSessionsPerPrincipal how many in all repos
	// (server.uploadSessions, quota.go); zero or negative means no bound.
	UploadSessionsPerRepo, UploadSessionsPerPrincipal int
	// MaxCreatedAhead and MaxCreatedBehind bound org.opencontainers.image.created of a version
	// pushed to a managed repo: at most this far after and before Now
	// (server.maxCreatedAhead and server.maxCreatedBehind, checkCreated). Zero or negative
	// means 5 minutes and 1 hour.
	MaxCreatedAhead, MaxCreatedBehind time.Duration
	// Log receives the causes of the 5xx answers, with the request id; nil means
	// slog.Default().
	Log *slog.Logger
	// Now is the clock of the upload sessions, of the tokens and of checkCreated; nil means
	// time.Now.
	Now func() time.Time
	// VisibilityTimeout is how long a blob that a manifest names and the backend does not
	// serve is read again before the PUT is MANIFEST_BLOB_UNKNOWN, on a registry not
	// classified strong (ingest.visibilityTimeout); a registry's consistencyBound raises it.
	// Zero or negative means 30 seconds.
	VisibilityTimeout time.Duration
	// OCIProxy serves the reads of oci proxy repos (proxy-and-group.md §11): *proxy.Core. Nil
	// serves an oci proxy repo as a hosted one, from what the backend holds.
	OCIProxy OCIProxy
}

// Ingester stores a manifest that a client pushed to a managed repo, and a referrer pushed to
// any repo, and deletes a manifest of any repo: *ingest.Pipeline. It is the facade's own
// interface because format.Publisher, the frontends' view of the pipeline, has Upload and
// Delete only.
type Ingester interface {
	Ingest(ctx context.Context, repo index.Repo, pkgPath, tag string, m ocispec.Manifest, raw []byte, principal auth.Principal) (*ingest.Result, error)
	PutReferrer(ctx context.Context, repo index.Repo, r ingest.Referrer, principal auth.Principal) (digest.Digest, error)
	Delete(ctx context.Context, repo index.Repo, del format.Deletion, principal auth.Principal) (*format.Deleted, error)
}

var _ Ingester = (*ingest.Pipeline)(nil)

// defaultMaxManifestSize is Options.MaxManifestSize when it is zero: a manifest is read into
// memory, and the spec asks registries to take at least 4 MiB.
const defaultMaxManifestSize = 4 << 20

// defaultVisibilityTimeout is Options.VisibilityTimeout when it is zero, ingest's default.
const defaultVisibilityTimeout = 30 * time.Second

// The defaults of Options.MaxCreatedAhead and MaxCreatedBehind: a client's clock may be a few
// minutes off, and oras writes the time when it packs the manifest, before it uploads the
// layers.
const (
	defaultMaxCreatedAhead  = 5 * time.Minute
	defaultMaxCreatedBehind = time.Hour
)

// ociFormat is the reserved format of the pass-through repos (format.Reserved): /v2 stores
// what it gets there, indexes nothing, and no frontend serves them.
const ociFormat = "oci"

// apiVersionHeader is on every answer: docker reads it on GET /v2/ to tell a registry of the
// distribution API.
const apiVersionHeader = "Docker-Distribution-API-Version"

// New returns the handler of the OCI distribution API, which the server mounts at /v2/
// (server.Options.Facade). Its endpoints (docs/spec/http-routing.md §2):
//
//	GET  /v2/                                   200 {} when authenticated, else 401 with the bearer challenge
//	GET  /v2/token?service=&scope=              a bearer token for the principal (rbac.md §4)
//	GET  /v2/_catalog?n=&last=                  OCI names of the repos the principal may read
//	HEAD|GET /v2/<name>/manifests/<ref>         proxied to the backend
//	PUT  /v2/<name>/manifests/<ref>             pass-through on an oci repo; the hook into ingest elsewhere
//	HEAD|GET /v2/<name>/blobs/<digest>          format.ServeBlob: 307 to the backend's storage, or streamed with Range
//	POST /v2/<name>/blobs/uploads/              a session, a monolithic upload (?digest=) or a mount
//	PATCH|PUT|GET|DELETE /v2/<name>/blobs/uploads/<id>   the session, from any replica
//	GET  /v2/<name>/tags/list?n=&last=          from the index; from the backend on an oci repo
//	GET  /v2/<name>/referrers/<digest>          the referrers, ?artifactType=: the backend's API or the tag schema
//	DELETE /v2/<name>/manifests/<ref>           ingest deletes it, its referrers first, and its rows: 202
//	DELETE /v2/<name>/blobs/<digest>            405: blobs are the backend's garbage collection's
//
// <name> is <format>/<repo>/<path>: the repo must exist (NAME_UNKNOWN), and the backend gets
// the OCI name <repo prefix>/<path>. Errors are the JSON of the distribution spec; a 401 has
// the bearer challenge. Every route but /v2/token verifies the bearer a request carries, and
// decides a request with one by its access claims. The handler keeps no state between
// requests but the verification keys, reloaded from Tokens and told the key of each token it
// issues, so that the token verifies here at once, and its count of the live upload
// sessions (quota.go), which is per replica: an upload session is a row of the index, so any
// replica continues it, and a token of one replica verifies on another; New hands that count to
// lajstrom_upload_sessions_live (metrics.CountUploadSessions). New panics when o.Index or
// o.Tokens is nil.
func New(o Options) http.Handler {
	if o.Index == nil {
		panic("facade: New: nil Options.Index")
	}
	if o.Tokens == nil {
		panic("facade: New: nil Options.Tokens")
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
	if o.VisibilityTimeout <= 0 {
		o.VisibilityTimeout = defaultVisibilityTimeout
	}
	if o.MaxCreatedAhead <= 0 {
		o.MaxCreatedAhead = defaultMaxCreatedAhead
	}
	if o.MaxCreatedBehind <= 0 {
		o.MaxCreatedBehind = defaultMaxCreatedBehind
	}
	verifier := token.NewVerifier(token.Options{Keys: o.Tokens, Now: o.Now, Log: o.Log})
	h := &handler{o: o, deps: format.Deps{Auth: o.Auth},
		issuer:   token.NewIssuer(token.Options{Keys: o.Tokens, Verifier: verifier, Now: o.Now, Log: o.Log}),
		verifier: verifier,
		uploads:  liveUploads{perRepo: o.UploadSessionsPerRepo, perPrincipal: o.UploadSessionsPerPrincipal}}
	metrics.CountUploadSessions(func() int { return h.uploads.count(o.Now()) })
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
	issuer    *token.Issuer
	verifier  *token.Verifier
	uploads   liveUploads // the live upload sessions of each principal (quota.go)
}

// endpoint serves one method of a route on the repository t; arg is the reference, the digest
// or the upload id of the path. An error is answered by fail.
type endpoint func(w http.ResponseWriter, r *http.Request, t *target, arg string) error

// ServeHTTP dispatches /v2/token, and, after the bearer of the request is verified, /v2/,
// /v2/_catalog and the routes below /v2/<name>/.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(apiVersionHeader, "registry/2.0")
	rest, _ := strings.CutPrefix(r.URL.Path, "/v2/")
	if rest == "token" {
		if err := h.issueToken(w, r); err != nil {
			h.fail(w, r, err)
		}
		return
	}
	r, err := h.bearer(r)
	if err == nil {
		switch rest {
		case "", "/v2":
			err = h.base(w, r)
		case "_catalog":
			err = h.catalog(w, r)
		default:
			err = h.repository(w, r, rest)
		}
	}
	if err != nil {
		h.fail(w, r, err)
	}
}

// base answers GET /v2/: 200 with {} to an authenticated request, by Basic credentials or a
// bearer (an anonymous token included: its client has done the flow), and 401 with the bearer
// challenge to the anonymous principal, which docker and oras answer with GET /v2/token: the
// anonymous client then gets a token with what anonymous read allows it, and one that logs in
// a token of its user.
func (h *handler) base(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return methodNotAllowed(r)
	}
	if _, bearer := grantFrom(r.Context()); !bearer && auth.PrincipalFrom(r.Context()).Kind == auth.KindAnonymous {
		return &apiError{status: http.StatusUnauthorized, msg: "authentication required: get a token from the realm of the challenge"}
	}
	writeJSON(w, http.StatusOK, struct{}{})
	return nil
}

// repository serves a route below /v2/<name>/: it parses the route, resolves the name to its
// repo and calls the endpoint of the method. A bearer is asked first whether it names the
// repository (namedByToken), before the repo is looked up, so that the answer does not tell
// whether it exists; a name that is not a repository name is resolve's to answer, by the name
// alone.
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
	if g, bearer := grantFrom(r.Context()); bearer && token.ValidName(rt.name) {
		if err := h.namedByToken(r.Context(), g, rt.name); err != nil {
			return err
		}
	}
	t, err := h.resolve(r.Context(), rt.name)
	if err != nil {
		return err
	}
	if t.repo.Kind == index.KindGroup { // an oci group: the read check, then the route (ocigroup.go)
		if t, err = h.throughGroup(w, r, t, rt.kind); err != nil {
			return err
		}
	}
	return ep(w, r, t, rt.arg)
}

// target is the repository a request addresses. Through a group of oci repos, repo is the member
// the group's routes chose and via the group (ocigroup.go); name stays the client's.
type target struct {
	repo index.Repo
	via  *index.Repo // the group of oci repos the request came through; nil otherwise
	name string      // <format>/<repo>/<path>, as the client addresses it
	path string      // <path>, the package path below the repo
	oci  string      // the OCI name the provider gets: the repo's prefix and <path>
}

// maxNameLen bounds <name>: clients limit host, port and name together to 255 bytes.
const maxNameLen = 255

// nameGrammar is the grammar of <name> (distribution spec, "Pulling manifests").
var nameGrammar = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)

// resolve returns the target of name: NAME_INVALID when it is not the grammar, NAME_UNKNOWN
// when it is not <format>/<repo>/<path>, and repoUnknown when it is not of a repo of the
// control plane or is of a group that is not of oci repos, which has no OCI repository
// (docs/spec/http-routing.md §6). A group of oci repos resolves to itself, with the path the
// client sent: throughGroup routes it after the read check. A proxy repo resolves as a hosted
// one: the push it refuses is putManaged's (proxyRefusal). One GetRepo per request.
func (h *handler) resolve(ctx context.Context, name string) (*target, error) {
	if len(name) > maxNameLen || !nameGrammar.MatchString(name) {
		return nil, &apiError{status: http.StatusBadRequest, code: codeNameInvalid,
			msg: fmt.Sprintf("invalid repository name %q", name)}
	}
	formatName, rest, _ := strings.Cut(name, "/")
	repoName, path, ok := strings.Cut(rest, "/")
	if !ok {
		return nil, nameUnknown(name)
	}
	// A package path with a component that is a file of the backend's image layout collides with
	// another repository's layout and the backend answers it a permanent 5xx (profile
	// .LayoutFileComponent, zot#4695). NAME_INVALID is decided by the name alone, so it is
	// returned before the repo lookup, as the grammar check above is, and does not reveal whether
	// the repo exists. Refusing it here keeps a per-name 5xx from counting against the registry's
	// circuit breaker, which would fail every other repo on that registry for a while.
	if c, bad := reservedPathComponent(path); bad {
		return nil, &apiError{status: http.StatusBadRequest, code: codeNameInvalid,
			msg: fmt.Sprintf("invalid repository name %q: the path component %q is a file of a registry's image layout", name, c)}
	}
	repo, err := h.o.Index.GetRepo(ctx, formatName, repoName)
	switch {
	case errors.Is(err, index.ErrNotFound), err == nil && repo.Kind == index.KindGroup && !h.ociGroup(*repo):
		return nil, h.repoUnknown(ctx, name)
	case err != nil:
		return nil, fmt.Errorf("facade: repo of %s: %w", name, err)
	case repo.Kind == index.KindGroup:
		return &target{repo: *repo, name: name, path: path}, nil
	}
	if h.o.OCIProxy != nil && repo.Format == ociFormat && repo.Kind == index.KindProxy {
		path = h.o.OCIProxy.OCIPath(*repo, path) // library/ of Docker Hub, on every route (§11.7)
	}
	return &target{repo: *repo, name: name, path: path, oci: ociName(*repo, path)}, nil
}

// repoUnknown is the answer to the repository name of a repo that does not exist: NAME_UNKNOWN,
// and 401 to the anonymous principal when an Authorizer is set, as the frontends answer, so
// that a denial does not tell whether the repo exists (docs/spec/rbac.md §3).
func (h *handler) repoUnknown(ctx context.Context, name string) error {
	if h.o.Auth != nil && auth.PrincipalFrom(ctx).Kind == auth.KindAnonymous {
		return &apiError{err: fmt.Errorf("%w: %s", format.ErrUnauthenticated, name)}
	}
	return nameUnknown(name)
}

// nameUnknown is NAME_UNKNOWN for name.
func nameUnknown(name string) error {
	return &apiError{status: http.StatusNotFound, code: codeNameUnknown,
		msg: fmt.Sprintf("repository name %s not known to registry: a name is <format>/<repo>/<path> of an existing repo", name)}
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

// reservedPathComponent returns the first component of a package path that is a file of a
// registry's image layout (profile.LayoutFileComponent), and whether there is one.
func reservedPathComponent(path string) (string, bool) {
	for _, c := range strings.Split(path, "/") {
		if profile.LayoutFileComponent(c) {
			return c, true
		}
	}
	return "", false
}

// authorize decides whether the request may do a on the repository t. A request with a bearer
// is decided by the token's access claims and not by the Authorizer (allowedByToken: 403 when
// the token lacks the action; repository has answered a token that does not name t, or names
// it without any action).
// Without one the Authorizer decides for the request's principal (format.Authorize): a
// principal that may not read the repo gets the answer of a repo that does not exist, word for
// word: NAME_UNKNOWN (nameUnknown), and to the anonymous principal 401 (repoUnknown). Through a
// group of oci repos the group is asked, never its member: read on the group is the whole
// permission (docs/spec/rbac.md §7); a token names the group's path, t.name.
func (h *handler) authorize(ctx context.Context, a auth.Action, t *target) error {
	if g, ok := grantFrom(ctx); ok {
		return h.allowedByToken(ctx, g, a, t.name)
	}
	repo := t.repo
	if t.via != nil {
		repo = *t.via
	}
	err := format.Authorize(ctx, h.deps, a, repo)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nameUnknown(t.name)
	case errors.Is(err, format.ErrUnauthenticated):
		return h.repoUnknown(ctx, t.name)
	case err != nil:
		return &apiError{err: err}
	}
	return nil
}

// registry returns the provider of t's repo.
func (h *handler) registry(t *target) (provider.Provider, error) {
	p, ok := h.o.Providers.Provider(t.repo.Registry)
	if !ok || p == nil {
		return nil, fmt.Errorf("facade: repo %s/%s: registry %q is not configured", t.repo.Format, t.repo.Name, t.repo.Registry)
	}
	return p, nil
}

// backend returns the provider's repository of t.
func (h *handler) backend(ctx context.Context, t *target) (provider.Repository, error) {
	p, err := h.registry(t)
	if err != nil {
		return nil, err
	}
	rr, err := p.Repository(ctx, t.oci)
	if err != nil {
		return nil, fmt.Errorf("facade: repository %s: %w", t.name, err)
	}
	return rr, nil
}
